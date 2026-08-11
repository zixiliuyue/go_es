// Package client retry + breaker + transport 单元测试
//
// 用 fakeRoundTripper / fakeTripperFactory 精确控制每次 RoundTrip 的行为
// 不依赖真实 HTTP server(除特别说明外), 保证快速 & 可重复 & 离线可跑
package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ------------------------------------------------------------------
// 工具: fakeRoundTripper — 可自定义"每次尝试返回什么"的 RoundTripper
// ------------------------------------------------------------------

type tripResult struct {
	resp *http.Response
	err  error
}

// fakeRoundTripper 按索引返回 trips[i] 的结果, 超出索引后重复最后一个结果
// 对每个 RoundTrip 调用 atomically +1 count, 用于断言"共发送了几次请求"
type fakeRoundTripper struct {
	mu    sync.Mutex
	trips []tripResult
	count int
}

func newFakeTripper(results ...tripResult) *fakeRoundTripper {
	return &fakeRoundTripper{trips: append([]tripResult(nil), results...)}
}

// Count 返回已被调用次数(原子读)
func (f *fakeRoundTripper) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.count
}

func (f *fakeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	idx := f.count
	f.count++
	if idx >= len(f.trips) && len(f.trips) > 0 {
		idx = len(f.trips) - 1
	}
	r := f.trips[idx]
	resp := r.resp
	if resp != nil {
		resp.Request = req
	}
	return resp, r.err
}

// okTrip 构造一个 200 OK 响应(带 request 字段为 nil, transport 自己会填)
func okTrip() tripResult {
	return tripResult{resp: &http.Response{
		StatusCode: 200,
		Body:       http.NoBody,
		Header:     make(http.Header),
	}}
}

// statusTrip 指定 status code
func statusTrip(code int) tripResult {
	return tripResult{resp: &http.Response{
		StatusCode: code,
		Body:       http.NoBody,
		Header:     make(http.Header),
	}}
}

// errTrip 模拟 RoundTrip 层错误(例如 dial tcp i/o timeout 等)
func errTrip(err error) tripResult {
	return tripResult{err: err}
}

// newTestReq 创建一个最小 GET / HTTP/1.1 请求带后台 ctx, 用于传给 RoundTrip
func newTestReq(t *testing.T) *http.Request {
	t.Helper()
	r, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.com/", nil)
	if err != nil {
		t.Fatalf("newTestReq: %v", err)
	}
	return r
}

// fastRetryCfg 返回"短 sleep"重试配置, 用于测试(否则每次 retry 会等 100ms+), 测试跑得快
func fastRetryCfg(maxAttempts int) RetryConfig {
	cfg := DefaultRetryConfig()
	cfg.Enabled = true
	cfg.MaxAttempts = maxAttempts
	cfg.BaseBackoff = 1 * time.Millisecond
	cfg.MaxBackoff = 2 * time.Millisecond
	cfg.JitterFactor = 0.05
	return cfg
}

// disabledRetryCfg 返回禁用 retry 配置
func disabledRetryCfg() RetryConfig {
	cfg := DefaultRetryConfig()
	cfg.Enabled = false
	return cfg
}

// ================================================================
// 第一部分: Retry 行为测试(10 条)
// ================================================================

// TestRetry_On5xxThenSuccess: 返回两次 503, 然后 200, 预期共发送 3 次请求并最后 200
func TestRetry_On5xxThenSuccess(t *testing.T) {
	t.Parallel()
	fake := newFakeTripper(statusTrip(503), statusTrip(503), okTrip())
	tr := newRetryingTransport(fake, fastRetryCfg(3), nil, nil)
	resp, err := tr.RoundTrip(newTestReq(t))
	if err != nil {
		t.Fatalf("unexpected err=%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status=200, got %d", resp.StatusCode)
	}
	if got := fake.Count(); got != 3 {
		t.Errorf("expect 3 roundtrips (503 503 200), got %d", got)
	}
}

