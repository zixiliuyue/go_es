// Package alias 包的单元测试
// 测试索引别名的增删改查与原子切换功能,需要真实ES环境,无ES时自动Skip
package alias

import (
	"context"
	"fmt"
	"testing"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/stretchr/testify/assert"
	"github.com/zixiliuyue/go_es/pkg/client"
	"github.com/zixiliuyue/go_es/pkg/index"
	"go.uber.org/zap"
)

// setupAliasTest 创建测试所需的Manager/Context以及两个空索引
// 不可用ES时自动 t.Skip
func setupAliasTest(t *testing.T) (*Manager, *index.Manager, context.Context, string, string) {
	t.Helper()
	logger, _ := zap.NewDevelopment()
	cfg := client.Config{Addresses: []string{"http://localhost:9200"}, Logger: logger}
	c, err := client.NewClient(cfg)
	if err != nil {
		t.Skipf("Skipping test because Elasticsearch is not available: %v", err)
	}

	im := index.NewManager(c.GetES(), logger)
	ctx := context.Background()

	idx1 := fmt.Sprintf("test_alias_idx_%d", 1)
	idx2 := fmt.Sprintf("test_alias_idx_%d", 2)
	_ = im.DeleteIndex(ctx, idx1)
	_ = im.DeleteIndex(ctx, idx2)

	mapping := map[string]interface{}{
		"mappings": map[string]interface{}{
			"properties": map[string]interface{}{
				"title": map[string]string{"type": "text"},
			},
		},
	}
	assert.NoError(t, im.CreateIndex(ctx, idx1, mapping))
	assert.NoError(t, im.CreateIndex(ctx, idx2, mapping))

	// 同步刷新,避免刚建好的索引在某些场景下不可见
	_ = im.Refresh(ctx, idx1)
	_ = im.Refresh(ctx, idx2)

	return NewManager(c.GetES(), logger), im, ctx, idx1, idx2
}

// cleanupAliasTest 清理测试产生的索引
func cleanupAliasTest(t *testing.T, im *index.Manager, ctx context.Context, indices ...string) {
	t.Helper()
	for _, idx := range indices {
		_ = im.DeleteIndex(ctx, idx)
	}
}

func TestNewManager(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	es, _ := elasticsearch.NewClient(elasticsearch.Config{Addresses: []string{"http://localhost:9200"}})
	m := NewManager(es, logger)
	assert.NotNil(t, m)
}

func TestManager_AddAndGetAlias(t *testing.T) {
	m, im, ctx, idx1, idx2 := setupAliasTest(t)
	aliasName := "test_alias_get"
	defer cleanupAliasTest(t, im, ctx, idx1, idx2)

	// 同时给两个索引加上同一个别名
	assert.NoError(t, m.AddAlias(ctx, idx1, aliasName))
	assert.NoError(t, m.AddAlias(ctx, idx2, aliasName))

	bound, err := m.GetAlias(ctx, aliasName)
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{idx1, idx2}, bound)
}

func TestManager_RemoveAlias(t *testing.T) {
	m, im, ctx, idx1, idx2 := setupAliasTest(t)
	aliasName := "test_alias_remove"
	defer cleanupAliasTest(t, im, ctx, idx1, idx2)

	assert.NoError(t, m.AddAlias(ctx, idx1, aliasName))
	assert.NoError(t, m.AddAlias(ctx, idx2, aliasName))

	// 只删一个
	assert.NoError(t, m.RemoveAlias(ctx, idx1, aliasName))
	bound, err := m.GetAlias(ctx, aliasName)
	assert.NoError(t, err)
	assert.Equal(t, []string{idx2}, bound)
}

func TestManager_SwitchAlias(t *testing.T) {
	m, im, ctx, idx1, idx2 := setupAliasTest(t)
	aliasName := "test_alias_switch"
	defer cleanupAliasTest(t, im, ctx, idx1, idx2)

	assert.NoError(t, m.AddAlias(ctx, idx1, aliasName))

	// 原子切换到 idx2
	assert.NoError(t, m.SwitchAlias(ctx, aliasName, idx1, idx2))

	bound, err := m.GetAlias(ctx, aliasName)
	assert.NoError(t, err)
	assert.Equal(t, []string{idx2}, bound)
}

func TestManager_UpdateAliases_Empty(t *testing.T) {
	m, _, _, _, _ := setupAliasTest(t)
	err := m.UpdateAliases(context.Background(), nil)
	assert.Error(t, err)
}

func TestManager_Exists(t *testing.T) {
	m, im, ctx, idx1, _ := setupAliasTest(t)
	defer cleanupAliasTest(t, im, ctx, idx1)

	aliasName := "test_alias_exists"
	// 不存在
	exists, err := m.Exists(ctx, aliasName)
	assert.NoError(t, err)
	assert.False(t, exists)

	// 添加后再查
	assert.NoError(t, m.AddAlias(ctx, idx1, aliasName))
	exists, err = m.Exists(ctx, aliasName)
	assert.NoError(t, err)
	assert.True(t, exists)
}

func TestManager_AddAction_WithFilterAndWriteIndex(t *testing.T) {
	// 纯构造测试,只验证 builder 行为
	a := AddAction("idx", "alias").
		WithFilter(map[string]interface{}{"term": map[string]interface{}{"tag": "v1"}}).
		WithWriteIndex(true)
	m := a.toMap()
	add, ok := m["add"].(map[string]interface{})
	assert.True(t, ok)
	assert.Equal(t, "idx", add["index"])
	assert.Equal(t, "alias", add["alias"])
	assert.Equal(t, true, add["is_write_index"])
	assert.NotNil(t, add["filter"])
}
