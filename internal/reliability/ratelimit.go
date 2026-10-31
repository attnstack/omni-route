// Package reliability 提供网关可靠性原语：每 provider 令牌桶限流、
// 滑动窗口熔断器、重试退避。全部为进程内实现——单节点 MVP 不引入分布式依赖。
package reliability

import (
	"context"
	"sync"
	"time"
)

// Limiter 是按 RPM 配额的令牌桶（桶容量=1 秒突发上限的近似：capacity=RPM/60 向上取整，
// 上限至少 1）。阻塞式 Wait 让请求排队而不是直接拒绝——对上游配额的尊重
// 优先于本地排队延迟，429 换回来的重试更贵。
type Limiter struct {
	mu       sync.Mutex
	tokens   float64
	capacity float64
	perSec   float64
	last     time.Time
}

func NewLimiter(rpm int) *Limiter {
	if rpm <= 0 {
		rpm = 600 // 无配额信息时的保守默认
	}
	perSec := float64(rpm) / 60.0
	return &Limiter{
		tokens:   perSec, // 冷启动允许一个小突发
		capacity: perSec,
		perSec:   perSec,
		last:     time.Now(),
	}
}

func (l *Limiter) refillLocked() {
	now := time.Now()
	l.tokens += now.Sub(l.last).Seconds() * l.perSec
	if l.tokens > l.capacity {
		l.tokens = l.capacity
	}
	l.last = now
}

// Wait 阻塞直到拿到一个令牌或 ctx 结束。
func (l *Limiter) Wait(ctx context.Context) error {
	for {
		l.mu.Lock()
		l.refillLocked()
		if l.tokens >= 1 {
			l.tokens--
			l.mu.Unlock()
			return nil
		}
		// 距离下一个令牌的等待时间
		wait := time.Duration((1 - l.tokens) / l.perSec * float64(time.Second))
		l.mu.Unlock()
		if wait < time.Millisecond {
			wait = time.Millisecond
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
