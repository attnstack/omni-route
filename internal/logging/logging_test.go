// logging_test.go：日志初始化的级别与格式选择。
package logging

import (
	"testing"
)

func TestSetupFormats(t *testing.T) {
	for _, format := range []string{"text", "json", "unknown-falls-back-to-text"} {
		if logger := Setup("info", format); logger == nil {
			t.Fatalf("Setup(%q) returned nil", format)
		}
	}
	// 各级别都能构造（级别过滤行为由 slog 保证，这里验证分发逻辑不 panic）
	for _, level := range []string{"debug", "info", "warn", "error", "odd"} {
		if logger := Setup(level, "json"); logger == nil {
			t.Fatalf("Setup(%q) returned nil", level)
		}
	}
}
