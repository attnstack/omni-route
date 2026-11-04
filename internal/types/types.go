// Package types 定义 provider 无关的统一请求/响应/流式 chunk 模型。
// 所有 adapter 的职责就是把各家协议翻译成这里的结构——这是整个网关的协议契约。
package types

import (
	"errors"
	"fmt"
)

// 消息角色常量（OpenAI 语义）。
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Message 是统一消息模型。工具调用消息（role=tool）与 assistant 的 tool_calls
// 都在这里显式建模，供 Anthropic 适配器做双向翻译。
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// ToolCall 是一次完整的工具调用（非流式或流式聚合后的形态）。
type ToolCall struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	ArgsJSON string `json:"arguments"` // 原始 JSON 字符串，与 OpenAI 协议一致
}

// ToolCallDelta 是流式过程中的工具调用增量。
// OpenAI 约定：首个增量携带 index/id/name，后续增量只携带 arguments 的分片。
type ToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Name     string `json:"name,omitempty"`
	ArgsText string `json:"arguments,omitempty"`
}

// Usage 记录 token 用量。Estimated=true 表示 provider 没报 usage，
// 由网关本地估算（省钱的前提是成本算得准，估算必须显式标注）。
type Usage struct {
	PromptTokens     int  `json:"prompt_tokens"`
	CompletionTokens int  `json:"completion_tokens"`
	TotalTokens      int  `json:"total_tokens"`
	Estimated        bool `json:"estimated,omitempty"`
}

// Request 是统一后的对话请求。Raw 保留原始 body（OpenAI 协议透传用），
// 但路由与协议翻译只依赖类型化字段，避免"隐式字段"在 provider 间漂移。
type Request struct {
	RequestID   string
	Model       string // 客户端点名的模型；空或 "auto" 表示交给路由
	Messages    []Message
	Stream      bool
	Tools       []map[string]any // OpenAI tool schema，原样透传
	ToolChoice  any
	Temperature *float64
	MaxTokens   int
	Raw         map[string]any

	// CacheKey 由网关在缓存判定后填入（无 json 标签，绝不透传给上游）。
	CacheKey string
}

// Response 是统一后的完整回答。
type Response struct {
	ID           string
	Model        string // 实际产出该回答的模型
	Content      string
	Reasoning    string // 推理模型的思考文本（如 deepseek-reasoner）
	ToolCalls    []ToolCall
	FinishReason string // stop | length | tool_calls | content_filter
	Usage        Usage
}

// Chunk 是流式增量，已翻译为 OpenAI 语义。网关据此重新序列化给客户端，
// 保证不同 provider 的流式输出对外表现完全一致。
type Chunk struct {
	DeltaContent     string
	ReasoningContent string
	ToolCalls        []ToolCallDelta
	FinishReason     string
	Usage            *Usage
	HasRole          bool // 上游首 chunk 携带 role 标记（重新序列化时统一处理）
}

// Stream 是统一的 chunk 流。Next 返回 io.EOF 表示流正常结束；
// 其他错误表示上游断流。Close 释放底层连接并取消关联的 context。
type Stream interface {
	Next() (Chunk, error)
	Close() error
}

// Caps 描述模型能力，路由的硬约束过滤依据。
type Caps struct {
	Streaming        bool `json:"streaming"`
	Tools            bool `json:"tools"`
	JSONMode         bool `json:"json_mode"`
	MaxContextTokens int  `json:"max_context_tokens"`
}

// 领域级错误：可恢复与否的判定依据（见 executor 与 gateway 的错误分支）。
var (
	// ErrNoCandidate：硬约束过滤后没有任何可用模型——请求本身无法被满足。
	ErrNoCandidate = errors.New("omni-route: no eligible model after hard filters")
	// ErrBreakerOpen：provider 熔断中，应 failover 而不是重试同一 provider。
	ErrBreakerOpen = errors.New("omni-route: provider circuit open")
)

// UpstreamError 携带 provider 的 HTTP 状态码，
// 网关据此决定：重试？熔断计数？透传给客户端的状态？
type UpstreamError struct {
	Status int
	Body   string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("omni-route: upstream status %d: %s", e.Status, truncate(e.Body, 300))
}

// Retryable 报告该错误是否值得重试（幂等前提下）。
// 4xx（除 429）是请求本身的问题，重试没有意义。
func (e *UpstreamError) Retryable() bool {
	return e.Status == 429 || e.Status >= 500
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
