// chat.go：/v1/chat/completions 主处理。
// 非流式：direct/lagrangian → 单模型调用（失败按候选序 failover）；
// cascade → 级联执行器。流式：一律单模型直选（协议级取舍，见 cascade.go）。
// 每条请求结束都要：写响应头（可解释）、记录 SQLite、回写在线学习状态。
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/attnstack/omni-route/internal/cascade"
	"github.com/attnstack/omni-route/internal/router"
	"github.com/attnstack/omni-route/internal/store"
	"github.com/attnstack/omni-route/internal/tokens"
	"github.com/attnstack/omni-route/internal/types"
)

// ---- 请求解析 ----

// parseChatRequest 把原始 JSON body 收敛成统一 Request。
// 策略：类型化字段驱动协议行为，Raw 保留原始 body 供 OpenAI 系透传。
func parseChatRequest(raw map[string]any) (*types.Request, error) {
	req := &types.Request{Raw: raw}
	req.Model, _ = raw["model"].(string)
	if b, ok := raw["stream"].(bool); ok {
		req.Stream = b
	}
	if v, ok := raw["max_tokens"].(float64); ok {
		req.MaxTokens = int(v)
	}
	if v, ok := raw["max_completion_tokens"].(float64); ok && req.MaxTokens == 0 {
		req.MaxTokens = int(v)
	}
	if v, ok := raw["temperature"].(float64); ok {
		req.Temperature = &v
	}
	if tc, ok := raw["tool_choice"]; ok {
		req.ToolChoice = tc
	}
	if toolsRaw, ok := raw["tools"].([]any); ok && len(toolsRaw) > 0 {
		tools := make([]map[string]any, 0, len(toolsRaw))
		for _, t := range toolsRaw {
			m, ok := t.(map[string]any)
			if !ok {
				return nil, errors.New("tools[i] must be an object")
			}
			tools = append(tools, m)
		}
		req.Tools = tools
	}
	msgsRaw, ok := raw["messages"]
	if !ok {
		return nil, errors.New("messages is required")
	}
	blob, err := json.Marshal(msgsRaw)
	if err != nil {
		return nil, fmt.Errorf("messages: %w", err)
	}
	var msgs []types.Message
	if err := json.Unmarshal(blob, &msgs); err != nil {
		return nil, fmt.Errorf("messages: %w", err)
	}
	if len(msgs) == 0 {
		return nil, errors.New("messages must not be empty")
	}
	for _, m := range msgs {
		switch m.Role {
		case types.RoleSystem, types.RoleUser, types.RoleAssistant, types.RoleTool:
		default:
			return nil, fmt.Errorf("invalid message role %q", m.Role)
		}
	}
	req.Messages = msgs
	return req, nil
}

// ---- 主入口 ----

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	world := s.current()
	start := time.Now()
	rid := requestIDFrom(r.Context())

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "read_body", err.Error())
		return
	}
	if len(body) > maxBodyBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "body_too_large",
			fmt.Sprintf("request body exceeds %d bytes", maxBodyBytes))
		return
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", err.Error())
		return
	}
	req, err := parseChatRequest(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_request", err.Error())
		return
	}
	req.RequestID = rid

	hints := router.Hints{
		TaskOverride: r.Header.Get("X-Or-Task"),
		DataClass:    r.Header.Get("X-Or-Data-Class"),
	}
	dec, err := world.rt.Decide(req, hints)
	if err != nil {
		if errors.Is(err, types.ErrNoCandidate) {
			writeError(w, http.StatusServiceUnavailable, "api_error", "no_candidate",
				"no eligible model satisfies the request constraints (capability/compliance/context)")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request_error", "route_error", err.Error())
		return
	}

	if req.Stream {
		s.streamCompletion(w, r, world, req, dec, start)
		return
	}
	s.complete(w, r, world, req, dec, start)
}

// ---- 非流式 ----

