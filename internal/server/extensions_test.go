// internal/server 扩展能力测试
// 覆盖新增的 metrics / health / tasks / auth / rate limit / body limit
package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/zixiliuyue/go_es/internal/search"
	"github.com/zixiliuyue/go_es/internal/storage"
	"go.uber.org/zap"
)

// newTestServerWith 启动带自定义配置的服务端
func newTestServerWith(t *testing.T, opts ServerOptions) *httptest.Server {
	t.Helper()
	store, err := storage.Open("")
	assert.NoError(t, err)
	engine := search.New(store)
	srv := NewWithOptions(store, engine, zap.NewNop(), opts)
	srv.MarkStartupDone()
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = store.Close()
	})
	return ts
}

func TestHealth_AllEndpoints(t *testing.T) {
	ts := newTestServer(t)

	for _, p := range []string{"/_health/liveness", "/_health/readiness", "/_health/startup"} {
		resp, body := do(t, ts, "GET", p, nil)
		assert.Equal(t, 200, resp.StatusCode, p)
		assert.Contains(t, string(body), "status", p)
	}
}

func TestHealth_ReadinessFailsAfterShutdown(t *testing.T) {
	store, err := storage.Open("")
	assert.NoError(t, err)
	defer func() { _ = store.Close() }()
	engine := search.New(store)
	srv := New(store, engine, zap.NewNop())
	srv.MarkStartupDone()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 标记关闭
	srv.Shutdown(context.Background())

	resp, _ := do(t, ts, "GET", "/_health/readiness", nil)
	assert.Equal(t, 503, resp.StatusCode)

	// liveness 仍可用
	resp, _ = do(t, ts, "GET", "/_health/liveness", nil)
	assert.Equal(t, 200, resp.StatusCode)
}

func TestMetrics_EndpointReturnsPrometheusFormat(t *testing.T) {
	ts := newTestServer(t)

	// 先发一些请求以产生指标
	_ = doMust(t, ts, "GET", "/", nil)
	_ = doMust(t, ts, "GET", "/_health/liveness", nil)
	_ = doMust(t, ts, "PUT", "/articles", map[string]interface{}{"mappings": map[string]interface{}{}})

	resp, body := do(t, ts, "GET", "/metrics", nil)
	assert.Equal(t, 200, resp.StatusCode)
	text := string(body)
	// 必含核心指标名
	assert.Contains(t, text, "go_es_http_requests_total")
	assert.Contains(t, text, "go_es_http_request_duration_seconds")
	assert.Contains(t, text, "go_es_start_time_seconds")
	// 启动 build_info 常量 1
	assert.Contains(t, text, "go_es_build_info")
}

func TestAuth_BasicAuth(t *testing.T) {
	ts := newTestServerWith(t, ServerOptions{
		Auth: AuthConfig{
			Enabled: true,
			Basic:   map[string]string{"alice": "secret"},
		},
	})
	// 先建一个索引(带 auth 头)
	authHdr := map[string]string{"Authorization": "Basic " + basic("alice", "secret")}
	_, _ = doWithHeaders(t, ts, "PUT", "/articles", map[string]interface{}{}, authHdr)

	// 未认证 -> 401
	resp, _ := do(t, ts, "GET", "/articles", nil)
	assert.Equal(t, 401, resp.StatusCode)
	assert.Equal(t, `Basic realm="go_es"`, resp.Header.Get("WWW-Authenticate"))

	// 错误密码 -> 401
	req, _ := http.NewRequest("GET", ts.URL+"/articles", nil)
	req.Header.Set("Authorization", "Basic "+basic("alice","wrong"))
	r, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	_ = r.Body.Close()
	assert.Equal(t, 401, r.StatusCode)

	// 正确密码 -> 200
	req, _ = http.NewRequest("GET", ts.URL+"/articles", nil)
	req.Header.Set("Authorization", "Basic "+basic("alice","secret"))
	r2, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	_ = r2.Body.Close()
	assert.Equal(t, 200, r2.StatusCode)

	// 健康端点不需鉴权
	resp, _ = do(t, ts, "GET", "/_health/liveness", nil)
	assert.Equal(t, 200, resp.StatusCode)

	// metrics 不需鉴权
	resp, _ = do(t, ts, "GET", "/metrics", nil)
	assert.Equal(t, 200, resp.StatusCode)
}

func TestAuth_ApiKey(t *testing.T) {
	ts := newTestServerWith(t, ServerOptions{
		Auth: AuthConfig{
			Enabled: true,
			APIKeys: []string{"topsecret"},
		},
	})
	authHdr := map[string]string{"Authorization": "ApiKey topsecret"}
	_, _ = doWithHeaders(t, ts, "PUT", "/idx", map[string]interface{}{}, authHdr)

	// 错误 key
	req, _ := http.NewRequest("GET", ts.URL+"/idx", nil)
	req.Header.Set("Authorization", "ApiKey wrong")
	r, _ := http.DefaultClient.Do(req)
	_ = r.Body.Close()
	assert.Equal(t, 401, r.StatusCode)

	// 正确 key
	req, _ = http.NewRequest("GET", ts.URL+"/idx", nil)
	req.Header.Set("Authorization", "ApiKey topsecret")
	r3, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	_ = r3.Body.Close()
	assert.Equal(t, 200, r3.StatusCode)
}

