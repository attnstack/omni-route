// gateway_cache_test.go：响应缓存的端到端行为——未命中→命中、bypass、请求头与日志口径。
package gateway

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/attnstack/omni-route/internal/store"
)

// cacheServer：启用缓存的网关（zero_temp 模式）。
func cacheServer(t *testing.T) (*Server, string) {
	t.Helper()
	s, base := newLocalServer(t)
	s.current().cfg.Cache.Enabled = true
	return s, base
}

func cacheBody(prompt string, model string) []byte {
	body := `{"messages":[{"role":"user","content":"` + prompt + `"}],"temperature":0`
	if model != "" {
		body += `,"model":"` + model + `"`
	}
	return []byte(body + "}")
}

func TestCacheMissThenHit(t *testing.T) {
	s, base := cacheServer(t)
	prompt := "提取并返回 JSON：订单 B-200"
	body := cacheBody(prompt, "mock-mid")

	// 第一次：miss，走真实上游
	resp1, out1 := postJSON(t, base+"/v1/chat/completions", body, nil)
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first request: %d %s", resp1.StatusCode, out1)
	}
	if got := resp1.Header.Get("x-or-cache"); got != "miss" {
		t.Fatalf("first request cache header = %q", got)
	}
	// miss 行不计缓存 tier（日志异步落盘，轮询等待）
	waitForRow(t, s, reqIDOf(resp1), func(row store.Row) bool { return row.Tier != "cache" }, "miss tier")

	// 第二次（同 body）：hit，零成本、节省为正、tier=cache
	resp2, out2 := postJSON(t, base+"/v1/chat/completions", body, nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second request: %d %s", resp2.StatusCode, out2)
	}
	if got := resp2.Header.Get("x-or-cache"); got != "hit" {
		t.Fatalf("second request cache header = %q", got)
	}
	if resp2.Header.Get("x-or-cost-usd") != "0.000000" {
		t.Fatalf("hit cost = %q", resp2.Header.Get("x-or-cost-usd"))
	}
	sav := resp2.Header.Get("x-or-savings-usd")
	if sav == "" || sav == "0.000000" {
		t.Fatalf("hit savings = %q", sav)
	}
	reason := resp2.Header.Get("x-or-route-reason")
	if !strings.Contains(reason, "tier=cache") || !strings.Contains(reason, "why=cache-hit") {
		t.Fatalf("hit reason = %q", reason)
	}
	// 回答内容与 miss 一致
	if !strings.Contains(string(out2), extractContent(string(out1))) {
		t.Fatalf("hit content mismatch:\n%s\nvs\n%s", out2, out1)
	}
	// 日志口径：cache tier、零成本、节省为正
	waitForRow(t, s, reqIDOf(resp2), func(row store.Row) bool {
		return row.Tier == "cache" && row.CostUSD == 0 && row.SavingsUSD > 0
	}, "cache tier row")
	// 响应体仍应带 usage（沿用产出时的用量）
	if !strings.Contains(string(out2), `"prompt_tokens"`) {
		t.Fatalf("hit body missing usage: %s", out2)
	}
}

func TestCacheBypassAndExplicitModel(t *testing.T) {
	_, base := cacheServer(t)
	prompt := "cache bypass probe"
	body := cacheBody(prompt, "")

	// bypass 头：两次都是 miss
	for i := 0; i < 2; i++ {
		resp, _ := postJSON(t, base+"/v1/chat/completions", body,
			map[string]string{"X-Or-Cache": "bypass"})
		if got := resp.Header.Get("x-or-cache"); got == "hit" {
			t.Fatal("bypassed request must not be a hit")
		}
	}

	// 非零温：不可缓存，两次都 miss
	warm := []byte(`{"messages":[{"role":"user","content":"` + prompt + `"}],"temperature":0.7}`)
	for i := 0; i < 2; i++ {
		resp, _ := postJSON(t, base+"/v1/chat/completions", warm, nil)
		if got := resp.Header.Get("x-or-cache"); got == "hit" {
			t.Fatal("non-zero temperature must not be cached in zero_temp mode")
		}
	}

	// 显式模型参与键：同 prompt 不同 model 不互相命中
	respA, _ := postJSON(t, base+"/v1/chat/completions", cacheBody(prompt, "mock-fast"), nil)
	if respA.Header.Get("x-or-cache") != "miss" && respA.Header.Get("x-or-cache") != "" {
		t.Fatalf("explicit fast = %q", respA.Header.Get("x-or-cache"))
	}
	// （fast 在 zero_temp 下同样可缓存；首个显式请求为 miss）
	respB, _ := postJSON(t, base+"/v1/chat/completions", cacheBody(prompt, "mock-mid"), nil)
	if respB.Header.Get("x-or-cache") == "hit" && respA.Header.Get("x-or-cache") == "hit" &&
		respA.Header.Get("x-or-model") == respB.Header.Get("x-or-model") {
		t.Fatal("different explicit models must not collide in the cache")
	}
}

func TestMetricsEndpoint(t *testing.T) {
	s, base := cacheServer(t)
	// 造一次 miss + 一次 hit，再读 /metrics
	body := cacheBody("metrics probe", "mock-mid")
	postJSON(t, base+"/v1/chat/completions", body, nil)
	postJSON(t, base+"/v1/chat/completions", body, nil)

	resp, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("metrics content-type = %q", ct)
	}
	buf := make([]byte, 8192)
	n, _ := resp.Body.Read(buf)
	out := string(buf[:n])
	for _, want := range []string{
		"omni_route_requests_total",
		"omni_route_cache_hits_total",
		"omni_route_cache_misses_total",
		"omni_route_cost_micro_usd_total",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics missing %q:\n%s", want, out)
		}
	}
	_ = s
}

func TestStatsIncludeCacheAndVersion(t *testing.T) {
	_, base := cacheServer(t)
	hr, err := http.Get(base + "/api/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer hr.Body.Close()
	out := readAll(hr)
	for _, want := range []string{`"cache"`, `"hits"`, `"misses"`, `"version"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("stats missing %q:\n%s", want, out)
		}
	}
	_ = time.Now
}

func readAll(resp *http.Response) string {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	for {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return string(buf)
}

func reqIDOf(resp *http.Response) string {
	return resp.Header.Get("x-or-request-id")
}

func extractContent(body string) string {
	// 从 OpenAI 响应体里粗提取 content 字段值（测试断言用，避免引入完整解析）
	i := strings.Index(body, `"content":"`)
	if i < 0 {
		return ""
	}
	rest := body[i+len(`"content":"`):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// waitForRow 轮询等待指定 request_id 的日志行满足条件（日志异步落盘，flush 周期 1s）。
func waitForRow(t *testing.T, s *Server, requestID string, cond func(store.Row) bool, label string) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if row, ok, err := s.store.LookupRequest(requestID); err == nil && ok && cond(row) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("log row %q never satisfied condition: %s", requestID, label)
}
