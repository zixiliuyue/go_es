// Package index 包的单元测试
// 测试索引管理功能
package index

import (
	"context"
	"testing"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/stretchr/testify/assert"
	"github.com/zixiliuyue/go_es/pkg/client"
	"github.com/zixiliuyue/go_es/pkg/errors"
	"go.uber.org/zap"
)

func setupTest(t *testing.T) (*elasticsearch.Client, context.Context) {
	logger, _ := zap.NewDevelopment()

	cfg := client.Config{
		Addresses: []string{"http://localhost:9200"},
		Logger:    logger,
	}

	c, err := client.NewClient(cfg)
	if err != nil {
		t.Logf("Cannot connect to Elasticsearch: %v", err)
		t.Skip("Skipping test because Elasticsearch is not available")
	}

	return c.GetES(), context.Background()
}

func TestNewManager(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	es, _ := elasticsearch.NewClient(elasticsearch.Config{
		Addresses: []string{"http://localhost:9200"},
	})

	manager := NewManager(es, logger)
	assert.NotNil(t, manager)
}

func TestManager_CreateIndex(t *testing.T) {
	es, ctx := setupTest(t)
	logger, _ := zap.NewDevelopment()
	manager := NewManager(es, logger)

	// 先删除可能存在的测试索引
	_ = manager.DeleteIndex(ctx, "test_index")

	// 创建索引
	mapping := map[string]interface{}{
		"mappings": map[string]interface{}{
			"properties": map[string]interface{}{
				"title": map[string]string{"type": "text"},
				"age":   map[string]string{"type": "integer"},
			},
		},
	}

	err := manager.CreateIndex(ctx, "test_index", mapping)
	assert.NoError(t, err)

	// 检查是否存在
	exists, err := manager.IndexExists(ctx, "test_index")
	assert.NoError(t, err)
	assert.True(t, exists)

	// 清理
	err = manager.DeleteIndex(ctx, "test_index")
	assert.NoError(t, err)
}

func TestManager_CreateIndex_AlreadyExists(t *testing.T) {
	es, ctx := setupTest(t)
	logger, _ := zap.NewDevelopment()
	manager := NewManager(es, logger)

	_ = manager.DeleteIndex(ctx, "test_index")

	// 创建第一个
	mapping := map[string]interface{}{
		"mappings": map[string]interface{}{
			"properties": map[string]interface{}{
				"title": map[string]string{"type": "text"},
			},
		},
	}
	err := manager.CreateIndex(ctx, "test_index", mapping)
	assert.NoError(t, err)

	// 尝试再次创建同一个索引，应该返回错误
	err = manager.CreateIndex(ctx, "test_index", mapping)
	assert.Error(t, err)
	assert.True(t, errors.Is(err, errors.ErrCodeIndexExists))

	// 清理
	_ = manager.DeleteIndex(ctx, "test_index")
}

func TestManager_DeleteIndex_NotFound(t *testing.T) {
	es, ctx := setupTest(t)
	logger, _ := zap.NewDevelopment()
	manager := NewManager(es, logger)

	// 删除不存在的索引应该返回错误
	err := manager.DeleteIndex(ctx, "non_existent_index")
	assert.Error(t, err)
	assert.True(t, errors.Is(err, errors.ErrCodeIndexNotFound))
}

func TestManager_IndexExists(t *testing.T) {
	es, ctx := setupTest(t)
	logger, _ := zap.NewDevelopment()
	manager := NewManager(es, logger)

	_ = manager.DeleteIndex(ctx, "test_exists")

	// 检查不存在的索引
	exists, err := manager.IndexExists(ctx, "test_exists")
	assert.NoError(t, err)
	assert.False(t, exists)

	// 创建索引
	mapping := map[string]interface{}{
		"mappings": map[string]interface{}{
			"properties": map[string]interface{}{
				"title": map[string]string{"type": "text"},
			},
		},
	}
	err = manager.CreateIndex(ctx, "test_exists", mapping)
	assert.NoError(t, err)

	// 检查存在
	exists, err = manager.IndexExists(ctx, "test_exists")
	assert.NoError(t, err)
	assert.True(t, exists)

	// 删除
	_ = manager.DeleteIndex(ctx, "test_exists")
}

func TestManager_GetMapping(t *testing.T) {
	es, ctx := setupTest(t)
	logger, _ := zap.NewDevelopment()
	manager := NewManager(es, logger)

	_ = manager.DeleteIndex(ctx, "test_mapping")

	mapping := map[string]interface{}{
		"mappings": map[string]interface{}{
			"properties": map[string]interface{}{
				"title":   map[string]string{"type": "text"},
				"content": map[string]string{"type": "text"},
				"count":   map[string]string{"type": "integer"},
			},
		},
	}
	err := manager.CreateIndex(ctx, "test_mapping", mapping)
	assert.NoError(t, err)

	result, err := manager.GetMapping(ctx, "test_mapping")
	assert.NoError(t, err)
	assert.NotNil(t, result)
	// 返回结果中应该包含索引名称
	_, ok := result["test_mapping"]
	assert.True(t, ok)

	// 清理
	_ = manager.DeleteIndex(ctx, "test_mapping")
}

func TestManager_GetMapping_NotFound(t *testing.T) {
	es, ctx := setupTest(t)
	logger, _ := zap.NewDevelopment()
	manager := NewManager(es, logger)

	result, err := manager.GetMapping(ctx, "non_existent")
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.True(t, errors.Is(err, errors.ErrCodeIndexNotFound))
}
