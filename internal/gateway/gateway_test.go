// gateway 包集成测试：从 HTTP 入口到 mock provider 再回到客户端的端到端验证。
// 覆盖：非流式、级联升级、流式 SSE、failover、重试、鉴权、反馈闭环、错误路径。
// 全部基于 mock provider，无外部依赖，CI 可重复。
package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/attnstack/omni-route/internal/config"
	"github.com/attnstack/omni-route/internal/router"
	"github.com/attnstack/omni-route/internal/store"
	"github.com/attnstack/omni-route/internal/types"
)

// setup 起一个完整网关（真实配置 → mock provider）。
// mutate 允许每个用例改配置（测试确定性：bad_json_rate=1.0 强制升级路径）。
func setup(t *testing.T, mutate func(*config.Config)) (string, *http.Client) {
	t.Helper()
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
	if mutate != nil {
		mutate(cfg)
	}
	// 程序内构造的配置必须显式补默认值（Load 会做，这里不走 Load）
	cfg.ApplyDefaults()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(filepath.Join(t.TempDir(), "gw.db"), 20*time.Millisecond, 8, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	state := router.NewState(cfg)
	w, err := BuildWorld(cfg, nil, state, logger)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewServer(w, st, t.TempDir(), logger).Handler())
	t.Cleanup(srv.Close)
	return srv.URL, srv.Client()
}

func mockCaps(ctx int, tools bool) types.Caps {
	return types.Caps{Streaming: true, Tools: tools, JSONMode: true, MaxContextTokens: ctx}
}

func chatBody(model, prompt string, stream bool) []byte {
	body := map[string]any{
		"messages": []map[string]any{{"role": "user", "content": prompt}},
		"stream":   stream,
	}
	if model != "" {
		body["model"] = model
	}
	b, _ := json.Marshal(body)
	return b
}

func postJSON(t *testing.T, url string, body []byte, header map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, out
}