func (s *Server) complete(w http.ResponseWriter, r *http.Request, world *world, req *types.Request, dec *router.Decision, start time.Time) {
	var (
		resp         *types.Response
		rep          *cascade.Report
		err          error
		finalModel   = dec.Chosen
		failoverFrom string
		upgraded     bool
	)

	if dec.Tier == "cascade" {
		chain := make([]string, 0, len(dec.Candidates))
		for _, c := range dec.Candidates {
			chain = append(chain, c.Model)
		}
		costFn := func(model string, r2 *types.Response) float64 {
			c, _ := world.reg.Cost(model, r2.Usage.PromptTokens, r2.Usage.CompletionTokens)
			return c
		}
		resp, rep, err = world.runner.Run(r.Context(), req, chain, string(dec.Task), costFn)
		if err != nil {
			s.finishError(world, req, dec, err, start, false)
			writeError(w, http.StatusBadGateway, "api_error", "upstream_error", err.Error())
			return
		}
		finalModel = rep.FinalModel
		upgraded = rep.Upgraded
	} else {
		resp, err = world.exec.Call(r.Context(), dec.Chosen, req)
		// failover：同级候选顺延。上限 2 次，避免一次请求放大成 4 个上游调用
		for i, alt := 0, 0; err != nil && i < len(dec.Candidates) && alt < 2; i++ {
			if dec.Candidates[i].Model == finalModel || dec.Candidates[i].Model == "" {
				continue
			}
			alt++
			s.logger.Warn("failover", "from", finalModel, "to", dec.Candidates[i].Model,
				"error", err.Error(), "request_id", req.RequestID)
			failoverFrom = finalModel
			finalModel = dec.Candidates[i].Model
			resp, err = world.exec.Call(r.Context(), finalModel, req)
		}
		if err != nil {
			s.finishError(world, req, dec, err, start, false)
			writeError(w, http.StatusBadGateway, "api_error", "upstream_error", err.Error())
			return
		}
	}

	latency := time.Since(start)
	cost, usage := s.settleCost(world, finalModel, resp, req)
	savings := s.savings(world, string(dec.Task), usage, cost)
	reward := cascade.HeuristicScore(cascade.CheckReq{Messages: req.Messages, Response: resp}).Score
	world.rt.ReportOutcome(string(dec.Task), finalModel, reward, cost, latency.Milliseconds())
	s.maybeAsyncJudge(world, string(dec.Task), req, resp)

	if failoverFrom != "" {
		dec.Reason = "failover"
	}

	// 响应头：可解释路由的对外契约
	h := w.Header()
	h.Set("x-or-route-reason", dec.RouteReasonHeader(finalModel, upgraded))
	h.Set("x-or-model", finalModel)
	h.Set("x-or-request-id", req.RequestID)
	h.Set("x-or-cost-usd", fmt.Sprintf("%.6f", cost))
	h.Set("x-or-savings-usd", fmt.Sprintf("%.6f", savings))
	if failoverFrom != "" {
		h.Set("x-or-failover-from", failoverFrom)
	}
	h.Set("Content-Type", "application/json")

	s.logRow(world, req, dec, finalModel, usage, cost, savings, latency, false, upgraded, failoverFrom, "ok", "")
	writeJSON(w, http.StatusOK, buildChatResponse(req, resp, finalModel, usage, rep))
}

// settleCost：优先用 provider 上报的 usage；缺失则本地估算并打标。
func (s *Server) settleCost(world *world, model string, resp *types.Response, req *types.Request) (float64, types.Usage) {
	usage := resp.Usage
	if usage.PromptTokens == 0 {
		usage.PromptTokens = tokens.EstimateMessages(req.Messages)
		usage.Estimated = true
	}
	if usage.CompletionTokens == 0 {
		usage.CompletionTokens = tokens.Estimate(resp.Content)
		usage.Estimated = true
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	cost, _ := world.reg.Cost(model, usage.PromptTokens, usage.CompletionTokens)
	return cost, usage
}

// savings：相对"该任务永远用基线（最强）模型"的省钱额。
// 基线缺失时按最贵模型兜底——省钱数字必须有参照系才有意义。
func (s *Server) savings(world *world, task string, usage types.Usage, cost float64) float64 {
	base, ok := world.cfg.Routing.Baseline[task]
	if !ok {
		var maxPrice float64
		var maxModel string
		for _, m := range world.reg.Models() {
			if m.PriceOut >= maxPrice {
				maxPrice = m.PriceOut
				maxModel = m.Name
			}
		}
		base = maxModel
	}
	if base == "" {
		return 0
	}
	baseCost, _ := world.reg.Cost(base, usage.PromptTokens, usage.CompletionTokens)
	return baseCost - cost
}

// maybeAsyncJudge：LLM judge 抽检（默认 5%）。异步、带超时、失败静默——
// 质量抽检绝不能变成用户请求的额外延迟或错误源。
func (s *Server) maybeAsyncJudge(world *world, task string, req *types.Request, resp *types.Response) {
	jc := world.cfg.Cascade.Confidence.LLMJudge
	if !jc.Enabled || jc.Model == "" || jc.SampleRate <= 0 {
		return
	}
	if world.rt.NextRandom() >= jc.SampleRate {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(jc.TimeoutMs)*time.Millisecond)
		defer cancel()
		v := world.checker.Evaluate(ctx, cascade.CheckReq{Messages: req.Messages, Response: resp, Task: task})
		if v.Skip {
			return
		}
		world.rt.State().ApplyQualitySignal(task, resp.Model, v.Score)
		s.logger.Debug("judge sampled", "request_id", req.RequestID, "model", resp.Model, "score", v.Score)
	}()
}

