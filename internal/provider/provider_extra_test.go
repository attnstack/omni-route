// provider_extra_test.go：注册表接口、协议翻译边角、SSE 错误路径与 mock 工具调用的补充覆盖。
package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/attnstack/omni-route/internal/config"
	"github.com/attnstack/omni-route/internal/types"
)

func TestRegistryAccessors(t *testing.T) {
	reg, _ := mockRegistry(t, 0, 0)

	if _, ok := reg.Model("mock-fast"); !ok {
		t.Fatal("mock-fast missing")
	}
	models := reg.Models()
	if len(models) != 2 || models[0].Name >= models[1].Name {
		t.Fatalf("Models not sorted: %v", models)
	}
	// Resolve：模型 → provider 实例；未知模型报错
	p, info, err := reg.Resolve("mock-fast")
	if err != nil || p.Name() != "mock" || info.Provider != "mock" {
		t.Fatalf("Resolve = %v %v %v", p, info, err)
	}
	if _, _, err := reg.Resolve("ghost"); err == nil {
		t.Fatal("unknown model must error")
	}
	// Cost：单价结算；未知模型返回 false
	if c, ok := reg.Cost("mock-fast", 1_000_000, 1_000_000); !ok || c <= 0 {
		t.Fatalf("Cost = %f %v", c, ok)
	}
	if _, ok := reg.Cost("ghost", 1, 1); ok {
		t.Fatal("unknown model cost must be !ok")
	}
	// Name()
	m, _ := reg.Provider("mock")
	if m.Name() != "mock" {
		t.Fatalf("mock Name = %s", m.Name())
	}
}

func TestTranslateToolChoice(t *testing.T) {
	if got := translateToolChoice("auto"); got["type"] != "auto" {
		t.Fatalf("auto = %v", got)
	}
	if got := translateToolChoice("none"); got["type"] != "none" {
		t.Fatalf("none = %v", got)
	}
	if got := translateToolChoice("required"); got["type"] != "any" {
		t.Fatalf("required = %v", got)
	}
	if got := translateToolChoice(map[string]any{"function": map[string]any{"name": "f"}}); got["type"] != "tool" || got["name"] != "f" {
		t.Fatalf("named = %v", got)
	}
	if got := translateToolChoice(42); got != nil {
		t.Fatalf("unknown shape = %v", got)
	}
}

func TestNormalizeFinish(t *testing.T) {
	if normalizeFinish("function_call") != "tool_calls" {
		t.Fatal("function_call must map to tool_calls")
	}
	if normalizeFinish("") != "" {
		t.Fatal("empty stays empty")
	}
	if normalizeFinish("exotic") != "exotic" {
		t.Fatal("unknown values pass through")
	}
	if normalizeFinish("length") != "length" {
		t.Fatal("known values unchanged")
	}
}

func TestOpenAIStreamNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream exploded"))
	}))
	defer srv.Close()
	p := newOpenAICompat("openai", config1(srv.URL), "sk", srv.Client())
	_, err := p.Stream(context.Background(), &types.Request{
		Model: "m", Stream: true,
		Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}},
	})
	ue, ok := err.(*types.UpstreamError)
	if !ok || ue.Status != 502 || !strings.Contains(ue.Body, "exploded") {
		t.Fatalf("stream non-200 = %v", err)
	}
}

func TestOpenAIStreamContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(5 * time.Second) // 挂住连接，等客户端取消
	}))
	defer srv.Close()
	p := newOpenAICompat("openai", config1(srv.URL), "sk", srv.Client())

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	st, err := p.Stream(ctx, &types.Request{
		Model: "m", Stream: true,
		Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for {
		_, err := st.Next()
		if err == nil {
			continue
		}
		if strings.Contains(err.Error(), "context") {
			return // 取消传播成功
		}
		t.Fatalf("want context error, got %v", err)
	}
}

func TestMockStreamToolCall(t *testing.T) {
	reg, _ := mockRegistry(t, 0, 0)
	p, _ := reg.Provider("mock")
	// 中高档模型 + 带 tools 的请求按哈希概率触发工具调用；多试几个 prompt 保证命中
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "get_weather"}}}
	hit := false
	for i := 0; i < 12 && !hit; i++ {
		prompt := []types.Message{{Role: types.RoleUser, Content: strings.Repeat("p", i+1)}}
		st, err := p.Stream(context.Background(), &types.Request{
			Model: "mock-mid", Stream: true, Messages: prompt, Tools: tools,
		})
		if err != nil {
			t.Fatal(err)
		}
		var name string
		for {
			ch, err := st.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, tc := range ch.ToolCalls {
				if tc.Name != "" {
					name = tc.Name
				}
			}
		}
		st.Close()
		if name == "get_weather" {
			hit = true
		}
	}
	if !hit {
		t.Fatal("tool-call stream path never triggered in 12 tries")
	}
}

func TestLastUserContent(t *testing.T) {
	if got := lastUserContent([]types.Message{{Role: types.RoleAssistant, Content: "a"}}); got != "a" {
		t.Fatalf("no-user fallback = %q", got)
	}
	if got := lastUserContent(nil); got != "" {
		t.Fatalf("empty = %q", got)
	}
}

// config0/config1 是本文件的极简配置构造（避免与其他测试文件的 fixture 耦合）。
func config0() config.Provider { return config.Provider{Kind: "anthropic"} }

func config1(base string) config.Provider {
	return config.Provider{Kind: "openai", BaseURL: base + "/v1"}
}
