// Package ilm 包的单元测试
// 测试ILM Policy的CRUD以及便捷构造方法
package ilm

import (
	"context"
	"fmt"
	"testing"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/stretchr/testify/assert"
	"github.com/zixiliuyue/go_es/pkg/client"
	"go.uber.org/zap"
)

// setupILMTest 准备测试所需的 Manager,无ES时自动Skip
func setupILMTest(t *testing.T) (*Manager, context.Context) {
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

func TestManager_PutAndGetAndDeletePolicy(t *testing.T) {
	m, ctx := setupILMTest(t)
	policyName := fmt.Sprintf("test_policy_%d", 1)
	defer func() { _ = m.DeletePolicy(ctx, policyName) }()

	policy := BuildTimedRolloverPolicy("1d", "7d", "30d")
	assert.NoError(t, m.PutPolicy(ctx, policyName, policy))

	got, err := m.GetPolicy(ctx, policyName)
	assert.NoError(t, err)
	assert.NotNil(t, got)
	assert.Contains(t, got.Phases, "hot")
	assert.Contains(t, got.Phases, "warm")
	assert.Contains(t, got.Phases, "delete")

	assert.NoError(t, m.DeletePolicy(ctx, policyName))
}

func TestManager_PutPolicy_EmptyName(t *testing.T) {
	m, ctx := setupILMTest(t)
	err := m.PutPolicy(ctx, "", BuildTimedRolloverPolicy("1d", "7d", "30d"))
	assert.Error(t, err)
}

func TestManager_PutPolicy_EmptyPhases(t *testing.T) {
	m, ctx := setupILMTest(t)
	err := m.PutPolicy(ctx, "empty", Policy{})
	assert.Error(t, err)
}

func TestBuildTimedRolloverPolicy(t *testing.T) {
	p := BuildTimedRolloverPolicy("1d", "7d", "30d")
	assert.Contains(t, p.Phases, "hot")
	assert.Contains(t, p.Phases, "warm")
	assert.Contains(t, p.Phases, "delete")
	assert.Equal(t, "0ms", p.Phases["hot"].MinAge)
	assert.Equal(t, "7d", p.Phases["warm"].MinAge)
	assert.Equal(t, "30d", p.Phases["delete"].MinAge)
}
