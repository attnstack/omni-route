// types_test.go：错误分类行为的直接断言（UpstreamError 的可重试性判定是网关的故障分流依据）。
package types

import (
	"errors"
	"testing"
)

func TestUpstreamErrorRetryable(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{429, true},  // 限流：值得重试
		{500, true},  // 上游内部错误：值得重试
		{503, true},  // 暂不可用：值得重试
		{400, false}, // 请求本身的问题：重试无意义
		{401, false}, // 鉴权失败：重试无意义
		{404, false}, // 路径不存在：重试无意义
	}
	for _, c := range cases {
		e := &UpstreamError{Status: c.status, Body: "x"}
		if e.Retryable() != c.want {
			t.Errorf("status %d retryable = %v, want %v", c.status, e.Retryable(), c.want)
		}
	}
	if got := (&UpstreamError{Status: 500, Body: "boom"}).Error(); got == "" {
		t.Fatal("Error() must not be empty")
	}
}

func TestSentinelErrors(t *testing.T) {
	// 领域哨兵错误必须可用 errors.Is 判别（executor 与 gateway 的错误分支依赖它）
	if !errors.Is(ErrNoCandidate, ErrNoCandidate) || !errors.Is(ErrBreakerOpen, ErrBreakerOpen) {
		t.Fatal("sentinel errors broken")
	}
	if errors.Is(ErrNoCandidate, ErrBreakerOpen) {
		t.Fatal("sentinels must be distinct")
	}
}

func TestTruncate(t *testing.T) {
	if truncate("short", 100) != "short" {
		t.Fatal("short strings pass through")
	}
	if got := truncate("0123456789", 4); len(got) != 7 || got[:4] != "0123" {
		t.Fatalf("truncate = %q", got)
	}
}
