// router.go：路由决策主流程。
// 决策管线：显式覆盖检查 → 任务分类 → 硬约束过滤（能力/合规/上下文长度）
// → 拉格朗日打分（Q − λC，Q 可来自 Beta 后验采样=Thompson 探索）→ 取最大。
// 拉格朗日是"骨架"——保证成本约束不被突破；Bandit 是"贴优"——
// 在骨架内做探索利用。两层解耦，各自可关、可测、可解释。
package router

import (
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"sort"
	"strings"

	"github.com/attnstack/omni-route/internal/classifier"
	"github.com/attnstack/omni-route/internal/config"
	"github.com/attnstack/omni-route/internal/provider"
	"github.com/attnstack/omni-route/internal/tokens"
	"github.com/attnstack/omni-route/internal/types"
)

// Hints 是请求头带来的路由提示。
type Hints struct {
	TaskOverride string // x-or-task
	DataClass    string // x-or-data-class：命中合规组时收窄 provider 集合
}

// Candidate 是一个可行模型及其打分。
type Candidate struct {
	Model   string  `json:"model"`
	Cost    float64 `json:"cost"`    // 预估单请求 USD
	Quality float64 `json:"quality"` // 采样或点估计的质量分
	Score   float64 `json:"score"`
}

// Decision 是一次路由决策的全部信息——直接映射到 x-or-route-reason 响应头。
type Decision struct {
	Task           classifier.Task
	TaskConfidence float64
	Override       bool   // 用户显式指定了任务类型
	Tier           string // explicit | direct | cascade
	Chosen         string
	Reason         string      // explicit | quality-optimal | cost-optimal | budget-balanced | explore | fallback | cascade-start
	Candidates     []Candidate // 排序后的可行集，failover 顺延用
	Lambda         float64
}

// RouteReasonHeader 把决策序列化成 x-or-route-reason 头。
// 格式刻意保持 key=value 逗号分隔——curl 里肉眼可读，脚本里可 split。
func (d *Decision) RouteReasonHeader(finalModel string, upgraded bool) string {
	parts := []string{
		"task=" + string(d.Task),
		"tier=" + d.Tier,
		"model=" + finalModel,
		fmt.Sprintf("upgraded=%v", upgraded),
		"why=" + d.Reason,
	}
	if d.Override {
		parts = append(parts, "override=true")
	}
	return strings.Join(parts, ",")
}

// Router 是无状态决策器（状态都在 State 里，热加载只换 cfg 引用）。
type Router struct {
	cfg    *config.Config
	reg    *provider.Registry
	state  *State
	rng    *rand.Rand
	logger *slog.Logger
}

func New(cfg *config.Config, reg *provider.Registry, state *State, seed int64, logger *slog.Logger) *Router {
	return &Router{
		cfg:    cfg,
		reg:    reg,
		state:  state,
		rng:    rand.New(rand.NewSource(seed)),
		logger: logger,
	}
}

func (r *Router) State() *State                { return r.state }
func (r *Router) Registry() *provider.Registry { return r.reg }
func (r *Router) NextRandom() float64          { return r.rng.Float64() }

