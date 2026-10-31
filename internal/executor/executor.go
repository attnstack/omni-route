// Package executor 是"带可靠性保证的上游调用层"：
// 限流排队 → 熔断检查 → provider 调用 → 指数退避重试。
// 路由（选谁）与执行（怎么调通）在这里分离——级联、failover 都建立在本层之上。
package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"github.com/attnstack/omni-route/internal/config"
	"github.com/attnstack/omni-route/internal/provider"
	"github.com/attnstack/omni-route/internal/reliability"
	"github.com/attnstack/omni-route/internal/types"
)

// Executor 封装"对某个模型发起一次可靠调用"。
type Executor struct {
	reg    *provider.Registry
	cfg    *config.Config
	lim    map[string]*reliability.Limiter // per provider
	brk    map[string]*reliability.Breaker // per provider
	rng    *rand.Rand
	logger *slog.Logger
}

func New(reg *provider.Registry, cfg *config.Config, seed int64, logger *slog.Logger) *Executor {
	e := &Executor{
		reg:    reg,
		cfg:    cfg,
		lim:    map[string]*reliability.Limiter{},
		brk:    map[string]*reliability.Breaker{},
		rng:    rand.New(rand.NewSource(seed + 1)),
		logger: logger,
	}
	for name, pc := range cfg.Providers {
		e.lim[name] = reliability.NewLimiter(pc.RPM)
	}
	return e
}

// Breaker 暴露（懒创建）指定 provider 的熔断器，供测试注入时钟。
func (e *Executor) Breaker(providerName string) *reliability.Breaker {
	e.lazyBreaker(providerName)
	return e.brk[providerName]
}

func (e *Executor) lazyBreaker(providerName string) *reliability.Breaker {
	if b, ok := e.brk[providerName]; ok {
		return b
	}
	b := reliability.NewBreaker(
		e.cfg.Reliability.Breaker.WindowSec,
		e.cfg.Reliability.Breaker.MinSamples,
		e.cfg.Reliability.Breaker.ErrorRate,
		e.cfg.Reliability.Breaker.OpenSec,
	)
	e.brk[providerName] = b
	return b
}

func (e *Executor) attempts() int {
	return e.cfg.Reliability.Retries + 1
}

// Call 非流式调用：完整走限流/熔断/重试。返回错误时调用方决定是否 failover。
func (e *Executor) Call(ctx context.Context, model string, req *types.Request) (*types.Response, error) {
	prov, info, err := e.reg.Resolve(model)
	if err != nil {
		return nil, err
	}
	// 路由选定谁，出站请求就必须带谁的名字。用副本避免并发共享时
	// 被级联/判别路径读到被改写的 Model。
	outReq := *req
	outReq.Model = model
	totalCtx, cancel := context.WithTimeout(ctx, time.Duration(e.cfg.Timeouts.TotalMs)*time.Millisecond)
	defer cancel()

	var lastErr error
	for attempt := 0; attempt < e.attempts(); attempt++ {
		if attempt > 0 {
			backoff := reliability.Backoff(attempt-1, e.cfg.Reliability.BackoffBaseMs, e.rng)
			select {
			case <-time.After(backoff):
			case <-totalCtx.Done():
				return nil, fmt.Errorf("call %s: %w", model, totalCtx.Err())
			}
			e.logger.Info("retrying upstream call", "model", model, "attempt", attempt+1, "last_error", lastErr.Error())
		}
		if err := e.lim[info.Provider].Wait(totalCtx); err != nil {
			return nil, fmt.Errorf("call %s: rate limiter: %w", model, err)
		}
		brk := e.lazyBreaker(info.Provider)
		if ok, reason := brk.Allow(); !ok {
			return nil, fmt.Errorf("%w: provider %s: %s", types.ErrBreakerOpen, info.Provider, reason)
		}
		resp, err := prov.Complete(totalCtx, &outReq)
		brk.Record(err == nil)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !reliability.Retryable(err) || errors.Is(err, context.Canceled) {
			return nil, fmt.Errorf("call %s: %w", model, err)
		}
	}
	return nil, fmt.Errorf("call %s: exhausted %d attempts: %w", model, e.attempts(), lastErr)
}

// Stream 建立流式连接。只对"建立失败"做重试——一旦开始向客户端吐 chunk，
// 重试会造成内容重复，协议上不可撤销（这也是级联只做非流式的同一原因）。
func (e *Executor) Stream(ctx context.Context, model string, req *types.Request) (types.Stream, error) {
	prov, info, err := e.reg.Resolve(model)
	if err != nil {
		return nil, err
	}
	outReq := *req
	outReq.Model = model
	totalCtx, cancel := context.WithTimeout(ctx, time.Duration(e.cfg.Timeouts.TotalMs)*time.Millisecond)

	var lastErr error
	for attempt := 0; attempt < e.attempts(); attempt++ {
		if attempt > 0 {
			backoff := reliability.Backoff(attempt-1, e.cfg.Reliability.BackoffBaseMs, e.rng)
			select {
			case <-time.After(backoff):
			case <-totalCtx.Done():
				cancel()
				return nil, fmt.Errorf("stream %s: %w", model, totalCtx.Err())
			}
			e.logger.Info("retrying upstream stream", "model", model, "attempt", attempt+1, "last_error", lastErr.Error())
		}
		if err := e.lim[info.Provider].Wait(totalCtx); err != nil {
			cancel()
			return nil, fmt.Errorf("stream %s: rate limiter: %w", model, err)
		}
		brk := e.lazyBreaker(info.Provider)
		if ok, reason := brk.Allow(); !ok {
			cancel()
			return nil, fmt.Errorf("%w: provider %s: %s", types.ErrBreakerOpen, info.Provider, reason)
		}
		st, err := prov.Stream(totalCtx, &outReq)
		brk.Record(err == nil)
		if err == nil {
			return &cancelingStream{inner: st, cancel: cancel}, nil
		}
		lastErr = err
		if !reliability.Retryable(err) || errors.Is(err, context.Canceled) {
			cancel()
			return nil, fmt.Errorf("stream %s: %w", model, err)
		}
	}
	cancel()
	return nil, fmt.Errorf("stream %s: exhausted %d attempts: %w", model, e.attempts(), lastErr)
}

// cancelingStream 把 totalCtx 的 cancel 与流生命周期绑定，防止泄漏。
type cancelingStream struct {
	inner  types.Stream
	cancel context.CancelFunc
	once   bool
}

func (s *cancelingStream) Next() (types.Chunk, error) {
	return s.inner.Next()
}

func (s *cancelingStream) Close() error {
	if !s.once {
		s.once = true
		defer s.cancel()
	}
	return s.inner.Close()
}
