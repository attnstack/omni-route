// router_extra_test.go：direct 模式三条路径、状态接口与成本估算的补充覆盖。
package router

import (
	"testing"

	"github.com/attnstack/omni-route/internal/types"
)

func TestDecideDirectAllPaths(t *testing.T) {
	req := &types.Request{Messages: []types.Message{{Role: types.RoleUser, Content: "hello"}}}

	// 命中 task_defaults
	cfg := testConfig()
	cfg.Routing.Mode = "direct"
	cfg.Routing.TaskDefaults = map[string]string{"chat": "m-b"}
	r := New(cfg, newTestRegistry(cfg), NewState(cfg), 1, discardLogger())
	dec, err := r.Decide(req, Hints{})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Tier != "direct" || dec.Chosen != "m-b" || dec.Reason != "task-default" {
		t.Fatalf("task-default path = %+v", dec)
	}

	// task_defaults 未命中 → 全局 default_model（testConfig 默认 m-b，这里换掉以区分路径）
	cfg2 := testConfig()
	cfg2.Routing.Mode = "direct"
	cfg2.Routing.DefaultModel = "m-b"
	cfg2.Routing.TaskDefaults = map[string]string{"code": "m-a"} // chat 不在其中
	r2 := New(cfg2, newTestRegistry(cfg2), NewState(cfg2), 1, discardLogger())
	dec2, err := r2.Decide(req, Hints{})
	if err != nil {
		t.Fatal(err)
	}
	if dec2.Chosen != "m-b" || dec2.Reason != "default" {
		t.Fatalf("default path = %+v", dec2)
	}

	// 两个默认都没配 → 兜底选最便宜可行（m-a 单价最低）
	cfg3 := testConfig()
	cfg3.Routing.Mode = "direct"
	cfg3.Routing.DefaultModel = "" // 两个默认都没配，逼出 fallback-cheapest
	cfg3.Routing.TaskDefaults = nil
	r3 := New(cfg3, newTestRegistry(cfg3), NewState(cfg3), 1, discardLogger())
	dec3, err := r3.Decide(req, Hints{})
	if err != nil {
		t.Fatal(err)
	}
	if dec3.Chosen != "m-a" || dec3.Reason != "fallback-cheapest" {
		t.Fatalf("fallback-cheapest path = %+v", dec3)
	}
}

func TestEstimateCostForRequest(t *testing.T) {
	cfg := testConfig()
	r := New(cfg, newTestRegistry(cfg), NewState(cfg), 1, discardLogger())

	// usage 缺失 → 本地估算并打 Estimated 标记
	cost, usage := r.EstimateCostForRequest("m-a", types.Usage{}, 100)
	if usage.PromptTokens != 100 || !usage.Estimated {
		t.Fatalf("estimated usage = %+v", usage)
	}
	if cost <= 0 {
		t.Fatalf("cost = %f", cost)
	}

	// usage 已有值 → 原样结算，不估算
	cost2, usage2 := r.EstimateCostForRequest("m-a", types.Usage{PromptTokens: 10, CompletionTokens: 20}, 999)
	if usage2.PromptTokens != 10 || usage2.Estimated {
		t.Fatalf("reported usage = %+v", usage2)
	}
	if usage2.TotalTokens != 30 || cost2 <= 0 {
		t.Fatalf("settle = %+v %f", usage2, cost2)
	}
}

func TestStateAccessors(t *testing.T) {
	cfg := testConfig()
	r := New(cfg, newTestRegistry(cfg), NewState(cfg), 1, discardLogger())

	if r.State() == nil || r.Registry() == nil {
		t.Fatal("nil accessors")
	}
	if v := r.NextRandom(); v < 0 || v >= 1 {
		t.Fatalf("NextRandom out of range: %f", v)
	}

	// 记录带延迟的结果 → LatencyMs EWMA 生效
	r.ReportOutcome("code", "m-a", 0.9, 1e-6, 250)
	if lat := r.state.LatencyMs("code", "m-a"); lat != 250 {
		t.Fatalf("LatencyMs = %f", lat)
	}
	r.ReportOutcome("code", "m-a", 0.9, 1e-6, 50)
	// 0.7*250 + 0.3*50 = 190
	if lat := r.state.LatencyMs("code", "m-a"); lat != 190 {
		t.Fatalf("LatencyMs EWMA = %f", lat)
	}

	// ApplyQualitySignal：只动质量，不动消费窗口
	spend0, _, _ := r.state.SpendAndLambda("code")
	qBefore := r.State().Quality("code", "m-a")
	r.state.ApplyQualitySignal("code", "m-a", 1.0)
	spend1, window, lambda := r.state.SpendAndLambda("code")
	if spend0 != 0 || spend1 != 0 {
		t.Fatalf("quality signal must not touch spend: %f -> %f", spend0, spend1)
	}
	if window != cfg.Routing.SpendWindow || lambda != cfg.Routing.Lambda.Init {
		t.Fatalf("spend window = %d lambda = %f", window, lambda)
	}
	if qAfter := r.State().Quality("code", "m-a"); qAfter <= qBefore {
		t.Fatalf("positive signal must raise quality: %f -> %f", qBefore, qAfter)
	}

	// QualityTable：应包含已记录的 (task, model)
	table := r.state.QualityTable()
	if _, ok := table["code"]["m-a"]; !ok {
		t.Fatalf("quality table missing entry: %v", table)
	}
}
