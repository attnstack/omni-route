// breaker.go：按 provider 的滑动窗口熔断器。
// 5 秒一个桶、60 个桶 = 5 分钟窗口；错误率超阈值且样本足够 → open；
// 冷却后半开，单次探测成功即闭合。4xx（除 429）不计入错误——
// 那是请求本身的问题，熔断 provider 只会让所有请求一起失败。
package reliability

import (
	"sync"
	"time"
)

const (
	bucketSec   = 5
	bucketCount = 60 // 5 分钟窗口
)

type breakerBucket struct {
	start   time.Time
	total   int
	failure int
}

type Breaker struct {
	mu         sync.Mutex
	buckets    [bucketCount]breakerBucket
	minSamples int
	errorRate  float64
	openSec    int
	openUntil  time.Time
	halfOpen   bool
	probeUsed  bool
	now        func() time.Time // 测试时钟注入
}

func NewBreaker(windowSec, minSamples int, errorRate float64, openSec int) *Breaker {
	return &Breaker{
		minSamples: minSamples,
		errorRate:  errorRate,
		openSec:    openSec,
		now:        time.Now,
	}
}

// SetNow 注入测试时钟。
func (b *Breaker) SetNow(f func() time.Time) { b.now = f }

func (b *Breaker) currentBucketLocked() *breakerBucket {
	sec := b.now().Unix() / bucketSec
	idx := sec % bucketCount
	bk := &b.buckets[idx]
	if bk.start != time.Unix(sec*bucketSec, 0) {
		bk.start = time.Unix(sec*bucketSec, 0)
		bk.total, bk.failure = 0, 0
	}
	return bk
}

// Allow 报告当前是否放行。半开状态只放一次探测请求。
func (b *Breaker) Allow() (bool, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if !b.openUntil.IsZero() {
		if now.Before(b.openUntil) {
			return false, "circuit open (cooldown)"
		}
		// 冷却结束 → 半开：放一次探测
		b.halfOpen = true
		b.probeUsed = false
		b.openUntil = time.Time{}
	}
	if b.halfOpen {
		if b.probeUsed {
			return false, "circuit half-open (probe in flight)"
		}
		b.probeUsed = true
		return true, "half-open probe"
	}
	return true, ""
}

// Record 记录一次调用结果并推进状态机。
func (b *Breaker) Record(success bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	bk := b.currentBucketLocked()
	bk.total++
	if !success {
		bk.failure++
	}
	if b.halfOpen {
		if success {
			// 探测成功：清空窗口计数，闭合
			b.halfOpen = false
			for i := range b.buckets {
				b.buckets[i] = breakerBucket{}
			}
		} else {
			// 探测失败：重新熔断
			b.openUntil = b.now().Add(time.Duration(b.openSec) * time.Second)
			b.halfOpen = false
		}
		return
	}
	// 常态：检查整窗错误率（样本足够才统计，避免小样本误熔断）
	total, failure := 0, 0
	for _, bk := range b.buckets {
		total += bk.total
		failure += bk.failure
	}
	if total >= b.minSamples && float64(failure)/float64(total) > b.errorRate {
		b.openUntil = b.now().Add(time.Duration(b.openSec) * time.Second)
	}
}

// State 供观测面板展示。
func (b *Breaker) State() (string, float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	total, failure := 0, 0
	for _, bk := range b.buckets {
		total += bk.total
		failure += bk.failure
	}
	rate := 0.0
	if total > 0 {
		rate = float64(failure) / float64(total)
	}
	switch {
	case !b.openUntil.IsZero():
		return "open", rate
	case b.halfOpen:
		return "half-open", rate
	default:
		return "closed", rate
	}
}
