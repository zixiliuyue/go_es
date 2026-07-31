// Package client 包的单元测试
// 测试客户端连接管理功能
package client

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

// TestNewClient 测试创建新客户端
// 注意：这个测试需要ES服务可用才能通过，如果不需要测试连接可以注释掉实际连接部分
func TestNewClient(t *testing.T) {
	logger, _ := zap.NewDevelopment()

	cfg := Config{
		Addresses: []string{"http://localhost:9200"},
		Username:  "",
		Password:  "",
		Logger:    logger,
	}

	// 如果ES没有运行，这个测试会失败，但这是预期的
	// 在实际环境中，当ES可用时，这个测试应该通过
	client, err := NewClient(cfg)
	if err != nil {
		t.Logf("Warning: Cannot connect to Elasticsearch at %v: %v", cfg.Addresses, err)
		t.Skip("Skipping test because Elasticsearch is not available")
	}

	assert.NoError(t, err)
	assert.NotNil(t, client)
	assert.NotNil(t, client.GetES())

	// 测试Ping
	ok, err := client.Ping()
	assert.NoError(t, err)
	assert.True(t, ok)

	client.Close()
}

// TestNewClientConfig 测试配置验证
func TestNewClient_EmptyAddresses(t *testing.T) {
	logger, _ := zap.NewDevelopment()

	cfg := Config{
		Addresses: []string{},
		Logger:    logger,
	}

	// go-elasticsearch会使用默认地址http://localhost:9200
	// 所以即使空数组也不会报错，但连接会失败
	client, err := NewClient(cfg)
	if err == nil {
		client.Close()
	}
	// 这里不做断言，因为结果取决于是否有ES运行
}

func TestClient_GetES(t *testing.T) {
	logger, _ := zap.NewDevelopment()

	cfg := Config{
		Addresses: []string{"http://localhost:9200"},
		Logger:    logger,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Skip("Skipping test because Elasticsearch is not available")
	}
	defer client.Close()

	rawClient := client.GetES()
	assert.NotNil(t, rawClient)
}

func TestClient_Close(t *testing.T) {
	logger, _ := zap.NewDevelopment()

	cfg := Config{
		Addresses: []string{"http://localhost:9200"},
		Logger:    logger,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Skip("Skipping test because Elasticsearch is not available")
	}

	// Close方法不应该panic
	assert.NotPanics(t, func() {
		client.Close()
	})
}
