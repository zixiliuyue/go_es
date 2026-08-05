// internal/server 包的单元/集成测试
// 覆盖所有路由的端到端流程(在 httptest 下启动一个自研服务端进程)
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/zixiliuyue/go_es/internal/search"
	"github.com/zixiliuyue/go_es/internal/storage"
	"go.uber.org/zap"
)

// newTestServer 启动一个基于内存存储的自研服务端(无需外部 ES)
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	store, err := storage.Open("")
	assert.NoError(t, err)
	engine := search.New(store)
	srv := New(store, engine, zap.NewNop())
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = store.Close()
	})
	return ts
}

// do 是测试 HTTP 调用的辅助
func do(t *testing.T, ts *httptest.Server, method, path string, body interface{}) (*http.Response, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, ts.URL+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

func TestServer_RootAndHealth(t *testing.T) {
	ts := newTestServer(t)
	resp, body := do(t, ts, "GET", "/", nil)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, string(body), "go_es_cluster")

	resp, body = do(t, ts, "GET", "/_cluster/health", nil)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, string(body), "status")
}

func TestServer_IndexCreateAndDocCRUD(t *testing.T) {
	ts := newTestServer(t)

	// 创建索引
	resp, _ := do(t, ts, "PUT", "/articles", map[string]interface{}{
		"mappings": map[string]interface{}{"properties": map[string]interface{}{"title": map[string]string{"type": "text"}}},
	})
	assert.Equal(t, 200, resp.StatusCode)

	// 重复创建应 400
	resp, _ = do(t, ts, "PUT", "/articles", nil)
	assert.Equal(t, 400, resp.StatusCode)

	// HEAD /articles
	resp, _ = do(t, ts, "HEAD", "/articles", nil)
	assert.Equal(t, 200, resp.StatusCode)

	// 写文档
	resp, _ = do(t, ts, "PUT", "/articles/_doc/1", map[string]interface{}{"title": "hello go"})
	assert.Equal(t, 200, resp.StatusCode)

	// 读文档
	resp, body := do(t, ts, "GET", "/articles/_doc/1", nil)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, string(body), "hello go")

	// 删文档
	resp, _ = do(t, ts, "DELETE", "/articles/_doc/1", nil)
	assert.Equal(t, 200, resp.StatusCode)

	// 删完再 GET 应 404
	resp, _ = do(t, ts, "GET", "/articles/_doc/1", nil)
	assert.Equal(t, 404, resp.StatusCode)

	// 删索引
	resp, _ = do(t, ts, "DELETE", "/articles", nil)
	assert.Equal(t, 200, resp.StatusCode)
}

func TestServer_Search(t *testing.T) {
	ts := newTestServer(t)
	_ = doMust(t, ts, "PUT", "/articles", nil)
	_ = doMust(t, ts, "PUT", "/articles/_doc/1", map[string]interface{}{"title": "hello go", "tag": "go"})
	_ = doMust(t, ts, "PUT", "/articles/_doc/2", map[string]interface{}{"title": "hello py", "tag": "py"})
	_ = doMust(t, ts, "PUT", "/articles/_doc/3", map[string]interface{}{"title": "goodbye go", "tag": "go"})

	resp, body := do(t, ts, "POST", "/articles/_search", map[string]interface{}{
		"query": map[string]interface{}{"match": map[string]interface{}{"title": "hello"}},
	})
	assert.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, string(body), "hits")

	// 应当命中 1 和 2
	var parsed struct {
		Hits struct {
			Total struct {
				Value int `json:"value"`
			} `json:"total"`
		} `json:"hits"`
	}
	_ = json.Unmarshal(body, &parsed)
	assert.Equal(t, 2, parsed.Hits.Total.Value)
}

func TestServer_Alias(t *testing.T) {
	ts := newTestServer(t)
	_ = doMust(t, ts, "PUT", "/idx_v1", nil)
	_ = doMust(t, ts, "PUT", "/idx_v2", nil)

	// add alias idx_v1 -> a
	resp, _ := do(t, ts, "POST", "/_aliases", map[string]interface{}{
		"actions": []map[string]interface{}{
			{"add": map[string]interface{}{"index": "idx_v1", "alias": "a"}},
		},
	})
	assert.Equal(t, 200, resp.StatusCode)

	// get alias
	resp, body := do(t, ts, "GET", "/_alias/a", nil)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, string(body), "idx_v1")

	// switch alias to v2 atomically
	resp, _ = do(t, ts, "POST", "/_aliases", map[string]interface{}{
		"actions": []map[string]interface{}{
			{"add": map[string]interface{}{"index": "idx_v2", "alias": "a"}},
			{"remove": map[string]interface{}{"index": "idx_v1", "alias": "a"}},
		},
	})
	assert.Equal(t, 200, resp.StatusCode)

	resp, body = do(t, ts, "GET", "/_alias/a", nil)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, string(body), "idx_v2")
	assert.NotContains(t, string(body), "idx_v1")
}

