// Package router 实现路由引擎：任务分类 → 硬约束过滤 → 拉格朗日成本-质量打分
// （+ Thompson Sampling 在线探索）→ 决策。state.go 持有全部在线学习状态。
// 并发模型：单进程内所有请求共享状态，用一把互斥锁保护——状态更新是 O(1)，
// 锁竞争远小于上游网络调用耗时，不值得为此上分片锁（复杂度换不来可观测收益）。
package router

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"

	"github.com/attnstack/omni-route/internal/config"
)

// modelKeyOf 生成 (task, model) 的状态键。用字符串而非结构体：
// 它要作为 JSON map key 持久化（结构体键无法序列化）。
func modelKeyOf(task, model string) string {
	return task + "\x1f" + model
}

// modelStat 是单个 (task, model) 的在线学习状态。
type modelStat struct {
	Quality   float64 `json:"quality"`    // EMA，[0,1]
	LatencyMs float64 `json:"latency_ms"` // EWMA，毫秒
	BetaA     float64 `json:"beta_a"`     // Thompson 后验 Beta(a,b)：好结果
	BetaB     float64 `json:"beta_b"`     // 坏结果
	Requests  int64   `json:"requests"`
}

// taskStat 是任务级状态：滚动消费窗口（驱动 λ 自适应）与每任务 λ 值。
type taskStat struct {
	Ring   []float64 `json:"ring"` // 最近 spendWindow 次请求的实际成本
	Idx    int       `json:"idx"`
	Filled int       `json:"filled"`
	Sum    float64   `json:"sum"`
	Lambda float64   `json:"lambda"`
}

// State 是路由的全部在线状态。快照持久化到磁盘，重启不丢学习成果。
type State struct {
	mu     sync.Mutex
	models map[string]*modelStat
	tasks  map[string]*taskStat

	spendWindow  int
	budget       map[string]float64
	lambdaCfg    config.LambdaCfg
	qualityAlpha float64
	latencyAlpha float64
}

// qualityInit 是冷启动质量先验。取 0.7 而非 0.5：给模型一次证明机会，
// 避免新模型因悲观先验永远选不中；错误会被反馈闭环快速拉回。
const qualityInit = 0.7

func NewState(cfg *config.Config) *State {
	s := &State{
		models:       map[string]*modelStat{},
		tasks:        map[string]*taskStat{},
		spendWindow:  cfg.Routing.SpendWindow,
		budget:       map[string]float64{},
		lambdaCfg:    cfg.Routing.Lambda,
		qualityAlpha: 0.2, // α=0.2，兼顾响应速度与抗噪
		latencyAlpha: 0.3,
	}
	for task, b := range cfg.Routing.BudgetPerTask {
		s.budget[task] = b
	}
	for _, task := range allKnownTasks(cfg) {
		s.tasks[task] = &taskStat{
			Ring:   make([]float64, s.spendWindow),
			Lambda: cfg.Routing.Lambda.Init,
		}
	}
	return s
}

func allKnownTasks(cfg *config.Config) []string {
	seen := map[string]bool{}
	var out []string
	for task := range cfg.Routing.BudgetPerTask {
		if !seen[task] {
			seen[task] = true
			out = append(out, task)
		}
	}
	for task := range cfg.Cascades {
		if !seen[task] {
			seen[task] = true
			out = append(out, task)
		}
	}
	return out
}

func (s *State) model(task, model string) *modelStat {
	k := modelKeyOf(task, model)
	m, ok := s.models[k]
	if !ok {
		m = &modelStat{Quality: qualityInit, BetaA: 1, BetaB: 1}
		s.models[k] = m
	}
	return m
}

// Quality 返回点估计（展示用；决策路径请用 SampleQuality 做探索）。
func (s *State) Quality(task, model string) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.model(task, model).Quality
}

// Lambda 返回任务当前的拉格朗日乘子。
func (s *State) Lambda(task string) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.tasks[task]; ok {
		return t.Lambda
	}
	return s.lambdaCfg.Init
}

// LatencyMs 返回延迟 EWMA（无数据时 0，调用方需自行处理）。
func (s *State) LatencyMs(task, model string) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.model(task, model).LatencyMs
}

// SampleQuality 从 Beta 后验采样质量分——Thompson Sampling 的探索来源。
// 返回 (sample, ok)；ok=false 表示样本太少（<3 次），退回点估计。
// Beta(a,b) 采样用两个 Gamma 之比（Marsaglia-Tsang 拒绝采样），
// O(1) 且无第三方依赖，见 bandit.go。
func (s *State) SampleQuality(task, model string, rng func() float64) (float64, bool) {
	s.mu.Lock()
	m := s.model(task, model)
	a, b := m.BetaA, m.BetaB
	n := m.Requests
	s.mu.Unlock()
	if n < 3 {
		return m.Quality, false
	}
	return sampleBeta(a, b, rng), true
}

