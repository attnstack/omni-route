// chat_cache.go：响应缓存的网关侧接线——命中应答、可缓存判定与回写。
// 设计要点：显式点名的模型参与缓存键（调用方要特定模型的回答），
// 自动路由则不参与（同一个问题命中即省，无论第一次由谁作答）。
package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"github.com/attnstack/omni-route/internal/cache"
	"github.com/attnstack/omni-route/internal/classifier"
	"github.com/attnstack/omni-route/internal/router"
	"github.com/attnstack/omni-route/internal/types"
)

// cacheEligible 报告该请求在当前缓存模式下是否可缓存。
// zero_temp：仅显式 temperature=0（temperature 缺省时 provider 语义各异，不缓存）；
// always：全部非流式请求（调用方需自担"非确定性回答被复用"的语义）。
func cacheEligible(mode string, req *types.Request) bool {
	if mode == "always" {
		return true
	}
	return req.Temperature != nil && *req.Temperature == 0
}

// explicitModel 返回客户端显式点名的模型（auto/空不算）。
func explicitModel(m string) string {
	if m == "auto" {
		return ""
	}
	return m
}

// serveCacheHit 以缓存条目应答：跳过路由与上游，成本为零，节省按原成本入账。
func (s *Server) serveCacheHit(w http.ResponseWriter, r *http.Request, world *world,
	req *types.Request, key string, e cache.Entry, start time.Time) {

	resp := &types.Response{
		Model:        e.Model,
		Content:      e.Response.Content,
		Reasoning:    e.Response.Reasoning,
		ToolCalls:    toTypesToolCalls(e.Response.ToolCalls),
		FinishReason: e.Response.FinishReason,
		Usage: types.Usage{
			PromptTokens:     e.Usage.PromptTokens,
			CompletionTokens: e.Usage.CompletionTokens,
			TotalTokens:      e.Usage.TotalTokens,
			Estimated:        e.Usage.Estimated,
		},
	}
	dec := &router.Decision{
		Task:       classifier.Classify(req.Messages).Task,
		Tier:       "cache",
		Chosen:     e.Model,
		Reason:     "cache-hit",
		Candidates: []router.Candidate{{Model: e.Model}},
	}
	latency := time.Since(start)
	s.logRow(world, req, dec, e.Model, resp.Usage, 0, e.CostUSD, latency, false, false, "", "ok", "")

	h := w.Header()
	h.Set("x-or-cache", "hit")
	h.Set("x-or-route-reason", dec.RouteReasonHeader(e.Model, false))
	h.Set("x-or-model", e.Model)
	h.Set("x-or-request-id", req.RequestID)
	h.Set("x-or-cost-usd", "0.000000")
	h.Set("x-or-savings-usd", fmt.Sprintf("%.6f", e.CostUSD))
	h.Set("Content-Type", "application/json")
	writeJSON(w, http.StatusOK, buildChatResponse(req, resp, e.Model, resp.Usage, nil))
}

// maybeCachePut 在成功的非流式响应后回写缓存（键已在 handleChat 算好挂在 Raw 上）。
func (s *Server) maybeCachePut(world *world, req *types.Request, dec *router.Decision,
	finalModel string, resp *types.Response, usage types.Usage, cost float64) {

	ck := req.CacheKey
	if ck == "" || world.cache == nil {
		return
	}
	world.cache.Put(ck, cache.Entry{
		Model: finalModel,
		Response: &cache.ResponseCopy{
			Content:      resp.Content,
			Reasoning:    resp.Reasoning,
			ToolCalls:    toCacheToolCalls(resp.ToolCalls),
			FinishReason: resp.FinishReason,
		},
		Usage: cache.Usage{
			PromptTokens:     usage.PromptTokens,
			CompletionTokens: usage.CompletionTokens,
			TotalTokens:      usage.TotalTokens,
			Estimated:        usage.Estimated,
		},
		CostUSD:   cost,
		CreatedAt: time.Now(),
	})
}

func toCacheToolCalls(tcs []types.ToolCall) []cache.ToolCall {
	if len(tcs) == 0 {
		return nil
	}
	out := make([]cache.ToolCall, 0, len(tcs))
	for _, tc := range tcs {
		out = append(out, cache.ToolCall{ID: tc.ID, Name: tc.Name, ArgsJSON: tc.ArgsJSON})
	}
	return out
}

func toTypesToolCalls(tcs []cache.ToolCall) []types.ToolCall {
	if len(tcs) == 0 {
		return nil
	}
	out := make([]types.ToolCall, 0, len(tcs))
	for _, tc := range tcs {
		out = append(out, types.ToolCall{ID: tc.ID, Name: tc.Name, ArgsJSON: tc.ArgsJSON})
	}
	return out
}

// tenantHash 把认证 key 缩短为定长哈希（缓存键里不放原始凭据）。
// 空串返回空串——未启用鉴权时缓存全站共享。
func tenantHash(tenant string) string {
	if tenant == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(tenant))
	return hex.EncodeToString(sum[:8])
}
