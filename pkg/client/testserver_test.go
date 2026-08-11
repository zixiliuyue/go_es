// Package client testserver.go 的单元测试
//
// 覆盖 NewTestServer / NewTestServerWithOptions / NewClientForTest / TestServer 生命周期
// 覆盖率目标 ≥ 80%
//
// 注意: 不使用 t.Parallel(), 因为 internal/search.SetSourceLookup 和
// internal/server.SetGlobalTracerProvider 写全局变量, 并行构造多个 TestServer
// 会触发 -race 检测(这是 internal 包既有设计, 不是 fixture 引入的)
package client

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestNewTestServer_StartsAndCleansUp: 基本生命周期
func TestNewTestServer_StartsAndCleansUp(t *testing.T) {
	ts := NewTestServer(t)
	require.NotNil(t, ts)
	require.NotNil(t, ts.HTTP)
	require.NotNil(t, ts.Server)
	require.NotNil(t, ts.Store)
	require.NotNil(t, ts.Engine)

	// URL 非空且以 http 开头
	url := ts.URL()
	assert.NotEmpty(t, url)
	assert.True(t, strings.HasPrefix(url, "http://"), "URL should start with http://, got %s", url)

	// Addr 非空
	addr := ts.Addr()
	assert.NotEmpty(t, addr)

	// HTTP 可达: GET / 应 200
	resp, err := http.Get(url + "/")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// /_cluster/health 可达
	resp2, err := http.Get(url + "/_cluster/health")
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)

	// /_health/readiness 应 200(startup done)
	resp3, err := http.Get(url + "/_health/readiness")
	require.NoError(t, err)
	defer resp3.Body.Close()
	assert.Equal(t, http.StatusOK, resp3.StatusCode)

	// t.Cleanup 会自动 Close, 不需要手动调
}

