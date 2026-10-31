// anthropic.go：Anthropic Messages API 适配器。
// 入站永远是 OpenAI 格式，本适配器负责双向翻译：请求侧（system 提升为顶层、
// max_tokens 必填、tool schema 映射、tool result 消息翻译），响应侧
// （content blocks → message/tool_calls、SSE 事件流 → 统一 chunk）。
// 这里集中了"协议兼容层"最容易被追问的细节——流式 tool_use
// 的分块拼接与 stop_reason 语义映射。
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/attnstack/omni-route/internal/config"
	"github.com/attnstack/omni-route/internal/types"
)

const anthropicVersion = "2023-06-01"

type anthropic struct {
	name   string
	cfg    config.Provider
	apiKey string
	client *http.Client
}

func newAnthropic(name string, cfg config.Provider, apiKey string, client *http.Client) *anthropic {
	return &anthropic{name: name, cfg: cfg, apiKey: apiKey, client: client}
}

func (p *anthropic) Name() string { return p.name }

func (p *anthropic) headers() map[string]string {
	return map[string]string{
		"x-api-key":         p.apiKey,
		"anthropic-version": anthropicVersion,
	}
}

// buildBody：OpenAI 请求 → Messages API 请求。
// Anthropic 的 system 不是消息而是顶层参数；max_tokens 必填；
// 工具的 parameters 在 OpenAI 叫 function.parameters，在 Anthropic 叫 input_schema。
func (p *anthropic) buildBody(req *types.Request) map[string]any {
	system, msgs := splitSystem(req.Messages)
	body := map[string]any{
		"model":    req.Model,
		"messages": translateMessages(msgs),
		"stream":   req.Stream,
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = p.cfg.DefaultMaxTokens
	}
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	body["max_tokens"] = maxTokens
	if system != "" {
		body["system"] = system
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if len(req.Tools) > 0 {
		body["tools"] = translateTools(req.Tools)
		if req.ToolChoice != nil {
			if tc := translateToolChoice(req.ToolChoice); tc != nil {
				body["tool_choice"] = tc
			}
		}
	}
	return body
}

func splitSystem(msgs []types.Message) (string, []types.Message) {
	var sb bytes.Buffer
	rest := make([]types.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == types.RoleSystem {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(m.Content)
			continue
		}
		rest = append(rest, m)
	}
	return sb.String(), rest
}

// translateMessages：role=tool（OpenAI 的工具结果消息）翻译为
// Anthropic 的 user + tool_result content block；assistant 的 tool_calls
// 翻译为 tool_use block。这是工具调用循环能跨协议跑通的关键。
func translateMessages(msgs []types.Message) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case types.RoleUser:
			out = append(out, map[string]any{"role": "user", "content": m.Content})
		case types.RoleAssistant:
			blocks := []map[string]any{}
			if m.Content != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
			}
			for _, tc := range m.ToolCalls {
				input := map[string]any{}
				if tc.ArgsJSON != "" {
					_ = json.Unmarshal([]byte(tc.ArgsJSON), &input) // 解析失败留给上游报参数错误
				}
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Name, "input": input})
			}
			if len(blocks) == 0 {
				blocks = append(blocks, map[string]any{"type": "text", "text": ""})
			}
			out = append(out, map[string]any{"role": "assistant", "content": blocks})
		case types.RoleTool:
			out = append(out, map[string]any{
				"role": "user",
				"content": []map[string]any{{
					"type":        "tool_result",
					"tool_use_id": m.ToolCallID,
					"content":     m.Content,
				}},
			})
		}
	}
	return out
}

func translateTools(tools []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		fn, _ := t["function"].(map[string]any)
		if fn == nil {
			continue
		}
		entry := map[string]any{
			"name":        fn["name"],
			"description": fn["description"],
		}
		if schema, ok := fn["parameters"]; ok {
			entry["input_schema"] = schema
		} else {
			entry["input_schema"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, entry)
	}
	return out
}

func translateToolChoice(tc any) map[string]any {
	switch v := tc.(type) {
	case string:
		switch v {
		case "auto":
			return map[string]any{"type": "auto"}
		case "none":
			return map[string]any{"type": "none"}
		case "required":
			return map[string]any{"type": "any"}
		}
	case map[string]any:
		if fn, ok := v["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok {
				return map[string]any{"type": "tool", "name": name}
			}
		}
	}
	return nil
}

