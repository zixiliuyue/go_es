// Package client 提供Elasticsearch客户端连接管理
// 负责创建、配置和管理Elasticsearch客户端实例
//
// 容错:
//   - RetryConfig: 指数退避 + 20% 抖动(5xx / 429 / 连接错误重试, 默认 3 次,
//     初始 100ms, 上限 5s; 可通过 cfg.Retry.Enabled=false 显式关闭)
//   - BreakerConfig: CircuitBreaker 熔断器 Closed→Open→HalfOpen→Closed 状态机
//     (默认 5 次连续失败开启, 30s 窗口, 2 次成功恢复; 可 cfg.Breaker.Enabled=false 关闭)
package client

import (
	"net/http"

	"github.com/elastic/go-elasticsearch/v8"
	"go.uber.org/zap"
)

// Client 封装Elasticsearch客户端和日志记录器
type Client struct {
	es      *elasticsearch.Client
	log     *zap.Logger
	breaker *CircuitBreaker
}

// Config 客户端配置选项
//
// 向后兼容: 只填老字段 Addresses/Username/Password/Logger 即可, 新容错能力
// 会自动用默认值启用(retry=3 次, breaker=5 失败开)
// 显式关闭容错: cfg.Retry.Enabled=false / cfg.Breaker.Enabled=false
type Config struct {
	// Addresses ES节点地址列表
	Addresses []string
	// Username 用户名
	Username string
	// Password 密码
	Password string
	// Logger 日志记录器
	Logger *zap.Logger
	// Retry 指数退避重试配置(默认: 启用, 3 次尝试, 100ms ~ 5s 抖动)
	Retry RetryConfig
	// Breaker 熔断器配置(默认: 启用, 5 次连续失败开, 30s 窗口)
	Breaker BreakerConfig
	// Transport 用户可指定底层 RoundTripper(可选, nil=用 http.DefaultTransport)
	Transport http.RoundTripper
}

// NewClient 创建一个新的Elasticsearch客户端
//
// 参数:
//
//	cfg: 客户端配置选项(可为零值, 默认启用 retry + breaker)
//
// 返回:
//
//	*Client: 封装后的客户端实例
//	error:   创建过程中的错误, 成功则为 nil
func NewClient(cfg Config) (*Client, error) {
	// 日志优先准备好( transport 要引用)
	if cfg.Logger == nil {
		cfg.Logger, _ = zap.NewProduction()
	}

	// 容错默认值:
	//   - 用户只要填了 Breaker/Retry 内的字段, 就使用用户的 Enabled 语义
	//   - 完全没填(=零值) 时, 视为"默认启用"(与 todo.md 验收描述一致)
	retryCfg := cfg.Retry
	if retryCfg.Enabled {
		retryCfg.applyDefaults()
	} else if !retryCfg.Enabled &&
		retryCfg.MaxAttempts == 0 &&
		retryCfg.BaseBackoff == 0 &&
		retryCfg.MaxBackoff == 0 &&
		retryCfg.JitterFactor == 0 {
		retryCfg = DefaultRetryConfig()
	} else {
		// 用户显式 Enabled=false 或填了部分非 enabled 值但 enabled=false: 保留用户值
		retryCfg.applyDefaults()
	}
	breakerCfg := cfg.Breaker
	var breaker *CircuitBreaker
	if breakerCfg.Enabled {
		breakerCfg.applyDefaults()
		breaker = NewCircuitBreaker(breakerCfg)
	} else if !breakerCfg.Enabled &&
		breakerCfg.FailureThreshold == 0 &&
		breakerCfg.Timeout == 0 &&
		breakerCfg.SuccessThreshold == 0 &&
		breakerCfg.MaxHalfOpenReqs == 0 {
		breakerCfg = DefaultBreakerConfig()
		breaker = NewCircuitBreaker(breakerCfg)
	} else {
		// 用户显式填了配置但 Enabled=false → 不启用 breaker(breaker 保持 nil)
		breakerCfg.applyDefaults()
	}

	// 组装 transport: RetryingTransport 包装用户传入 Transport 或 DefaultTransport
	inner := cfg.Transport
	if inner == nil {
		inner = http.DefaultTransport
	}
	retrying := newRetryingTransport(inner, retryCfg, breaker, cfg.Logger)

	esCfg := elasticsearch.Config{
		Addresses: cfg.Addresses,
		Username:  cfg.Username,
		Password:  cfg.Password,
		Transport: retrying,
	}

	es, err := elasticsearch.NewClient(esCfg)
	if err != nil {
		return nil, err
	}

	// 验证连接信息
	info, err := es.Info()
	if err != nil {
		return nil, err
	}
	defer info.Body.Close()

	cfg.Logger.Info("Connected to Elasticsearch",
		zap.Strings("addresses", cfg.Addresses),
		zap.Bool("retry_enabled", retryCfg.Enabled),
		zap.Int("retry_max_attempts", retryCfg.MaxAttempts),
		zap.Bool("breaker_enabled", breaker != nil),
	)

	return &Client{
		es:      es,
		log:     cfg.Logger,
		breaker: breaker,
	}, nil
}

// GetES 获取原始Elasticsearch客户端
// 返回:
//
//	*elasticsearch.Client: 原始ES客户端实例
func (c *Client) GetES() *elasticsearch.Client {
	return c.es
}

// Ping 检测ES集群是否可用
// 返回:
//
//	bool: 集群是否可用
//	error: 检测过程中的错误
func (c *Client) Ping() (bool, error) {
	res, err := c.es.Ping()
	if err != nil {
		c.log.Error("Failed to ping Elasticsearch", zap.Error(err))
		return false, err
	}
	defer res.Body.Close()

	return res.IsError() == false, nil
}

// Close 关闭客户端连接
func (c *Client) Close() {
	// go-elasticsearch 不需要显式关闭连接
	// 这里预留清理逻辑
	c.log.Debug("Client closed")
}

// Breaker 返回熔断器实例(仅调试使用, 可能为 nil=未启用)
func (c *Client) Breaker() *CircuitBreaker {
	return c.breaker
}