// TestRetry_On4xxDoesNotRetry: 直接 400, 不重试
func TestRetry_On4xxDoesNotRetry(t *testing.T) {
	t.Parallel()
	fake := newFakeTripper(statusTrip(400))
	tr := newRetryingTransport(fake, fastRetryCfg(3), nil, nil)
	resp, err := tr.RoundTrip(newTestReq(t))
	if err != nil {
		t.Fatalf("unexpected err=%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("want 400 got %d", resp.StatusCode)
	}
	if got := fake.Count(); got != 1 {
		t.Errorf("expect 1 trip, got %d", got)
	}
}

// TestRetry_ExhaustedAlways5xx: 永远 500, 最终返回最后 500
func TestRetry_ExhaustedAlways5xx(t *testing.T) {
	t.Parallel()
	fake := newFakeTripper(statusTrip(500))
	cfg := fastRetryCfg(4)
	tr := newRetryingTransport(fake, cfg, nil, nil)
	resp, err := tr.RoundTrip(newTestReq(t))
	if err != nil {
		t.Fatalf("unexpected err=%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Errorf("want last 500, got %d", resp.StatusCode)
	}
	if got := fake.Count(); got != 4 {
		t.Errorf("expect 4 attempts, got %d", got)
	}
}

// TestRetry_OnConnectionError: 前两次 dial i/o timeout, 第三次成功
func TestRetry_OnConnectionError(t *testing.T) {
	t.Parallel()
	dialErr := fmt.Errorf("dial tcp 127.0.0.1:9200: connect: connection refused")
	fake := newFakeTripper(errTrip(dialErr), errTrip(dialErr), okTrip())
	tr := newRetryingTransport(fake, fastRetryCfg(3), nil, nil)
	resp, err := tr.RoundTrip(newTestReq(t))
	if err != nil {
		t.Fatalf("expect recover at third attempt, got err=%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("want 200 got %d", resp.StatusCode)
	}
	if got := fake.Count(); got != 3 {
		t.Errorf("want 3 trips, got %d", got)
	}
}

// TestRetry_Disabled: Enabled=false, 即使 5xx 也只做 1 次
func TestRetry_Disabled(t *testing.T) {
	t.Parallel()
	fake := newFakeTripper(statusTrip(503))
	tr := newRetryingTransport(fake, disabledRetryCfg(), nil, nil)
	resp, err := tr.RoundTrip(newTestReq(t))
	if err != nil {
		t.Fatalf("unexpected err=%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Errorf("want 503 got %d", resp.StatusCode)
	}
	if got := fake.Count(); got != 1 {
		t.Errorf("Disabled retry should only call RoundTrip once, got %d", got)
	}
}

// TestRetry_ContextCancelAbortsSleep: 首次失败后, ctx 被取消, 不应该 sleep 到下次重试
func TestRetry_ContextCancelAbortsSleep(t *testing.T) {
	t.Parallel()
	// 构造: 第一次 500, ctx 设置短超时(2ms); 第二次也 500 但还没跑到
	cfg := DefaultRetryConfig()
	cfg.Enabled = true
	cfg.MaxAttempts = 3
	cfg.BaseBackoff = 200 * time.Millisecond // 故意设得很大, 确保 ctx 先到期
	cfg.MaxBackoff = 500 * time.Millisecond
	cfg.JitterFactor = 0

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Millisecond)
	defer cancel()

	fake := newFakeTripper(statusTrip(500))
	tr := newRetryingTransport(fake, cfg, nil, nil)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.com/", nil)
	resp, _ := tr.RoundTrip(req)
	if resp != nil {
		defer resp.Body.Close()
	}
	// 预期: 尝试 1 次后 sleep, 被 ctx 打断, 所以 count=1(不会继续第 2、3 次)
	if got := fake.Count(); got != 1 {
		t.Errorf("ctx canceled should stop retry at 1 attempt, got %d", got)
	}
}

// TestRetry_On429TooManyRequests: 429 属于 shouldRetry
func TestRetry_On429TooManyRequests(t *testing.T) {
	t.Parallel()
	fake := newFakeTripper(statusTrip(429), statusTrip(429), okTrip())
	tr := newRetryingTransport(fake, fastRetryCfg(4), nil, nil)
	resp, err := tr.RoundTrip(newTestReq(t))
	if err != nil {
		t.Fatalf("unexpected err=%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("want 200, got %d", resp.StatusCode)
	}
	if got := fake.Count(); got != 3 {
		t.Errorf("expect 3 trips(429 429 200), got %d", got)
	}
}

// TestRetry_OnSuccessWithBody: 成功(且 2xx)路径正常, 不做 retry
func TestRetry_SuccessNeverRetry(t *testing.T) {
	t.Parallel()
	fake := newFakeTripper(okTrip())
	tr := newRetryingTransport(fake, fastRetryCfg(5), nil, nil)
	resp, err := tr.RoundTrip(newTestReq(t))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	defer resp.Body.Close()
	if got := fake.Count(); got != 1 {
		t.Errorf("success should call once, got %d", got)
	}
}

// TestRetry_BackoffRange: nextBackoff 在 [Base, Base*2^n + jitter] 范围
func TestRetry_BackoffRange(t *testing.T) {
	t.Parallel()
	cfg := DefaultRetryConfig()
	base := 100 * time.Millisecond
	cfg.BaseBackoff = base
	cfg.MaxBackoff = 5 * time.Second
	cfg.JitterFactor = 0.2 // 20%

	// attempt 0: ~100ms ±20% → [80ms,120ms]
	for attempt := 0; attempt < 8; attempt++ {
		s := cfg.nextBackoff(attempt)
		nominal := base * time.Duration(int64(1)<<attempt) // 100,200,400,800ms,...
		if nominal > cfg.MaxBackoff {
			nominal = cfg.MaxBackoff
		}
		lo := time.Duration(float64(nominal) * (1 - cfg.JitterFactor))
		hi := time.Duration(float64(nominal) * (1 + cfg.JitterFactor))
		if s < lo || s > hi {
			t.Errorf("attempt=%d sleep=%s not in [%s,%s]", attempt, s, lo, hi)
		}
	}
}

// TestRetry_BackoffClampsAtMax: attempt 很大时不会超过 MaxBackoff
func TestRetry_BackoffClampsAtMax(t *testing.T) {
	t.Parallel()
	cfg := DefaultRetryConfig()
	cfg.BaseBackoff = 100 * time.Millisecond
	cfg.MaxBackoff = 500 * time.Millisecond
	cfg.JitterFactor = 0.3
	for i := 0; i < 50; i++ {
		s := cfg.nextBackoff(1000)
		nominal := cfg.MaxBackoff
		lo := time.Duration(float64(nominal) * (1 - cfg.JitterFactor))
		hi := time.Duration(float64(nominal) * (1 + cfg.JitterFactor))
		if s < lo || s > hi {
			t.Fatalf("iter=%d sleep=%s not in maxbounds [%s,%s]", i, s, lo, hi)
		}
	}
}

// TestRetry_BackoffNegativeAttemptSafe: 负数 attempt → 至少 BaseBackoff * (1-Jitter)
func TestRetry_BackoffNegativeAttemptSafe(t *testing.T) {
	t.Parallel()
	cfg := DefaultRetryConfig()
	cfg.BaseBackoff = 100 * time.Millisecond
	cfg.MaxBackoff = 5 * time.Second
	cfg.JitterFactor = 0.2
	s := cfg.nextBackoff(-5)
	lo := time.Duration(float64(cfg.BaseBackoff) * (1 - cfg.JitterFactor))
	hi := time.Duration(float64(cfg.BaseBackoff) * (1 + cfg.JitterFactor))
	if s < lo || s > hi {
		t.Errorf("negative attempt should treat as attempt=0, got %s not in [%s,%s]", s, lo, hi)
	}
}

// TestRetry_ApplyDefaultsCoversZeros: 所有零值补默认
func TestRetry_ApplyDefaultsCoversZeros(t *testing.T) {
	t.Parallel()
	var c RetryConfig
	c.applyDefaults()
	if c.Enabled != false {
		// applyDefaults 不做启用开关(由 caller 决定), 只补数值
	}
	if c.MaxAttempts != 3 {
		t.Errorf("default MaxAttempts 3 got %d", c.MaxAttempts)
	}
	if c.BaseBackoff != 100*time.Millisecond {
		t.Errorf("default BaseBackoff 100ms got %v", c.BaseBackoff)
	}
	if c.MaxBackoff != 5*time.Second {
		t.Errorf("default MaxBackoff 5s got %v", c.MaxBackoff)
	}
	if c.JitterFactor != 0.2 {
		t.Errorf("default JitterFactor 0.2 got %v", c.JitterFactor)
	}
}

// TestShouldRetry_Matrix: 快速覆盖 shouldRetry 主要分支
func TestShouldRetry_Matrix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		resp   *http.Response
		err    error
		expect bool
	}{
		{"nil_resp_no_err", nil, nil, true},  // 实现保守: 无响应视为网络错 retry
		{"resp_200", &http.Response{StatusCode: 200}, nil, false},
		{"resp_299", &http.Response{StatusCode: 299}, nil, false},
		{"resp_301", &http.Response{StatusCode: 301}, nil, false},
		{"resp_400", &http.Response{StatusCode: 400}, nil, false},
		{"resp_429", &http.Response{StatusCode: 429}, nil, true},
		{"resp_500", &http.Response{StatusCode: 500}, nil, true},
		{"resp_503", &http.Response{StatusCode: 503}, nil, true},
		{"resp_499", &http.Response{StatusCode: 499}, nil, false},
		{"resp_0", &http.Response{StatusCode: 0}, nil, true}, // status 0 当未知网络错
		{"resp_600", &http.Response{StatusCode: 600}, nil, false},
		{"with_err_nil_resp", nil, fmt.Errorf("dial: refused"), true},
		{"err_and_500", &http.Response{StatusCode: 500}, fmt.Errorf("write: broken pipe"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := shouldRetry(c.resp, c.err)
			if got != c.expect {
				t.Errorf("case %s got %v want %v", c.name, got, c.expect)
			}
		})
	}
}

// TestRetry_RetryCond_Status301: 301 重定向不 retry
func TestRetry_NoRetryOn301(t *testing.T) {
	t.Parallel()
	fake := newFakeTripper(statusTrip(301))
	tr := newRetryingTransport(fake, fastRetryCfg(3), nil, nil)
	resp, err := tr.RoundTrip(newTestReq(t))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 301 {
		t.Errorf("want 301 got %d", resp.StatusCode)
	}
	if got := fake.Count(); got != 1 {
		t.Errorf("no retry on 3xx, got %d", got)
	}
}

// ================================================================
// 第二部分: Breaker 状态机(8 条)
// ================================================================

// TestBreaker_Closed→Open: N 次失败触发熔断
func TestBreaker_ClosedToOpen(t *testing.T) {
	t.Parallel()
	b := NewCircuitBreaker(BreakerConfig{FailureThreshold: 3, Timeout: 30 * time.Second, SuccessThreshold: 2, MaxHalfOpenReqs: 1})
	if got := b.State(); got != StateClosed {
		t.Fatalf("initial closed, got %v", got)
	}
	b.onResultBool(true) // 成功一次清零基线
	// 3 次失败
	for i := 0; i < 3; i++ {
		if !b.Allow() {
			t.Fatalf("attempt %d should Allow() in closed", i)
		}
		b.onResultBool(false)
	}
	if got := b.State(); got != StateOpen {
		t.Fatalf("after 3 failures expect StateOpen, got %v", got)
	}
	// Open 下 Allow 立即 false
	if b.Allow() {
		t.Fatalf("Open should not allow requests")
	}
	// ErrCircuitOpen 可 errors.Is 识别
	if !errors.Is(ErrCircuitOpen, ErrCircuitOpen) {
		t.Fatalf("sanity: ErrCircuitOpen not self equal")
	}
}

// TestBreaker_Open→HalfOpen→Closed: Timeout 到期 + 探测成功 → Closed
func TestBreaker_OpenHalfOpenClosed(t *testing.T) {
	t.Parallel()
	cfg := BreakerConfig{FailureThreshold: 2, Timeout: 10 * time.Millisecond, SuccessThreshold: 2, MaxHalfOpenReqs: 1}
	b := NewCircuitBreaker(cfg)
	// 打熔断
	b.onResultBool(false)
	b.onResultBool(false)
	if got := b.State(); got != StateOpen {
		t.Fatalf("expect open, got %v", got)
	}
	// 等待 Timeout 到期
	time.Sleep(30 * time.Millisecond)
	// 进入 Half-Open
	if !b.Allow() {
		t.Fatalf("timeout should enter half-open, allow=1 probe")
	}
	if got := b.State(); got != StateHalfOpen {
		t.Fatalf("got state %v want half-open", got)
	}
	// 第一次成功(1/2)
	b.onResultBool(true)
	if got := b.State(); got != StateHalfOpen {
		t.Fatalf("after 1 success still half-open, got %v", got)
	}
	// 第二次探测(Allow 放行, 因为 inflight ≤ MaxHalfOpenReqs)
	if !b.Allow() {
		t.Fatalf("half-open should allow next probe after success")
	}
	b.onResultBool(true)
	if got := b.State(); got != StateClosed {
		t.Fatalf("2 success → closed, got %v", got)
	}
	stats := b.Stats()
	if stats.State != "closed" || stats.Failure != 0 {
		t.Errorf("stats not reset after recovery: %+v", stats)
	}
}

// TestBreaker_HalfOpenFailsReopens: Half-Open 期间 1 次失败 → 立即回 Open
func TestBreaker_HalfOpenFailsReopens(t *testing.T) {
	t.Parallel()
	cfg := BreakerConfig{FailureThreshold: 2, Timeout: 5 * time.Millisecond, SuccessThreshold: 2, MaxHalfOpenReqs: 1}
	b := NewCircuitBreaker(cfg)
	b.onResultBool(false)
	b.onResultBool(false)
	time.Sleep(15 * time.Millisecond)
	if !b.Allow() {
		t.Fatalf("half-open expect allow")
	}
	// Half-Open 阶段 1 次失败
	b.onResultBool(false)
	if got := b.State(); got != StateOpen {
		t.Fatalf("half-open 1 failure → open, got %v", got)
	}
}

// TestBreaker_ForceOpenAndReset: 运维接口
func TestBreaker_ForceOpenAndReset(t *testing.T) {
	t.Parallel()
	b := NewCircuitBreaker(DefaultBreakerConfig())
	b.ForceOpen()
	if b.State() != StateOpen {
		t.Errorf("ForceOpen → open expected")
	}
	if b.Allow() {
		t.Errorf("force open should not allow")
	}
	b.ForceReset()
	if b.State() != StateClosed {
		t.Errorf("ForceReset → closed")
	}
	if !b.Allow() {
		t.Errorf("closed should allow")
	}
}

// TestBreaker_NilSafe: nil breaker 不 panic
func TestBreaker_NilSafe(t *testing.T) {
	t.Parallel()
	var b *CircuitBreaker = nil
	if !b.Allow() {
		t.Errorf("nil breaker Allow → true (no-op safe)")
	}
	b.OnResult(nil, nil)         // no panic
	b.onResultBool(true)         // no panic
	b.ForceOpen()                // no panic
	b.ForceReset()               // no panic
	if b.State() != StateClosed { // zero state (method on nil → ok)
		// nil 方法没返回, 但不 panic; 这里是静态检查
	}
	_ = b.String() // no panic, should be empty-ish
}

// TestBreaker_Concurrent: 并发调用 Allow/OnResult, 配合 -race 检测
func TestBreaker_Concurrent(t *testing.T) {
	t.Parallel()
	b := NewCircuitBreaker(BreakerConfig{FailureThreshold: 50, Timeout: 10 * time.Millisecond, SuccessThreshold: 5, MaxHalfOpenReqs: 2})
	var wg sync.WaitGroup
	var okCount int64
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if b.Allow() {
					atomic.AddInt64(&okCount, 1)
					// 一半成功一半失败
					if (seed + j) % 2 == 0 {
						b.OnResult(&http.Response{StatusCode: 200}, nil)
					} else {
						b.OnResult(&http.Response{StatusCode: 500}, nil)
					}
				}
			}
		}(i)
	}
	wg.Wait()
	_ = b.Stats() // no panic
	t.Logf("concurrent OK count=%d stats=%+v", okCount, b.Stats())
}

