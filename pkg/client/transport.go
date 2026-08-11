// Package client HTTP Transport 包装层
//
// 集成指数退避重试 + 熔断器:
//   - 每个 RoundTrip 请求首先经过熔断器 Allow() 判定, 被 Open 快速失败
//   - 允许通过后, 在 for attempt := 0; attempt < MaxAttempts; attempt++ 循环内发送:
//       成功 → breaker.OnResult(true) + 返回响应
//       失败且 shouldRetry=true → 休眠 nextBackoff(attempt) → 下一轮
//       失败且 shouldRetry=false → 立即退出, 返回最后结果
package client

import (
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"
)

// RetryingTransport 包装 http.RoundTripper, 注入 retry + breaker 能力
//
// 字段:
//   - Inner:    底层真正发送请求的 RoundTripper, 通常为 http.DefaultTransport 或用户定制
//   - Retry:    指数退避配置
//   - Breaker:  熔断器实例(可以为 nil = 不启用熔断, 但仍走 retry)
//   - Logger:   日志(可 nil, nil 时不输出 WARN/DEBUG 行)
type RetryingTransport struct {
	Inner   http.RoundTripper
	Retry   RetryConfig
	Breaker *CircuitBreaker
	Logger  *zap.Logger
}

// newRetryingTransport 用 cfg + 可选 logger 构造包装器
// 当用户传入 inner 为 nil 时退回到 http.DefaultTransport
func newRetryingTransport(inner http.RoundTripper, retry RetryConfig, breaker *CircuitBreaker, log *zap.Logger) *RetryingTransport {
	if inner == nil {
		inner = http.DefaultTransport
	}
	retry.applyDefaults()
	return &RetryingTransport{
		Inner:   inner,
		Retry:   retry,
		Breaker: breaker,
		Logger:  log,
	}
}

// RoundTrip 实现 http.RoundTripper, 顺序:
//  1) 熔断判定 → Open 直接返回 ErrCircuitOpen
//  2) attempt 循环尝试发送
//  3) 成功或达到 MaxAttempts/不可重试后返回最后结果
func (t *RetryingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// 1) 熔断器快速失败
	if t.Breaker != nil && !t.Breaker.Allow() {
		if t.Logger != nil {
			t.Logger.Warn("circuit breaker open, reject request fast",
				zap.String("method", req.Method),
				zap.String("url", req.URL.Redacted()),
				zap.String("breaker_state", t.Breaker.State().String()))
		}
		return nil, ErrCircuitOpen
	}

	// 2) 重试循环
	var (
		lastResp *http.Response
		lastErr  error
	)
	maxAttempts := t.Retry.MaxAttempts
	if !t.Retry.Enabled {
		maxAttempts = 1
	}
	attempt := 0
	for attempt < maxAttempts {
		// 每次尝试使用请求的 body 副本(如有 body, 需要允许重放)
		// go-elasticsearch 已经通过 GetBody 处理了 body 重放, 这里直接使用 req 克隆
		r := req
		if attempt > 0 {
			// 深拷贝请求, 避免被 RoundTripper 改写字段影响后续重试
			r = req.Clone(req.Context())
			if t.Logger != nil {
				t.Logger.Debug("retrying http request",
					zap.Int("attempt", attempt+1),
					zap.Int("max_attempts", maxAttempts),
					zap.String("method", req.Method),
					zap.String("url", req.URL.Redacted()))
			}
		}
		resp, err := t.Inner.RoundTrip(r)
		lastResp = resp
		lastErr = err

		// 3) 报告熔断器结果(无论成功失败都走一次统计)
		if t.Breaker != nil {
			t.Breaker.OnResult(resp, err)
		}

		// 4) 判断是否需要重试
		if !shouldRetry(resp, err) {
			return resp, err
		}

		attempt++
		if attempt >= maxAttempts {
			// 达到次数上限, 返回最后一次结果
			break
		}

		// 可重试, 但检查上下文是否已取消(避免在 ctx 过期后还等待)
		if req.Context() != nil {
			select {
			case <-req.Context().Done():
				if t.Logger != nil {
					t.Logger.Warn("request context done, aborting retry sleep",
						zap.Error(req.Context().Err()))
				}
				return lastResp, lastErr
			default:
			}
		}

		sleep := t.Retry.nextBackoff(attempt - 1)
		if t.Logger != nil {
			status := 0
			if resp != nil {
				status = resp.StatusCode
			}
			t.Logger.Warn("request retry scheduled",
				zap.Int("attempt", attempt),
				zap.Int("max_attempts", maxAttempts),
				zap.Int("status", status),
				zap.NamedError("err", err),
				zap.String("sleep", sleep.String()),
				zap.String("method", req.Method),
				zap.String("url", req.URL.Redacted()))
		}

		// 支持可中断 sleep
		if !t.sleepContext(req, sleep) {
			// 被上下文打断 → 返回最后结果
			return lastResp, lastErr
		}
	}
	if t.Logger != nil && attempt >= maxAttempts && maxAttempts > 1 {
		status := 0
		if lastResp != nil {
			status = lastResp.StatusCode
		}
		t.Logger.Error("request retries exhausted, returning last result",
			zap.Int("max_attempts", maxAttempts),
			zap.Int("last_status", status),
			zap.NamedError("last_err", lastErr),
			zap.String("method", req.Method),
			zap.String("url", req.URL.Redacted()))
	}
	return lastResp, lastErr
}

// sleepContext 可中断的 sleep(优先基于 request.Context())
// 返回 true 表示睡满, 返回 false 表示 context 已被取消
func (t *RetryingTransport) sleepContext(req *http.Request, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	ctx := req.Context()
	if ctx == nil {
		time.Sleep(d)
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// String 打印当前 transport 摘要(调试用)
func (t *RetryingTransport) String() string {
	breakerState := "disabled"
	if t.Breaker != nil {
		breakerState = t.Breaker.String()
	}
	return fmt.Sprintf("transport{retry=%+v breaker=%s}", t.Retry, breakerState)
}
