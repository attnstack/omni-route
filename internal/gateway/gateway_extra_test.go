// gateway_extra_test.go：热加载端点、仪表盘页、成本估算兜底与反馈未命中路径的补充覆盖。
package gateway

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/attnstack/omni-route/internal/cache"
	"github.com/attnstack/omni-route/internal/config"
	"github.com/attnstack/omni-route/internal/router"
	"github.com/attnstack/omni-route/internal/store"
	"github.com/attnstack/omni-route/internal/types"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestState(cfg *config.Config) *router.State {
	return router.NewState(cfg)
}

// demoConfig 与 gateway_test.go 的 setup() 使用同一套 mock 模型，保证单价表非空。
func demoConfig() *config.Config {
	cfg := &config.Config{
		Listen: ":0",
		Seed:   42,
		Providers: map[string]config.Provider{
			"mock": {Kind: "mock", Mock: config.MockOptions{
				BadJSONRate: 1.0, ShortRate: 0, LatencyMs: 0, JitterMs: 0, StreamChunkDelayMs: 0,
			}},
		},
		Models: map[string]config.Model{
			"mock-fast":    {Provider: "mock", PriceIn: 0.1, PriceOut: 0.4, MockTier: "fast", Caps: mockCaps(8192, false)},
			"mock-mid":     {Provider: "mock", PriceIn: 0.6, PriceOut: 2.4, MockTier: "mid", Caps: mockCaps(32768, true)},
			"mock-premium": {Provider: "mock", PriceIn: 2.5, PriceOut: 10, MockTier: "premium", Caps: mockCaps(128000, true)},
		},
		Routing: config.Routing{
			Mode:         "cascade",
			DefaultModel: "mock-mid",
			BudgetPerTask: map[string]float64{
				"code": 1, "reasoning": 1, "summarization": 1,
				"translation": 1, "extraction": 1, "chat": 1,
			},
			Baseline: map[string]string{"chat": "mock-premium", "extraction": "mock-premium"},
			Lambda:   config.LambdaCfg{Init: 100, Min: 1, Max: 5000, Eta: 0.1},
		},
		Cascades: map[string][]string{
			"extraction": {"mock-fast", "mock-premium"},
			"chat":       {"mock-fast", "mock-mid"},
		},
		Cascade: config.Cascade{
			Confidence: config.Confidence{Threshold: 0.6, UseFormat: true, UseHeuristic: true},
		},
	}
	cfg.ApplyDefaults()
	return cfg
}

// newLocalServer 自建 Server（不经 setup），允许在对外服务前注入 reloadFn。
func newLocalServer(t *testing.T) (*Server, string) {
	t.Helper()
	cfg := demoConfig()
	logger := quietLogger()
	st, err := store.Open("", time.Second, 8, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cacheInst := cache.New(time.Duration(cfg.Cache.TTLSeconds)*time.Second, cfg.Cache.MaxEntries)
	w, err := BuildWorld(cfg, nil, newTestState(cfg), cacheInst, logger)
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(w, st, t.TempDir(), logger)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts.URL
}

func TestE2EReloadEndpoint(t *testing.T) {
	// 注入 reloadFn 后调用：无 admin key 时直接放行并执行回调
	s, base := newLocalServer(t)
	called := false
	s.SetReloadFn(func() error { called = true; return nil })
	resp, _ := postJSON(t, base+"/admin/reload", []byte("{}"), nil)
	if resp.StatusCode != http.StatusOK || !called {
		t.Fatalf("reload without admin key: status = %d called = %v", resp.StatusCode, called)
	}

	// 配置 admin_key_env：错误 key → 401；正确 key 通过鉴权（reloadFn 为 nil → 501）
	s2, base2 := newLocalServer(t)
	s2.current().cfg.AdminKeyEnv = "OMNI_TEST_ADMIN"
	t.Setenv("OMNI_TEST_ADMIN", "secret")
	resp2, _ := postJSON(t, base2+"/admin/reload", []byte("{}"), nil)
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong admin key: status = %d", resp2.StatusCode)
	}
	resp3, _ := postJSON(t, base2+"/admin/reload", []byte("{}"),
		map[string]string{"Authorization": "Bearer secret"})
	if resp3.StatusCode == http.StatusUnauthorized {
		t.Fatal("correct admin key should pass auth")
	}
}

func TestDashboardEndpoint(t *testing.T) {
	_, base := newLocalServer(t)
	hr, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer hr.Body.Close()
	buf := make([]byte, 256)
	n, _ := hr.Body.Read(buf)
	if hr.StatusCode != http.StatusOK || n == 0 {
		t.Fatalf("dashboard status = %d", hr.StatusCode)
	}
}

func TestSettleCostEstimatesWhenUsageMissing(t *testing.T) {
	s, _ := newLocalServer(t)
	world := s.current()
	resp := &types.Response{Content: "a short answer"} // 无 usage
	req := &types.Request{Messages: []types.Message{{Role: types.RoleUser, Content: "问一下"}}}
	cost, usage := s.settleCost(world, "mock-mid", resp, req)
	if !usage.Estimated || usage.PromptTokens == 0 || usage.CompletionTokens == 0 {
		t.Fatalf("estimated usage = %+v", usage)
	}
	if usage.TotalTokens != usage.PromptTokens+usage.CompletionTokens || cost <= 0 {
		t.Fatalf("settle = %f %+v", cost, usage)
	}
	// savings：chat 基线是 mock-premium，成本必然低于基线 → 节省为正
	sav := s.savings(world, "chat", usage, cost)
	if sav <= 0 {
		t.Fatalf("savings = %f", sav)
	}
}

func TestFeedbackUnknownRequest(t *testing.T) {
	_, base := newLocalServer(t)
	body := `{"request_id":"or-unknown","verdict":"good"}`
	resp, out := postJSON(t, base+"/v1/feedback", []byte(body), nil)
	if resp.StatusCode != http.StatusOK || !contains(out, `"applied":false`) {
		t.Fatalf("unknown-request feedback = %d %s", resp.StatusCode, out)
	}
}

func contains(b []byte, sub string) bool {
	return indexOf(b, sub) >= 0
}

func indexOf(b []byte, sub string) int {
	for i := 0; i+len(sub) <= len(b); i++ {
		if string(b[i:i+len(sub)]) == sub {
			return i
		}
	}
	return -1
}