func TestRateLimit_PerIP(t *testing.T) {
	ts := newTestServerWith(t, ServerOptions{
		Limit: LimitConfig{RatePerSecond: 2, Burst: 2},
	})

	// 第一次与第二次通过(消耗 burst)
	ok, fail := 0, 0
	for i := 0; i < 6; i++ {
		resp, _ := do(t, ts, "GET", "/_health/liveness", nil)
		if resp.StatusCode == 200 {
			ok++
		} else {
			fail++
		}
	}
	// 至少应见到 1 个 429
	assert.Greater(t, fail, 0, "expected some 429s, got ok=%d fail=%d", ok, fail)
}

func TestBodyLimit_RejectsOversize(t *testing.T) {
	ts := newTestServerWith(t, ServerOptions{
		Limit: LimitConfig{MaxBodyBytes: 64}, // 64 B
	})

	// Content-Length 超过 64 -> 直接 413
	big := strings.Repeat("x", 200)
	req, _ := http.NewRequest("PUT", ts.URL+"/articles", strings.NewReader(big))
	req.ContentLength = int64(len(big))
	req.Header.Set("Content-Type", "application/json")
	r, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	_ = r.Body.Close()
	assert.Equal(t, 413, r.StatusCode)
}

func TestTasks_AsyncReindex(t *testing.T) {
	ts := newTestServer(t)
	_ = doMust(t, ts, "PUT", "/src", nil)
	_ = doMust(t, ts, "PUT", "/dst", nil)
	for i := 1; i <= 5; i++ {
		_ = doMust(t, ts, "PUT", "/src/_doc/"+itoaInt(int64(i)), map[string]interface{}{"n": i})
	}

	// 异步模式
	resp, body := do(t, ts, "POST", "/_reindex?wait_for_completion=false", map[string]interface{}{
		"source": map[string]interface{}{"index": []string{"src"}},
		"dest":   map[string]interface{}{"index": "dst"},
	})
	assert.Equal(t, 200, resp.StatusCode)
	var async struct {
		Task string `json:"task"`
	}
	assert.NoError(t, json.Unmarshal(body, &async))
	assert.NotEmpty(t, async.Task)

	// 轮询直到完成
	deadline := time.Now().Add(5 * time.Second)
	var final TaskInfo
	for time.Now().Before(deadline) {
		resp, body = do(t, ts, "GET", "/_tasks/"+async.Task, nil)
		assert.Equal(t, 200, resp.StatusCode)
		var got struct {
			Completed bool   `json:"completed"`
			Task      TaskInfo `json:"task"`
		}
		_ = json.Unmarshal(body, &got)
		if got.Completed {
			final = got.Task
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	assert.Equal(t, TaskStatusCompleted, final.Status)
	assert.EqualValues(t, 5, final.Progress.Created)

	// 列表中应能看到
	resp, body = do(t, ts, "GET", "/_tasks", nil)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, string(body), async.Task)

	// 取消一个不存在的任务 -> 404
	resp, _ = do(t, ts, "DELETE", "/_tasks/nonexistent", nil)
	assert.Equal(t, 404, resp.StatusCode)
}

func TestTasks_GetMissing404(t *testing.T) {
	ts := newTestServer(t)
	resp, _ := do(t, ts, "GET", "/_tasks/nope", nil)
	assert.Equal(t, 404, resp.StatusCode)
}

func TestRequestID_HeaderPropagated(t *testing.T) {
	ts := newTestServer(t)
	resp, _ := do(t, ts, "GET", "/_health/liveness", nil)
	assert.NotEmpty(t, resp.Header.Get("X-Request-Id"))

	// 自带 X-Request-Id 应被原样回传
	req, _ := http.NewRequest("GET", ts.URL+"/_health/liveness", nil)
	req.Header.Set("X-Request-Id", "rid-12345")
	r, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	defer func() { _ = r.Body.Close() }()
	assert.Equal(t, "rid-12345", r.Header.Get("X-Request-Id"))
}

// 工具: 生成 Basic Auth 头
func basic(u, p string) string {
	return base64.StdEncoding.EncodeToString([]byte(u + ":" + p))
}

// 工具: int64 -> string, 不引 strconv
func itoaInt(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// 工具: 立刻结束的 ctx
func noopCtx() context.Context { return context.Background() }

// 抑制 io/bytes 未用警告
var (
	_ = bytes.NewReader
	_ = io.ReadAll
)
