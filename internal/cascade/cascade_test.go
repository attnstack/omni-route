// cascade 包单测：阈值校准方向、判别器行为、执行器的升级/放行/延迟预算路径。
package cascade

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/attnstack/omni-route/internal/config"
	"github.com/attnstack/omni-route/internal/types"
)

func TestCalibratorDirection(t *testing.T) {
	// 全局阈值语义：所有任务的观测共享同一个窗口与 θ
	c := NewCalibrator(0.6, 0.30, 0.5, 10)
	// 第一窗全部升级（率 100% > 30%）→ θ 应下降（放宽）
	for i := 0; i < 10; i++ {
		c.Observe("code", true)
	}
	if got := c.Threshold("code"); got >= 0.6 {
		t.Fatalf("high upgrade rate should relax threshold, got %f", got)
	}
	// 第二、三窗全部放行（率 0 < 30%）→ θ 应上升（收紧）
	for i := 0; i < 20; i++ {
		c.Observe("chat", false)
	}
	if got := c.Threshold("chat"); got <= 0.6 {
		t.Fatalf("low upgrade rate should tighten threshold, got %f", got)
	}
	// 边界夹紧
	if got := c.Threshold("chat"); got > 0.98 || got < 0.05 {
		t.Fatalf("threshold out of clamp: %f", got)
	}
}

func TestFormatCheck(t *testing.T) {
	jsonPrompt := []types.Message{{Role: types.RoleUser, Content: "提取结果并返回 JSON"}}
	cases := []struct {
		name    string
		resp    *types.Response
		wantVer Verdict
	}{
		{"合法 JSON", &types.Response{Content: `{"a":1}`, FinishReason: "stop"}, Verdict{Score: 1, Pass: true}},
		{"围栏 JSON", &types.Response{Content: "```json\n{\"a\":1}\n```", FinishReason: "stop"}, Verdict{Score: 1, Pass: true}},
		{"非法 JSON", &types.Response{Content: `{"a: `, FinishReason: "stop"}, Verdict{Score: 0, Pass: false}},
		{"截断", &types.Response{Content: `{"a": 1`, FinishReason: "length"}, Verdict{Score: 0.2, Pass: false}},
		{"过短", &types.Response{Content: "ok", FinishReason: "stop"}, Verdict{Score: 0.1, Pass: false}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := checkFormat(CheckReq{Messages: jsonPrompt, Response: c.resp})
			if got.Pass != c.wantVer.Pass {
				t.Fatalf("pass = %v (score=%f reason=%s)", got.Pass, got.Score, got.Reason)
			}
		})
	}
	// 非 JSON 任务不做 JSON 校验
	plain := []types.Message{{Role: types.RoleUser, Content: "讲个笑话"}}
	if v := checkFormat(CheckReq{Messages: plain, Response: &types.Response{Content: "这不是 JSON 的长回答，足够长了。", FinishReason: "stop"}}); !v.Pass {
		t.Fatalf("plain text should pass: %+v", v)
	}
}

func TestHeuristicScore(t *testing.T) {
	prompt := []types.Message{{Role: types.RoleUser, Content: "如何在 Go 中实现令牌桶限流算法"}}
	good := &types.Response{Content: strings.Repeat("令牌桶限流算法的核心是以固定速率补充令牌。", 8), FinishReason: "stop"}
	if v := HeuristicScore(CheckReq{Messages: prompt, Response: good}); v.Score < 0.6 {
		t.Fatalf("good answer scored too low: %+v", v)
	}
	lazy := &types.Response{Content: "ok.", FinishReason: "stop"}
	if v := HeuristicScore(CheckReq{Messages: prompt, Response: lazy}); v.Score >= 0.6 {
		t.Fatalf("lazy answer should score below cascade threshold: %+v", v)
	}
	refusal := &types.Response{Content: "抱歉，我无法回答这个问题。", FinishReason: "stop"}
	if v := HeuristicScore(CheckReq{Messages: prompt, Response: refusal}); v.Pass {
		t.Fatalf("refusal should fail: %+v", v)
	}
}

// fakeCaller 是级联测试的可编程执行器。
type fakeCaller struct {
	resps map[string]*types.Response // model → 固定响应
	errs  map[string]error           // model → 固定错误
	calls []string
}

