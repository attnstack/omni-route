// openai_compat.go：OpenAI 协议适配器——一个适配器覆盖 OpenAI/DeepSeek/Qwen/
// Ollama 等所有 OpenAI 系 provider。翻译策略：类型化字段驱动出站 body 构造，
// 未知扩展字段（top_p、frequency_penalty 等）经 req.Raw 透传，不做白名单。
package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/attnstack/omni-route/internal/config"
	"github.com/attnstack/omni-route/internal/types"
)

type openAICompat struct {
	name   string
	cfg    config.Provider
	apiKey string
	client *http.Client
}

func newOpenAICompat(name string, cfg config.Provider, apiKey string, client *http.Client) *openAICompat {
	return &openAICompat{name: name, cfg: cfg, apiKey: apiKey, client: client}
}

func (p *openAICompat) Name() string { return p.name }

func (p *openAICompat) endpoint(path string) string {
	base := strings.TrimRight(p.cfg.BaseURL, "/")
	return base + path
}

func (p *openAICompat) headers() map[string]string {
	h := map[string]string{}
	if p.apiKey != "" {
		h["Authorization"] = "Bearer " + p.apiKey
	}
	return h
}

// buildBody 构造出站请求体。stream_options.include_usage 由网关强制补上：
// 没有用量数据，省钱仪表盘就是无米之炊。provider 不认识该字段时会忽略或报错，
// 报错场景由 config 层面关流处理（不猜协议，让错误显式暴露）。
func (p *openAICompat) buildBody(req *types.Request) map[string]any {
	body := map[string]any{}
	for k, v := range req.Raw {
		if k != "model" && k != "messages" && k != "stream" && k != "stream_options" &&
			k != "tools" && k != "tool_choice" && k != "temperature" && k != "max_tokens" && k != "max_completion_tokens" {
			body[k] = v
		}
	}
	body["model"] = req.Model
	body["messages"] = encodeMessages(req.Messages)
	if req.Stream {
		body["stream"] = true
		body["stream_options"] = map[string]any{"include_usage": true}
	} else {
		body["stream"] = false
	}
	if len(req.Tools) > 0 {
		body["tools"] = req.Tools
		if req.ToolChoice != nil {
			body["tool_choice"] = req.ToolChoice
		}
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	return body
}

func encodeMessages(msgs []types.Message) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		entry := map[string]any{"role": m.Role, "content": m.Content}
		if len(m.ToolCalls) > 0 {
			tcs := make([]map[string]any, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				tcs = append(tcs, map[string]any{
					"id": tc.ID, "type": "function",
					"function": map[string]any{"name": tc.Name, "arguments": tc.ArgsJSON},
				})
			}
			entry["tool_calls"] = tcs
		}
		if m.ToolCallID != "" {
			entry["tool_call_id"] = m.ToolCallID
		}
		if m.Name != "" {
			entry["name"] = m.Name
		}
		out = append(out, entry)
	}
	return out
}

func (p *openAICompat) Complete(ctx context.Context, req *types.Request) (*types.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint("/chat/completions"), jsonBody(p.buildBody(req)))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	for k, v := range p.headers() {
		httpReq.Header.Set(k, v)
	}
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return nil, err
	}
	var wire openaiResponse
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return nil, err
	}
	return wire.toResponse(), nil
}

func (p *openAICompat) Stream(ctx context.Context, req *types.Request) (types.Stream, error) {
	resp, err := doStream(ctx, p.client, p.endpoint("/chat/completions"), p.headers(), p.buildBody(req))
	if err != nil {
		return nil, err
	}
	return newSSEStream(resp, decodeOpenAIData), nil
}

// ---- OpenAI 协议解码 ----

type openaiResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Role             string           `json:"role"`
			Content          string           `json:"content"`
			ReasoningContent string           `json:"reasoning_content"`
			ToolCalls        []openaiToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage openaiUsage `json:"usage"`
}

type openaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func (u openaiUsage) toUsage() types.Usage {
	return types.Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
	}
}

func (w *openaiResponse) toResponse() *types.Response {
	r := &types.Response{ID: w.ID, Model: w.Model, Usage: w.Usage.toUsage()}
	if len(w.Choices) > 0 {
		c := w.Choices[0]
		r.Content = c.Message.Content
		r.Reasoning = c.Message.ReasoningContent
		r.FinishReason = normalizeFinish(c.FinishReason)
		for _, tc := range c.Message.ToolCalls {
			r.ToolCalls = append(r.ToolCalls, types.ToolCall{ID: tc.ID, Name: tc.Function.Name, ArgsJSON: tc.Function.Arguments})
		}
	}
	return r
}

func normalizeFinish(s string) string {
	switch s {
	case "stop", "length", "tool_calls", "content_filter":
		return s
	case "":
		return ""
	case "function_call":
		return "tool_calls"
	default:
		return s
	}
}

// decodeOpenAIData 把一条 data 行翻译为 0..n 个统一 chunk。
func decodeOpenAIData(data []byte) ([]types.Chunk, error) {
	var wire struct {
		ID      string `json:"id"`
		Choices []struct {
			Delta struct {
				Role             string                `json:"role"`
				Content          string                `json:"content"`
				ReasoningContent string                `json:"reasoning_content"`
				ToolCalls        []openaiToolCallDelta `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *openaiUsage `json:"usage"`
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, err
	}
	if wire.Error != nil {
		return nil, &types.UpstreamError{Status: 502, Body: "upstream stream error: " + wire.Error.Message}
	}
	var out []types.Chunk
	for _, c := range wire.Choices {
		ch := types.Chunk{DeltaContent: c.Delta.Content, ReasoningContent: c.Delta.ReasoningContent, HasRole: c.Delta.Role != ""}
		for _, tc := range c.Delta.ToolCalls {
			ch.ToolCalls = append(ch.ToolCalls, types.ToolCallDelta{
				Index: tc.Index, ID: tc.ID, Name: tc.Function.Name, ArgsText: tc.Function.Arguments,
			})
		}
		if c.FinishReason != "" {
			ch.FinishReason = normalizeFinish(c.FinishReason)
		}
		out = append(out, ch)
	}
	// OpenAI 约定：usage 出现在 choices 为空的收尾 chunk；部分 provider 会
	// 在最后一个带 finish_reason 的 chunk 里同时带 usage——两种都接。
	if wire.Usage != nil {
		u := wire.Usage.toUsage()
		if len(out) > 0 {
			out[len(out)-1].Usage = &u
		} else {
			out = append(out, types.Chunk{Usage: &u})
		}
	}
	return out, nil
}

type openaiToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// 保证接口实现（编译期检查，避免方法签名漂移后静默失效）。
var (
	_ Provider = (*openAICompat)(nil)
	_ Provider = (*anthropic)(nil)
	_ Provider = (*mock)(nil)
)