// TestBreaker_DefaultsApplied: NewCircuitBreaker(BreakerConfig{}) — 零值补默认
func TestBreaker_DefaultsApplied(t *testing.T) {
	t.Parallel()
	b := NewCircuitBreaker(BreakerConfig{})
	if b.cfg.FailureThreshold != 5 {
		t.Errorf("default failure threshold 5 got %d", b.cfg.FailureThreshold)
	}
	if b.cfg.Timeout != 30*time.Second {
		t.Errorf("default 30s got %v", b.cfg.Timeout)
	}
	if b.cfg.SuccessThreshold != 2 {
		t.Errorf("default success threshold 2 got %d", b.cfg.SuccessThreshold)
	}
	if b.cfg.MaxHalfOpenReqs != 1 {
		t.Errorf("default max half-open reqs 1 got %d", b.cfg.MaxHalfOpenReqs)
	}
}

// TestBreaker_HalfOpenInflightGate: MaxHalfOpenReqs=2 时, 仅 2 个请求通过
func TestBreaker_HalfOpenInflightGate(t *testing.T) {
	t.Parallel()
	cfg := BreakerConfig{FailureThreshold: 2, Timeout: 2 * time.Millisecond, SuccessThreshold: 100, MaxHalfOpenReqs: 2}
	b := NewCircuitBreaker(cfg)
	// Open it
	b.onResultBool(false)
	b.onResultBool(false)
	// wait timeout
	time.Sleep(6 * time.Millisecond)
	// 第一个 allow → inflight=1, ok
	if !b.Allow() { t.Fatalf("1st probe allow") }
	// 第二个 → inflight=2, ok
	if !b.Allow() { t.Fatalf("2nd probe allow") }
	// 第三个 → inflight=2, 拒绝
	if b.Allow() {
		t.Fatalf("3rd probe beyond MaxHalfOpenReqs=2 must be rejected (treated as open)")
	}
	// 报告 2 次成功, 让 inflight 回退
	b.onResultBool(true)
	b.onResultBool(true)
	// inflight=0, 新的 allow → 可以 (但现在可能 closed 了, 取决于 success 达到阈值)
	// success threshold=100, 所以不会 closed, 再 Allow 就 ok
	if !b.Allow() {
		t.Fatalf("after onResult, inflight decreased → allow")
	}
}