func (f *fakeCaller) Call(_ context.Context, model string, _ *types.Request) (*types.Response, error) {
	f.calls = append(f.calls, model)
	if err, ok := f.errs[model]; ok {
		return nil, err
	}
	if r, ok := f.resps[model]; ok {
		cp := *r
		return &cp, nil
	}
	return &types.Response{Model: model, Content: "ok.", FinishReason: "stop"}, nil
}

func testRunnerCfg() *config.Config {
	return &config.Config{
		Cascade: config.Cascade{
			MaxLatencyMs: 15000,
			Confidence: config.Confidence{
				Threshold: 0.6, UseFormat: true, UseHeuristic: true,
			},
		},
	}
}

func TestRunnerUpgradePath(t *testing.T) {
	cfg := testRunnerCfg()
	fake := &fakeCaller{resps: map[string]*types.Response{
		"small": {Model: "small", Content: `{"a: `, FinishReason: "stop"},  // 坏 JSON
		"mid":   {Model: "mid", Content: `{"a": 1}`, FinishReason: "stop"}, // 好 JSON
		"big":   {Model: "big", Content: `{"a": 1}`, FinishReason: "stop"},
	}}
	runner := NewRunner(fake, NewChecker(cfg, fake, nil), NewCalibrator(0.6, 0.3, 0.05, 50), cfg, discardLogger())
	req := &types.Request{RequestID: "r1", Messages: []types.Message{{Role: types.RoleUser, Content: "提取并返回 JSON"}}}
	costFn := func(string, *types.Response) float64 { return 1e-4 }

	resp, rep, err := runner.Run(context.Background(), req, []string{"small", "mid", "big"}, "extraction", costFn)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != `{"a": 1}` {
		t.Fatalf("final content = %q", resp.Content)
	}
	if !rep.Upgraded || rep.FinalModel != "mid" {
		t.Fatalf("expected upgrade to mid: %+v", rep)
	}
	if len(rep.Legs) != 2 || !rep.Legs[0].Upgraded {
		t.Fatalf("legs broken: %+v", rep.Legs)
	}
	if fake.calls[0] != "small" || fake.calls[1] != "mid" {
		t.Fatalf("chain order broken: %v", fake.calls)
	}
}

func TestRunnerFirstLegPasses(t *testing.T) {
	cfg := testRunnerCfg()
	fake := &fakeCaller{resps: map[string]*types.Response{
		"small": {Model: "small", Content: `{"summary": "x", "items": ["a"]}`, FinishReason: "stop"},
	}}
	runner := NewRunner(fake, NewChecker(cfg, fake, nil), NewCalibrator(0.6, 0.3, 0.05, 50), cfg, discardLogger())
	req := &types.Request{Messages: []types.Message{{Role: types.RoleUser, Content: "提取并返回 JSON"}}}
	_, rep, err := runner.Run(context.Background(), req, []string{"small", "big"}, "extraction",
		func(string, *types.Response) float64 { return 1e-4 })
	if err != nil {
		t.Fatal(err)
	}
	if rep.Upgraded || rep.FinalModel != "small" || len(rep.Legs) != 1 {
		t.Fatalf("no upgrade expected: %+v", rep)
	}
}

func TestRunnerErrorLegSkips(t *testing.T) {
	cfg := testRunnerCfg()
	fake := &fakeCaller{
		errs: map[string]error{"small": errors.New("boom")},
		resps: map[string]*types.Response{
			"big": {Model: "big", Content: `{"ok": true}`, FinishReason: "stop"},
		},
	}
	runner := NewRunner(fake, NewChecker(cfg, fake, nil), NewCalibrator(0.6, 0.3, 0.05, 50), cfg, discardLogger())
	req := &types.Request{Messages: []types.Message{{Role: types.RoleUser, Content: "提取并返回 JSON"}}}
	resp, rep, err := runner.Run(context.Background(), req, []string{"small", "big"}, "extraction",
		func(string, *types.Response) float64 { return 1e-4 })
	if err != nil {
		t.Fatal(err)
	}
	if rep.FinalModel != "big" || len(rep.Legs) != 2 || rep.Legs[0].Error == "" {
		t.Fatalf("error leg should be skipped: %+v", rep)
	}
	if resp.Content != `{"ok": true}` {
		t.Fatalf("content = %q", resp.Content)
	}
}

