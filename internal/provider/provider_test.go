// provider 包单测：协议翻译的契约测试。用 testdata 里录制的真实形态
// 响应/流，通过 httptest 回放——CI 不依赖任何真实 API key。
package provider

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/attnstack/omni-route/internal/config"
	"github.com/attnstack/omni-route/internal/types"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	return b
}

// ---- OpenAI 兼容适配器 ----

func TestOpenAIBuildBody(t *testing.T) {
	p := newOpenAICompat("deepseek", config.Provider{Kind: "openai", BaseURL: "https://x/v1"}, "sk", nil)
	temp := 0.3
	req := &types.Request{
		Model:       "deepseek-chat",
		Messages:    []types.Message{{Role: types.RoleSystem, Content: "be brief"}, {Role: types.RoleUser, Content: "hi"}},
		Stream:      true,
		Temperature: &temp,
		MaxTokens:   128,
		Tools:       []map[string]any{{"type": "function", "function": map[string]any{"name": "f"}}},
		Raw:         map[string]any{"top_p": 0.9, "model": "should-be-replaced"},
	}
	body := p.buildBody(req)
	blob, _ := json.Marshal(body)
	var m map[string]any
	_ = json.Unmarshal(blob, &m)
	if m["model"] != "deepseek-chat" {
		t.Fatalf("model not overridden: %v", m["model"])
	}
	if m["top_p"] != 0.9 {
		t.Fatal("raw passthrough lost")
	}
	so, ok := m["stream_options"].(map[string]any)
	if !ok || so["include_usage"] != true {
		t.Fatalf("stream_options.include_usage missing: %v", m["stream_options"])
	}
	if m["max_tokens"] != float64(128) {
		t.Fatalf("max_tokens = %v", m["max_tokens"])
	}
}

