// Package client 指数退避与重试工具 (不带外部依赖: 不使用 cenkalti/backoff, 纯标准库实现)
//
// 设计要点:
//   - 仅依赖标准库 math/rand / crypto/rand / time / net/http, 不引入新第三方库
//   - nextBackoff 采用指数倍增 + 20% 随机抖动, 避免惊群效应
//   - shouldRetry 规则: 任何 err != nil (连接/IO/超时错误)、HTTP 5xx (500~599)、
//     以及 429 TooManyRequests 可重试; 其他 4xx 一律不重试
package client

import (
	crypto_rand "crypto/rand"
	"math/big"
	math_rand "math/rand"
	"net/http"
	"time"
)

// RetryConfig 指数退避重试配置
//
// 字段:
//   - Enabled:      是否启用重试, 默认 true
//   - MaxAttempts:  最多尝试次数(默认 3 = 初始请求 + 2 次重试)
//   - BaseBackoff:  第 1 次重试的等待时间(默认 100ms)
//   - MaxBackoff:   指数增长上限(默认 5s, 防止指数溢出导致超长等待)
//   - JitterFactor: 抖动系数(0~1, 0.2 = ±20% 随机扰动)
type RetryConfig struct {
	Enabled      bool
	MaxAttempts  int
	BaseBackoff  time.Duration
	MaxBackoff   time.Duration
	JitterFactor float64
}

// DefaultRetryConfig 返回默认重试配置
//  默认: 启用, 3 次尝试, 初始 100ms, 上限 5s, 抖动 ±20%
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		Enabled:      true,
		MaxAttempts:  3,
		BaseBackoff:  100 * time.Millisecond,
		MaxBackoff:   5 * time.Second,
		JitterFactor: 0.2,
	}
}

// applyDefaults 为零值字段补默认(创建 NewClient 时内部调用)
func (c *RetryConfig) applyDefaults() {
	// Enabled 字段的零值 false 表示"用户未显式开启" → 视为默认 true 行为
	// 如需真正关闭, 用户显式设置 Enabled=false 并与全局开关组合使用,
	// 因此这里不主动改 Enabled, 留 transport 层判断
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	if c.BaseBackoff <= 0 {
		c.BaseBackoff = 100 * time.Millisecond
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = 5 * time.Second
	}
	if c.JitterFactor <= 0 {
		c.JitterFactor = 0.2
	}
	if c.JitterFactor > 1.0 {
		c.JitterFactor = 1.0
	}
	// 校正大小关系: Base <= Max
	if c.BaseBackoff > c.MaxBackoff {
		c.MaxBackoff = c.BaseBackoff
	}
}

// nextBackoff 计算第 attempt 次重试的休眠时长
//
// 参数:
//   - attempt: 重试序号, 从 0 开始(0 表示首次请求之后准备第 1 次重试)
//
// 公式:
//   duration = min(MaxBackoff, BaseBackoff * 2^attempt)
//   duration *= 1 ± JitterFactor(随机)
//   最终再做一次 MaxBackoff 截断
func (cfg RetryConfig) nextBackoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	// 计算 base * 2^attempt, 防溢出
	var dur float64
	if attempt >= 63 {
		dur = float64(cfg.MaxBackoff)
	} else {
		mult := 1 << uint(attempt)
		dur = float64(cfg.BaseBackoff) * float64(mult)
	}
	if dur > float64(cfg.MaxBackoff) {
		dur = float64(cfg.MaxBackoff)
	}
	// 抖动
	if cfg.JitterFactor > 0 {
		// 优先 crypto/rand, 回退 math/rand
		var nInt int64 = 500
		if n, err := crypto_rand.Int(crypto_rand.Reader, big.NewInt(1000)); err == nil {
			nInt = n.Int64()
		} else {
			nInt = int64(math_rand.Intn(1000))
		}
		// nInt ∈ [0, 1000) → /500 - 1 ∈ [-1.0, 1.0)
		ratio := 1.0 + (float64(nInt)/500.0-1.0)*cfg.JitterFactor
		if ratio < 0.05 {
			ratio = 0.05
		}
		dur *= ratio
	}
	if dur > float64(cfg.MaxBackoff) {
		dur = float64(cfg.MaxBackoff)
	}
	if dur < 0 {
		dur = 0
	}
	return time.Duration(dur)
}

// shouldRetry 判断 (响应, 错误) 组合是否需要重试
//
// 返回 true 的情况:
//   1) err != nil (DNS 失败、TCP RST、TLS 握手失败、任何 IO 错误、超时)
//   2) resp == nil (正常但无响应, 当作网络层错误重试)
//   3) resp.StatusCode ∈ [500, 599] (服务端故障, 等待自愈)
//   4) resp.StatusCode == 429 (限流/配额超标, 等一等再试)
//
// 返回 false 的情况:
//   - 1xx/2xx/3xx (成功) + 4xx 非 429 (BadRequest/Unauthorized/NotFound 等不应重试)
func shouldRetry(resp *http.Response, err error) bool {
	if err != nil {
		return true
	}
	if resp == nil {
		return true
	}
	code := resp.StatusCode
	if code == 0 {
		return true
	}
	if code >= 500 && code <= 599 {
		return true
	}
	if code == 429 {
		return true
	}
	return false
}