// Decide 是路由入口。任何情况下都不 panic、不阻塞在网络上——
// 它只读内存状态，耗时应为微秒级（见 BenchmarkDecide）。
func (r *Router) Decide(req *types.Request, hints Hints) (*Decision, error) {
	cls := classifier.Classify(req.Messages)
	task := cls.Task
	override := false
	if hints.TaskOverride != "" {
		if !classifier.Valid(hints.TaskOverride) {
			return nil, fmt.Errorf("invalid x-or-task %q", hints.TaskOverride)
		}
		task = classifier.Task(hints.TaskOverride)
		override = true
	}

	// 1) 显式模型：跳过路由，但保留分类信息用于观测与反馈闭环
	if req.Model != "" && req.Model != "auto" {
		if _, ok := r.reg.Model(req.Model); !ok {
			return nil, fmt.Errorf("model %q not registered", req.Model)
		}
		return &Decision{
			Task: task, TaskConfidence: cls.Confidence, Override: override,
			Tier: "explicit", Chosen: req.Model, Reason: "explicit",
			Candidates: []Candidate{{Model: req.Model}},
		}, nil
	}

	// 2) 候选池：cascade 模式优先用该任务的级联链（保序），
	//    否则用全量模型（按名称排序保证确定性）
	var pool []string
	if r.cfg.Routing.Mode == "cascade" {
		if chain, ok := r.cfg.Cascades[string(task)]; ok {
			pool = chain
		}
	}
	if pool == nil {
		for _, m := range r.reg.Models() {
			pool = append(pool, m.Name)
		}
	}

	// 3) 硬约束过滤：不满足直接出局，不参与打分
	estIn := tokens.EstimateMessages(req.Messages)
	estOut := estimateOutputTokens(req, estIn)
	allowedProviders := r.complianceProviders(hints.DataClass)
	var feasible []Candidate
	for _, name := range pool {
		info, ok := r.reg.Model(name)
		if !ok {
			continue
		}
		if allowedProviders != nil && !allowedProviders[info.Provider] {
			continue
		}
		if len(req.Tools) > 0 && !info.Caps.Tools {
			continue
		}
		if info.Caps.MaxContextTokens > 0 && estIn+estOut > info.Caps.MaxContextTokens {
			continue // 超长任务不能路由给小窗口模型——宁可报错也不烧一次注定截断的调用
		}
		cost, _ := r.reg.Cost(name, estIn, estOut)
		feasible = append(feasible, Candidate{Model: name, Cost: cost})
	}
	if len(feasible) == 0 {
		return nil, types.ErrNoCandidate
	}

	cfg := r.cfg
	switch cfg.Routing.Mode {
	case "direct":
		return r.decideDirect(task, cls, override, feasible)
	case "lagrangian":
		return r.decideScored(task, cls, override, feasible)
	default: // cascade
		// 级联模式：首个可行模型开跑，升级顺序就是链序。
		// 若过滤后只剩一个可行模型，级联退化为 direct，如实标注 tier。
		if len(feasible) == 1 {
			d, err := r.decideScored(task, cls, override, feasible)
			if err == nil {
				d.Tier = "cascade"
			}
			return d, err
		}
		return &Decision{
			Task: task, TaskConfidence: cls.Confidence, Override: override,
			Tier: "cascade", Chosen: feasible[0].Model, Reason: "cascade-start",
			Candidates: feasible,
			Lambda:     r.state.Lambda(string(task)),
		}, nil
	}
}

func (r *Router) decideDirect(task classifier.Task, cls classifier.Result, override bool, feasible []Candidate) (*Decision, error) {
	pick := func(name string) (*Candidate, bool) {
		for i := range feasible {
			if feasible[i].Model == name {
				return &feasible[i], true
			}
		}
		return nil, false
	}
	if c, ok := pick(r.cfg.Routing.TaskDefaults[string(task)]); ok {
		return &Decision{
			Task: task, TaskConfidence: cls.Confidence, Override: override,
			Tier: "direct", Chosen: c.Model, Reason: "task-default", Candidates: feasible,
		}, nil
	}
	if c, ok := pick(r.cfg.Routing.DefaultModel); ok {
		return &Decision{
			Task: task, TaskConfidence: cls.Confidence, Override: override,
			Tier: "direct", Chosen: c.Model, Reason: "default", Candidates: feasible,
		}, nil
	}
	// 兜底：最便宜可行——direct 模式没有更多信息时，省钱是更安全的默认
	best := feasible[0]
	for _, c := range feasible {
		if c.Cost < best.Cost {
			best = c
		}
	}
	return &Decision{
		Task: task, TaskConfidence: cls.Confidence, Override: override,
		Tier: "direct", Chosen: best.Model, Reason: "fallback-cheapest", Candidates: feasible,
	}, nil
}

