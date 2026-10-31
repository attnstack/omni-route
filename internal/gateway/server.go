// Package gateway 是 HTTP 入口层：OpenAI 兼容端点、观测端点、鉴权与限长中间件。
// server.go 负责装配：world（配置+依赖的原子快照）支持热加载整包替换，
// 读方永远拿到一致的一份，不需要读锁。
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/attnstack/omni-route/internal/cascade"
	"github.com/attnstack/omni-route/internal/config"
	"github.com/attnstack/omni-route/internal/executor"
	"github.com/attnstack/omni-route/internal/provider"
	"github.com/attnstack/omni-route/internal/router"
	"github.com/attnstack/omni-route/internal/store"
)

// maxBodyBytes 是请求体上限：长上下文请求可能很大，32MB 覆盖绝大多数场景。
const maxBodyBytes = 32 << 20

// world 是一次热加载的完整依赖快照。
type world struct {
	cfg     *config.Config
	reg     *provider.Registry
	exec    *executor.Executor
	rt      *router.Router
	checker *cascade.Checker
	cal     *cascade.Calibrator
	runner  *cascade.Runner
}

// BuildWorld 按配置装配全部运行时依赖。state 与 store 跨热加载复用（学习成果不丢）。
func BuildWorld(cfg *config.Config, client *http.Client, state *router.State, logger *slog.Logger) (*world, error) {
	reg, err := provider.Build(cfg, client, logger)
	if err != nil {
		return nil, err
	}
	seed := cfg.Seed
	exec := executor.New(reg, cfg, seed, logger)
	rt := router.New(cfg, reg, state, seed, logger)
	cal := cascade.NewCalibrator(
		cfg.Cascade.Confidence.Threshold,
		cfg.Cascade.TargetUpgradeRate,
		cfg.Cascade.CalibrationStep,
		cfg.Cascade.CalibrationWindow,
	)
	checker := cascade.NewChecker(cfg, exec, nil)
	// 关键接线：判别阈值来自校准器而非静态配置——
	// 否则升级率校准只更新数据、不影响决策，闭环是断的
	checker.SetThresholdFunc(cal.Threshold)
	runner := cascade.NewRunner(exec, checker, cal, cfg, logger)
	return &world{cfg: cfg, reg: reg, exec: exec, rt: rt, checker: checker, cal: cal, runner: runner}, nil
}

// Server 是网关 HTTP 服务。
type Server struct {
	world    atomic.Value // *world
	store    *store.Store
	logger   *slog.Logger
	reloadFn func() error // 由 main 注入（重读配置→重建 world）
	stateDir string
	started  time.Time
}

func NewServer(w *world, st *store.Store, stateDir string, logger *slog.Logger) *Server {
	s := &Server{store: st, logger: logger, stateDir: stateDir, started: time.Now()}
	s.world.Store(w)
	return s
}

// SetReloadFn 注入热加载实现。
func (s *Server) SetReloadFn(f func() error) { s.reloadFn = f }

// SwapWorld 热加载后整包替换。
func (s *Server) SwapWorld(w *world) { s.world.Store(w) }

func (s *Server) current() *world { return s.world.Load().(*world) }

// Handler 装配全部路由。Go 1.22 的 ServeMux 支持方法+路径模式，
// 网关这种"十来个端点"的规模不值得引入第三方路由框架。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.handleChat)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("POST /v1/feedback", s.handleFeedback)
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("GET /api/requests", s.handleRequests)
	mux.HandleFunc("POST /admin/reload", s.handleReload)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /{$}", s.handleDashboard)

	return s.recoverMiddleware(s.logMiddleware(s.authMiddleware(mux)))
}

// authMiddleware：网关自身鉴权（api_keys 非空时对 /v1/* 生效）。
// 网关替你保管上游 key，自己裸奔等于把所有上游 key 拱手送人。
// 客户端照旧发 Authorization: Bearer <网关key>，与 OpenAI SDK 天然兼容。
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) >= 4 && r.URL.Path[:4] == "/v1/" {
			keys := s.current().cfg.APIKeys
			if len(keys) > 0 {
				got := trimBearer(r.Header.Get("Authorization"))
				ok := false
				for _, k := range keys {
					if got == k {
						ok = true
						break
					}
				}
				if !ok {
					writeError(w, http.StatusUnauthorized, "api_error", "unauthorized",
						"invalid or missing API key (configure api_keys in config)")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// recoverMiddleware：任何 handler panic 都转成 OpenAI 风格 500，
// 网关不能把堆栈直接甩给客户端，更不能带崩整个进程。
func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error("panic in handler", "path", r.URL.Path, "panic", fmt.Sprint(rec))
				writeError(w, http.StatusInternalServerError, "api_error", "internal", "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// logMiddleware：请求级结构化日志 + request_id 注入。
func (s *Server) logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rid := r.Header.Get("x-request-id")
		if rid == "" {
			rid = newRequestID()
		}
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, rid))
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		if r.URL.Path != "/healthz" { // 健康检查不刷日志
			s.logger.Info("http",
				"method", r.Method, "path", r.URL.Path, "status", sw.status,
				"latency_ms", time.Since(start).Milliseconds(), "request_id", rid)
		}
	})
}

type requestIDKey struct{}

func requestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey{}).(string); ok {
		return v
	}
	return ""
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush 透传 flush（SSE 必需，同时保持 status 记录）。
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("or-%d", time.Now().UnixNano())
	}
	return "or-" + hex.EncodeToString(b[:])
}

// handleModels：OpenAI 客户端（langchain 等）常先拉模型列表，必须实现。
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	world := s.current()
	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	entries := []modelEntry{}
	for _, m := range world.reg.Models() {
		entries = append(entries, modelEntry{ID: m.Name, Object: "model", Created: 1700000000, OwnedBy: m.Provider})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": entries})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"uptime":  time.Since(s.started).Round(time.Second).String(),
		"version": "0.1.0",
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
