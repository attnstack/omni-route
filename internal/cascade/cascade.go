// cascade.go：级联执行器。按链序逐级尝试：小模型作答 → 判别 → 不过关则升级。
// 明确的协议级取舍：级联只用于非流式——流式一旦吐出 chunk 就无法
// "撤回"，升级会向客户端重复发送内容。流式请求在网关层直接单模型直选。
package cascade

import (
	"context"
	"log/slog"
	"time"

	"github.com/attnstack/omni-route/internal/config"
	"github.com/attnstack/omni-route/internal/types"
)

// Leg 是级联的一条腿（一次模型调用及其判别结果）。
type Leg struct {
	Model         string  `json:"model"`
	CostUSD       float64 `json:"cost_usd"`
	Score         float64 `json:"score"`
	VerdictReason string  `json:"verdict_reason"`
	Upgraded      bool    `json:"upgraded"` // 该腿的答案被放弃、升级到下一级
	Error         string  `json:"error,omitempty"`
	LatencyMs     int64   `json:"latency_ms"`
}

// Report 是级联全程的报告——可解释性的数据来源。
type Report struct {
	Legs          []Leg   `json:"legs"`
	Upgraded      bool    `json:"upgraded"`
	TotalCostUSD  float64 `json:"total_cost_usd"`
	SkippedReason string  `json:"skipped_reason,omitempty"` // latency-budget | last-leg | no-checker
	FinalModel    string  `json:"final_model"`
}

// Runner 执行级联。
type Runner struct {
	exec    Caller
	checker *Checker
	cal     *Calibrator
	cfg     *config.Config
	logger  *slog.Logger
	now     func() time.Time
}

func NewRunner(exec Caller, checker *Checker, cal *Calibrator, cfg *config.Config, logger *slog.Logger) *Runner {
	return &Runner{exec: exec, checker: checker, cal: cal, cfg: cfg, logger: logger, now: time.Now}
}

// SetNow 注入测试时钟。
func (r *Runner) SetNow(f func() time.Time) { r.now = f }

// Run 执行级联链。chain 已经过硬约束过滤（保序）。
// 返回值：最终回答、报告。全链失败时返回最后一个错误。
func (r *Runner) Run(ctx context.Context, req *types.Request, chain []string, task string, costFn func(model string, resp *types.Response) float64) (*types.Response, *Report, error) {
	report := &Report{}
	start := r.now()
	maxLatency := time.Duration(r.cfg.Cascade.MaxLatencyMs) * time.Millisecond

	var lastErr error
	for i, model := range chain {
		isLast := i == len(chain)-1
		t0 := r.now()
		resp, err := r.exec.Call(ctx, model, req)
		leg := Leg{Model: model, LatencyMs: r.now().Sub(t0).Milliseconds()}
		if err != nil {
			leg.Error = err.Error()
			report.Legs = append(report.Legs, leg)
			lastErr = err
			if isLast {
				return nil, report, err
			}
			// 本级挂了：直接升级下一级（这也是一种"判别"——硬失败最可靠）
			continue
		}
		leg.CostUSD = costFn(model, resp)
		report.TotalCostUSD += leg.CostUSD

		// 延迟预算：超限直接放行当前结果，不再判别/升级。
		// 级联的省钱包不住一切——超过用户可感知的延迟阈值，省钱失去意义。
		// 预算为 0 表示"不预算"：第一级成功后立即放行（测试与急停开关用）。
		if !isLast && r.now().Sub(start) >= maxLatency {
			leg.VerdictReason = "skipped:latency-budget"
			report.Legs = append(report.Legs, leg)
			report.FinalModel = model
			report.SkippedReason = "latency-budget"
			r.observe(task, false)
			return resp, report, nil
		}

		verdict := r.checker.Evaluate(ctx, CheckReq{Messages: req.Messages, Response: resp, Task: task})
		leg.Score = verdict.Score
		leg.VerdictReason = verdict.Reason
		if isLast || verdict.Pass {
			leg.Upgraded = false
			report.Legs = append(report.Legs, leg)
			report.FinalModel = model
			if isLast && !verdict.Pass {
				report.SkippedReason = "last-leg"
			}
			r.observe(task, false)
			return resp, report, nil
		}
		leg.Upgraded = true
		report.Upgraded = true
		report.Legs = append(report.Legs, leg)
		r.logger.Info("cascade upgrade",
			"task", task, "from", model, "score", verdict.Score,
			"reason", verdict.Reason, "request_id", req.RequestID)
	}
	// 不可达：循环内 isLast 必然返回
	return nil, report, lastErr
}

// observe：一次级联决策 = 一次升级/放行样本。整个 Run 只观察一次。
func (r *Runner) observe(task string, upgraded bool) {
	if r.cal != nil {
		r.cal.Observe(task, upgraded)
	}
}
