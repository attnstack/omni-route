// router 测试助手：mock-only 注册表与静音 logger。
// 三个模型覆盖路由的关键维度：便宜/小窗口（m-a）、贵/全能力（m-b）、
// 本地 provider（m-local，合规组测试用）。
package router

import (
	"io"
	"log/slog"
	"os"

	"github.com/attnstack/omni-route/internal/config"
	"github.com/attnstack/omni-route/internal/provider"
	"github.com/attnstack/omni-route/internal/types"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestRegistry(cfg *config.Config) *provider.Registry {
	testCfg := *cfg // 浅拷贝，不污染调用方的 Routing 配置
	testCfg.Providers = map[string]config.Provider{
		"mock":  {Kind: "mock"},
		"local": {Kind: "mock"},
	}
	testCfg.Models = map[string]config.Model{
		"m-a": {
			Provider: "mock", PriceIn: 0.1, PriceOut: 0.4, MockTier: "fast",
			Caps: types.Caps{Streaming: true, Tools: false, JSONMode: true, MaxContextTokens: 4096},
		},
		"m-b": {
			Provider: "mock", PriceIn: 2.5, PriceOut: 10.0, MockTier: "premium",
			Caps: types.Caps{Streaming: true, Tools: true, JSONMode: true, MaxContextTokens: 128000},
		},
		"m-local": {
			// 本地模型：免费标签容易误导测试——给它一个象征性单价，
			// 并保持小上下文（本地 7B 的真实约束），让"上下文过滤"可测
			Provider: "local", PriceIn: 0.5, PriceOut: 2.0, MockTier: "mid",
			Caps: types.Caps{Streaming: true, Tools: true, MaxContextTokens: 2048},
		},
	}
	reg, err := provider.Build(&testCfg, nil, discardLogger())
	if err != nil {
		panic(err)
	}
	return reg
}

func osWriteFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
