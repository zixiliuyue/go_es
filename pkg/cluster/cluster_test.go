// Package cluster 包的单元测试
// 覆盖集群健康、节点列表以及仓库/快照的入参校验
package cluster

import (
	"context"
	"fmt"
	"testing"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/stretchr/testify/assert"
	"github.com/zixiliuyue/go_es/pkg/client"
	"go.uber.org/zap"
)

func setupClusterTest(t *testing.T) (*Manager, context.Context) {
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

func TestManager_Health(t *testing.T) {
	m, ctx := setupClusterTest(t)
	res, err := m.Health(ctx, "cluster")
	assert.NoError(t, err)
	assert.NotNil(t, res)
	assert.NotEmpty(t, res.ClusterName)
	// status 只能是 green/yellow/red 之一
	switch res.Status {
	case HealthGreen, HealthYellow, HealthRed:
		// ok
	default:
		t.Fatalf("unexpected health status: %s", res.Status)
	}
}

func TestManager_ListNodes(t *testing.T) {
	m, ctx := setupClusterTest(t)
	nodes, err := m.ListNodes(ctx)
	assert.NoError(t, err)
	assert.NotEmpty(t, nodes)
}

func TestManager_PutRepository_Validation(t *testing.T) {
	m, ctx := setupClusterTest(t)
	// 名称为空
	err := m.PutRepository(ctx, "", SnapshotRepository{Type: "fs"})
	assert.Error(t, err)
	// 类型为空
	err = m.PutRepository(ctx, "repo", SnapshotRepository{Type: ""})
	assert.Error(t, err)
}

func TestManager_CreateSnapshot_Validation(t *testing.T) {
	m, ctx := setupClusterTest(t)
	// 名称缺失
	assert.Error(t, m.CreateSnapshot(ctx, "", "snap", nil, false, true))
	assert.Error(t, m.CreateSnapshot(ctx, "repo", "", nil, false, true))
}

func TestManager_RestoreSnapshot_Validation(t *testing.T) {
	m, ctx := setupClusterTest(t)
	assert.Error(t, m.RestoreSnapshot(ctx, "", "snap", nil, true))
	assert.Error(t, m.RestoreSnapshot(ctx, "repo", "", nil, true))
}

func TestManager_PutAndDeleteRepository_FS(t *testing.T) {
	// 仅当测试环境 path.repo 合法时才会成功;否则返回错误也可被忽略
	m, ctx := setupClusterTest(t)
	repoName := fmt.Sprintf("test_repo_%d", 1)
	defer func() { _ = m.DeleteRepository(ctx, repoName) }()

	err := m.PutRepository(ctx, repoName, SnapshotRepository{
		Type: "fs",
		Settings: map[string]interface{}{
			"location": "/tmp/test_es_snap",
		},
	})
	// 容器或单节点环境若没配置 path.repo 会出现错误,这里不强求成功
	if err != nil {
		t.Logf("PutRepository returned (可能是环境未配置 path.repo): %v", err)
		return
	}
	assert.NoError(t, m.DeleteRepository(ctx, repoName))
}
