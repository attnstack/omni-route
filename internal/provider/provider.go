// Package provider 定义 provider 适配器接口、协议实现与模型注册表。
// 设计取舍：客户端永远看到 OpenAI 协议；对 OpenAI 系 provider 做字段级翻译，
// 对 Anthropic 做完整双向翻译。新增 provider = 实现一个接口，路由/级联/观测零改动。
package provider

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"

	"github.com/attnstack/omni-route/internal/config"
	"github.com/attnstack/omni-route/internal/types"
)

// Provider 是协议适配器接口。热路径只有两个方法：
// Complete（非流式）与 Stream（流式，返回统一 chunk 流）。
type Provider interface {
	Name() string
	// Complete 发送非流式请求，返回已翻译为 OpenAI 语义的完整回答。
	Complete(ctx context.Context, req *types.Request) (*types.Response, error)
	// Stream 建立流式连接。返回后即已收到 200，之后的 chunk 通过 Next 拉取。
	Stream(ctx context.Context, req *types.Request) (types.Stream, error)
}

// ModelInfo 是注册表中的模型元数据（配置的内存镜像）。
type ModelInfo struct {
	Name     string
	Provider string
	PriceIn  float64 // USD / 1M prompt tokens
	PriceOut float64 // USD / 1M completion tokens
	Caps     types.Caps
	MockTier string
}

// Registry 是 provider 与模型的内存注册表。构造时全量校验，
// 运行期只读（热加载 = 整包重建注册表）。
type Registry struct {
	cfg       *config.Config
	providers map[string]Provider
	models    map[string]ModelInfo
}

// Build 按 config 构造全部 provider 与模型条目。
func Build(cfg *config.Config, client *http.Client, logger *slog.Logger) (*Registry, error) {
	if client == nil {
		client = &http.Client{Timeout: 0}
	}
	reg := &Registry{
		cfg:       cfg,
		providers: map[string]Provider{},
		models:    map[string]ModelInfo{},
	}
	// mock provider 需要知道自己名下模型的档位（决定好/坏答案的概率），先收集
	modelsByProvider := map[string]map[string]config.Model{}
	for name, mc := range cfg.Models {
		if modelsByProvider[mc.Provider] == nil {
			modelsByProvider[mc.Provider] = map[string]config.Model{}
		}
		modelsByProvider[mc.Provider][name] = mc
	}
	for name, pc := range cfg.Providers {
		key := ""
		if pc.KeyEnv != "" {
			key = os.Getenv(pc.KeyEnv)
			if key == "" {
				logger.Warn("provider api key env not set; provider will fail on use", "provider", name, "env", pc.KeyEnv)
			}
		}
		var p Provider
		var err error
		switch pc.Kind {
		case "openai":
			p = newOpenAICompat(name, pc, key, client)
		case "anthropic":
			p = newAnthropic(name, pc, key, client)
		case "mock":
			p = newMock(name, pc, modelsByProvider[name])
		default:
			err = fmt.Errorf("provider %s: unknown kind %q", name, pc.Kind)
		}
		if err != nil {
			return nil, err
		}
		reg.providers[name] = p
	}
	for name, mc := range cfg.Models {
		reg.models[name] = ModelInfo{
			Name:     name,
			Provider: mc.Provider,
			PriceIn:  mc.PriceIn,
			PriceOut: mc.PriceOut,
			Caps:     mc.Caps,
			MockTier: mc.MockTier,
		}
	}
	return reg, nil
}

func (r *Registry) Model(name string) (ModelInfo, bool) {
	m, ok := r.models[name]
	return m, ok
}

// Models 返回按名称排序的模型列表（/v1/models 与决策池共用）。
func (r *Registry) Models() []ModelInfo {
	out := make([]ModelInfo, 0, len(r.models))
	for _, m := range r.models {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Resolve 找到模型对应的 provider 实例。
func (r *Registry) Resolve(model string) (Provider, ModelInfo, error) {
	m, ok := r.models[model]
	if !ok {
		return nil, ModelInfo{}, fmt.Errorf("model %q not registered", model)
	}
	p, ok := r.providers[m.Provider]
	if !ok {
		return nil, ModelInfo{}, fmt.Errorf("model %q: provider %q missing", model, m.Provider)
	}
	return p, m, nil
}

// Cost 按单价表计算一次调用的美元成本。
func (r *Registry) Cost(model string, promptTokens, completionTokens int) (float64, bool) {
	m, ok := r.models[model]
	if !ok {
		return 0, false
	}
	return (m.PriceIn*float64(promptTokens) + m.PriceOut*float64(completionTokens)) / 1e6, true
}

func (r *Registry) Provider(name string) (Provider, bool) {
	p, ok := r.providers[name]
	return p, ok
}
