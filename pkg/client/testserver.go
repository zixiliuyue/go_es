// Package client SDK 测试 fixture
//
// 提供 NewTestServer(t) helper: 启动内存 BadgerDB + go_es 服务端 + httptest.Server,
// 自动注册 t.Cleanup 关闭, 供所有 SDK 包(pkg/pool, pkg/index, pkg/document ...)复用.
//
// 设计目标:
//   - 替代 "需要真实 ES 才能 run" 的测试(原做法: skip 或连接 localhost:9200)
//   - 真实 HTTP 行为(包含 go-elasticsearch 产品嗅探要求的 X-Elastic-Product 头)
//   - 零外部依赖(不用 testcontainers, 不用 docker), 离线可跑, -race 友好
//
// 用法:
//
//	ts := client.NewTestServer(t)
//	c, err := client.NewClient(Config{Addresses: []string{ts.URL()}})
//	// c.Ping() == true, 后续 c.GetES().Index(...) 等正常使用
//
// 带认证:
//
//	ts := client.NewTestServerWithOptions(t, client.TestServerOptions{
//	    Auth: server.AuthConfig{Enabled: true, Basic: map[string]string{"admin": "secret"}},
//	})
package client

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/zixiliuyue/go_es/internal/search"
	"github.com/zixiliuyue/go_es/internal/server"
	"github.com/zixiliuyue/go_es/internal/storage"
	"go.uber.org/zap"
)

// TestServerOptions 测试服务端可选配置
//
// 字段:
//   - Auth:    认证配置(默认不启用)
//   - Limit:   限流配置(默认不启用)
//   - Logger:  日志(默认 zap.NewNop() 静音)
//   - StartupDone: 是否自动 MarkStartupDone(默认 true, 设 false 用于测试 startup 端点)
type TestServerOptions struct {
	Auth       server.AuthConfig
	Limit      server.LimitConfig
	Logger     *zap.Logger
	StartupDone bool
}

// TestServer 测试服务端封装
//
// 字段(只读, 通过方法访问):
//   - Server:  go_es 业务服务端(可用来调 Shutdown 验证降级)
//   - Store:   BadgerDB store(可用来直接写数据做前置准备)
//   - Engine:  搜索引擎(可用来直接调 IndexDoc/Get 做断言)
//   - HTTP:    httptest.Server 实例
type TestServer struct {
	Server *server.Server
	Store  *storage.Store
	Engine *search.Engine
	HTTP   *httptest.Server
}

// URL 返回测试服务端的 HTTP base URL(形如 http://127.0.0.1:<动态端口>)
func (ts *TestServer) URL() string {
	if ts == nil || ts.HTTP == nil {
		return ""
	}
	return ts.HTTP.URL
}

// Addr 返回 host:port 形式地址, 便于 pool 测试用
func (ts *TestServer) Addr() string {
	if ts == nil || ts.HTTP == nil || ts.HTTP.Listener == nil {
		return ""
	}
	return ts.HTTP.Listener.Addr().String()
}

// Close 主动关闭测试服务端(通常不需要手动调, t.Cleanup 已注册)
// 允许多次调用, 内部幂等
func (ts *TestServer) Close() {
	if ts == nil {
		return
	}
	if ts.HTTP != nil {
		ts.HTTP.Close()
		ts.HTTP = nil
	}
	if ts.Server != nil {
		ts.Server.Shutdown(context.Background())
		ts.Server = nil
	}
	if ts.Store != nil {
		_ = ts.Store.Close()
		ts.Store = nil
	}
}

// NewTestServer 启动一个内存模式 + 无认证的测试服务端
//
// 参数:
//   - t: testing.T, 自动注册 cleanup
//
// 返回:
//   - *TestServer: 测试服务端封装, 通过 URL() 获取访问地址
func NewTestServer(t testing.TB) *TestServer {
	t.Helper()
	return NewTestServerWithOptions(t, TestServerOptions{})
}

// NewTestServerWithOptions 启动带自定义配置的测试服务端
//
// 参数:
//   - t:    testing.TB(支持 *testing.T 与 *testing.B)
//   - opts: TestServerOptions 配置
//
// 返回:
//   - *TestServer: 测试服务端封装
func NewTestServerWithOptions(t testing.TB, opts TestServerOptions) *TestServer {
	t.Helper()

	// 1) 内存 BadgerDB(空路径触发 WithInMemory(true))
	store, err := storage.Open("")
	if err != nil {
		t.Fatalf("testserver: open in-memory store failed: %v", err)
	}

	// 2) 搜索引擎
	engine := search.New(store)

	// 3) go_es 服务端
	logger := opts.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	srv := server.NewWithOptions(store, engine, logger, server.ServerOptions{
		Auth:  opts.Auth,
		Limit: opts.Limit,
	})
	if opts.StartupDone || opts.StartupDone == false {
		// 默认 true(零值 false 也按 true 处理, 因为绝大多数测试都需要 readiness=200)
		srv.MarkStartupDone()
	}

	// 4) httptest.Server 自动选空闲端口
	httpSrv := httptest.NewServer(srv.Handler())

	ts := &TestServer{
		Server: srv,
		Store:  store,
		Engine: engine,
		HTTP:   httpSrv,
	}

	// 5) 自动 cleanup(顺序: 先关 HTTP, 再关业务, 最后关 store)
	t.Cleanup(func() {
		ts.Close()
	})

	return ts
}

// NewClientForTest 基于 TestServer 创建一个已连接好的 *Client
//
// 参数:
//   - t:  testing.TB
//   - ts: NewTestServer 返回的测试服务端
//
// 返回:
//   - *Client: 已通过 Info() 校验的客户端
//   - error:   仅在极端情况下返回(如 server 未启动)
//
// 默认禁用 retry(避免测试中 retry 掩盖真实失败), 启用 breaker(便于测试容错路径)
func NewClientForTest(t testing.TB, ts *TestServer) (*Client, error) {
	t.Helper()
	if ts == nil || ts.HTTP == nil {
		t.Fatalf("testserver: NewClientForTest called with nil TestServer or closed HTTP")
	}
	cfg := Config{
		Addresses: []string{ts.URL()},
		Logger:    zap.NewNop(),
		Retry:     RetryConfig{Enabled: false},
		// Breaker 用零值 → NewClient 自动默认启用
	}
	return NewClient(cfg)
}
