// config 包单测：默认值、校验聚合、热加载监听。
package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func validYAML() []byte {
	return []byte(`
listen: ":9090"
providers:
  mock: { kind: mock }
models:
  m-a: { provider: mock, price_in: 0.1, price_out: 0.4, mock_tier: fast }
routing:
  mode: cascade
  budget_per_task: { chat: 1.0 }
cascades:
  chat: [m-a]
`)
}

func TestLoadValid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ok.yaml")
	if err := os.WriteFile(path, validYAML(), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":9090" {
		t.Fatalf("listen = %s", cfg.Listen)
	}
	// 默认值必须补齐
	if cfg.Routing.Lambda.Init == 0 || cfg.Cascade.Confidence.Threshold == 0 ||
		cfg.Store.FlushIntervalMs == 0 || cfg.Timeouts.TotalMs == 0 {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	if cfg.Routing.UseBandit == nil || !*cfg.Routing.UseBandit {
		t.Fatal("bandit should default on")
	}
}

func TestLoadInvalid(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{"未知 mode", "routing: {mode: turbo}\nproviders: {a: {kind: mock}}\n"},
		{"模型引用不存在", "providers: {a: {kind: mock}}\nmodels: {m: {provider: ghost}}\n"},
		{"级联链引用不存在", "providers: {a: {kind: mock}}\ncascades: {chat: [ghost]}\n"},
		{"未知任务", "routing: {budget_per_task: {hacking: 1}}\nproviders: {a: {kind: mock}}\n"},
		{"未知 provider kind", "providers: {a: {kind: carrier-pigeon}}\n"},
		{"mock 缺 base_url 不报错但真实 provider 必须有", "providers: {a: {kind: openai}}\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bad.yaml")
			if err := os.WriteFile(path, []byte(c.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil {
				t.Fatalf("expected validation error for %s", c.name)
			}
		})
	}
	// 聚合错误一次报全
	path := filepath.Join(t.TempDir(), "multi.yaml")
	if err := os.WriteFile(path, []byte("providers: {a: {kind: mock}}\nmodels: {m: {provider: x}}\ncascades: {chat: [y, z]}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || countLines(err.Error()) < 3 {
		t.Fatalf("aggregated errors expected, got: %v", err)
	}
}

func countLines(s string) int {
	n := 0
	for _, r := range s {
		if r == '\n' {
			n++
		}
	}
	return n + 1
}

func TestWatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hot.yaml")
	if err := os.WriteFile(path, validYAML(), 0o644); err != nil {
		t.Fatal(err)
	}
	changed := make(chan *Config, 1)
	stop := Watch(path, 20*time.Millisecond, func(c *Config, err error) {
		if err == nil {
			select {
			case changed <- c:
			default:
			}
		}
	})
	defer stop()
	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(path, append(validYAML(), []byte("# touched\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	case <-time.After(2 * time.Second):
		t.Fatal("watch did not fire on change")
	}
}

func TestHolder(t *testing.T) {
	cfg := &Config{Listen: ":1"}
	h := NewHolder(cfg)
	if h.Get() != cfg {
		t.Fatal("holder roundtrip")
	}
	cfg2 := &Config{Listen: ":2"}
	h.Set(cfg2)
	if h.Get() != cfg2 {
		t.Fatal("holder swap")
	}
}