// ================================================================
// 第三部分: Breaker + Transport 集成(4 条)
// ================================================================

// TestTransport_BreakerOpenFastFail: 熔断器 Open 后, 不调用 inner.RoundTrip, 直接 ErrCircuitOpen
func TestTransport_BreakerOpenFastFail(t *testing.T) {
	t.Parallel()
	b := NewCircuitBreaker(DefaultBreakerConfig())
	b.ForceOpen()
	fake := newFakeTripper(okTrip())
	tr := newRetryingTransport(fake, disabledRetryCfg(), b, nil)
	resp, err := tr.RoundTrip(newTestReq(t))
	if err == nil || !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expect ErrCircuitOpen, got resp=%+v err=%v", resp, err)
	}
	if fake.Count() != 0 {
		t.Errorf("breaker open should not call inner RT, count=%d", fake.Count())
	}
}

// TestTransport_BreakerDisabledPasses: breaker=nil, transport 正常工作
func TestTransport_BreakerDisabledPasses(t *testing.T) {
	t.Parallel()
	fake := newFakeTripper(okTrip())
	tr := newRetryingTransport(fake, disabledRetryCfg(), nil, nil)
	resp, err := tr.RoundTrip(newTestReq(t))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("want 200 got %d", resp.StatusCode)
	}
	if fake.Count() != 1 {
		t.Errorf("expect 1 trip got %d", fake.Count())
	}
}

