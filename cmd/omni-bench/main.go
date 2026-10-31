// omni-bench：成本/质量策略对比基准。支持两种质量口径：
//
//  1. mock 口径（默认）：mock provider 的确定性标准答案为 oracle，
//     准确率 = 内容与 oracle 完全一致。可复现、零成本，用于 CI 与回归。
//  2. 真实模型口径（-judge <model>）：用指定模型做 LLM-as-judge 评分（0~1），
//     输出 judge 均分与通过率。judge 是代理指标（会偏袒同源模型），仅作方向性结论。
//
// 四种策略：premium（永远基线模型）/ cheap（永远最便宜）/ cascade（判别升级）/
// lagrangian（Q−λC 打分直选）。输出四要素口径（测试集/样本量/基线/显著性）。
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/attnstack/omni-route/internal/cascade"
	"github.com/attnstack/omni-route/internal/config"
	"github.com/attnstack/omni-route/internal/executor"
	"github.com/attnstack/omni-route/internal/provider"
	"github.com/attnstack/omni-route/internal/router"
	"github.com/attnstack/omni-route/internal/types"
)

type benchItem struct {
	ID     string `json:"id"`
	Task   string `json:"task"`
	Prompt string `json:"prompt"`
}

type policyResult struct {
	Name       string
	Requests   int
	Correct    int // oracle 口径：与标准答案完全一致
	JudgeSum   float64
	JudgeN     int
	JudgePass  int // judge 口径：score ≥ 0.6
	CostUSD    float64
	Upgrades   int
	Legs       int
	LatencySum time.Duration
}

// benchOpts 控制质量口径与输出上限。
type benchOpts struct {
	judge       *cascade.LLMJudge // nil = mock oracle 口径
	maxTokens   int               // >0 时给每个策略请求设置输出上限（真实模型必须设，否则长答案拖垮总时长）
	temperature *float64          // 非 nil 时固定采样温度（0 = 近确定性，测量卫生的关键）
	detail      bool              // 输出逐项明细（定位判别器在哪类任务上失灵）
}

// itemDetail 是单条任务在单个策略下的一次完整记录（-detail 时输出）。
type itemDetail struct {
	Policy     string   `json:"policy"`
	ID         string   `json:"id"`
	Task       string   `json:"task"`
	Model      string   `json:"model"`
	Legs       int      `json:"legs"`
	Upgraded   bool     `json:"upgraded"`
	LegsReason []string `json:"legs_reason,omitempty"` // 每腿的判别 verdict（定位判别器行为）
	CostUSD    float64  `json:"cost_usd"`
	LatencyMs  int64    `json:"latency_ms"`
	Score      float64  `json:"score"` // judge 或 oracle 二值；Skip 时 -1
	Error      string   `json:"error,omitempty"`
}

