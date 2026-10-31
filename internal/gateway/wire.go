// wire.go：对外（客户端侧）OpenAI 协议的响应序列化结构。
// 网关永远以 OpenAI 格式应答，无论上游是哪家协议——这是"改 base_url 即接入"的根基。
package gateway

import (
	"time"

	"github.com/attnstack/omni-route/internal/cascade"
	"github.com/attnstack/omni-route/internal/types"
)

type usageWire struct {
	PromptTokens     int  `json:"prompt_tokens"`
	CompletionTokens int  `json:"completion_tokens"`
	TotalTokens      int  `json:"total_tokens"`
	Estimated        bool `json:"estimated,omitempty"` // 网关扩展字段：usage 是否为本地估算
}

type messageWire struct {
	Role             string         `json:"role"`
	Content          *string        `json:"content"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []toolCallWire `json:"tool_calls,omitempty"`
}

type toolCallWire struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatChoice struct {
	Index        int         `json:"index"`
	Message      messageWire `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type chatCompletion struct {
	ID        string       `json:"id"`
	Object    string       `json:"object"`
	Created   int64        `json:"created"`
	Model     string       `json:"model"`
	Choices   []chatChoice `json:"choices"`
	Usage     usageWire    `json:"usage"`
	OmniRoute any          `json:"omni_route,omitempty"` // 网关扩展：级联报告（客户端不识别则忽略）
}

type chatChunk struct {
	ID      string            `json:"id"`
	Object  string            `json:"object"`
	Created int64             `json:"created"`
	Model   string            `json:"model"`
	Choices []chatChunkChoice `json:"choices"`
	Usage   *usageWire        `json:"usage,omitempty"`
}

type chatChunkChoice struct {
	Index        int       `json:"index"`
	Delta        deltaWire `json:"delta"`
	FinishReason *string   `json:"finish_reason"`
}

type deltaWire struct {
	Role             string              `json:"role,omitempty"`
	Content          *string             `json:"content,omitempty"`
	ReasoningContent string              `json:"reasoning_content,omitempty"`
	ToolCalls        []toolCallDeltaWire `json:"tool_calls,omitempty"`
}

type toolCallDeltaWire struct {
	Index    int               `json:"index"`
	ID       string            `json:"id,omitempty"`
	Type     string            `json:"type,omitempty"`
	Function *toolCallFuncWire `json:"function,omitempty"`
}

type toolCallFuncWire struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

func strPtr(s string) *string { return &s }

// buildChatResponse 组装非流式 OpenAI 响应体；rep 非空时附上级联报告。
func buildChatResponse(req *types.Request, resp *types.Response, model string, usage types.Usage, rep *cascade.Report) chatCompletion {
	id := resp.ID
	if id == "" {
		id = "chatcmpl-or-" + req.RequestID
	}
	finish := resp.FinishReason
	if finish == "" {
		finish = "stop"
	}
	msg := messageWire{Role: "assistant"}
	c := resp.Content
	msg.Content = &c
	msg.ReasoningContent = resp.Reasoning
	for _, tc := range resp.ToolCalls {
		w := toolCallWire{ID: tc.ID, Type: "function"}
		w.Function.Name = tc.Name
		w.Function.Arguments = tc.ArgsJSON
		msg.ToolCalls = append(msg.ToolCalls, w)
	}
	var omni any
	if rep != nil {
		omni = rep
	}
	return chatCompletion{
		ID:      id,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []chatChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: finish,
		}},
		Usage: usageWire{
			PromptTokens:     usage.PromptTokens,
			CompletionTokens: usage.CompletionTokens,
			TotalTokens:      usage.TotalTokens,
			Estimated:        usage.Estimated,
		},
		OmniRoute: omni,
	}
}