func TestOpenAICompleteFixture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Errorf("auth header missing")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(readFile(t, "../../testdata/openai/response.json"))
	}))
	defer srv.Close()

	p := newOpenAICompat("openai", config.Provider{Kind: "openai", BaseURL: srv.URL + "/v1"}, "sk-test", srv.Client())
	resp, err := p.Complete(context.Background(), &types.Request{
		Model:    "gpt-4o-mini",
		Messages: []types.Message{{Role: types.RoleUser, Content: "capital of France?"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "The capital of France is Paris." {
		t.Fatalf("content = %q", resp.Content)
	}
	if resp.Usage.PromptTokens != 12 || resp.Usage.CompletionTokens != 8 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	if resp.FinishReason != "stop" {
		t.Fatalf("finish = %s", resp.FinishReason)
	}
}

func TestOpenAIStreamFixture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(readFile(t, "../../testdata/openai/stream.sse"))
	}))
	defer srv.Close()

	p := newOpenAICompat("openai", config.Provider{Kind: "openai", BaseURL: srv.URL + "/v1"}, "sk", srv.Client())
	st, err := p.Stream(context.Background(), &types.Request{
		Model: "gpt-4o-mini", Stream: true,
		Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var content strings.Builder
	var finish string
	var usage *types.Usage
	for {
		ch, err := st.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		content.WriteString(ch.DeltaContent)
		if ch.FinishReason != "" {
			finish = ch.FinishReason
		}
		if ch.Usage != nil {
			usage = ch.Usage
		}
	}
	if content.String() != "Hello, world!" {
		t.Fatalf("content = %q", content.String())
	}
	if finish != "stop" {
		t.Fatalf("finish = %s", finish)
	}
	if usage == nil || usage.PromptTokens != 5 || usage.CompletionTokens != 4 {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestOpenAIStreamToolCallsFixture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(readFile(t, "../../testdata/openai/stream_toolcalls.sse"))
	}))
	defer srv.Close()

	p := newOpenAICompat("openai", config.Provider{Kind: "openai", BaseURL: srv.URL + "/v1"}, "sk", srv.Client())
	st, err := p.Stream(context.Background(), &types.Request{
		Model: "gpt-4o", Stream: true,
		Messages: []types.Message{{Role: types.RoleUser, Content: "weather?"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var id, name, args string
	var finish string
	for {
		ch, err := st.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range ch.ToolCalls {
			if tc.ID != "" {
				id = tc.ID
			}
			if tc.Name != "" {
				name = tc.Name
			}
			args += tc.ArgsText
		}
		if ch.FinishReason != "" {
			finish = ch.FinishReason
		}
	}
	if id != "call_abc" || name != "get_weather" {
		t.Fatalf("tool call header = %s/%s", id, name)
	}
	if args != `{"city":"Paris"}` {
		t.Fatalf("args = %q", args)
	}
	if finish != "tool_calls" {
		t.Fatalf("finish = %s", finish)
	}
}

func TestOpenAIUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer srv.Close()
	p := newOpenAICompat("openai", config.Provider{Kind: "openai", BaseURL: srv.URL + "/v1"}, "sk", srv.Client())
	_, err := p.Complete(context.Background(), &types.Request{
		Model: "m", Messages: []types.Message{{Role: types.RoleUser, Content: "x"}},
	})
	ue, ok := err.(*types.UpstreamError)
	if !ok || ue.Status != 429 || !ue.Retryable() {
		t.Fatalf("want UpstreamError 429, got %v", err)
	}
}

// ---- Anthropic 适配器 ----

func TestAnthropicRequestTranslation(t *testing.T) {
	p := newAnthropic("anthropic", config.Provider{Kind: "anthropic", DefaultMaxTokens: 8192}, "sk", nil)
	req := &types.Request{
		Model: "claude-sonnet",
		Messages: []types.Message{
			{Role: types.RoleSystem, Content: "be brief"},
			{Role: types.RoleUser, Content: "weather in Paris?"},
			{Role: types.RoleAssistant, Content: "", ToolCalls: []types.ToolCall{{ID: "t1", Name: "get_weather", ArgsJSON: `{"city":"Paris"}`}}},
			{Role: types.RoleTool, ToolCallID: "t1", Content: "20C sunny"},
		},
		Tools: []map[string]any{{
			"type": "function",
			"function": map[string]any{
				"name":        "get_weather",
				"description": "get weather",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
			},
		}},
	}
	body := p.buildBody(req)
	blob, _ := json.Marshal(body)
	var m struct {
		System    string `json:"system"`
		MaxTokens int    `json:"max_tokens"`
		Messages  []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(blob, &m); err != nil {
		t.Fatal(err)
	}
	if m.System != "be brief" || m.MaxTokens != 8192 {
		t.Fatalf("system/max_tokens = %q/%d", m.System, m.MaxTokens)
	}
	if len(m.Messages) != 3 {
		t.Fatalf("messages = %d (system must be lifted)", len(m.Messages))
	}
	// assistant 的 tool_calls → tool_use block
	asstBlob, _ := json.Marshal(m.Messages[1].Content)
	if !strings.Contains(string(asstBlob), "tool_use") || !strings.Contains(string(asstBlob), "get_weather") {
		t.Fatalf("assistant blocks = %s", asstBlob)
	}
	// role=tool → user + tool_result
	toolBlob, _ := json.Marshal(m.Messages[2].Content)
	if !strings.Contains(string(toolBlob), "tool_result") || !strings.Contains(string(toolBlob), "t1") {
		t.Fatalf("tool result blocks = %s", toolBlob)
	}
	if len(m.Tools) != 1 || m.Tools[0]["input_schema"] == nil {
		t.Fatalf("tools translation = %+v", m.Tools)
	}
}

func TestAnthropicCompleteFixture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "sk-an" || r.Header.Get("anthropic-version") == "" {
			t.Errorf("anthropic headers missing")
		}
		_, _ = w.Write(readFile(t, "../../testdata/anthropic/response.json"))
	}))
	defer srv.Close()
	p := newAnthropic("anthropic", config.Provider{Kind: "anthropic", BaseURL: srv.URL}, "sk-an", srv.Client())
	resp, err := p.Complete(context.Background(), &types.Request{
		Model:    "claude-sonnet",
		Messages: []types.Message{{Role: types.RoleUser, Content: "weather?"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "Let me check the weather. " {
		t.Fatalf("content = %q", resp.Content)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "get_weather" || resp.ToolCalls[0].ArgsJSON == "" {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
	if resp.FinishReason != "tool_calls" {
		t.Fatalf("finish = %s", resp.FinishReason)
	}
	if resp.Usage.PromptTokens != 21 || resp.Usage.CompletionTokens != 33 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

func TestAnthropicStreamFixture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(readFile(t, "../../testdata/anthropic/stream.sse"))
	}))
	defer srv.Close()
	p := newAnthropic("anthropic", config.Provider{Kind: "anthropic", BaseURL: srv.URL}, "sk-an", srv.Client())
	st, err := p.Stream(context.Background(), &types.Request{
		Model: "claude-sonnet", Stream: true,
		Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var content, args string
	var prompt, completion int
	var finish string
	var sawToolHeader bool
	for {
		ch, err := st.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		content += ch.DeltaContent
		for _, tc := range ch.ToolCalls {
			if tc.ID != "" && tc.Name != "" {
				sawToolHeader = true
			}
			args += tc.ArgsText
		}
		if ch.Usage != nil {
			if ch.Usage.PromptTokens > 0 {
				prompt = ch.Usage.PromptTokens
			}
			if ch.Usage.CompletionTokens > 0 {
				completion = ch.Usage.CompletionTokens
			}
		}
		if ch.FinishReason != "" {
			finish = ch.FinishReason
		}
	}
	if content != "Hello from Claude" {
		t.Fatalf("content = %q", content)
	}
	if !sawToolHeader || args != `{"city":"Tokyo"}` {
		t.Fatalf("tool stream broken: header=%v args=%q", sawToolHeader, args)
	}
	if prompt != 17 || completion != 42 {
		t.Fatalf("usage = %d/%d", prompt, completion)
	}
	if finish != "tool_calls" {
		t.Fatalf("finish = %s", finish)
	}
}

// ---- mock provider ----

func mockRegistry(t *testing.T, badRate, shortRate float64) (*Registry, *config.Config) {
	t.Helper()
	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"mock": {Kind: "mock", Mock: config.MockOptions{BadJSONRate: badRate, ShortRate: shortRate}},
		},
		Models: map[string]config.Model{
			"mock-fast":    {Provider: "mock", MockTier: "fast", PriceIn: 0.1, PriceOut: 0.4},
			"mock-premium": {Provider: "mock", MockTier: "premium", PriceIn: 2.5, PriceOut: 10},
		},
	}
	reg, err := Build(cfg, nil, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	return reg, cfg
}

func TestMockDeterministic(t *testing.T) {
	reg, _ := mockRegistry(t, 0, 0)
	p, _ := reg.Provider("mock")
	req := &types.Request{Model: "mock-fast", Messages: []types.Message{{Role: types.RoleUser, Content: "讲个故事"}}}
	r1, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	r2, _ := p.Complete(context.Background(), req)
	if r1.Content != r2.Content {
		t.Fatalf("mock must be deterministic:\n%q\n%q", r1.Content, r2.Content)
	}
	if r1.Content != GoodAnswer("mock-fast", "讲个故事") {
		t.Fatal("good answer must match exported oracle")
	}
	if r1.Usage.PromptTokens == 0 || r1.Usage.CompletionTokens == 0 {
		t.Fatalf("mock must report usage: %+v", r1.Usage)
	}
}

func TestMockBadJSONRateOne(t *testing.T) {
	// bad_rate=1.0 → fast 档必坏；premium 档按档位衰减后必好
	reg, _ := mockRegistry(t, 1.0, 0)
	p, _ := reg.Provider("mock")
	jsonReq := &types.Request{Messages: []types.Message{{Role: types.RoleUser, Content: "提取并返回 JSON 结果"}}}
	fast, _ := p.Complete(context.Background(), &types.Request{Model: "mock-fast", Messages: jsonReq.Messages})
	if fast.Content != `{"summary": ` {
		t.Fatalf("fast bad json = %q", fast.Content)
	}
	premium, _ := p.Complete(context.Background(), &types.Request{Model: "mock-premium", Messages: jsonReq.Messages})
	if !json.Valid([]byte(premium.Content)) {
		t.Fatalf("premium must stay good: %q", premium.Content)
	}
}

func TestMockFailFirstN(t *testing.T) {
	_, cfg := mockRegistry(t, 0, 0)
	pc := cfg.Providers["mock"]
	pc.Mock.FailFirstN = 2
	cfg.Providers["mock"] = pc
	reg, err := Build(cfg, nil, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	p, _ := reg.Provider("mock")
	req := &types.Request{Model: "mock-fast", Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}}}
	for i := 0; i < 2; i++ {
		if _, err := p.Complete(context.Background(), req); err == nil {
			t.Fatalf("call %d should fail (fail_first_n=2)", i)
		}
	}
	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("third call should succeed: %v", err)
	}
}

func TestMockStream(t *testing.T) {
	reg, _ := mockRegistry(t, 0, 0)
	p, _ := reg.Provider("mock")
	st, err := p.Stream(context.Background(), &types.Request{
		Model: "mock-fast", Stream: true,
		Messages: []types.Message{{Role: types.RoleUser, Content: "讲个故事"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var content strings.Builder
	var finish string
	for {
		ch, err := st.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		content.WriteString(ch.DeltaContent)
		if ch.FinishReason != "" {
			finish = ch.FinishReason
		}
	}
	if content.String() != GoodAnswer("mock-fast", "讲个故事") {
		t.Fatalf("streamed content mismatch")
	}
	if finish != "stop" {
		t.Fatalf("finish = %s", finish)
	}
}
