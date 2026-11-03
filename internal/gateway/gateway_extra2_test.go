// gateway_extra2_test.go：savings 兜底、异步 judge 采样、配置热替换与反馈非法输入的补充覆盖。
package gateway

import (
	"net/http"
	"testing"
	"time"

	"github.com/attnstack/omni-route/internal/config"
	"github.com/attnstack/omni-route/internal/types"
)

func TestSavingsFallsBackToPriciestModel(t *testing.T) {
	s, _ := newLocalServer(t)
	world := s.current()
	usage := types.Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500}
	// code 没配 baseline → 兜底用最贵模型（mock-premium），cascade 路径成本低 → 节省为正
	sav := s.savings(world, "code", usage, 0.0005)
	if sav <= 0 {
		t.Fatalf("fallback savings = %f", sav)
	}
	// 成本高于基线时节省为负（如显式选了 premium 之上的不存在场景，用放大成本验证符号）
	if sav2 := s.savings(world, "code", usage, 1.0); sav2 >= 0 {
		t.Fatalf("over-baseline savings should be negative: %f", sav2)
	}
}

func TestAsyncJudgeSamplesQuality(t *testing.T) {
	// llm_judge 全量采样（1.0）：请求结束后异步打分并回写质量表
	s, base := newLocalServer(t)
	world := s.current()
	world.cfg.Cascade.Confidence.LLMJudge = config.LLMJudgeCfg{
		Enabled: true, Model: "mock-mid", SampleRate: 1.0, TimeoutMs: 10000,
	}
	world.rt.State().ApplyQualitySignal("chat", "mock-mid", 0.5)
	qBefore := world.rt.State().Quality("chat", "mock-mid")

	resp, body := postJSON(t, base+"/v1/chat/completions",
		chatBody("mock-mid", "hello there friend", false), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	// 异步采样有延迟：轮询等待质量分被更新（judge 用 mock-mid 自评，score>0.5 会抬分）
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if world.rt.State().Quality("chat", "mock-mid") != qBefore {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("async judge never updated quality")
}

func TestSwapWorldReplacesSnapshot(t *testing.T) {
	s, _ := newLocalServer(t)
	old := s.current()
	cfg2 := demoConfig()
	cfg2.Listen = ":9"
	w2, err := BuildWorld(cfg2, nil, newTestState(cfg2), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	s.SwapWorld(w2)
	if s.current() == old || s.current().cfg.Listen != ":9" {
		t.Fatal("SwapWorld did not replace the world snapshot")
	}
}

func TestFeedbackInvalidJSON(t *testing.T) {
	_, base := newLocalServer(t)
	resp, _ := postJSON(t, base+"/v1/feedback", []byte("{not json"), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid feedback json: status = %d", resp.StatusCode)
	}
}
