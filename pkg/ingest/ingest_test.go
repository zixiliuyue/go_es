// Package ingest 包的单元测试
// 覆盖 Pipeline 的 CRUD、Simulate 模拟执行与构造校验
package ingest

import (
	"context"
	"fmt"
	"testing"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/stretchr/testify/assert"
	"github.com/zixiliuyue/go_es/pkg/client"
	"go.uber.org/zap"
)

func setupIngestTest(t *testing.T) (*Manager, context.Context) {
	t.Helper()
	logger, _ := zap.NewDevelopment()
	cfg := client.Config{Addresses: []string{"http://localhost:9200"}, Logger: logger}
	c, err := client.NewClient(cfg)
	if err != nil {
		t.Skipf("Skipping test because Elasticsearch is not available: %v", err)
	}
	return NewManager(c.GetES(), logger), context.Background()
}

func TestNewManager(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	es, _ := elasticsearch.NewClient(elasticsearch.Config{Addresses: []string{"http://localhost:9200"}})
	m := NewManager(es, logger)
	assert.NotNil(t, m)
}

func TestManager_PutAndGetAndDeletePipeline(t *testing.T) {
	m, ctx := setupIngestTest(t)
	name := fmt.Sprintf("test_pipe_%d", 1)
	defer func() { _ = m.DeletePipeline(ctx, name) }()

	// 校验
	assert.Error(t, m.PutPipeline(ctx, "", Pipeline{Processors: []map[string]interface{}{{"set": map[string]interface{}{"field": "x", "value": 1}}}}))
	assert.Error(t, m.PutPipeline(ctx, name, Pipeline{}))

	// 真实创建
	p := Pipeline{
		Description: "test pipeline",
		Processors: []map[string]interface{}{
			{"set": map[string]interface{}{"field": "tag", "value": "demo"}},
		},
	}
	assert.NoError(t, m.PutPipeline(ctx, name, p))

	got, err := m.GetPipeline(ctx, name)
	assert.NoError(t, err)
	assert.NotNil(t, got)
	assert.Equal(t, "test pipeline", got.Description)
	assert.Len(t, got.Processors, 1)

	assert.NoError(t, m.DeletePipeline(ctx, name))
}

func TestManager_Simulate(t *testing.T) {
	m, ctx := setupIngestTest(t)
	p := Pipeline{
		Processors: []map[string]interface{}{
			{"set": map[string]interface{}{"field": "hello", "value": "world"}},
		},
	}
	docs := []map[string]interface{}{
		{"_source": map[string]interface{}{"x": 1}},
	}
	res, err := m.Simulate(ctx, p, docs)
	assert.NoError(t, err)
	assert.NotNil(t, res)
	assert.Len(t, res.Docs, 1)
	if len(res.Docs) > 0 {
		assert.Equal(t, "world", res.Docs[0].Doc["_source"].(map[string]interface{})["hello"])
	}
}

func TestManager_IndexWithPipeline_EmptyPipeline(t *testing.T) {
	m, _ := setupIngestTest(t)
	err := m.IndexWithPipeline(context.Background(), "any", "1", "", map[string]interface{}{})
	assert.Error(t, err)
}