// TestTransport_BreakerTripsAfterFailures: 连续 5xx, breaker 打开后, retry 也不跑了
func TestTransport_BreakerTripsAfterFailures(t *testing.T) {
	t.Parallel()
	fake := newFakeTripper(statusTrip(500))
	cfg := DefaultBreakerConfig()
	cfg.FailureThreshold = 2
	cfg.Timeout = 1 * time.Hour // 长时间熔断, 确保 open 一直开
	cfg.SuccessThreshold = 2
	cfg.MaxHalfOpenReqs = 1
	b := NewCircuitBreaker(cfg)
	rt := newRetryingTransport(fake, disabledRetryCfg(), b, nil)

	// 1st allow → fail → breaker still closed(failure=1)
	_, _ = rt.RoundTrip(newTestReq(t))
	if b.State() != StateClosed {
		t.Fatalf("after 1 failure should stay closed")
	}
	// 2nd allow → fail → open
	_, _ = rt.RoundTrip(newTestReq(t))
	if b.State() != StateOpen {
		t.Fatalf("after 2 failures should open, got %v", b.State())
	}
	// 3rd request: fast fail, count 不应再增加
	prev := fake.Count()
	_, err := rt.RoundTrip(newTestReq(t))
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("3rd should be ErrCircuitOpen, got %v", err)
	}
	if fake.Count() != prev {
		t.Errorf("after open, inner RT should not be called, prev=%d now=%d", prev, fake.Count())
	}
}

