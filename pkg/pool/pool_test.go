// Package pool 包的单元测试
// 测试连接池管理功能
//
// 使用 client.NewTestServer 启动内存 go_es 服务端,
// 不再依赖真实 ES, 离线 + -race 全通过
package pool

import (
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zixiliuyue/go_es/pkg/client"
	"go.uber.org/zap"
)

// newTestPoolTS 启动一个内存测试服务端, 返回 *client.TestServer
// t.Cleanup 自动关闭
func newTestPoolTS(t *testing.T) *client.TestServer {
	t.Helper()
	return client.NewTestServer(t)
}

func TestNewPool(t *testing.T) {
	logger, _ := zap.NewDevelopment()

	// 空 pool
	config := PoolConfig{
		Nodes:               []NodeConfig{},
		HealthCheckInterval: 0, // use default
		Logger:              logger,
	}

	pool, err := NewPool(config)
	assert.NoError(t, err)
	assert.NotNil(t, pool)
	assert.Equal(t, 0, pool.TotalCount())
	assert.Equal(t, 0, pool.HealthyCount())
	pool.Close()
}

func TestPool_AddRemoveNode(t *testing.T) {
	ts := newTestPoolTS(t)

	logger, _ := zap.NewDevelopment()

	config := PoolConfig{
		Nodes: []NodeConfig{
			{Addresses: []string{ts.URL()}},
		},
		Logger: logger,
	}

	pool, err := NewPool(config)
	require.NoError(t, err)
	require.NotNil(t, pool)
	assert.Equal(t, 1, pool.TotalCount())

	// 启动第二个测试服务端用于添加节点测试
	ts2 := newTestPoolTS(t)

	// Add another node
	err = pool.AddNode(NodeConfig{
		Addresses: []string{ts2.URL()},
	})
	assert.NoError(t, err)
	assert.Equal(t, 2, pool.TotalCount())

	// Remove node
	err = pool.RemoveNode(ts2.URL())
	assert.NoError(t, err)
	assert.Equal(t, 1, pool.TotalCount())

	pool.Close()
}

func TestPool_Stats(t *testing.T) {
	ts1 := newTestPoolTS(t)
	ts2 := newTestPoolTS(t)

	logger, _ := zap.NewDevelopment()

	config := PoolConfig{
		Nodes: []NodeConfig{
			{Addresses: []string{ts1.URL()}},
			{Addresses: []string{ts2.URL()}},
		},
		Logger: logger,
	}

	pool, err := NewPool(config)
	require.NoError(t, err)

	total, healthy := pool.Stats()
	assert.Equal(t, 2, total)
	// 两个节点都是内存 go_es, 健康检查应通过
	assert.Equal(t, 2, healthy)

	pool.Close()
}

func TestPool_GetReturnsHealthyClient(t *testing.T) {
	ts := newTestPoolTS(t)

	logger, _ := zap.NewDevelopment()

	config := PoolConfig{
		Nodes: []NodeConfig{
			{Addresses: []string{ts.URL()}},
		},
		Logger:             logger,
		HealthCheckInterval: 100 * time.Hour, // 关掉后台健康检查干扰
	}

	pool, err := NewPool(config)
	require.NoError(t, err)
	defer pool.Close()

	// Get 应返回健康客户端(而不是 "no healthy nodes" 错误)
	c, err := pool.Get()
	require.NoError(t, err, "pool.Get should succeed with a running test server")
	require.NotNil(t, c)

	// 验证客户端可真实 ping
	ok, err := c.Ping()
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestPool_GetWhenNoHealthyNodes(t *testing.T) {
	logger, _ := zap.NewDevelopment()

	config := PoolConfig{
		Nodes: []NodeConfig{
			// 使用一个不可能有服务的地址(端口 1 是特权端口, 几乎必然 connection refused)
			{Addresses: []string{"http://127.0.0.1:1"}},
		},
		Logger: logger,
	}

	pool, err := NewPool(config)
	assert.NoError(t, err)
	defer pool.Close()

	// Get 应返回错误: no healthy nodes
	client, err := pool.Get()
	assert.Error(t, err)
	assert.Nil(t, client)
	assert.Contains(t, err.Error(), "no healthy nodes")
}

func TestPool_GetAllHealthy(t *testing.T) {
	ts := newTestPoolTS(t)

	logger, _ := zap.NewDevelopment()

	config := PoolConfig{
		Nodes: []NodeConfig{
			{Addresses: []string{ts.URL()}},
		},
		Logger: logger,
	}

	pool, err := NewPool(config)
	require.NoError(t, err)
	defer pool.Close()

	clients := pool.GetAllHealthy()
	// 测试服务端是健康的, 应返回 1 个客户端
	require.Len(t, clients, 1, "should have 1 healthy client from test server")
	require.NotNil(t, clients[0])

	// 验证客户端可用
	ok, err := clients[0].Ping()
	require.NoError(t, err)
	assert.True(t, ok)
}

// TestPool_WeightedRoundRobin: 加权轮询在多节点时能返回不同客户端
func TestPool_WeightedRoundRobin(t *testing.T) {
	ts1 := newTestPoolTS(t)
	ts2 := newTestPoolTS(t)

	logger, _ := zap.NewDevelopment()

	config := PoolConfig{
		Nodes: []NodeConfig{
			{Addresses: []string{ts1.URL()}, Weight: 1},
			{Addresses: []string{ts2.URL()}, Weight: 1},
		},
		LoadBalancer:        "weighted_round_robin",
		Logger:              logger,
		HealthCheckInterval: 100 * time.Hour,
	}

	pool, err := NewPool(config)
	require.NoError(t, err)
	defer pool.Close()

	require.Equal(t, 2, pool.HealthyCount())

	// 连续 Get 多次, 应能取到两个不同的客户端
	// 用 *client.Client 指针地址区分(SWRR 在两节点等权下应交替返回)
	seen := make(map[uintptr]bool)
	for i := 0; i < 10; i++ {
		c, err := pool.Get()
		require.NoError(t, err)
		// 用 reflect 取指针值做去重 key
		seen[uintptr(unsafe.Pointer(c))] = true
	}
	// 两节点等权 → SWRR 应在 2 次内轮换, 10 次必能见到 2 个不同指针
	assert.Len(t, seen, 2, "weighted round robin should distribute across 2 nodes")
}
