// confidence_extra_test.go：LLM judge、自一致性、校准器快照与 Runner 测试时钟的补充覆盖。
package cascade

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/attnstack/omni-route/internal/config"
	"github.com/attnstack/omni-route/internal/types"
)

func judgeCfg(model string) *config.Config {
	cfg := testRunnerCfg()
	cfg.Cascade.Confidence.LLMJudge = config.LLMJudgeCfg{
		Enabled: true, Model: model, SampleRate: 1.0, TimeoutMs: 5000,
	}
	return cfg
}

func TestLLMJudgeScores(t *testing.T) {
	fake := &fakeCaller{resps: map[string]*types.Response{
		"judge": {Model: "judge", Content: "0.75", FinishReason: "stop"},
	}}
	j := NewLLMJudge(judgeCfg("judge"), fake, nil)

	v := j.Check(context.Background(), CheckReq{
		Messages: []types.Message{{Role: types.RoleUser, Content: "q"}},
		Response: &types.Response{Content: "a very long answer"},
	})
	if v.Skip || v.Score != 0.75 {
		t.Fatalf("judge score = %+v", v)
	}

	// 超长输入被截断到 2000 rune，不 panic 也不改变判定流程
	long := strings.Repeat("x", 5000)
	v = j.Check(context.Background(), CheckReq{
		Messages: []types.Message{{Role: types.RoleUser, Content: long}},
		Response: &types.Response{Content: long},
	})
	if v.Skip || v.Score != 0.75 {
		t.Fatalf("long-input judge = %+v", v)
	}

	// judge 上游错误 → Skip（判别器失败不能连累业务请求）
	j2 := NewLLMJudge(judgeCfg("judge"), &fakeCaller{errs: map[string]error{"judge": errBoom}}, nil)
	if v := j2.Check(context.Background(), CheckReq{
		Messages: []types.Message{{Role: types.RoleUser, Content: "q"}},
		Response: &types.Response{Content: "a"},
	}); !v.Skip || v.Reason != "judge-unavailable" {
		t.Fatalf("error judge = %+v", v)
	}

	// 回复里没有数字 → 无法解析 → Skip
	j3 := NewLLMJudge(judgeCfg("judge"), &fakeCaller{resps: map[string]*types.Response{
		"judge": {Model: "judge", Content: "I think it is fine."},
	}}, nil)
	if v := j3.Check(context.Background(), CheckReq{
		Messages: []types.Message{{Role: types.RoleUser, Content: "q"}},
		Response: &types.Response{Content: "a"},
	}); !v.Skip || v.Reason != "judge-unparseable" {
		t.Fatalf("unparseable judge = %+v", v)
	}

	// 分数夹紧到 [0,1]
	j4 := NewLLMJudge(judgeCfg("judge"), &fakeCaller{resps: map[string]*types.Response{
		"judge": {Model: "judge", Content: "1.75"},
	}}, nil)
	if v := j4.Check(context.Background(), CheckReq{
		Messages: []types.Message{{Role: types.RoleUser, Content: "q"}},
		Response: &types.Response{Content: "a"},
	}); v.Skip || v.Score != 1.0 {
		t.Fatalf("clamp judge = %+v", v)
	}
}

func TestSelfConsistency(t *testing.T) {
	same := &types.Response{Model: "m", Content: "The answer is 42.", FinishReason: "stop"}
	// k=3 采样完全一致 → majority 3/3 = 1.0
	fake := &fakeCaller{resps: map[string]*types.Response{"m": same}}
	cfg := testRunnerCfg()
	cfg.Cascade.Confidence.SelfConsistency = config.SelfConsistencyCfg{Enabled: true, K: 3}
	sc := NewSelfConsistency(cfg, fake, nil)
	v := sc.Check(context.Background(), CheckReq{
		Messages: []types.Message{{Role: types.RoleUser, Content: "q"}},
		Response: same,
	})
	if v.Skip || v.Score != 1.0 || !v.Pass {
		t.Fatalf("consistent selfc = %+v", v)
	}

	// 标点/大小写差异被 normalizeAnswer 中性化后算同一答案 → majority 2/3，按 ≥0.5 判通过
	diverge := &fakeCaller{seq: []string{"The answer, is 42!", "the ANSWER is 42", "The answer is 43"}}
	sc2 := NewSelfConsistency(cfg, diverge, nil)
	v2 := sc2.Check(context.Background(), CheckReq{
		Messages: []types.Message{{Role: types.RoleUser, Content: "q"}},
		Response: same,
	})
	if v2.Skip || v2.Score != float64(2)/3 || !v2.Pass {
		t.Fatalf("divergent selfc = %+v", v2)
	}

	// 三个答案互不相同 → majority 1/3 < 0.5 → 不通过
	chaos := &fakeCaller{seq: []string{"a", "b", "c"}}
	sc3b := NewSelfConsistency(cfg, chaos, nil)
	v3b := sc3b.Check(context.Background(), CheckReq{
		Messages: []types.Message{{Role: types.RoleUser, Content: "q"}},
		Response: same,
	})
	if v3b.Skip || v3b.Score != float64(1)/3 || v3b.Pass {
		t.Fatalf("chaotic selfc = %+v", v3b)
	}

	// 上游失败 → Skip
	sc3 := NewSelfConsistency(cfg, &fakeCaller{errs: map[string]error{"m": errBoom}}, nil)
	if v3 := sc3.Check(context.Background(), CheckReq{
		Messages: []types.Message{{Role: types.RoleUser, Content: "q"}},
		Response: same,
	}); !v3.Skip || v3.Reason != "selfc-unavailable" {
		t.Fatalf("error selfc = %+v", v3)
	}

	// normalizeAnswer 单测：标点/大小写/空白全部中性化
	if normalizeAnswer("  Hello, WORLD!  ") != "helloworld" {
		t.Fatalf("normalizeAnswer = %q", normalizeAnswer("  Hello, WORLD!  "))
	}
}

func TestCalibratorSnapshot(t *testing.T) {
	c := NewCalibrator(0.6, 0.3, 0.5, 8)
	c.Observe("chat", false)
	snap := c.Snapshot()
	if v, ok := snap["_global"]; !ok || v != 0.6 {
		t.Fatalf("snapshot = %v (theta should stay 0.6 until window fills)", snap)
	}
}

func TestRunnerSetNowLatencyBudget(t *testing.T) {
	// 用注入时钟确定性触发延迟预算放行，替代"预算=0"的急停路径
	cfg := testRunnerCfg()
	cfg.Cascade.MaxLatencyMs = 10000
	fake := &fakeCaller{resps: map[string]*types.Response{
		"small": {Model: "small", Content: `{"a: `, FinishReason: "stop"},
	}}
	runner := NewRunner(fake, NewChecker(cfg, fake, nil), NewCalibrator(0.6, 0.3, 0.05, 50), cfg, discardLogger())

	t0 := time.Unix(0, 0)
	cur := t0
	runner.SetNow(func() time.Time { return cur })
	// Call 之后推进 20s：超过 MaxLatencyMs，第二级之前直接放行第一级结果
	fake.beforeCall = func() { cur = cur.Add(20 * time.Second) }

	req := &types.Request{Messages: []types.Message{{Role: types.RoleUser, Content: "提取并返回 JSON"}}}
	resp, rep, err := runner.Run(context.Background(), req, []string{"small", "big"}, "extraction",
		func(string, *types.Response) float64 { return 1e-4 })
	if err != nil {
		t.Fatal(err)
	}
	if rep.SkippedReason != "latency-budget" || resp.Content != `{"a: ` {
		t.Fatalf("injected-clock latency budget = %+v", rep)
	}
}
