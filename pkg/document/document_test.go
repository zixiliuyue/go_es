// Package document 包的单元测试
// 测试文档CRUD操作功能
package document

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/stretchr/testify/assert"
	"github.com/zixiliuyue/go_es/pkg/client"
	"github.com/zixiliuyue/go_es/pkg/errors"
	"github.com/zixiliuyue/go_es/pkg/index"
	"go.uber.org/zap"
)

func setupDocumentTest(t *testing.T) (*Manager, context.Context, string) {
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

	// 创建测试索引
	indexManager := index.NewManager(c.GetES(), logger)
	indexName := "test_docs"
	_ = indexManager.DeleteIndex(context.Background(), indexName)

	mapping := map[string]interface{}{
		"mappings": map[string]interface{}{
			"properties": map[string]interface{}{
				"title":   map[string]string{"type": "text"},
				"content": map[string]string{"type": "text"},
				"count":   map[string]string{"type": "integer"},
			},
		},
	}
	err = indexManager.CreateIndex(context.Background(), indexName, mapping)
	assert.NoError(t, err)

	manager := NewManager(c.GetES(), logger)
	return manager, context.Background(), indexName
}

func cleanupDocumentTest(t *testing.T, manager *Manager, ctx context.Context, indexName, docID string) {
	// 不做清理，在setup中已经处理
}

func TestNewManager(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	es, _ := elasticsearch.NewClient(elasticsearch.Config{
		Addresses: []string{"http://localhost:9200"},
	})

	manager := NewManager(es, logger)
	assert.NotNil(t, manager)
}

func TestManager_IndexWithID(t *testing.T) {
	manager, ctx, indexName := setupDocumentTest(t)
	defer func() {
		// 清理
		_ = manager.Delete(ctx, indexName, "1")
	}()

	doc := map[string]interface{}{
		"title":   "Test Document",
		"content": "This is a test document",
		"count":   100,
	}

	resp, err := manager.IndexWithID(ctx, indexName, "1", doc)
	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, "1", resp.ID)
	assert.Equal(t, indexName, resp.Index)
}

func TestManager_Index(t *testing.T) {
	manager, ctx, indexName := setupDocumentTest(t)

	doc := map[string]interface{}{
		"title": "Auto ID Document",
		"count": 50,
	}

	resp, err := manager.Index(ctx, indexName, doc)
	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.NotEmpty(t, resp.ID) // ID should be auto-generated

	// 清理
	_ = manager.Delete(ctx, indexName, resp.ID)
}

func TestManager_Get(t *testing.T) {
	manager, ctx, indexName := setupDocumentTest(t)
	defer cleanupDocumentTest(t, manager, ctx, indexName, "2")

	doc := map[string]interface{}{
		"title":   "Get Test",
		"content": "Get me please",
		"count":   200,
	}

	_, err := manager.IndexWithID(ctx, indexName, "2", doc)
	assert.NoError(t, err)

	resp, err := manager.Get(ctx, indexName, "2")
	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.True(t, resp.Found)
	assert.Equal(t, "2", resp.ID)

	// 解析source看看是否正确
	var result map[string]interface{}
	err = json.Unmarshal(resp.Source, &result)
	assert.NoError(t, err)
	assert.Equal(t, "Get Test", result["title"])
	assert.Equal(t, float64(200), result["count"]) // JSON unmarshal 总是 float64
}

func TestManager_Get_NotFound(t *testing.T) {
	manager, ctx, indexName := setupDocumentTest(t)

	resp, err := manager.Get(ctx, indexName, "non_existent")
	assert.Error(t, err)
	assert.Nil(t, resp)
	assert.True(t, errors.Is(err, errors.ErrCodeDocumentNotFound))
}

func TestManager_GetInto(t *testing.T) {
	manager, ctx, indexName := setupDocumentTest(t)
	defer cleanupDocumentTest(t, manager, ctx, indexName, "3")

	type TestDoc struct {
		Title   string `json:"title"`
		Content string `json:"content"`
		Count   int    `json:"count"`
	}

	doc := TestDoc{
		Title:   "GetInto Test",
		Content: "Test GetInto",
		Count:   300,
	}

	_, err := manager.IndexWithID(ctx, indexName, "3", doc)
	assert.NoError(t, err)

	var result TestDoc
	err = manager.GetInto(ctx, indexName, "3", &result)
	assert.NoError(t, err)
	assert.Equal(t, doc.Title, result.Title)
	assert.Equal(t, doc.Count, result.Count)
}