func main() {
	configPath := flag.String("config", "config.demo.yaml", "基准用配置")
	datasetPath := flag.String("dataset", "data/bench_tasks.jsonl", "任务数据集（JSONL）")
	policies := flag.String("policies", "premium,cheap,cascade,lagrangian", "要跑的策略，逗号分隔")
	realDelay := flag.Bool("real-delay", false, "保留 mock 的模拟延迟与限流（真实 provider 不受此参数影响，延迟本来就是真的）")
	judgeModel := flag.String("judge", "", "真实模型口径：用该模型做 LLM-as-judge 评分（如 qwen2.5:7b）；留空 = mock oracle 口径")
	maxTokens := flag.Int("max-tokens", 0, "每个请求的输出 token 上限（真实模型建议 120~220，控制总时长）")
	limit := flag.Int("limit", 0, "只取数据集前 N 条（冒烟用）")
	temperature := flag.Float64("temperature", -1, "固定采样温度（0 = 近确定性；<0 = 不设置，用 provider 默认）")
	detail := flag.Bool("detail", false, "输出逐项明细 JSON（分析判别器失灵点用）")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal("load config: %v", err)
	}
	if !*realDelay {
		// 去掉 mock 的模拟延迟与限流：令牌桶（默认 600RPM=10QPS）会给每个请求
		// 灌 ~100ms 假延迟。真实 provider 的延迟是物理事实，不受此开关影响。
		for name := range cfg.Providers {
			if cfg.Providers[name].Kind == "mock" {
				m := cfg.Providers[name]
				m.Mock.LatencyMs, m.Mock.JitterMs, m.Mock.StreamChunkDelayMs = 0, 0, 0
				m.RPM = 1_000_000
				cfg.Providers[name] = m
			}
		}
	}

	items := loadDataset(*datasetPath)
	if *limit > 0 && *limit < len(items) {
		items = items[:*limit]
	}
	if len(items) == 0 {
		fatal("dataset %s is empty or unreadable", *datasetPath)
	}
	// cascade 策略要求每个任务类型都有级联链
	if wantCascade(strings.Split(*policies, ",")) {
		if missing := missingCascadeTasks(cfg, items); len(missing) > 0 {
			fmt.Fprintf(os.Stderr, "skip cascade: no chain for tasks %v\n", missing)
			*policies = strings.ReplaceAll(*policies, "cascade", "")
		}
	}

	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 16}}
	reg, err := provider.Build(cfg, client, logger)
	if err != nil {
		fatal("build registry: %v", err)
	}

	opts := benchOpts{maxTokens: *maxTokens, detail: *detail}
	if *temperature >= 0 {
		t := *temperature
		opts.temperature = &t
	}
	if *judgeModel != "" {
		if _, ok := reg.Model(*judgeModel); !ok {
			fatal("judge model %q not registered in config", *judgeModel)
		}
		// 评分 judge 用独立 cfg 副本启用 LLM judge——级联 Checker 保持原配置
		//（级联判别用零成本档），评分只发生在 benchmark 的度量环节。
		judgeCfg := *cfg
		judgeCfg.Cascade.Confidence.LLMJudge.Enabled = true
		judgeCfg.Cascade.Confidence.LLMJudge.Model = *judgeModel
		judgeCfg.Cascade.Confidence.LLMJudge.TimeoutMs = 120000
		exec := executor.New(reg, &judgeCfg, cfg.Seed, logger)
		opts.judge = cascade.NewLLMJudge(&judgeCfg, exec, nil)
	}

	want := map[string]bool{}
	for _, p := range strings.FieldsFunc(*policies, func(r rune) bool { return r == ',' || r == ' ' }) {
		if p = strings.TrimSpace(p); p != "" {
			want[p] = true
		}
	}
	results := []policyResult{}
	allDetails := []itemDetail{}
	for _, name := range []string{"premium", "cheap", "cascade", "lagrangian"} {
		if !want[name] {
			continue
		}
		fmt.Fprintf(os.Stderr, "[%s] running %d tasks...\n", name, len(items))
		res, details := runPolicy(name, cfg, reg, items, logger, opts)
		results = append(results, res)
		allDetails = append(allDetails, details...)
	}

	if opts.detail {
		blob, _ := json.MarshalIndent(allDetails, "", "  ")
		fmt.Println("# per-item details")
		fmt.Println(string(blob))
	}
	printReport(cfg, *datasetPath, items, *judgeModel, results)
}

func wantCascade(parts []string) bool {
	for _, p := range parts {
		if strings.TrimSpace(p) == "cascade" {
			return true
		}
	}
	return false
}

func missingCascadeTasks(cfg *config.Config, items []benchItem) []string {
	seen := map[string]bool{}
	var missing []string
	for _, it := range items {
		if _, ok := cfg.Cascades[it.Task]; !ok && !seen[it.Task] {
			seen[it.Task] = true
			missing = append(missing, it.Task)
		}
	}
	sort.Strings(missing)
	return missing
}

