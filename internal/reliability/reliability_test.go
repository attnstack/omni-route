// reliability 包单测：令牌桶速率与熔断状态机。
package reliability

import (
	"context"
	"testing"
	"time"
)

func TestLimiterRPM(t *testing.T) {
	// 60 RPM = 1 QPS：拿 3 个令牌应约 2 秒（首个即时 + 2 个等待）
	l := NewLimiter(60)
	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	elapsed := time.Since(start)
	if elapsed < 1500*time.Millisecond || elapsed > 3500*time.Millisecond {
		t.Fatalf("3 tokens at 1 QPS took %v, want ~2s", elapsed)
	}
}

func TestLimiterContextCancel(t *testing.T) {
	l := NewLimiter(1) // 极慢
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := l.Wait(ctx); err == nil {
		t.Fatal("expected context deadline")
	}
}

func TestBreakerStateMachine(t *testing.T) {
	now := time.Unix(1735689600, 0)
	b := NewBreaker(300, 10, 0.5, 30) // 样本≥10、错误率>50% → 熔断 30s
	b.SetNow(func() time.Time { return now })

	// 正常期：4 成功 1 失败（错误率 20% < 50%）→ 保持闭合
	for i := 0; i < 4; i++ {
		if ok, _ := b.Allow(); !ok {
			t.Fatal("should allow when closed")
		}
		b.Record(true)
	}
	b.Record(false)
	if state, _ := b.State(); state != "closed" {
		t.Fatalf("want closed, got %s", state)
	}

	// 突增失败：错误率过半 → open
	for i := 0; i < 10; i++ {
		b.Allow()
		b.Record(false)
	}
	if state, _ := b.State(); state != "open" {
		t.Fatalf("want open, got %s", state)
	}
	if ok, reason := b.Allow(); ok {
		t.Fatal("open breaker must reject")
	} else if reason == "" {
		t.Fatal("rejection must carry reason")
	}

	// 冷却期结束 → 半开，探测一次
	now = now.Add(31 * time.Second)
	if ok, _ := b.Allow(); !ok {
		t.Fatal("half-open should allow probe")
	}
	if ok, _ := b.Allow(); ok {
		t.Fatal("half-open allows only one probe")
	}
	b.Record(true)
	if state, _ := b.State(); state != "closed" {
		t.Fatalf("probe success should close, got %s", state)
	}

	// 探测失败 → 重新 open
	b2 := NewBreaker(300, 1, 0.5, 30)
	b2.SetNow(func() time.Time { return now })
	b2.Allow()
	b2.Record(false)
	b2.Allow()
	b2.Record(false) // 样本够，错误率 100% → open
	if state, _ := b2.State(); state != "open" {
		t.Fatalf("want open, got %s", state)
	}
	now = now.Add(31 * time.Second)
	b2.Allow()
	b2.Record(false)
	if state, _ := b2.State(); state != "open" {
		t.Fatalf("failed probe should reopen, got %s", state)
	}
}

func TestBackoff(t *testing.T) {
	d0 := Backoff(0, 200, nil)
	if d0 != 200*time.Millisecond {
		t.Fatalf("base backoff = %v", d0)
	}
	d2 := Backoff(2, 200, nil)
	if d2 != 800*time.Millisecond {
		t.Fatalf("exponential backoff = %v", d2)
	}
	// 抖动只增不减
	for i := 0; i < 20; i++ {
		if Backoff(1, 200, nil) < 400*time.Millisecond {
			t.Fatal("jitter must not shrink below base*2")
		}
	}
}
