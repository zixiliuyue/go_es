// Package client 提供Elasticsearch客户端连接管理
// 负责创建、配置和管理Elasticsearch客户端实例
package client

import (
	"github.com/elastic/go-elasticsearch/v8"
	"go.uber.org/zap"
)

// Client 封装Elasticsearch客户端和日志记录器
type Client struct {
	es  *elasticsearch.Client
	log *zap.Logger
}

// Config 客户端配置选项
type Config struct {
	// Addresses ES节点地址列表
	Addresses []string
	// Username 用户名
	Username string
	// Password 密码
	Password string
	// Logger 日志记录器
	Logger *zap.Logger
}

// NewClient 创建一个新的Elasticsearch客户端
// 参数:
//
//	cfg: 客户端配置选项
//
// 返回:
//
//	*Client: 封装后的客户端实例
//	error: 创建过程中的错误，如果成功则为nil
func NewClient(cfg Config) (*Client, error) {
	esCfg := elasticsearch.Config{
		Addresses: cfg.Addresses,
		Username:  cfg.Username,
		Password:  cfg.Password,
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

	if cfg.Logger == nil {
		cfg.Logger, _ = zap.NewProduction()
	}

	cfg.Logger.Info("Connected to Elasticsearch",
		zap.Strings("addresses", cfg.Addresses))

	return &Client{
		es:  es,
		log: cfg.Logger,
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
