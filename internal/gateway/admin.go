// admin.go：观测与管理端点。dashboard 的数据源（/api/stats、/api/requests）、
// 用户反馈闭环（/v1/feedback）、配置热加载（/admin/reload）。
package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/attnstack/omni-route/internal/buildinfo"
	"github.com/attnstack/omni-route/internal/metrics"
	"github.com/attnstack/omni-route/internal/store"
	"github.com/attnstack/omni-route/web"
)

// handleStats：dashboard 主数据。路由在线状态（λ/质量表/校准阈值/熔断器）与
// SQLite 聚合一次给全，前端 2s 轮询。
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	world := s.current()
	agg, err := s.store.Stats()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "stats", err.Error())
		return
	}
	lambdas := map[string]float64{}
	spends := map[string]float64{}
	for _, task := range []string{"code", "reasoning", "summarization", "translation", "extraction", "chat"} {
		spend, _, lambda := world.rt.State().SpendAndLambda(task)
		lambdas[task] = lambda
		spends[task] = spend
	}
	breakers := map[string]string{}
	for name := range world.cfg.Providers {
		if b := world.exec.Breaker(name); b != nil {
			state, rate := b.State()
			breakers[name] = state + " err=" + strconv.FormatFloat(rate, 'f', 2, 64)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"store": agg,
		"cache": map[string]any{
			"hits":   metrics.Default.Get("omni_route_cache_hits_total", nil),
			"misses": metrics.Default.Get("omni_route_cache_misses_total", nil),
		},
		"version":      buildinfo.Version,
		"lambda":       lambdas,
		"spend_window": spends,
		"quality":      world.rt.State().QualityTable(),
		"thresholds":   world.cal.Snapshot(),
		"breakers":     breakers,
		"mode":         world.cfg.Routing.Mode,
		"uptime_sec":   int(time.Since(s.started).Seconds()),
	})
}

func (s *Server) handleRequests(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	rows, err := s.store.Recent(limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "requests", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": rows})
}

// handleFeedback：用户反馈闭环。👎 直接降该 (task, model) 的质量分，
// 路由会在后续请求中自然倾斜——这是"判别错了怎么办"的兜底机制。
func (s *Server) handleFeedback(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RequestID string `json:"request_id"`
		Verdict   string `json:"verdict"` // good | bad
		Comment   string `json:"comment"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", err.Error())
		return
	}
	if body.RequestID == "" || (body.Verdict != "good" && body.Verdict != "bad") {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_feedback",
			`request_id and verdict ("good"|"bad") are required`)
		return
	}
	row, found, err := s.store.LookupRequest(body.RequestID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "feedback_lookup", err.Error())
		return
	}
	good := body.Verdict == "good"
	if err := s.store.AddFeedback(store.Feedback{
		RequestID: body.RequestID, TS: time.Now().UnixMilli(), Good: good, Comment: body.Comment,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "feedback_store", err.Error())
		return
	}
	if found {
		world := s.current()
		world.rt.State().ApplyFeedback(row.Task, row.Model, good)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": found})
}

// handleReload：热加载入口。校验失败则保留旧配置（config.Watch 同样语义）。
func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	world := s.current()
	if world.cfg.AdminKeyEnv != "" {
		key := r.Header.Get("Authorization")
		key = trimBearer(key)
		if key == "" || key != os.Getenv(world.cfg.AdminKeyEnv) {
			writeError(w, http.StatusUnauthorized, "api_error", "unauthorized", "admin key required")
			return
		}
	}
	if s.reloadFn == nil {
		writeError(w, http.StatusNotImplemented, "api_error", "no_reload", "reload not configured")
		return
	}
	if err := s.reloadFn(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "reload_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reloaded_at": time.Now().UnixMilli()})
}

func trimBearer(s string) string {
	const prefix = "Bearer "
	if len(s) > len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):]
	}
	return s
}

// handleDashboard：内嵌单页仪表盘（构建期 embed，无外部静态文件依赖）。
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(web.DashboardHTML)
}
