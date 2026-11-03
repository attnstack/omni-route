// executor_test.go：执行层的直接单测——重试恢复、failover 候选、熔断开启与未知模型。
// （网关集成测试间接覆盖了本包；这里补直接断言，也让覆盖率报告反映真实情况。）
package executor

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/attnstack/omni-route/internal/config"
	"github.com/attnstack/omni-route/internal/provider"
	"github.com/attnstack/omni-route/internal/types"
)

func benchCfg(retries int, failFirstN int) (*config.Config, *provider.Registry) {
	cfg := &config.Config{
		Seed: 42,
		Providers: map[string]config.Provider{
			"mock": {Kind: "mock", Mock: config.MockOptions{FailFirstN: failFirstN}},
		},
		Models: map[string]config.Model{
			"m-fast": {Provider: "mock", PriceIn: 0.1, PriceOut: 0.4, MockTier: "fast"},
			"m-prem": {Provider: "mock", PriceIn: 2.5, PriceOut: 10, MockTier: "premium"},
		},
		Reliability: config.Reliability{Retries: retries, BackoffBaseMs: 1},
	}
	cfg.ApplyDefaults()
	reg, err := provider.Build(cfg, nil, quietLogger())
	if err != nil {
		panic(err)
	}
	return cfg, reg
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestCallRetryRecovers(t *testing.T) {
	cfg, reg := benchCfg(2, 1) // 前两次失败，第 3 次成功；重试预算 2（共 3 次尝试）
	e := New(reg, cfg, 1, quietLogger())
	resp, err := e.Call(context.Background(), "m-fast",
		&types.Request{Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}}})
	if err != nil || resp == nil {
		t.Fatalf("retry should recover: %v", err)
	}
}

func TestCallExhaustsRetries(t *testing.T) {
	cfg, reg := benchCfg(1, 100) // 永远失败
	e := New(reg, cfg, 1, quietLogger())
	_, err := e.Call(context.Background(), "m-fast",
		&types.Request{Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}}})
	if err == nil {
		t.Fatal("exhausted retries must error")
	}
}

func TestCallUnknownModel(t *testing.T) {
	cfg, reg := benchCfg(0, 0)
	e := New(reg, cfg, 1, quietLogger())
	if _, err := e.Call(context.Background(), "ghost", &types.Request{}); err == nil {
		t.Fatal("unknown model must error")
	}
}

func TestCallBreakerOpen(t *testing.T) {
	cfg, reg := benchCfg(0, 0)
	e := New(reg, cfg, 1, quietLogger())
	brk := e.Breaker("mock")
	for i := 0; i < 30; i++ {
		brk.Allow()
		brk.Record(false)
	}
	_, err := e.Call(context.Background(), "m-fast",
		&types.Request{Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}}})
	if err == nil || !containsErr(err, "circuit open") {
		t.Fatalf("open breaker must surface: %v", err)
	}
	// 等冷却（open_sec 默认 30s 太久，直接造一个短冷却的 breaker 验证半开探测）
	short := &config.Config{Providers: cfg.Providers, Models: cfg.Models}
	short.ApplyDefaults()
	short.Reliability.Breaker.OpenSec = 1
	short.Reliability.Breaker.MinSamples = 5
	e2 := New(reg, short, 1, quietLogger())
	b2 := e2.Breaker("mock")
	for i := 0; i < 10; i++ {
		b2.Allow()
		b2.Record(false)
	}
	time.Sleep(1100 * time.Millisecond)
	if ok, reason := b2.Allow(); !ok || reason == "" {
		t.Fatalf("half-open probe expected, got ok=%v reason=%q", ok, reason)
	}
}

func TestStreamEstablishRetry(t *testing.T) {
	cfg, reg := benchCfg(2, 1)
	e := New(reg, cfg, 1, quietLogger())
	st, err := e.Stream(context.Background(), "m-fast",
		&types.Request{Stream: true, Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatalf("stream retry should recover: %v", err)
	}
	defer st.Close()
	if _, err := st.Next(); err != nil {
		t.Fatalf("first chunk: %v", err)
	}
}

func containsErr(err error, sub string) bool {
	return err != nil && len(err.Error()) >= len(sub) && indexOfStr(err.Error(), sub) >= 0
}

func indexOfStr(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
