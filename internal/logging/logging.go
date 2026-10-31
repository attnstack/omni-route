// Package logging 统一结构化日志初始化。网关热路径的每一跳（路由决策、
// failover、升级、熔断）都必须留痕，否则线上问题无从排查。
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// Setup 按配置初始化全局 slog logger。format: text(开发) | json(生产采集)。
func Setup(level, format string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var handler slog.Handler
	if strings.ToLower(format) == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger
}
