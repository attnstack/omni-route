// omni-route 网关入口：装配配置/存储/路由/网关，处理优雅退出与热加载。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/attnstack/omni-route/internal/config"
	"github.com/attnstack/omni-route/internal/gateway"
	"github.com/attnstack/omni-route/internal/logging"
	"github.com/attnstack/omni-route/internal/router"
	"github.com/attnstack/omni-route/internal/store"
)

func main() {
	configPath := flag.String("config", "config.yaml", "配置文件路径（YAML）")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
	logger := logging.Setup(cfg.Log.Level, cfg.Log.Format)

	// 数据目录保证存在（SQLite 路径 + 状态快照）
	if dir := filepath.Dir(cfg.Store.Path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			logger.Error("create data dir failed", "dir", dir, "error", err.Error())
			os.Exit(1)
		}
	}

	st, err := store.Open(cfg.Store.Path,
		time.Duration(cfg.Store.FlushIntervalMs)*time.Millisecond,
		cfg.Store.BatchSize, logger)
	if err != nil {
		logger.Error("open store failed", "error", err.Error())
		os.Exit(1)
	}
	defer st.Close()

	// HTTP client：连接池调大（网关是上游的聚合点）；
	// DialContext 承担连接超时，ResponseHeaderTimeout 承担"首字节超时"（含 TLS 握手）
	client := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:          256,
			MaxIdleConnsPerHost:   64,
			IdleConnTimeout:       90 * time.Second,
			DialContext:           (&net.Dialer{Timeout: time.Duration(cfg.Timeouts.ConnectMs) * time.Millisecond}).DialContext,
			ResponseHeaderTimeout: time.Duration(cfg.Timeouts.FirstByteMs) * time.Millisecond,
		},
	}

	state := router.NewState(cfg)
	statePath := cfg.Store.Path + ".state.json"
	if err := state.Load(statePath); err != nil {
		logger.Warn("router state load failed, starting fresh", "error", err.Error())
	}

	w, err := gateway.BuildWorld(cfg, client, state, logger)
	if err != nil {
		logger.Error("build world failed", "error", err.Error())
		os.Exit(1)
	}
	srv := gateway.NewServer(w, st, filepath.Dir(statePath), logger)

	// reload：用给定配置（或重读文件）重建 world 并原子替换。
	// 任一步失败保留旧 world——配置错误不能让正在服务的网关挂掉。
	doReload := func(newCfg *config.Config) error {
		if newCfg == nil {
			var err error
			newCfg, err = config.Load(*configPath)
			if err != nil {
				return err
			}
		}
		newW, err := gateway.BuildWorld(newCfg, client, state, logger)
		if err != nil {
			return err
		}
		logging.Setup(newCfg.Log.Level, newCfg.Log.Format)
		srv.SwapWorld(newW)
		logger.Info("config reloaded",
			"providers", len(newCfg.Providers), "models", len(newCfg.Models))
		return nil
	}
	srv.SetReloadFn(func() error { return doReload(nil) })

	// 文件变更热加载（mtime 轮询，跨平台）
	stopWatch := config.Watch(*configPath, 5*time.Second, func(c *config.Config, err error) {
		if err != nil {
			logger.Warn("hot reload skipped: invalid config", "error", err.Error())
			return
		}
		if err := doReload(c); err != nil {
			logger.Warn("hot reload failed", "error", err.Error())
		}
	})
	defer stopWatch()

	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// 优雅退出：停止接新请求 → 等存量请求排空（上限 15s）→ 落盘状态
	go func() {
		logger.Info("omni-route listening", "addr", cfg.Listen,
			"providers", len(cfg.Providers), "models", len(cfg.Models),
			"mode", cfg.Routing.Mode, "api_keys", len(cfg.APIKeys))
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "error", err.Error())
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	logger.Info("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		logger.Warn("shutdown drain timeout", "error", err.Error())
	}
	stopWatch()
	if err := state.Save(statePath); err != nil {
		logger.Warn("state save failed", "error", err.Error())
	}
	logger.Info("bye")
}
