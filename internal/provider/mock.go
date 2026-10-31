// mock.go：内置确定性 mock provider。三个用途：
//  1. 零 API key 的端到端演示（"5 分钟跑通"的前提）
//  2. CI 与集成测试的上游替身（含故障注入：fail_first_n / fail_rate）
//  3. benchmark 的成本/质量模拟（结果按 (model, prompt) 哈希确定，可复现）
//
// 行为设计：按 mock_tier 划档——premium 永远给"好答案"；mid/fast 按
// bad_json_rate / short_rate 概率产出"坏答案"（非法 JSON / 过短），
// 从而能真实触发级联的格式校验与启发式判别升级路径。
package provider

import (
	"context"
	"hash/fnv"
	"io"
	"strings"
	"sync/atomic"
	"time"

	"github.com/attnstack/omni-route/internal/config"
	"github.com/attnstack/omni-route/internal/tokens"
	"github.com/attnstack/omni-route/internal/types"
)

// errMockEOF 复用 io.EOF 作为内存流的结束哨兵，与 SSE 流语义一致。
var errMockEOF = io.EOF

type mock struct {
	name    string
	cfg     config.Provider
	tiers   map[string]string // model → fast|mid|premium
	counter atomic.Int64
}

func newMock(name string, pc config.Provider, models map[string]config.Model) *mock {
	tiers := map[string]string{}
	for modelName, mc := range models {
		if mc.Provider != name {
			continue
		}
		tier := mc.MockTier
		if tier == "" {
			tier = "mid"
		}
		tiers[modelName] = tier
	}
	return &mock{name: name, cfg: pc, tiers: tiers}
}

func (m *mock) Name() string { return m.name }

func (m *mock) tier(model string) string {
	if t, ok := m.tiers[model]; ok {
		return t
	}
	return "mid"
}

// rateScale：同一组 bad_rate 参数按档位衰减——premium 不出错，
// mid 减半，fast 全额。避免给每个模型重复配三份概率。
func (m *mock) rateScale(tier string) float64 {
	switch tier {
	case "premium":
		return 0
	case "mid":
		return 0.5
	default:
		return 1
	}
}