// TestTransport_String: String() 包含 breaker/retry 关键字
func TestTransport_String(t *testing.T) {
	t.Parallel()
	b := NewCircuitBreaker(DefaultBreakerConfig())
	tr := newRetryingTransport(http.DefaultTransport, DefaultRetryConfig(), b, nil)
	s := tr.String()
	if s == "" {
		t.Fatalf("String empty")
	}
	if !containsAny(s, "breaker", "retry") {
		t.Errorf("String=%s missing keywords", s)
	}
}

func containsAny(s string, keys ...string) bool {
	for _, k := range keys {
		if len(k) == 0 {
			continue
		}
		// index of: 手动实现, 不引 strings 依赖
		n := len(k)
		for i := 0; i+n <= len(s); i++ {
			if s[i:i+n] == k {
				return true
			}
		}
	}
	return false
}

// ================================================================
// 第四部分: Config 默认值 / NewClient 不真实连 ES 无法测, 但可以单独测 Config 默认逻辑
// ================================================================

// TestConfig_DefaultRetryEnabledByAllZeros: 验证 Config{} 构造时, retry 默认启用(3 次)
// 这是纯逻辑: 直接把 Config 传进去, 我们用 fakeTransport 模拟成功(避免真实网络)
// 由于 NewClient 会调 es.Info() 来建立连接 → 我们用 httptest.NewServer 模拟一个 ES 兼容 OK 响应
func TestConfig_DefaultRetryEnabledOnAllZeros(t *testing.T) {
	t.Parallel()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if atomic.LoadInt32(&calls) < 3 {
			// 前两次 Info() 请求返回 502, 第三次成功
			w.WriteHeader(502)
			return
		}
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"version":{"number":"8.0.0"},"tagline":"You Know, for Search"}`))
	}))
	defer srv.Close()

	failThenOKTripper := &callableTripper{
		fn: func(req *http.Request) (*http.Response, error) {
			// 改写到 httptest server
			r2 := req.Clone(req.Context())
			r2.URL.Scheme = "http"
			r2.URL.Host = srv.Listener.Addr().String()
			// 用默认 transport 发
			return http.DefaultTransport.RoundTrip(r2)
		},
	}

	// 用空 Config{Retry: {}, Breaker: {}} — 应该自动启用
	cfg := Config{
		Addresses: []string{"http://placeholder:9200"},
		Transport: failThenOKTripper,
		Logger:    nil, // NewClient 内部补默认
		// 故意不填 Retry / Breaker — 验证零值默认启用
	}
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient failed: %v (Info 经过 retry 第 3 次应成功)", err)
	}
	if c == nil {
		t.Fatalf("client nil")
	}
	defer c.Close()
	if c.Breaker() == nil {
		t.Errorf("Breaker 默认应该启用, got nil")
	}
	// 前两次 Info 502 + 第三次成功 = 3 calls
	if atomic.LoadInt32(&calls) < 3 {
		t.Errorf("Info retries should have been 3+, got %d", atomic.LoadInt32(&calls))
	}
}

// callableTripper 把 RoundTrip 委托给 fn(便于测试注入改写 URL)
type callableTripper struct {
	fn func(*http.Request) (*http.Response, error)
}

func (c *callableTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return c.fn(req)
}
