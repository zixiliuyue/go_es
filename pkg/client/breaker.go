// Package client 熔断器实现 (不依赖 sony/gobreaker, 手写标准状态机)
//
// 状态机模型:
//
//   ┌─────────────────────┐  连续失败 >= FailureThreshold   ┌──────────────────┐
//   │       Closed        │ ──────────────────────────────▶ │       Open       │
//   │   (正常放行请求)    │                                 │  (快速失败,不发请求)│
//   └─────────────────────┘                                └──────────────────┘
//             ▲                                                        │
//             │ Half-Open 下 SuccessThreshold 次连续成功                │  Timeout 到期
//             │                                                        ▼
//   ┌─────────────────────┐                                 ┌──────────────────┐
//   │      HalfOpen       │ ◀────────────────────────────── │    (时间窗口)     │
//   │  有限放行 N 个探测   │                                 │                  │
//   └─────────────────────┘                                 └──────────────────┘
//             │
//             └─ Half-Open 期间 1 次失败 ──────▶ 回到 Open (重置 Timeout)
//
// 线程安全: mu sync.Mutex 保护所有状态读写
package client

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// BreakerState 熔断器当前状态
type BreakerState int

const (
	// StateClosed 正常放行
	StateClosed BreakerState = iota
	// StateOpen 熔断打开, 快速失败
	StateOpen
	// StateHalfOpen 探测恢复
	StateHalfOpen
)

// String 返回状态可读名
func (s BreakerState) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// ErrCircuitOpen 熔断器处于 Open 状态时直接返回此错误
// 调用方可用 errors.Is(err, ErrCircuitOpen) 识别, 做 fallback
var ErrCircuitOpen = errors.New("client: circuit breaker is open, request rejected fast-fail")

// BreakerConfig 熔断器配置
//
// 字段:
//   - Enabled:          是否启用熔断器(默认 true)
//   - FailureThreshold: Closed 状态下, 连续失败次数达到此值时切换 Open(默认 5)
//   - Timeout:          Open 持续时长, 到期后进入 Half-Open(默认 30s)
//   - SuccessThreshold: Half-Open 状态下, 连续成功次数达到此值才切回 Closed(默认 2)
//   - MaxHalfOpenReqs:  Half-Open 状态下最多同时放行的请求数(默认 1, 保守策略)
type BreakerConfig struct {
	Enabled          bool
	FailureThreshold int
	Timeout          time.Duration
	SuccessThreshold int
	MaxHalfOpenReqs  int
}

// DefaultBreakerConfig 返回默认熔断器配置
//  默认: 启用, 5 次连续失败开启, 30s 熔断窗口, 2 次成功恢复, Half-Open 下同时 1 个请求
func DefaultBreakerConfig() BreakerConfig {
	return BreakerConfig{
		Enabled:          true,
		FailureThreshold: 5,
		Timeout:          30 * time.Second,
		SuccessThreshold: 2,
		MaxHalfOpenReqs:  1,
	}
}

// applyDefaults 补零值默认
func (c *BreakerConfig) applyDefaults() {
	if c.FailureThreshold <= 0 {
		c.FailureThreshold = 5
	}
	if c.Timeout <= 0 {
		c.Timeout = 30 * time.Second
	}
	if c.SuccessThreshold <= 0 {
		c.SuccessThreshold = 2
	}
	if c.MaxHalfOpenReqs <= 0 {
		c.MaxHalfOpenReqs = 1
	}
}

// CircuitBreaker 熔断器实例
//
// 零值不可用, 请用 NewCircuitBreaker(cfg) 构造
type CircuitBreaker struct {
	mu sync.Mutex

	cfg BreakerConfig

	state     BreakerState // 当前状态
	failure   int          // Closed 下连续失败数
	success   int          // HalfOpen 下连续成功数
	inflight  int          // HalfOpen 下已放行请求数(≤ MaxHalfOpenReqs)
	openedAt  time.Time    // 切换到 Open 的时间点
	lastState time.Time    // 最近一次状态切换时间, 用于统计
}

// NewCircuitBreaker 创建一个新熔断器
// 初始状态: Closed
func NewCircuitBreaker(cfg BreakerConfig) *CircuitBreaker {
	cfg.applyDefaults()
	return &CircuitBreaker{
		cfg:       cfg,
		state:     StateClosed,
		lastState: time.Now(),
	}
}