func TestRunnerLatencyBudget(t *testing.T) {
	cfg := testRunnerCfg()
	cfg.Cascade.MaxLatencyMs = 0 // 预算为 0：第一级成功后立即放行（确定性急停路径）
	fake := &fakeCaller{resps: map[string]*types.Response{
		"small": {Model: "small", Content: `{"a: `, FinishReason: "stop"},
	}}
	runner := NewRunner(fake, NewChecker(cfg, fake, nil), NewCalibrator(0.6, 0.3, 0.05, 50), cfg, discardLogger())
	req := &types.Request{Messages: []types.Message{{Role: types.RoleUser, Content: "提取并返回 JSON"}}}
	resp, rep, err := runner.Run(context.Background(), req, []string{"small", "big"}, "extraction",
		func(string, *types.Response) float64 { return 1e-4 })
	if err != nil {
		t.Fatal(err)
	}
	if rep.SkippedReason != "latency-budget" {
		t.Fatalf("want latency-budget skip, got %+v", rep)
	}
	if resp == nil || resp.Content != `{"a: ` {
		t.Fatalf("should return first-leg response as-is")
	}
}

func TestRunnerAllLegsFail(t *testing.T) {
	cfg := testRunnerCfg()
	boom := errors.New("boom")
	fake := &fakeCaller{errs: map[string]error{"small": boom, "big": boom}}
	runner := NewRunner(fake, NewChecker(cfg, fake, nil), NewCalibrator(0.6, 0.3, 0.05, 50), cfg, discardLogger())
	req := &types.Request{Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}}}
	if _, _, err := runner.Run(context.Background(), req, []string{"small", "big"}, "chat",
		func(string, *types.Response) float64 { return 0 }); err == nil {
		t.Fatal("all legs failed must error")
	}
}

func TestCheckerNoChecker(t *testing.T) {
	// 全部判别器关闭：放行并明确标注
	cfg := testRunnerCfg()
	cfg.Cascade.Confidence.UseFormat = false
	cfg.Cascade.Confidence.UseHeuristic = false
	fake := &fakeCaller{}
	ch := NewChecker(cfg, fake, nil)
	v := ch.Evaluate(context.Background(), CheckReq{
		Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}},
		Response: &types.Response{Content: "ok.", FinishReason: "stop"},
	})
	if !v.Pass || v.Reason != "no-checker" {
		t.Fatalf("want no-checker pass, got %+v", v)
	}
}

func TestCheckerWiredCalibratorAffectsDecisions(t *testing.T) {
	// 校准闭环接线测试：同一回答，校准器阈值上调后必须由"放行"翻转为"判负"。
	// 校准闭环的接线正确性验证：校准器的输出必须被决策消费。
	cfg := testRunnerCfg()
	cfg.Cascade.Confidence.UseFormat = true
	cfg.Cascade.Confidence.UseHeuristic = true
	fake := &fakeCaller{}
	ch := NewChecker(cfg, fake, nil)

	// Task 必须传：校准器按任务键控阈值，空 task 会静默回退到基线阈值
	cr := CheckReq{
		Messages: []types.Message{{Role: types.RoleUser, Content: "讲讲令牌桶"}},
		Response: &types.Response{Content: strings.Repeat("令牌桶以固定速率补充令牌，允许突发。", 6), FinishReason: "stop"},
		Task:     "chat",
	}
	if v := ch.Evaluate(context.Background(), cr); !v.Pass {
		t.Fatalf("good answer should pass at base threshold: %+v", v)
	}
	cal := NewCalibrator(0.6, 0.30, 0.5, 8) // 大步长：本测试验证接线而非步长调参
	for i := 0; i < 8; i++ {
		cal.Observe("chat", false) // 升级率 0% << 30% 目标 → θ 上调
	}
	ch.SetThresholdFunc(cal.Threshold)
	if got := cal.Threshold("chat"); got <= 0.6 {
		t.Fatalf("calibrator should have raised theta, got %f", got)
	}
	if v := ch.Evaluate(context.Background(), cr); v.Pass {
		t.Fatalf("raised threshold must flip the verdict, got %+v", v)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