func TestServer_TemplateAndILM(t *testing.T) {
	ts := newTestServer(t)

	// index template
	resp, _ := do(t, ts, "PUT", "/_index_template/demo", map[string]interface{}{
		"index_patterns": []string{"demo-*"},
		"template":       map[string]interface{}{"settings": map[string]interface{}{"number_of_shards": 1}},
	})
	assert.Equal(t, 200, resp.StatusCode)

	resp, body := do(t, ts, "GET", "/_index_template/demo", nil)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, string(body), "demo-*")

	resp, _ = do(t, ts, "DELETE", "/_index_template/demo", nil)
	assert.Equal(t, 200, resp.StatusCode)

	// component template
	resp, _ = do(t, ts, "PUT", "/_component_template/settings", map[string]interface{}{
		"template": map[string]interface{}{"settings": map[string]interface{}{"number_of_shards": 1}},
	})
	assert.Equal(t, 200, resp.StatusCode)
	resp, _ = do(t, ts, "DELETE", "/_component_template/settings", nil)
	assert.Equal(t, 200, resp.StatusCode)

	// ILM
	resp, _ = do(t, ts, "PUT", "/_ilm/policy/p1", map[string]interface{}{
		"policy": map[string]interface{}{"phases": map[string]interface{}{"hot": map[string]interface{}{}}},
	})
	assert.Equal(t, 200, resp.StatusCode)
	resp, body = do(t, ts, "GET", "/_ilm/policy/p1", nil)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, string(body), "hot")
	resp, _ = do(t, ts, "DELETE", "/_ilm/policy/p1", nil)
	assert.Equal(t, 200, resp.StatusCode)
}

func TestServer_Ingest(t *testing.T) {
	ts := newTestServer(t)
	_ = doMust(t, ts, "PUT", "/p1", map[string]interface{}{
		"description": "set field",
		"processors": []map[string]interface{}{
			{"set": map[string]interface{}{"field": "tag", "value": "v1"}},
		},
	})

	// simulate
	resp, body := do(t, ts, "POST", "/_ingest/pipeline/_simulate", map[string]interface{}{
		"pipeline": map[string]interface{}{
			"processors": []map[string]interface{}{
				{"set": map[string]interface{}{"field": "tag", "value": "v1"}},
			},
		},
		"docs": []map[string]interface{}{
			{"_source": map[string]interface{}{"x": 1}},
		},
	})
	assert.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, string(body), "v1")
}

func TestServer_Reindex(t *testing.T) {
	ts := newTestServer(t)
	_ = doMust(t, ts, "PUT", "/src", nil)
	_ = doMust(t, ts, "PUT", "/dst", nil)
	_ = doMust(t, ts, "PUT", "/src/_doc/1", map[string]interface{}{"x": 1})
	_ = doMust(t, ts, "PUT", "/src/_doc/2", map[string]interface{}{"x": 2})

	resp, body := do(t, ts, "POST", "/_reindex", map[string]interface{}{
		"source": map[string]interface{}{"index": []string{"src"}},
		"dest":   map[string]interface{}{"index": "dst"},
	})
	assert.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, string(body), "created")

	// dst 应该有两条
	resp, body = do(t, ts, "POST", "/dst/_search", map[string]interface{}{
		"query": map[string]interface{}{"match_all": map[string]interface{}{}},
	})
	assert.Equal(t, 200, resp.StatusCode)
	var parsed struct {
		Hits struct {
			Total struct {
				Value int `json:"value"`
			} `json:"total"`
		} `json:"hits"`
	}
	_ = json.Unmarshal(body, &parsed)
	assert.Equal(t, 2, parsed.Hits.Total.Value)
}

func TestServer_SnapshotLifecycle(t *testing.T) {
	ts := newTestServer(t)
	// put repo
	resp, _ := do(t, ts, "PUT", "/_snapshot/repo1", map[string]interface{}{
		"type": "fs",
		"settings": map[string]interface{}{"location": "/tmp"},
	})
	assert.Equal(t, 200, resp.StatusCode)

	// create snapshot
	resp, _ = do(t, ts, "PUT", "/_snapshot/repo1/snap1", nil)
	assert.Equal(t, 200, resp.StatusCode)

	// get snapshot
	resp, body := do(t, ts, "GET", "/_snapshot/repo1/snap1", nil)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, string(body), "SUCCESS")

	// delete snapshot
	resp, _ = do(t, ts, "DELETE", "/_snapshot/repo1/snap1", nil)
	assert.Equal(t, 200, resp.StatusCode)

	// delete repo
	resp, _ = do(t, ts, "DELETE", "/_snapshot/repo1", nil)
	assert.Equal(t, 200, resp.StatusCode)
}

// doMust 发送请求并要求 2xx
func doMust(t *testing.T, ts *httptest.Server, method, path string, body interface{}) []byte {
	t.Helper()
	resp, raw := do(t, ts, method, path, body)
	if resp.StatusCode >= 300 {
		t.Fatalf("%s %s failed: status=%d body=%s", method, path, resp.StatusCode, raw)
	}
	return raw
}

// 抑制 import 未使用警告
var _ = fmt.Sprint