// finishError 记录失败请求的日志行。
func (s *Server) finishError(world *world, req *types.Request, dec *router.Decision, err error, start time.Time, streamed bool) {
	latency := time.Since(start)
	world.rt.ReportOutcome(string(dec.Task), dec.Chosen, 0, 0, latency.Milliseconds())
	status := "error"
	if errors.Is(err, context.Canceled) {
		status = "client_cancel"
	}
	s.logRow(world, req, dec, dec.Chosen, types.Usage{}, 0, 0, latency, streamed, false, "", status, err.Error())
}

// ---- 流式 ----

func (s *Server) streamCompletion(w http.ResponseWriter, r *http.Request, world *world, req *types.Request, dec *router.Decision, start time.Time) {
	// 流式一律单模型直选：级联无法撤回已发出的 chunk（协议级取舍）
	finalModel := dec.Chosen
	failoverFrom := ""
	st, err := world.exec.Stream(r.Context(), finalModel, req)
	for i, alt := 0, 0; err != nil && dec.Tier != "explicit" && i < len(dec.Candidates) && alt < 2; i++ {
		if dec.Candidates[i].Model == finalModel || dec.Candidates[i].Model == "" {
			continue
		}
		alt++
		s.logger.Warn("stream failover", "from", finalModel, "to", dec.Candidates[i].Model, "error", err.Error())
		failoverFrom = finalModel
		finalModel = dec.Candidates[i].Model
		st, err = world.exec.Stream(r.Context(), finalModel, req)
	}
	if err != nil {
		if errors.Is(err, r.Context().Err()) {
			s.finishError(world, req, dec, err, start, true)
			return // 客户端已断开，写响应无意义
		}
		s.finishError(world, req, dec, err, start, true)
		writeError(w, http.StatusBadGateway, "api_error", "upstream_error", err.Error())
		return
	}
	defer st.Close()

	// SSE 响应头必须在第一个字节前写完——路由决策在这里对客户端可见
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // nginx 反代场景禁用缓冲
	h.Set("x-or-route-reason", dec.RouteReasonHeader(finalModel, false))
	h.Set("x-or-model", finalModel)
	h.Set("x-or-request-id", req.RequestID)
	if failoverFrom != "" {
		h.Set("x-or-failover-from", failoverFrom)
		dec.Reason = "failover"
	}
	w.WriteHeader(http.StatusOK)
	flusher, ok := w.(http.Flusher)
	if !ok {
		return // statusWriter 已声明 Flush；此分支理论上不可达，防御性退出
	}

	created := time.Now().Unix()
	id := "chatcmpl-or-" + req.RequestID
	var content strings.Builder
	var finish string
	var usage types.Usage
	first := true

	writeChunk := func(v any) bool {
		blob, err := json.Marshal(v)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", blob); err != nil {
			return false // 客户端断开
		}
		flusher.Flush()
		return true
	}

	for {
		chunk, err := st.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// 中途断流：以 OpenAI 风格事件告知客户端（协议无标准，业界惯例），然后终止
			s.logger.Error("upstream stream error", "model", finalModel, "error", err.Error(), "request_id", req.RequestID)
			errPayload := map[string]any{"error": map[string]any{
				"message": err.Error(), "type": "api_error", "code": "upstream_stream_error"}}
			if blob, jerr := json.Marshal(errPayload); jerr == nil {
				fmt.Fprintf(w, "data: %s\n\n", blob)
				flusher.Flush()
			}
			break
		}
		if chunk.DeltaContent != "" {
			content.WriteString(chunk.DeltaContent)
		}
		if chunk.FinishReason != "" {
			finish = chunk.FinishReason
		}
		if chunk.Usage != nil {
			// usage 可能分两段到达（anthropic 的 message_start/message_delta），合并
			if chunk.Usage.PromptTokens > 0 {
				usage.PromptTokens = chunk.Usage.PromptTokens
			}
			if chunk.Usage.CompletionTokens > 0 {
				usage.CompletionTokens = chunk.Usage.CompletionTokens
			}
		}
		wire := chatChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: finalModel}
		delta := deltaWire{}
		if first {
			delta.Role = "assistant"
			delta.Content = strPtr("")
			first = false
		}
		if chunk.DeltaContent != "" {
			delta.Content = strPtr(chunk.DeltaContent)
		}
		if chunk.ReasoningContent != "" {
			delta.ReasoningContent = chunk.ReasoningContent
		}
		if len(chunk.ToolCalls) > 0 {
			for _, tc := range chunk.ToolCalls {
				delta.ToolCalls = append(delta.ToolCalls, toolCallDeltaWire{
					Index: tc.Index, ID: tc.ID, Type: "function",
					Function: &toolCallFuncWire{Name: tc.Name, Arguments: tc.ArgsText},
				})
			}
		}
		wire.Choices = []chatChunkChoice{{Index: 0, Delta: delta, FinishReason: nil}}
		if chunk.FinishReason != "" {
			wire.Choices[0].FinishReason = strPtr(chunk.FinishReason)
		}
		if !writeChunk(wire) {
			// 客户端断连：ctx 取消会中止上游调用，省下无谓的 token
			s.logRow(world, req, dec, finalModel, usage, 0, 0, time.Since(start), true, false, failoverFrom, "client_cancel", "")
			return
		}
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()

	// 收尾结算
	if usage.PromptTokens == 0 {
		usage.PromptTokens = tokens.EstimateMessages(req.Messages)
		usage.Estimated = true
	}
	if usage.CompletionTokens == 0 {
		usage.CompletionTokens = tokens.Estimate(content.String())
		usage.Estimated = true
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	cost, _ := world.reg.Cost(finalModel, usage.PromptTokens, usage.CompletionTokens)
	savings := s.savings(world, string(dec.Task), usage, cost)
	latency := time.Since(start)
	reward := cascade.HeuristicScore(cascade.CheckReq{
		Messages: req.Messages,
		Response: &types.Response{Content: content.String(), FinishReason: finish},
	}).Score
	world.rt.ReportOutcome(string(dec.Task), finalModel, reward, cost, latency.Milliseconds())
	s.maybeAsyncJudge(world, string(dec.Task), req, &types.Response{Content: content.String(), Model: finalModel, FinishReason: finish})
	s.logRow(world, req, dec, finalModel, usage, cost, savings, latency, true, false, failoverFrom, "ok", "")
}

