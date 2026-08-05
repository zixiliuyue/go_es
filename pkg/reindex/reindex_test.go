// Package reindex 包的单元测试
// 覆盖 Reindex 同步执行、任务轮询以及 buildBody 的纯逻辑分支
package reindex

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/stretchr/testify/assert"
	"github.com/zixiliuyue/go_es/pkg/client"
	"github.com/zixiliuyue/go_es/pkg/index"
	"go.uber.org/zap"
)

// setupReindexTest 准备两个测试索引,无ES时自动Skip
func setupReindexTest(t *testing.T) (*Manager, *index.Manager, context.Context, string, string) {
	t.Helper()
	logger, _ := zap.NewDevelopment()
	cfg := client.Config{Addresses: []string{"http://localhost:9200"}, Logger: logger}
	c, err := client.NewClient(cfg)
	if err != nil {
		t.Skipf("Skipping test because Elasticsearch is not available: %v", err)
	}
	im := index.NewManager(c.GetES(), logger)
	ctx := context.Background()

	src := "test_reindex_src"
	dst := "test_reindex_dst"
	_ = im.DeleteIndex(ctx, src)
	_ = im.DeleteIndex(ctx, dst)

	mapping := map[string]interface{}{
		"mappings": map[string]interface{}{
			"properties": map[string]interface{}{
				"title": map[string]string{"type": "text"},
			},
		},
	}
	assert.NoError(t, im.CreateIndex(ctx, src, mapping))
	assert.NoError(t, im.CreateIndex(ctx, dst, mapping))
	return NewManager(c.GetES(), logger), im, ctx, src, dst
}

func TestNewManager(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	es, _ := elasticsearch.NewClient(elasticsearch.Config{Addresses: []string{"http://localhost:9200"}})
	m := NewManager(es, logger)
	assert.NotNil(t, m)
}

func TestManager_Reindex_Sync(t *testing.T) {
	m, im, ctx, src, dst := setupReindexTest(t)
	defer func() {
		_ = im.DeleteIndex(ctx, src)
		_ = im.DeleteIndex(ctx, dst)
	}()

	// 通过 _bulk 写一些文档进源索引
	bulkBody := `{"index":{"_index":"` + src + `","_id":"1"}}
{"title":"hello"}
{"index":{"_index":"` + src + `","_id":"2"}}
{"title":"world"}
`
	_, err := m.es.Bulk(strings.NewReader(bulkBody), m.es.Bulk.WithContext(ctx), m.es.Bulk.WithRefresh("true"))
	if err != nil {
		t.Skipf("Skipping reindex test: %v", err)
	}

	resp, err := m.Reindex(ctx, Request{
		Source:           Source{Index: []string{src}},
		Dest:             Dest{Index: dst},
		WaitForCompletion: true,
		Refresh:          true,
	})
	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.GreaterOrEqual(t, resp.Total, int64(2))
}

func TestManager_BuildBody(t *testing.T) {
	m, _, _, _, _ := setupReindexTest(t)

	req := Request{
		Source: Source{
			Index: []string{"a", "b"},
			Query: map[string]interface{}{"match_all": map[string]interface{}{}},
			Sort:  []map[string]interface{}{{"ts": "asc"}},
			Slice: &SliceConfig{ID: 0, Max: 4},
		},
		Dest: Dest{
			Index:       "dest",
			OpType:      "create",
			VersionType: "external",
			Pipeline:    "my-pipe",
		},
		Script:  "ctx._source.title = ctx._source.title.toUpperCase()",
		Params:  map[string]interface{}{"lang": "painless"},
		Conflicts: "proceed",
		SlicesAuto: true,
	}
	body := m.buildBody(req)
	src := body["source"].(map[string]interface{})
	assert.Contains(t, src, "index")
	assert.Contains(t, src, "query")
	assert.Contains(t, src, "sort")
	assert.Contains(t, src, "slice")
	dst := body["dest"].(map[string]interface{})
	assert.Equal(t, "dest", dst["index"])
	assert.Equal(t, "create", dst["op_type"])
	assert.Equal(t, "external", dst["version_type"])
	assert.Equal(t, "my-pipe", dst["pipeline"])
	assert.Contains(t, body, "script")
	assert.Equal(t, "proceed", body["conflicts"])
	assert.Equal(t, "auto", body["slices"])
}

func TestManager_WaitForTask_Timeout(t *testing.T) {
	m, _, _, _, _ := setupReindexTest(t)
	// 传入不存在的任务ID,GetTask 会返回 404,我们期望 WaitForTask 返回错误
	_, err := m.WaitForTask(context.Background(), "abc:1", 50*time.Millisecond, 200*time.Millisecond)
	assert.Error(t, err)
}
