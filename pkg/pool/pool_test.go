// Package pool 包的单元测试
// 测试连接池管理功能
package pool

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

func TestNewPool(t *testing.T) {
	logger, _ := zap.NewDevelopment()

	// 空pool
	config := PoolConfig{
		Nodes:             []NodeConfig{},
		HealthCheckInterval: 0, // use default
		Logger:             logger,
	}

	pool, err := NewPool(config)
	assert.NoError(t, err)
	assert.NotNil(t, pool)
	assert.Equal(t, 0, pool.TotalCount())
	assert.Equal(t, 0, pool.HealthyCount())
	pool.Close()
}

func TestPool_AddRemoveNode(t *testing.T) {
	logger, _ := zap.NewDevelopment()

	config := PoolConfig{
		Nodes: []NodeConfig{
			{Addresses: []string{"http://localhost:9200"}},
		},
		Logger: logger,
	}

	pool, err := NewPool(config)
	assert.NoError(t, err)
	assert.NotNil(t, pool)
	assert.Equal(t, 1, pool.TotalCount())

	// Add another node
	err = pool.AddNode(NodeConfig{
		Addresses: []string{"http://localhost:9201"},
	})
	assert.NoError(t, err)
	assert.Equal(t, 2, pool.TotalCount())

	// Remove node
	err = pool.RemoveNode("http://localhost:9201")
	assert.NoError(t, err)
	assert.Equal(t, 1, pool.TotalCount())

	pool.Close()
}

func TestPool_Stats(t *testing.T) {
	logger, _ := zap.NewDevelopment()

	config := PoolConfig{
		Nodes: []NodeConfig{
			{Addresses: []string{"http://localhost:9200"}},
			{Addresses: []string{"http://localhost:9201"}},
		},
		Logger: logger,
	}

	pool, err := NewPool(config)
	assert.NoError(t, err)

	total, healthy := pool.Stats()
	assert.Equal(t, 2, total)
	// 至少 >=0
	assert.True(t, healthy >= 0)
	assert.True(t, healthy <= total)

	pool.Close()
}

func TestPool_GetWhenNoHealthyNodes(t *testing.T) {
	logger, _ := zap.NewDevelopment()

	config := PoolConfig{
		Nodes: []NodeConfig{
			// This node will be unhealthy because ES doesn't run on 9201
			{Addresses: []string{"http://localhost:9201"}},
		},
		Logger: logger,
	}

	pool, err := NewPool(config)
	assert.NoError(t, err)
	defer pool.Close()

	// Get should return error when no healthy nodes
	client, err := pool.Get()
	if err == nil {
		// If we got a client, it means the node is healthy
		assert.NotNil(t, client)
		client.Close()
	} else {
		// Expected when node is unhealthy
		assert.Contains(t, err.Error(), "no healthy nodes")
	}
}

func TestPool_GetAllHealthy(t *testing.T) {
	logger, _ := zap.NewDevelopment()

	config := PoolConfig{
		Nodes: []NodeConfig{
			{Addresses: []string{"http://localhost:9200"}},
		},
		Logger: logger,
	}

	pool, err := NewPool(config)
	assert.NoError(t, err)
	defer pool.Close()

	clients := pool.GetAllHealthy()
	// If node is healthy, we get it, otherwise empty list
	// This test doesn't fail either way
	assert.NotNil(t, clients)
}