func TestManager_Update(t *testing.T) {
	manager, ctx, indexName := setupDocumentTest(t)
	defer cleanupDocumentTest(t, manager, ctx, indexName, "4")

	doc := map[string]interface{}{
		"title": "Original Title",
		"count": 10,
	}

	_, err := manager.IndexWithID(ctx, indexName, "4", doc)
	assert.NoError(t, err)

	// 更新
	update := map[string]interface{}{
		"count": 20,
	}
	resp, err := manager.Update(ctx, indexName, "4", update)
	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, "updated", resp.Result)

	// 检查更新结果
	var result map[string]interface{}
	err = manager.GetInto(ctx, indexName, "4", &result)
	assert.NoError(t, err)
	assert.Equal(t, float64(20), result["count"])
	assert.Equal(t, "Original Title", result["title"]) // 原始字段保持不变
}

func TestManager_Delete(t *testing.T) {
	manager, ctx, indexName := setupDocumentTest(t)

	doc := map[string]interface{}{
		"title": "To be deleted",
	}

	_, err := manager.IndexWithID(ctx, indexName, "5", doc)
	assert.NoError(t, err)

	// 检查存在
	exists, err := manager.Exists(ctx, indexName, "5")
	assert.NoError(t, err)
	assert.True(t, exists)

	// 删除
	err = manager.Delete(ctx, indexName, "5")
	assert.NoError(t, err)

	// 检查已删除
	exists, err = manager.Exists(ctx, indexName, "5")
	assert.NoError(t, err)
	assert.False(t, exists)
}

func TestManager_Delete_NotFound(t *testing.T) {
	manager, ctx, indexName := setupDocumentTest(t)

	err := manager.Delete(ctx, indexName, "non_existent")
	assert.Error(t, err)
	assert.True(t, errors.Is(err, errors.ErrCodeDocumentNotFound))
}

func TestManager_Exists(t *testing.T) {
	manager, ctx, indexName := setupDocumentTest(t)
	defer cleanupDocumentTest(t, manager, ctx, indexName, "6")

	// 不存在
	exists, err := manager.Exists(ctx, indexName, "6")
	assert.NoError(t, err)
	assert.False(t, exists)

	// 创建
	doc := map[string]interface{}{"title": "Exists test"}
	_, err = manager.IndexWithID(ctx, indexName, "6", doc)
	assert.NoError(t, err)

	// 存在
	exists, err = manager.Exists(ctx, indexName, "6")
	assert.NoError(t, err)
	assert.True(t, exists)
}

func TestManager_Bulk(t *testing.T) {
	manager, ctx, indexName := setupDocumentTest(t)

	ops := []BulkOperation{
		{
			Operation: "index",
			Index:     indexName,
			ID:        "bulk_1",
			Doc: map[string]interface{}{
				"title": "Bulk Document 1",
				"count": 1,
			},
		},
		{
			Operation: "index",
			Index:     indexName,
			ID:        "bulk_2",
			Doc: map[string]interface{}{
				"title": "Bulk Document 2",
				"count": 2,
			},
		},
		{
			Operation: "index",
			Index:     indexName,
			ID:        "bulk_3",
			Doc: map[string]interface{}{
				"title": "Bulk Document 3",
				"count": 3,
			},
		},
	}

	success, failed, err := manager.Bulk(ctx, ops)
	assert.NoError(t, err)
	assert.Equal(t, 3, success)
	assert.Equal(t, 0, failed)

	// 验证文档都创建成功
	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("bulk_%d", i)
		exists, err := manager.Exists(ctx, indexName, id)
		assert.NoError(t, err)
		assert.True(t, exists)
	}

	// 清理
	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("bulk_%d", i)
		_ = manager.Delete(ctx, indexName, id)
	}
}