// runPolicy 用一套全新的在线状态跑一遍数据集（策略间不共享学习状态，保证可比性）。
func runPolicy(name string, cfg *config.Config, reg *provider.Registry, items []benchItem, logger *slog.Logger, opts benchOpts) (policyResult, []itemDetail) {
	exec := executor.New(reg, cfg, cfg.Seed, logger)
	state := router.NewState(cfg)
	rt := router.New(cfg, reg, state, cfg.Seed, logger)
	cal := cascade.NewCalibrator(cfg.Cascade.Confidence.Threshold,
		cfg.Cascade.TargetUpgradeRate, cfg.Cascade.CalibrationStep, cfg.Cascade.CalibrationWindow)
	checker := cascade.NewChecker(cfg, exec, nil)
	checker.SetThresholdFunc(cal.Threshold) // 与网关一致的接线：阈值来自校准器
	runner := cascade.NewRunner(exec, checker, cal, cfg, logger)

	res := policyResult{Name: name}
	var details []itemDetail
	for _, item := range items {
		req := &types.Request{
			RequestID:   item.ID,
			Messages:    []types.Message{{Role: types.RoleUser, Content: item.Prompt}},
			MaxTokens:   opts.maxTokens,
			Temperature: opts.temperature,
		}
		var legReasons []string
		start := time.Now()
		var finalModel string
		var resp *types.Response
		var legs int
		var err error

		switch name {
		case "premium":
			finalModel = baselineModel(cfg, reg, item.Task)
			resp, err = exec.Call(context.Background(), finalModel, req)
			legs = 1
		case "cheap":
			finalModel = cheapestModel(reg)
			resp, err = exec.Call(context.Background(), finalModel, req)
			legs = 1
		case "cascade":
			chain, _ := cfg.Cascades[item.Task]
			costFn := func(model string, r *types.Response) float64 {
				c, _ := reg.Cost(model, r.Usage.PromptTokens, r.Usage.CompletionTokens)
				return c
			}
			var rep *cascade.Report
			resp, rep, err = runner.Run(context.Background(), req, chain, item.Task, costFn)
			if rep != nil {
				finalModel = rep.FinalModel
				legs = len(rep.Legs)
				res.Upgrades += boolToInt(rep.Upgraded)
				for _, lg := range rep.Legs {
					legReasons = append(legReasons, lg.VerdictReason)
				}
			}
		case "lagrangian":
			dec, derr := rt.Decide(req, router.Hints{})
			if derr != nil {
				err = derr
				break
			}
			finalModel = dec.Chosen
			resp, err = exec.Call(context.Background(), finalModel, req)
			legs = 1
			if err == nil && resp != nil {
				// reward 用生产同款启发式信号（不是 oracle）——
				// benchmark 不能偷看评测答案，否则结论失真
				reward := cascade.HeuristicScore(cascade.CheckReq{Messages: req.Messages, Response: resp}).Score
				cost, _ := reg.Cost(finalModel, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
				rt.ReportOutcome(item.Task, finalModel, reward, cost, time.Since(start).Milliseconds())
			}
		default:
			fatal("unknown policy %q", name)
		}
		elapsed := time.Since(start)
		res.LatencySum += elapsed
		res.Requests++
		res.Legs += legs
		if err != nil || resp == nil {
			fmt.Fprintf(os.Stderr, "  [%s] %s: ERROR %v\n", name, item.ID, err)
			details = append(details, itemDetail{
				Policy: name, ID: item.ID, Task: item.Task, Model: finalModel,
				Legs: legs, LatencyMs: elapsed.Milliseconds(), Score: -1, Error: fmt.Sprint(err),
			})
			continue
		}
		cost, _ := reg.Cost(finalModel, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
		res.CostUSD += cost

		score := -1.0
		if opts.judge != nil {
			// judge 评分不计入策略成本（它是度量开销，四种策略等额承担）
			v := opts.judge.Check(context.Background(), cascade.CheckReq{Messages: req.Messages, Response: resp, Task: item.Task})
			if !v.Skip {
				score = v.Score
				res.JudgeSum += v.Score
				res.JudgeN++
				if v.Score >= 0.6 {
					res.JudgePass++
				}
			}
		} else if resp.Content == provider.GoodAnswer(finalModel, item.Prompt) {
			res.Correct++
			score = 1
		} else {
			score = 0
		}
		details = append(details, itemDetail{
			Policy: name, ID: item.ID, Task: item.Task, Model: finalModel,
			Legs: legs, Upgraded: legs > 1, LegsReason: legReasons,
			CostUSD: cost, LatencyMs: elapsed.Milliseconds(), Score: score,
		})
	}
	return res, details
}

func baselineModel(cfg *config.Config, reg *provider.Registry, task string) string {
	if m, ok := cfg.Routing.Baseline[task]; ok {
		return m
	}
	// 兜底：最贵模型
	var best string
	var maxPrice float64
	for _, m := range reg.Models() {
		if m.PriceOut >= maxPrice {
			maxPrice = m.PriceOut
			best = m.Name
		}
	}
	return best
}

func cheapestModel(reg *provider.Registry) string {
	var best string
	var minPrice float64 = -1
	for _, m := range reg.Models() {
		p := m.PriceIn + m.PriceOut
		if minPrice < 0 || p < minPrice {
			minPrice = p
			best = m.Name
		}
	}
	return best
}

func printReport(cfg *config.Config, datasetPath string, items []benchItem, judgeModel string, results []policyResult) {
	var premium *policyResult
	for i := range results {
		if results[i].Name == "premium" {
			premium = &results[i]
		}
	}
	judgeMode := judgeModel != ""
	fmt.Println("# omni-bench 结果")
	fmt.Println()
	if judgeMode {
		fmt.Printf("- 口径: **真实模型 + LLM-as-judge 评分**（judge = %s，score≥0.6 记为通过）\n", judgeModel)
		fmt.Println("- 注意: judge 评分是代理指标，对同源模型存在偏袒，仅作方向性结论；成本为影子价格折算（见配置注释）")
	} else {
		fmt.Println("- 口径: **mock oracle**（确定性标准答案完全匹配 = 正确；结构正确性代理指标，非真实任务质量）")
	}
	fmt.Printf("- 数据集: %s（%d 条任务）\n", datasetPath, len(items))
	fmt.Printf("- 样本量: 每策略 %d 次请求；seed=%d（路由可复现；真实模型输出本身有随机性）\n", len(items), cfg.Seed)
	fmt.Printf("- 对比基线: premium（永远使用基线/最强模型）\n")
	if judgeMode {
		fmt.Println("- 统计显著性: 小样本实测，方向性结论；扩样请增大 -dataset 或多轮取均值")
	} else {
		fmt.Println("- 统计显著性: 确定性模拟，无抽样误差（接真实 key 后请报告置信区间）")
	}
	fmt.Println()
	if judgeMode {
		fmt.Println("| 策略 | judge 均分 | judge 通过率 | 总成本 USD | 相对 premium 节省 | 平均每请求成本 | 升级次数 | 平均腿数 | 平均耗时 |")
		fmt.Println("|---|---|---|---|---|---|---|---|---|")
		for _, r := range results {
			savings := 0.0
			if premium != nil {
				savings = premium.CostUSD - r.CostUSD
			}
			avgScore := 0.0
			if r.JudgeN > 0 {
				avgScore = r.JudgeSum / float64(r.JudgeN)
			}
			fmt.Printf("| %s | %.3f | %.1f%% (%d/%d) | %.6f | %.1f%% | $%.6f | %d | %.2f | %s |\n",
				r.Name, avgScore,
				percent(r.JudgePass, r.JudgeN), r.JudgePass, r.JudgeN,
				r.CostUSD,
				percent(int(savings*1e9), int(premiumCost(premium)*1e9)),
				r.CostUSD/float64(max64(1, int64(r.Requests))),
				r.Upgrades,
				float64(r.Legs)/float64(max64(1, int64(r.Requests))),
				r.LatencySum.Round(time.Millisecond)/time.Duration(max64(1, int64(r.Requests))),
			)
		}
	} else {
		fmt.Println("| 策略 | 准确率 | 总成本 USD | 相对 premium 节省 | 平均每请求成本 | 升级次数 | 平均腿数 | 平均耗时 |")
		fmt.Println("|---|---|---|---|---|---|---|---|")
		for _, r := range results {
			savings := 0.0
			if premium != nil {
				savings = premium.CostUSD - r.CostUSD
			}
			fmt.Printf("| %s | %.1f%% (%d/%d) | %.6f | %.1f%% | $%.6f | %d | %.2f | %s |\n",
				r.Name,
				percent(r.Correct, r.Requests), r.Correct, r.Requests,
				r.CostUSD,
				percent(int(savings*1e9), int(premiumCost(premium)*1e9)),
				r.CostUSD/float64(max64(1, int64(r.Requests))),
				r.Upgrades,
				float64(r.Legs)/float64(max64(1, int64(r.Requests))),
				r.LatencySum.Round(time.Microsecond)/time.Duration(max64(1, int64(r.Requests))),
			)
		}
	}
	fmt.Println()
	if judgeMode {
		fmt.Println("> 回填 README 时保留口径标注：真实模型（Ollama 本地）+ judge 评分 + 影子价格成本。")
	} else {
		fmt.Println("> TODO(实测后填入)：接真实 key 后重跑并在此填写：各策略真实成本、真实任务质量（人工/评测集）")
		fmt.Println("> 记录方法：`go run ./cmd/omni-bench -config <真实配置> -dataset data/bench_tasks.jsonl -judge <judge模型>`，")
		fmt.Println("> 将 stdout 重定向到 reports/bench-<日期>.md 并提交。")
	}
}

func premiumCost(p *policyResult) float64 {
	if p == nil {
		return 1
	}
	if p.CostUSD <= 0 {
		return 1e-9
	}
	return p.CostUSD
}

func percent(num, den int) float64 {
	if den == 0 {
		return 0
	}
	return float64(num) / float64(den) * 100
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func loadDataset(path string) []benchItem {
	f, err := os.Open(path)
	if err != nil {
		fatal("open dataset: %v", err)
	}
	defer f.Close()
	var items []benchItem
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var it benchItem
		if err := json.Unmarshal([]byte(line), &it); err != nil {
			fatal("parse %s: %v", path, err)
		}
		if it.Task == "" || it.Prompt == "" {
			fatal("dataset item missing task/prompt: %+v", it)
		}
		items = append(items, it)
	}
	if err := sc.Err(); err != nil {
		fatal("read dataset: %v", err)
	}
	return items
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "omni-bench: "+format+"\n", args...)
	os.Exit(1)
}