// logRow 统一写请求日志。
func (s *Server) logRow(world *world, req *types.Request, dec *router.Decision, model string, usage types.Usage,
	cost, savings float64, latency time.Duration, stream, upgraded bool, failoverFrom, status, errMsg string) {
	providerName := ""
	if info, ok := world.reg.Model(model); ok {
		providerName = info.Provider
	}
	s.store.Log(store.Row{
		TS: time.Now().UnixMilli(), RequestID: req.RequestID,
		Task: string(dec.Task), TaskConfidence: dec.TaskConfidence,
		Model: model, Provider: providerName, Tier: dec.Tier,
		Upgraded: upgraded, FailoverFrom: failoverFrom,
		RouteReason:  dec.RouteReasonHeader(model, upgraded),
		PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens,
		TokensEstimated: usage.Estimated, CostUSD: cost, SavingsUSD: savings,
		LatencyMs: latency.Milliseconds(), Stream: stream, Status: status, Error: errMsg,
	})
	s.logger.Info("chat",
		"request_id", req.RequestID, "task", string(dec.Task),
		"tier", dec.Tier, "model", model, "upgraded", upgraded,
		"cost_usd", cost, "savings_usd", savings,
		"latency_ms", latency.Milliseconds(), "status", status,
		"reason", dec.Reason)
}