func (p *anthropic) Complete(ctx context.Context, req *types.Request) (*types.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.BaseURL+"/v1/messages", jsonBody(p.buildBody(req)))
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
	var wire anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return nil, err
	}
	return wire.toResponse(), nil
}

func (p *anthropic) Stream(ctx context.Context, req *types.Request) (types.Stream, error) {
	resp, err := doStream(ctx, p.client, p.cfg.BaseURL+"/v1/messages", p.headers(), p.buildBody(req))
	if err != nil {
		return nil, err
	}
	return newSSEStream(resp, decodeAnthropicData), nil
}

// ---- 响应翻译 ----

type anthropicResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Content []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func mapStopReason(s string) string {
	switch s {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	case "":
		return ""
	default:
		return s
	}
}

func (w *anthropicResponse) toResponse() *types.Response {
	r := &types.Response{
		ID:           w.ID,
		Model:        w.Model,
		FinishReason: mapStopReason(w.StopReason),
		Usage: types.Usage{
			PromptTokens:     w.Usage.InputTokens,
			CompletionTokens: w.Usage.OutputTokens,
			TotalTokens:      w.Usage.InputTokens + w.Usage.OutputTokens,
		},
	}
	for _, b := range w.Content {
		switch b.Type {
		case "text":
			r.Content += b.Text
		case "thinking":
			r.Reasoning += b.Text
		case "tool_use":
			r.ToolCalls = append(r.ToolCalls, types.ToolCall{
				ID:       b.ID,
				Name:     b.Name,
				ArgsJSON: string(b.Input),
			})
		}
	}
	return r
}

// decodeAnthropicData：一条 SSE data 行 → 0..n 统一 chunk。
// content_block_start(tool_use) 先发带 id/name 的增量（对齐 OpenAI 首增量的语义），
// 后续 input_json_delta 只带参数分片；客户端按 index 聚合。
func decodeAnthropicData(data []byte) ([]types.Chunk, error) {
	var evt struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
		Block struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			PartialJSON string `json:"partial_json"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		Message struct {
			Usage struct {
				InputTokens int `json:"input_tokens"`
			} `json:"usage"`
		} `json:"message"`
		Usage struct {
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &evt); err != nil {
		return nil, err
	}
	if evt.Error != nil {
		return nil, &types.UpstreamError{Status: 502, Body: "anthropic stream error: " + evt.Error.Message}
	}
	switch evt.Type {
	case "message_start":
		// input tokens 在流开始时就确定；先单独发一个 usage 增量，
		// 与 message_delta 的 output tokens 在网关侧合并成完整 usage。
		u := types.Usage{PromptTokens: evt.Message.Usage.InputTokens}
		return []types.Chunk{{Usage: &u}}, nil
	case "content_block_start":
		if evt.Block.Type != "tool_use" {
			return nil, nil
		}
		return []types.Chunk{{ToolCalls: []types.ToolCallDelta{{
			Index: evt.Index, ID: evt.Block.ID, Name: evt.Block.Name,
		}}}}, nil
	case "content_block_delta":
		switch evt.Delta.Type {
		case "text_delta":
			if evt.Delta.Text == "" {
				return nil, nil
			}
			return []types.Chunk{{DeltaContent: evt.Delta.Text}}, nil
		case "thinking_delta":
			return []types.Chunk{{ReasoningContent: evt.Delta.Thinking}}, nil
		case "input_json_delta":
			if evt.Delta.PartialJSON == "" {
				return nil, nil
			}
			return []types.Chunk{{ToolCalls: []types.ToolCallDelta{{
				Index: evt.Index, ArgsText: evt.Delta.PartialJSON,
			}}}}, nil
		}
		return nil, nil
	case "message_delta":
		ch := types.Chunk{FinishReason: mapStopReason(evt.Delta.StopReason)}
		u := types.Usage{CompletionTokens: evt.Usage.OutputTokens}
		ch.Usage = &u
		return []types.Chunk{ch}, nil
	case "message_stop":
		return nil, io.EOF
	case "ping", "content_block_stop":
		return nil, nil
	default:
		return nil, nil
	}
}