func (m *mock) Complete(ctx context.Context, req *types.Request) (*types.Response, error) {
	if err := m.maybeFail(req); err != nil {
		return nil, err
	}
	model := req.Model
	tier := m.tier(model)
	lastUser := lastUserContent(req.Messages)

	latency := m.cfg.Mock.LatencyMs + int(hashN(model+"\x1f"+lastUser, 3, uint64(m.cfg.Mock.JitterMs+1)))
	if latency > 0 {
		select {
		case <-time.After(time.Duration(latency) * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	resp := m.respond(model, tier, lastUser, req)
	resp.Usage = types.Usage{
		PromptTokens:     tokens.EstimateMessages(req.Messages),
		CompletionTokens: tokens.Estimate(resp.Content),
	}
	resp.Usage.TotalTokens = resp.Usage.PromptTokens + resp.Usage.CompletionTokens
	return resp, nil
}

func (m *mock) Stream(ctx context.Context, req *types.Request) (types.Stream, error) {
	if err := m.maybeFail(req); err != nil {
		return nil, err
	}
	model := req.Model
	tier := m.tier(model)
	lastUser := lastUserContent(req.Messages)

	// 工具调用模拟：中高档模型 25% 概率返回工具调用
	if len(req.Tools) > 0 && tier != "fast" && hashN(model+"\x1f"+lastUser, 1, 4) == 0 {
		name := firstToolName(req.Tools)
		return &mockStream{chunks: []types.Chunk{{
			ToolCalls:    []types.ToolCallDelta{{Index: 0, ID: "call_mock_1", Name: name, ArgsText: "{}"}},
			FinishReason: "tool_calls",
		}}}, nil
	}

	resp := m.respond(model, tier, lastUser, req)
	resp.Usage = types.Usage{
		PromptTokens:     tokens.EstimateMessages(req.Messages),
		CompletionTokens: tokens.Estimate(resp.Content),
	}
	resp.Usage.TotalTokens = resp.Usage.PromptTokens + resp.Usage.CompletionTokens

	// 按固定步长切 chunk，模拟逐 token 输出（demo 时肉眼看得到流式效果）
	var chunks []types.Chunk
	runes := []rune(resp.Content)
	const step = 12
	for i := 0; i < len(runes); i += step {
		end := i + step
		if end > len(runes) {
			end = len(runes)
		}
		chunks = append(chunks, types.Chunk{DeltaContent: string(runes[i:end])})
	}
	if resp.FinishReason != "" {
		chunks = append(chunks, types.Chunk{FinishReason: resp.FinishReason, Usage: &resp.Usage})
	} else {
		chunks = append(chunks, types.Chunk{FinishReason: "stop", Usage: &resp.Usage})
	}
	return &mockStream{chunks: chunks, delayMs: m.cfg.Mock.StreamChunkDelayMs}, nil
}

// respond 生成确定性回答。所有"好/坏"的判定都来自 (model, prompt) 哈希，
// 同一输入永远同一结果——这是 benchmark 可复现的基础。
func (m *mock) respond(model, tier, lastUser string, req *types.Request) *types.Response {
	h := fnvHash(model + "\x1f" + lastUser)
	r1 := float64(h%10000) / 10000
	r2 := float64((h/10000)%10000) / 10000
	scale := m.rateScale(tier)

	if wantsJSON(lastUser) {
		if r1 < m.cfg.Mock.BadJSONRate*scale {
			// 坏答案：截断的非法 JSON——格式校验判别器的靶子
			return &types.Response{Model: model, Content: `{"summary": `, FinishReason: "stop"}
		}
		return &types.Response{Model: model, Content: GoodAnswer(model, lastUser), FinishReason: "stop"}
	}
	if r2 < m.cfg.Mock.ShortRate*scale {
		// 坏答案：过短回答——启发式 judge 的靶子
		return &types.Response{Model: model, Content: "ok.", FinishReason: "stop"}
	}
	return &types.Response{Model: model, Content: GoodAnswer(model, lastUser), FinishReason: "stop"}
}

func (m *mock) maybeFail(req *types.Request) error {
	h := fnvHash(req.Model + "\x1f" + lastUserContent(req.Messages))
	if m.cfg.Mock.FailRate > 0 && float64((h/100000000)%10000)/10000 < m.cfg.Mock.FailRate {
		return &types.UpstreamError{Status: 500, Body: "mock: injected failure"}
	}
	// fail_first_n：只挂前 N 次（原子计数），供重试与 failover 的确定性测试
	if m.cfg.Mock.FailFirstN > 0 && m.counter.Add(1) <= int64(m.cfg.Mock.FailFirstN) {
		return &types.UpstreamError{Status: 500, Body: "mock: fail_first_n injected"}
	}
	return nil
}

// mockStream 是内存 chunk 流（无网络），delayMs=0 时瞬时走完（测试/基准加速）。
type mockStream struct {
	chunks  []types.Chunk
	i       int
	delayMs int
}

func (s *mockStream) Next() (types.Chunk, error) {
	if s.i >= len(s.chunks) {
		return types.Chunk{}, errMockEOF
	}
	c := s.chunks[s.i]
	s.i++
	if s.delayMs > 0 && s.i < len(s.chunks) {
		time.Sleep(time.Duration(s.delayMs) * time.Millisecond)
	}
	return c, nil
}

func (s *mockStream) Close() error { return nil }

// GoodAnswer 是 mock 的"标准答案"生成器（导出给 benchmark 当质量 oracle：
// 响应内容 == 本函数输出即视为答对）。
func GoodAnswer(model, prompt string) string {
	topic := topicOf(prompt)
	if wantsJSON(prompt) {
		return `{"summary": "` + topic + `", "items": ["alpha", "beta", "gamma"], "count": 3}`
	}
	return "Here is a thorough answer about " + topic + ". " +
		"It covers the main points with concrete detail and avoids vague generalities. " +
		"The key considerations are correctness, clarity, and completeness of the response. " +
		"In summary, the recommended approach is to start simple, measure carefully, and iterate with evidence. " +
		"This deterministic answer is produced by the built-in mock provider for demos and benchmarks."
}

func topicOf(prompt string) string {
	t := strings.TrimSpace(prompt)
	if len(t) > 60 {
		t = t[:60]
	}
	var sb strings.Builder
	for _, r := range t {
		if r == '"' || r == '\\' || r == '\n' || r == '\r' || r == '\t' {
			sb.WriteRune(' ')
			continue
		}
		sb.WriteRune(r)
	}
	out := strings.TrimSpace(sb.String())
	if out == "" {
		out = "the question"
	}
	return out
}

func wantsJSON(prompt string) bool {
	return strings.Contains(strings.ToUpper(prompt), "JSON")
}

func lastUserContent(msgs []types.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == types.RoleUser {
			return msgs[i].Content
		}
	}
	if len(msgs) > 0 {
		return msgs[0].Content
	}
	return ""
}

func firstToolName(tools []map[string]any) string {
	for _, t := range tools {
		if fn, ok := t["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok {
				return name
			}
		}
	}
	return "unknown_tool"
}

func fnvHash(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

// hashN 取哈希的第 slot 段（每段 bits 位）对 mod 取模——
// 同一哈希派生多个"独立"随机数，避免同一请求的不同决策相关。
func hashN(s string, slot int, mod uint64) int {
	if mod == 0 {
		return 0
	}
	h := fnvHash(s)
	return int((h >> (uint(slot) * 8)) % mod)
}
