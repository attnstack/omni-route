// cache_test.go：键稳定性、TTL 过期、LRU 逐出与热调整。
package cache

import (
	"testing"
	"time"
)

func TestKeyStableAcrossFieldOrder(t *testing.T) {
	a := map[string]any{"model": "auto", "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "temperature": 0}
	b := map[string]any{"temperature": 0, "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "model": "auto"}
	if Key(a, "", "") != Key(b, "", "") {
		t.Fatal("same body in different key order must produce the same cache key")
	}
	// 显式模型参与键；auto 不参与
	auto := Key(map[string]any{"messages": "x", "model": "auto"}, "", "")
	noModel := Key(map[string]any{"messages": "x"}, "", "")
	if auto != noModel {
		t.Fatal("auto model must be excluded from the key")
	}
	explicit := Key(map[string]any{"messages": "x", "model": "m-a"}, "m-a", "")
	if explicit == noModel {
		t.Fatal("explicit model must be part of the key")
	}
	// stream 标志不参与
	// 租户隔离：同请求不同租户不共享
	t1 := Key(map[string]any{"messages": "x"}, "", "tenant-1")
	t2 := Key(map[string]any{"messages": "x"}, "", "tenant-2")
	if t1 == t2 || t1 == noModel {
		t.Fatal("tenant salt must isolate cache keys")
	}
	if Key(map[string]any{"messages": "x", "stream": true}, "", "") != Key(map[string]any{"messages": "x"}, "", "") {
		t.Fatal("stream flag must not affect the key")
	}
}

func TestPutGetAndTTL(t *testing.T) {
	c := New(50*time.Millisecond, 8)
	e := Entry{Model: "m", Response: &ResponseCopy{Content: "ans"}, CreatedAt: time.Now()}
	c.Put("k1", e)
	if got, ok := c.Get("k1"); !ok || got.Model != "m" || got.Response.Content != "ans" {
		t.Fatalf("roundtrip = %v %v", got, ok)
	}
	time.Sleep(80 * time.Millisecond)
	if _, ok := c.Get("k1"); ok {
		t.Fatal("entry must expire after TTL")
	}
}

func TestLRUEviction(t *testing.T) {
	c := New(time.Hour, 2)
	for _, k := range []string{"a", "b", "c"} {
		c.Put(k, Entry{Model: k, CreatedAt: time.Now()})
	}
	// 容量 2：最早的 a 被逐出，b/c 保留；访问 c 后再放 d → b 被逐出
	if _, ok := c.Get("a"); ok {
		t.Fatal("oldest entry should be evicted")
	}
	c.Get("c")
	c.Put("d", Entry{Model: "d", CreatedAt: time.Now()})
	if _, ok := c.Get("b"); ok {
		t.Fatal("LRU victim should be b after touching c")
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatal("recently used c must survive")
	}
}

func TestConfigureShrinksAndKeepsRecent(t *testing.T) {
	c := New(time.Hour, 4)
	for _, k := range []string{"k1", "k2", "k3", "k4"} {
		c.Put(k, Entry{Model: k, CreatedAt: time.Now()})
	}
	c.Configure(time.Hour, 2)
	if _, ok := c.Get("k1"); ok || c.lru.Len() != 2 {
		t.Fatalf("configure should evict down to 2, len=%d", c.lru.Len())
	}
	if _, ok := c.Get("k4"); !ok {
		t.Fatal("most recent entries survive shrink")
	}
}

func TestEntryRoundtripKeepsToolCalls(t *testing.T) {
	c := New(time.Hour, 4)
	e := Entry{
		Model: "m",
		Response: &ResponseCopy{
			ToolCalls: []ToolCall{{ID: "call_1", Name: "f", ArgsJSON: "{\"x\":1}"}},
		},
		Usage:     Usage{PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7},
		CreatedAt: time.Now(),
	}
	c.Put("tool", e)
	got, ok := c.Get("tool")
	if !ok || len(got.Response.ToolCalls) != 1 || got.Response.ToolCalls[0].Name != "f" || got.Usage.TotalTokens != 7 {
		t.Fatalf("toolcall roundtrip = %+v %v", got, ok)
	}
}
