// Package template 包的单元测试
// 测试 Composable Index Template / Component Template / Simulate
package template

import (
	"context"
	"fmt"
	"testing"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/stretchr/testify/assert"
	"github.com/zixiliuyue/go_es/pkg/client"
	"go.uber.org/zap"
)

func setupTemplateTest(t *testing.T) (*Manager, context.Context) {
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

func TestManager_IndexTemplate_Lifecycle(t *testing.T) {
	m, ctx := setupTemplateTest(t)
	name := fmt.Sprintf("test_idx_tpl_%d", 1)
	defer func() { _ = m.DeleteIndexTemplate(ctx, name) }()

	tpl := IndexTemplate{
		IndexPatterns: []string{"logs-*-test"},
		Priority:     100,
		Template: map[string]interface{}{
			"settings": map[string]interface{}{"number_of_shards": 1},
			"mappings": map[string]interface{}{
				"properties": map[string]interface{}{
					"msg": map[string]string{"type": "text"},
				},
			},
		},
	}

	// 校验
	assert.Error(t, m.PutIndexTemplate(ctx, "", tpl))
	tpl.IndexPatterns = nil
	assert.Error(t, m.PutIndexTemplate(ctx, name, tpl))
	tpl.IndexPatterns = []string{"logs-*-test"}

	// 创建
	assert.NoError(t, m.PutIndexTemplate(ctx, name, tpl))

	// 存在
	exists, err := m.IndexTemplateExists(ctx, name)
	assert.NoError(t, err)
	assert.True(t, exists)

	// 获取
	got, err := m.GetIndexTemplate(ctx, name)
	assert.NoError(t, err)
	assert.NotNil(t, got)
	assert.Equal(t, []string{"logs-*-test"}, got.IndexPatterns)

	// 删除
	assert.NoError(t, m.DeleteIndexTemplate(ctx, name))
}

func TestManager_ComponentTemplate_Lifecycle(t *testing.T) {
	m, ctx := setupTemplateTest(t)
	name := fmt.Sprintf("test_comp_tpl_%d", 1)
	defer func() { _ = m.DeleteComponentTemplate(ctx, name) }()

	tpl := ComponentTemplate{
		Template: map[string]interface{}{
			"settings": map[string]interface{}{"number_of_shards": 1},
		},
		Version: 1,
	}

	assert.Error(t, m.PutComponentTemplate(ctx, "", tpl))
	assert.NoError(t, m.PutComponentTemplate(ctx, name, tpl))
	assert.NoError(t, m.DeleteComponentTemplate(ctx, name))
}

func TestManager_Simulate(t *testing.T) {
	m, ctx := setupTemplateTest(t)
	name := fmt.Sprintf("test_idx_tpl_sim_%d", 1)
	defer func() { _ = m.DeleteIndexTemplate(ctx, name) }()

	tpl := IndexTemplate{
		IndexPatterns: []string{"sim-*-test"},
		Priority:     100,
		Template: map[string]interface{}{
			"mappings": map[string]interface{}{
				"properties": map[string]interface{}{
					"msg": map[string]string{"type": "text"},
				},
			},
		},
	}
	assert.NoError(t, m.PutIndexTemplate(ctx, name, tpl))

	out, err := m.Simulate(ctx, "sim-2024-test", nil)
	assert.NoError(t, err)
	assert.NotNil(t, out)
}

func TestManager_Simulate_EmptyName(t *testing.T) {
	m, ctx := setupTemplateTest(t)
	_, err := m.Simulate(ctx, "", nil)
	assert.Error(t, err)
}