// decideScored 是 lagrangian 模式与 cascade 末级兜底共用的打分选择。
func (r *Router) decideScored(task classifier.Task, cls classifier.Result, override bool, feasible []Candidate) (*Decision, error) {
	lambda := r.state.Lambda(string(task))
	taskKey := string(task)
	useBandit := r.cfg.Routing.UseBandit == nil || *r.cfg.Routing.UseBandit
	latencyWeight := r.cfg.Routing.LatencyWeight

	for i := range feasible {
		c := &feasible[i]
		q := r.state.Quality(taskKey, c.Model)
		// Thompson 探索：样本太少时退回点估计，避免冷启动纯随机抖动
		if useBandit {
			if sq, ok := r.state.SampleQuality(taskKey, c.Model, r.rng.Float64); ok {
				q = sq
			}
		}
		c.Quality = q
		c.Score = q - lambda*c.Cost
		if latencyWeight > 0 {
			if lat := r.state.LatencyMs(taskKey, c.Model); lat > 0 {
				c.Score -= latencyWeight * lat / 1000.0
			}
		}
	}
	sort.SliceStable(feasible, func(i, j int) bool { return feasible[i].Score > feasible[j].Score })
	chosen := feasible[0]

	// 可解释性：给"为什么选它"一个诚实的标签
	reason := explainChoice(chosen, feasible, lambda)
	return &Decision{
		Task: task, TaskConfidence: cls.Confidence, Override: override,
		Tier: "lagrangian", Chosen: chosen.Model, Reason: reason,
		Candidates: feasible, Lambda: lambda,
	}, nil
}

// explainChoice 从可行集的客观关系推导 reason，而不是拍脑袋写标签。
// 并列最高质量（如全员冷启动 0.7）不算 quality-optimal——
// 那时真正起作用的是成本项，标签必须诚实反映决策依据。
func explainChoice(chosen Candidate, all []Candidate, lambda float64) string {
	maxQ, minC, qTies := math.Inf(-1), math.Inf(1), 0
	for _, c := range all {
		if c.Quality > maxQ {
			maxQ = c.Quality
			qTies = 1
		} else if c.Quality == maxQ {
			qTies++
		}
		if c.Cost < minC {
			minC = c.Cost
		}
	}
	switch {
	case chosen.Quality >= maxQ && qTies == 1:
		return "quality-optimal"
	case chosen.Cost <= minC:
		return "cost-optimal"
	default:
		return fmt.Sprintf("budget-balanced(λ=%.1f)", lambda)
	}
}

// complianceProviders 返回数据类别允许的 provider 集合；无匹配组时返回 nil（不限制）。
func (r *Router) complianceProviders(dataClass string) map[string]bool {
	if dataClass == "" {
		return nil
	}
	providers, ok := r.cfg.Routing.ComplianceGroups[dataClass]
	if !ok {
		return nil
	}
	set := map[string]bool{}
	for _, p := range providers {
		set[p] = true
	}
	return set
}

// EstimateCostForRequest 用真实 usage 结算成本；usage 缺失时本地估算并打标。
func (r *Router) EstimateCostForRequest(model string, usage types.Usage, promptCharsEstimate int) (float64, types.Usage) {
	if usage.PromptTokens == 0 {
		usage.PromptTokens = promptCharsEstimate
		usage.Estimated = true
	}
	if usage.CompletionTokens == 0 {
		usage.Estimated = true
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	cost, _ := r.reg.Cost(model, usage.PromptTokens, usage.CompletionTokens)
	return cost, usage
}

func estimateOutputTokens(req *types.Request, estIn int) int {
	if req.MaxTokens > 0 {
		return req.MaxTokens
	}
	// 无显式上限时假设输出 ≈ 输入的一半，且不少于 64——
	// 低估输出会系统性偏向贵模型（成本被低估），高估则偏向便宜模型。
	est := estIn / 2
	if est < 64 {
		est = 64
	}
	return est
}

// ReportOutcome 把请求结果写回在线状态（质量 EMA / Beta 后验 / λ / 延迟）。
func (r *Router) ReportOutcome(task, model string, reward, costUSD float64, latencyMs int64) {
	if !classifier.Valid(task) {
		task = string(classifier.Chat)
	}
	r.state.RecordOutcome(task, model, reward, costUSD, latencyMs)
}