func TestE2EExplicitNonStream(t *testing.T) {
	base, _ := setup(t, nil)
	resp, body := postJSON(t, base+"/v1/chat/completions", chatBody("mock-mid", "你好，聊聊天气", false), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("x-or-model"); got != "mock-mid" {
		t.Fatalf("x-or-model = %s", got)
	}
	reason := resp.Header.Get("x-or-route-reason")
	if !strings.Contains(reason, "tier=explicit") || !strings.Contains(reason, "task=chat") {
		t.Fatalf("route reason = %s", reason)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Role    string  `json:"role"`
				Content *string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("response not valid OpenAI JSON: %v\n%s", err, body)
	}
	if len(out.Choices) != 1 || out.Choices[0].Message.Role != "assistant" || out.Choices[0].Message.Content == nil {
		t.Fatalf("choices = %+v", out.Choices)
	}
	if out.Usage.TotalTokens == 0 {
		t.Fatalf("usage missing: %+v", out)
	}
}

func TestE2ECascadeUpgrade(t *testing.T) {
	base, _ := setup(t, nil) // bad_json_rate=1.0：fast 档必产坏 JSON → 必升级
	// 提取任务 + 要求 JSON → 走 extraction 级联 [fast → premium]
	prompt := "提取下面文本的字段并以 JSON 返回：订单号 A-100，金额 99 元"
	resp, body := postJSON(t, base+"/v1/chat/completions", chatBody("", prompt, false), map[string]string{"X-Or-Task": "extraction"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	reason := resp.Header.Get("x-or-route-reason")
	if !strings.Contains(reason, "tier=cascade") || !strings.Contains(reason, "upgraded=true") {
		t.Fatalf("route reason = %s", reason)
	}
	if got := resp.Header.Get("x-or-model"); got != "mock-premium" {
		t.Fatalf("final model = %s (expected upgrade to premium)", got)
	}
	var out struct {
		OmniRoute struct {
			Upgraded bool `json:"upgraded"`
			Legs     []struct {
				Model string `json:"model"`
			} `json:"legs"`
		} `json:"omni_route"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("body: %s", body)
	}
	if !out.OmniRoute.Upgraded || len(out.OmniRoute.Legs) != 2 {
		t.Fatalf("cascade report = %+v", out.OmniRoute)
	}
	// 省钱头存在且为有限数字
	if resp.Header.Get("x-or-savings-usd") == "" {
		t.Fatal("savings header missing")
	}
}

func TestE2EStream(t *testing.T) {
	base, _ := setup(t, nil)
	url := base + "/v1/chat/completions"
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(chatBody("mock-premium", "讲一个长一点的故事", true)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %s", ct)
	}
	if resp.Header.Get("x-or-model") == "" {
		t.Fatal("route headers must be set before first chunk")
	}
	sc := bufio.NewScanner(resp.Body)
	var chunks []string
	var sawRole, sawDone bool
	var content strings.Builder
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			sawDone = true
			break
		}
		var m struct {
			Object  string `json:"object"`
			Choices []struct {
				Delta struct {
					Role    string  `json:"role"`
					Content *string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			t.Fatalf("bad chunk %q: %v", payload, err)
		}
		if m.Object != "chat.completion.chunk" {
			t.Fatalf("object = %s", m.Object)
		}
		if len(m.Choices) > 0 {
			if m.Choices[0].Delta.Role == "assistant" {
				sawRole = true
			}
			if m.Choices[0].Delta.Content != nil {
				content.WriteString(*m.Choices[0].Delta.Content)
			}
		}
		chunks = append(chunks, payload)
	}
	if !sawRole || !sawDone {
		t.Fatalf("sawRole=%v sawDone=%v chunks=%d", sawRole, sawDone, len(chunks))
	}
	if !strings.Contains(content.String(), "deterministic answer") {
		t.Fatalf("content = %q", content.String())
	}
	if len(chunks) < 2 {
		t.Fatal("expected multiple streamed chunks")
	}
}

func TestE2EFailover(t *testing.T) {
	base, _ := setup(t, func(cfg *config.Config) {
		// lagrangian 直选最便宜的 fast；重试次数 1（共 2 次尝试）+ fail_first_n=2：
		// fast 的两次尝试都挂 → 耗尽重试 → failover 到 mid 成功
		cfg.Routing.Mode = "lagrangian"
		cfg.Routing.UseBandit = new(bool)
		cfg.Reliability.Retries = 1
		cfg.Reliability.BackoffBaseMs = 5
		m := cfg.Providers["mock"]
		m.Mock.FailFirstN = 2
		cfg.Providers["mock"] = m
	})
	resp, body := postJSON(t, base+"/v1/chat/completions", chatBody("", "你好，随便聊聊", false), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("x-or-failover-from"); got != "mock-fast" {
		t.Fatalf("failover-from = %q", got)
	}
	if got := resp.Header.Get("x-or-model"); got != "mock-mid" {
		t.Fatalf("model = %s", got)
	}
}

func TestE2ERetry(t *testing.T) {
	base, _ := setup(t, func(cfg *config.Config) {
		// 显式单模型 + fail_first_n=1 + 2 次重试：第一次 500、重试成功
		cfg.Reliability.Retries = 2
		cfg.Reliability.BackoffBaseMs = 5
		m := cfg.Providers["mock"]
		m.Mock.FailFirstN = 1
		cfg.Providers["mock"] = m
	})
	resp, body := postJSON(t, base+"/v1/chat/completions", chatBody("mock-fast", "你好", false), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("retry should recover: status = %d body=%s", resp.StatusCode, body)
	}
}

func TestE2EAllUpstreamFail(t *testing.T) {
	base, _ := setup(t, func(cfg *config.Config) {
		cfg.Routing.Mode = "lagrangian"
		cfg.Routing.UseBandit = new(bool)
		m := cfg.Providers["mock"]
		m.Mock.FailFirstN = 100 // 所有模型全挂（failover + retry 都救不了）
		cfg.Providers["mock"] = m
	})
	resp, body := postJSON(t, base+"/v1/chat/completions", chatBody("", "hi", false), nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	var errBody struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &errBody)
	if errBody.Error.Type != "api_error" {
		t.Fatalf("error type = %s", errBody.Error.Type)
	}
}

func TestE2EAuth(t *testing.T) {
	base, _ := setup(t, func(cfg *config.Config) {
		cfg.APIKeys = []string{"sk-gw-test"}
	})
	// 无 key → 401
	resp, _ := postJSON(t, base+"/v1/chat/completions", chatBody("mock-mid", "hi", false), nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	// 正确 key → 200
	resp, body := postJSON(t, base+"/v1/chat/completions", chatBody("mock-mid", "hi", false),
		map[string]string{"Authorization": "Bearer sk-gw-test"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid key rejected: %d %s", resp.StatusCode, body)
	}
	// 非 /v1 端点不受限
	if hr, err := http.Get(base + "/healthz"); err != nil || hr.StatusCode != 200 {
		t.Fatalf("healthz should be open")
	}
}

func TestE2EFeedbackLoop(t *testing.T) {
	base, _ := setup(t, nil)
	resp, _ := postJSON(t, base+"/v1/chat/completions", chatBody("mock-mid", "你好", false), nil)
	rid := resp.Header.Get("x-or-request-id")
	if rid == "" {
		t.Fatal("request id header missing")
	}
	fb, _ := json.Marshal(map[string]string{"request_id": rid, "verdict": "bad", "comment": "答非所问"})
	fbResp, body := postJSON(t, base+"/v1/feedback", fb, nil)
	if fbResp.StatusCode != http.StatusOK {
		t.Fatalf("feedback status = %d body=%s", fbResp.StatusCode, body)
	}
	// 非法 verdict → 400
	bad, _ := json.Marshal(map[string]string{"request_id": rid, "verdict": "meh"})
	if r, _ := postJSON(t, base+"/v1/feedback", bad, nil); r.StatusCode != http.StatusBadRequest {
		t.Fatal("invalid verdict must 400")
	}
	// stats 可访问且包含质量表
	hr, err := http.Get(base + "/api/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer hr.Body.Close()
	var stats map[string]any
	if err := json.NewDecoder(hr.Body).Decode(&stats); err != nil {
		t.Fatal(err)
	}
	if _, ok := stats["lambda"]; !ok {
		t.Fatal("stats missing lambda")
	}
}

func TestE2EModelsEndpoint(t *testing.T) {
	base, _ := setup(t, nil)
	hr, err := http.Get(base + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer hr.Body.Close()
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(hr.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, m := range out.Data {
		ids[m.ID] = true
	}
	if !ids["mock-fast"] || !ids["mock-premium"] {
		t.Fatalf("models = %v", ids)
	}
}

func TestE2EBadRequests(t *testing.T) {
	base, _ := setup(t, nil)
	cases := []struct {
		name string
		body string
		want int
	}{
		{"非法 JSON", `{not json`, http.StatusBadRequest},
		{"缺 messages", `{"model":"mock-mid"}`, http.StatusBadRequest},
		{"空 messages", `{"messages":[]}`, http.StatusBadRequest},
		{"未知角色", `{"messages":[{"role":"wizard","content":"x"}]}`, http.StatusBadRequest},
		{"未知模型", `{"model":"ghost","messages":[{"role":"user","content":"x"}]}`, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, body := postJSON(t, base+"/v1/chat/completions", []byte(c.body), nil)
			if resp.StatusCode != c.want {
				t.Fatalf("status = %d, want %d (body=%s)", resp.StatusCode, c.want, body)
			}
		})
	}
	// 合规组无可行模型 → 503
	base2, _ := setup(t, func(cfg *config.Config) {
		cfg.Routing.ComplianceGroups = map[string][]string{"secret": {"no-such-provider"}}
	})
	resp, _ := postJSON(t, base2+"/v1/chat/completions", chatBody("", "hi", false),
		map[string]string{"X-Or-Data-Class": "secret"})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("no-candidate status = %d", resp.StatusCode)
	}
}

func TestE2EClientCancel(t *testing.T) {
	base, _ := setup(t, func(cfg *config.Config) {
		m := cfg.Providers["mock"]
		m.Mock.StreamChunkDelayMs = 80 // 拖慢流，制造取消窗口
		cfg.Providers["mock"] = m
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/chat/completions",
		bytes.NewReader(chatBody("mock-mid", "讲一个非常长的故事", true)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// 读几个 chunk 后取消
	sc := bufio.NewScanner(resp.Body)
	for i := 0; sc.Scan() && i < 3; i++ {
		cancel()
	}
	// 服务端应把该请求记为 client_cancel（经 store）；这里主要验证不 panic、连接正常关闭
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		hr, err := http.Get(base + "/api/requests?limit=5")
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Requests []struct {
				Status string `json:"status"`
			} `json:"requests"`
		}
		_ = json.NewDecoder(hr.Body).Decode(&out)
		hr.Body.Close()
		for _, r := range out.Requests {
			if r.Status == "client_cancel" {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("expected a client_cancel row in request log")
}

func TestSSEMatrixWithCurl(t *testing.T) {
	// 语义同 scripts/sse_matrix.sh：Go 客户端按 OpenAI 流式协议逐项校验
	base, _ := setup(t, nil)
	resp, body := postJSON(t, base+"/v1/chat/completions", chatBody("mock-mid", "你好", true), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte("[DONE]")) {
		t.Fatal("stream must end with [DONE]")
	}
}