// State 返回当前熔断器状态(线程安全快照)
//
// 对 nil receiver 安全, 返回 StateClosed(= 无熔断器 = 相当于一直放行)
func (b *CircuitBreaker) State() BreakerState {
	if b == nil {
		return StateClosed
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tickStateNoLock(time.Now())
	return b.state
}

// Stats 熔断器统计快照(用于调试与可观测)
type BreakerStats struct {
	State    string
	Failure  int // Closed 下累积连续失败
	Success  int // HalfOpen 下累积成功
	Inflight int // HalfOpen 下行中的探测请求
	OpenFor  time.Duration
}

// Stats 返回当前统计信息, nil 安全
func (b *CircuitBreaker) Stats() BreakerStats {
	if b == nil {
		return BreakerStats{State: StateClosed.String()}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tickStateNoLock(time.Now())
	s := BreakerStats{
		State:    b.state.String(),
		Failure:  b.failure,
		Success:  b.success,
		Inflight: b.inflight,
	}
	if b.state == StateOpen {
		s.OpenFor = time.Since(b.openedAt)
	}
	return s
}

// tickStateNoLock 仅在持锁后调用, 用于处理 Open → HalfOpen 的基于时间自动迁移
func (b *CircuitBreaker) tickStateNoLock(now time.Time) {
	if b.state != StateOpen {
		return
	}
	if now.Sub(b.openedAt) >= b.cfg.Timeout {
		b.transitionNoLock(StateHalfOpen, now)
		// HalfOpen 重置计数与 inflight
		b.success = 0
		b.inflight = 0
	}
}

// transitionNoLock 持锁后安全切换状态, 更新 lastState
func (b *CircuitBreaker) transitionNoLock(next BreakerState, now time.Time) {
	if b.state == next {
		return
	}
	b.state = next
	b.lastState = now
	if next == StateOpen {
		b.openedAt = now
	}
}

// Allow 判断当前是否允许发送请求
//
// 返回值:
//   - true:  允许放行, 调用方随后必须调用 OnResult 报告成功/失败
//   - false: 拒绝放行(Open), 调用方直接返回 ErrCircuitOpen
func (b *CircuitBreaker) Allow() bool {
	if b == nil {
		// 未启用 / 未构造, 视为允许
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.tickStateNoLock(now)

	switch b.state {
	case StateClosed:
		return true
	case StateOpen:
		return false
	case StateHalfOpen:
		if b.inflight >= b.cfg.MaxHalfOpenReqs {
			// HalfOpen 窗口已满, 其余请求当作 Open 快速失败
			return false
		}
		b.inflight++
		return true
	}
	return false
}

// OnResult 报告一个请求的结果, 用于驱动熔断器状态机
//
// 参数:
//   - resp: HTTP 响应(可为 nil)
//   - err:  RoundTrip 错误(可为 nil)
//
// 成功/失败判定: err == nil && resp != nil && status < 500 视为成功, 其余视为失败
func (b *CircuitBreaker) OnResult(resp *http.Response, err error) {
	if b == nil {
		return
	}
	ok := err == nil && resp != nil && resp.StatusCode < 500 && resp.StatusCode != 429
	b.onResultBool(ok)
}

// onResultBool 内部 bool 版 OnResult, 便于单测直接注入 true/false
func (b *CircuitBreaker) onResultBool(ok bool) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.tickStateNoLock(now)

	switch b.state {
	case StateClosed:
		if ok {
			// Closed 下成功即清零失败累积(恢复 0 基线)
			b.failure = 0
			return
		}
		b.failure++
		if b.failure >= b.cfg.FailureThreshold {
			b.transitionNoLock(StateOpen, now)
			// 保留失败计数方便调试, 但不再用于 Open/Half 逻辑
		}

	case StateOpen:
		// Open 期间不应该有请求进来, 理论上 Allow() 先拒绝
		// 但为幂等, 再打一次票视为失败: 保持 Open, 重置窗口(滑动)
		if !ok {
			b.openedAt = now
		}

	case StateHalfOpen:
		// inflight 减一, 如果超过 MaxHalfOpenReqs 的请求（理论上 Allow 就拒了）
		if b.inflight > 0 {
			b.inflight--
		}
		if !ok {
			// Half-Open 下 1 次失败 → 立即回到 Open, 重置 Timeout 窗口
			b.transitionNoLock(StateOpen, now)
			b.success = 0
			return
		}
		b.success++
		if b.success >= b.cfg.SuccessThreshold {
			// 足够多的探测成功 → 切回 Closed 正常工作
			b.transitionNoLock(StateClosed, now)
			b.failure = 0
			b.success = 0
			b.inflight = 0
		}
	}
}

// ForceOpen 强制熔断器打开(常用于运维命令、手动降级测试)
func (b *CircuitBreaker) ForceOpen() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.transitionNoLock(StateOpen, time.Now())
}

// ForceReset 强制熔断器重置(回到 Closed, 清所有计数) — 仅测试 / 运维使用
func (b *CircuitBreaker) ForceReset() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.transitionNoLock(StateClosed, time.Now())
	b.failure = 0
	b.success = 0
	b.inflight = 0
}

// String 调试打印熔断器状态摘要
func (b *CircuitBreaker) String() string {
	s := b.Stats()
	return fmt.Sprintf("breaker{state=%s failure=%d success=%d inflight=%d open=%s}",
		s.State, s.Failure, s.Success, s.Inflight, s.OpenFor)
}