// TestNewTestServerWithOptions_WithLogger: 自定义 logger
func TestNewTestServerWithOptions_WithLogger(t *testing.T) {
	logger := zap.NewNop()
	ts := NewTestServerWithOptions(t, TestServerOptions{
		Logger: logger,
	})
	require.NotNil(t, ts)
	// 验证服务端可用
	resp, err := http.Get(ts.URL() + "/")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// TestNewTestServer_MultipleInstances: 多实例串行构造(端口隔离)
func TestNewTestServer_MultipleInstances(t *testing.T) {
	ts1 := NewTestServer(t)
	ts2 := NewTestServer(t)

	require.NotEqual(t, ts1.URL(), ts2.URL(), "two test servers should have different URLs")

	// 两个都可用
	for _, ts := range []*TestServer{ts1, ts2} {
		resp, err := http.Get(ts.URL() + "/")
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		resp.Body.Close()
	}
}

// TestTestServer_Close_Idempotent: Close 幂等(多次调用不 panic)
func TestTestServer_Close_Idempotent(t *testing.T) {
	// 不用 t.Parallel(), 因为这里手动 Close
	ts := NewTestServer(t)
	require.NotNil(t, ts)

	// 手动关闭一次
	ts.Close()
	// 再次关闭不应 panic
	assert.NotPanics(t, func() {
		ts.Close()
	})
}

// TestTestServer_NilSafe: nil TestServer 的方法安全
func TestTestServer_NilSafe(t *testing.T) {
	var ts *TestServer
	assert.NotPanics(t, func() {
		assert.Equal(t, "", ts.URL())
		assert.Equal(t, "", ts.Addr())
		ts.Close()
	})
}

// TestNewClientForTest_ConnectsToTestServer: 通过 NewClientForTest 创建的客户端可真实通信
func TestNewClientForTest_ConnectsToTestServer(t *testing.T) {
	ts := NewTestServer(t)
	c, err := NewClientForTest(t, ts)
	require.NoError(t, err)
	require.NotNil(t, c)
	defer c.Close()

	// Ping 应成功
	ok, err := c.Ping()
	require.NoError(t, err)
	assert.True(t, ok)

	// ES Info() 应返回 200
	info, err := c.GetES().Info()
	require.NoError(t, err)
	defer info.Body.Close()
	assert.False(t, info.IsError())

	// 读取 body 确认包含 cluster_name
	body, err := io.ReadAll(info.Body)
	// info.Body 已经被 ReadAll 消费, 上面 defer Close 安全
	_ = err
	if len(body) > 0 {
		assert.Contains(t, string(body), "cluster_name")
	}
}

// TestNewTestServer_IndexDocAndSearch: 端到端 smoke — 创建索引、写文档、搜索
func TestNewTestServer_IndexDocAndSearch(t *testing.T) {
	ts := NewTestServer(t)
	c, err := NewClientForTest(t, ts)
	require.NoError(t, err)
	defer c.Close()
	es := c.GetES()

	// 创建索引
	idxName := "test_smoke_idx"
	createResp, err := es.Indices.Create(idxName)
	require.NoError(t, err)
	defer createResp.Body.Close()
	assert.False(t, createResp.IsError(), "create index should succeed: %s", createResp.String())

	// 写一条文档
	docResp, err := es.Index(idxName, strings.NewReader(`{"title":"hello world","user":"alice"}`), es.Index.WithDocumentID("1"))
	require.NoError(t, err)
	defer docResp.Body.Close()
	assert.False(t, docResp.IsError(), "index doc should succeed: %s", docResp.String())

	// 搜索
	searchResp, err := es.Search(es.Search.WithIndex(idxName), es.Search.WithBody(strings.NewReader(`{"query":{"match_all":{}}}`)))
	require.NoError(t, err)
	defer searchResp.Body.Close()
	assert.False(t, searchResp.IsError(), "search should succeed: %s", searchResp.String())

	body, _ := io.ReadAll(searchResp.Body)
	assert.Contains(t, string(body), "hello world")
}

// TestNewTestServer_DeleteDoc: 端到端 smoke — 删除文档
func TestNewTestServer_DeleteDoc(t *testing.T) {
	ts := NewTestServer(t)
	c, err := NewClientForTest(t, ts)
	require.NoError(t, err)
	defer c.Close()
	es := c.GetES()

	idxName := "test_delete_idx"
	_, _ = es.Indices.Create(idxName)

	// 写文档
	_, err = es.Index(idxName, strings.NewReader(`{"name":"to-delete"}`), es.Index.WithDocumentID("42"))
	require.NoError(t, err)

	// 删除文档
	delResp, err := es.Delete(idxName, "42")
	require.NoError(t, err)
	defer delResp.Body.Close()
	assert.False(t, delResp.IsError(), "delete should succeed: %s", delResp.String())
}

// TestNewTestServer_ServerShutdownChangesReadiness: 调 Server.Shutdown 后 readiness 503
func TestNewTestServer_ServerShutdownChangesReadiness(t *testing.T) {
	// 不用 t.Parallel, 因为要手动 Shutdown
	ts := NewTestServer(t)

	// 初始 readiness 200
	resp, err := http.Get(ts.URL() + "/_health/readiness")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Shutdown 业务层
	ts.Server.Shutdown(context.Background())

	// readiness 应 503
	resp2, err := http.Get(ts.URL() + "/_health/readiness")
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp2.StatusCode)
}

// TestNewTestServer_BulkEndpoint: 端到端 smoke — _bulk 批量写入
func TestNewTestServer_BulkEndpoint(t *testing.T) {
	ts := NewTestServer(t)
	c, err := NewClientForTest(t, ts)
	require.NoError(t, err)
	defer c.Close()
	es := c.GetES()

	bulkBody := `{"index":{"_index":"bulk_test","_id":"1"}}
{"field":"value1"}
{"index":{"_index":"bulk_test","_id":"2"}}
{"field":"value2"}
`
	bulkResp, err := es.Bulk(strings.NewReader(bulkBody))
	require.NoError(t, err)
	defer bulkResp.Body.Close()
	assert.False(t, bulkResp.IsError(), "bulk should succeed: %s", bulkResp.String())
}

// TestNewTestServer_ClusterHealth: 端到端 smoke — /_cluster/health 返回正确 JSON
func TestNewTestServer_ClusterHealth(t *testing.T) {
	ts := NewTestServer(t)
	resp, err := http.Get(ts.URL() + "/_cluster/health")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "cluster_name")
	assert.Contains(t, string(body), "status")
}

// TestNewTestServer_AccessLogEndpoint: 端到端 smoke — /_health/liveness 200
func TestNewTestServer_LivenessEndpoint(t *testing.T) {
	ts := NewTestServer(t)
	resp, err := http.Get(ts.URL() + "/_health/liveness")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// TestNewTestServer_MetricsEndpoint: 端到端 smoke — /metrics 返回 200
func TestNewTestServer_MetricsEndpoint(t *testing.T) {
	ts := NewTestServer(t)
	resp, err := http.Get(ts.URL() + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	// Prometheus 格式应包含 go_es_ 前缀的指标
	assert.Contains(t, string(body), "go_es_")
}