// RecordOutcome 在请求结束后更新全部在线状态。reward∈[0,1]。
func (s *State) RecordOutcome(task, model string, reward, costUSD float64, latencyMs int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	m := s.model(task, model)
	m.Quality = (1-s.qualityAlpha)*m.Quality + s.qualityAlpha*reward
	if m.Quality < 0 {
		m.Quality = 0
	}
	if m.Quality > 1 {
		m.Quality = 1
	}
	// Beta 共轭更新接受连续伪观测（指数族标准做法），
	// 让"部分好"的结果也能渐进地改变后验，而不是二值跳变。
	m.BetaA += reward
	m.BetaB += 1 - reward
	if latencyMs > 0 {
		if m.LatencyMs == 0 {
			m.LatencyMs = float64(latencyMs)
		} else {
			m.LatencyMs = (1-s.latencyAlpha)*m.LatencyMs + s.latencyAlpha*float64(latencyMs)
		}
	}
	m.Requests++

	t, ok := s.tasks[task]
	if !ok {
		return
	}
	if len(t.Ring) == 0 {
		return
	}
	// 滚动窗口 O(1) 更新：出窗减、入窗加。
	if t.Filled == len(t.Ring) {
		t.Sum -= t.Ring[t.Idx]
	}
	t.Ring[t.Idx] = costUSD
	t.Sum += costUSD
	t.Idx = (t.Idx + 1) % len(t.Ring)
	if t.Filled < len(t.Ring) {
		t.Filled++
	}
	s.adaptLambdaLocked(task, t)
}

// ApplyFeedback 把用户显式反馈写入质量状态。负反馈权重更大：
// 主动点踩的用户已经替你做了一次"判别器"。
func (s *State) ApplyFeedback(task, model string, good bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.model(task, model)
	if good {
		m.Quality = math.Min(1, m.Quality+0.1)
		m.BetaA += 1
	} else {
		m.Quality = math.Max(0, m.Quality-0.2)
		m.BetaB += 1
	}
}

// ApplyQualitySignal 只更新质量 EMA 与 Beta 后验，不触碰消费窗口——
// 供异步 LLM judge 抽检回写（judge 的调用成本不属于请求本身的成本）。
func (s *State) ApplyQualitySignal(task, model string, score float64) {
	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.model(task, model)
	m.Quality = (1-s.qualityAlpha)*m.Quality + s.qualityAlpha*score
	m.BetaA += score
	m.BetaB += 1 - score
}

// adaptLambdaLocked：λ 自适应。
// ratio>1 说明预算消耗超前 → λ 上调 → 成本项惩罚加重 → 路由偏向便宜模型；
// ratio<1 相反。每次请求后 O(1) 更新，等效于方案里"每分钟更新"的连续化。
func (s *State) adaptLambdaLocked(task string, t *taskStat) {
	budget, ok := s.budget[task]
	if !ok || budget <= 0 {
		return
	}
	windowBudget := budget * float64(len(t.Ring)) / 1000.0
	if windowBudget <= 0 {
		return
	}
	ratio := t.Sum / windowBudget
	t.Lambda *= 1 + s.lambdaCfg.Eta*(ratio-1)
	if t.Lambda < s.lambdaCfg.Min {
		t.Lambda = s.lambdaCfg.Min
	}
	if t.Lambda > s.lambdaCfg.Max {
		t.Lambda = s.lambdaCfg.Max
	}
}

// SpendAndLambda 暴露任务级消费与 λ，供 /api/stats 展示。
func (s *State) SpendAndLambda(task string) (spend float64, window int, lambda float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[task]
	if !ok {
		return 0, s.spendWindow, s.lambdaCfg.Init
	}
	return t.Sum, len(t.Ring), t.Lambda
}

// QualityTable 导出全量 (task, model) 质量表，供 dashboard 与调试。
func (s *State) QualityTable() map[string]map[string]float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]map[string]float64{}
	for k, m := range s.models {
		task, model, ok := strings.Cut(k, "\x1f")
		if !ok {
			continue
		}
		if out[task] == nil {
			out[task] = map[string]float64{}
		}
		out[task][model] = math.Round(m.Quality*1000) / 1000
	}
	return out
}

// snapshot 是持久化文件的结构。
type snapshot struct {
	Version int                   `json:"version"`
	Models  map[string]*modelStat `json:"models"`
	Tasks   map[string]*taskStat  `json:"tasks"`
}

// Save 把状态落盘。失败只返回错误由调用方记日志——状态丢失可接受，
// 阻塞关闭流程不可接受。
func (s *State) Save(path string) error {
	s.mu.Lock()
	data, err := json.MarshalIndent(snapshot{Version: 1, Models: s.models, Tasks: s.tasks}, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("router state: marshal: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("router state: write: %w", err)
	}
	// 临时文件 + rename：避免写一半崩溃留下损坏快照。
	return os.Rename(tmp, path)
}

// Load 恢复状态；文件不存在或损坏则从零开始（返回 nil 表示没有可恢复状态）。
func (s *State) Load(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil // 首次启动，不算错误
	}
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return fmt.Errorf("router state: parse %s: %w", path, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, m := range snap.Models {
		if m != nil && m.Quality >= 0 && m.Quality <= 1 && m.BetaA >= 0 && m.BetaB >= 0 {
			s.models[k] = m
		}
	}
	for task, t := range snap.Tasks {
		if t == nil || len(t.Ring) != s.spendWindow {
			continue // 窗口大小变了就不恢复，避免 sum 与 ring 不一致
		}
		t.Sum = 0
		for _, v := range t.Ring {
			t.Sum += v
		}
		if t.Lambda <= 0 {
			t.Lambda = s.lambdaCfg.Init
		}
		s.tasks[task] = t
	}
	return nil
}
