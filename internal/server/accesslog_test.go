// Package server - 访问日志 单元测试
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/zixiliuyue/go_es/internal/search"
	"github.com/zixiliuyue/go_es/internal/storage"
)

// TestAccessLogger_Basic 测试基本写入
func TestAccessLogger_Basic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access.log")
	al := NewAccessLogger(AccessLogConfig{
		Enabled:    true,
		FilePath:   path,
		BufferSize: 100,
		SampleRate: 1.0,
	}, nil)
	defer al.Close()

	// 写 3 条
	for i := 0; i < 3; i++ {
		al.Log(AccessLogEntry{
			Timestamp:  time.Now().Format(time.RFC3339),
			Level:      "info",
			RequestID:  "rid-1",
			ClientIP:   "127.0.0.1",
			Method:     "GET",
			Path:       "/_cluster/health",
			Status:     200,
			DurationMs: 5,
		})
	}
	// 等异步写完成
	time.Sleep(100 * time.Millisecond)
	stats := al.Stats()
	assert.Equal(t, int64(3), stats.Written)

	// 验证文件内容
	content, err := os.ReadFile(path)
	assert.NoError(t, err)
	lines := bytes.Split(bytes.TrimRight(content, "\n"), []byte("\n"))
	assert.Equal(t, 3, len(lines))
	// 验证是 JSON
	for _, l := range lines {
		var entry map[string]interface{}
		err := json.Unmarshal(l, &entry)
		assert.NoError(t, err, "line should be valid JSON: %s", l)
	}
}

// TestAccessLogger_SampleRate 采样
func TestAccessLogger_SampleRate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access.log")
	al := NewAccessLogger(AccessLogConfig{
		Enabled:    true,
		FilePath:   path,
		BufferSize: 10000,
		SampleRate: 0.1, // 10% 采样
	}, nil)
	defer al.Close()

	// 写 1000 条
	for i := 0; i < 1000; i++ {
		al.Log(AccessLogEntry{
			Timestamp: time.Now().Format(time.RFC3339),
			Method:    "GET", Path: "/test", Status: 200,
		})
	}
	time.Sleep(200 * time.Millisecond)
	stats := al.Stats()
	// 应该大约 100 条左右, 不应少于 50
	assert.Greater(t, stats.Written, int64(50))
	assert.Less(t, stats.Written, int64(200))
}

// TestAccessLogger_DropOnFull 满时丢弃
func TestAccessLogger_DropOnFull(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access.log")
	al := NewAccessLogger(AccessLogConfig{
		Enabled:    true,
		FilePath:   path,
		BufferSize: 2, // 小 buffer
		SampleRate: 1.0,
	}, nil)
	// 不 Close, 直接看 Stats

	// 写 100 条, 同步发, 但 chan 容量只 2
	for i := 0; i < 100; i++ {
		al.Log(AccessLogEntry{
			Timestamp: time.Now().Format(time.RFC3339),
			Method:    "GET", Path: "/x", Status: 200,
		})
	}
	stats := al.Stats()
	assert.Greater(t, stats.Dropped, int64(0), "should drop some")
}

// TestAccessLogger_Disabled 关闭时不写
func TestAccessLogger_Disabled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access.log")
	al := NewAccessLogger(AccessLogConfig{
		Enabled:    false,
		FilePath:   path,
		BufferSize: 100,
	}, nil)
	defer al.Close()
	al.Log(AccessLogEntry{Method: "GET", Path: "/x", Status: 200})
	time.Sleep(50 * time.Millisecond)
	stats := al.Stats()
	assert.Equal(t, int64(0), stats.Written)
}

// TestAccessLogger_Stdout 写 stdout (FilePath 留空)
func TestAccessLogger_Stdout(t *testing.T) {
	al := NewAccessLogger(AccessLogConfig{
		Enabled:    true,
		BufferSize: 100,
		SampleRate: 1.0,
	}, nil)
	defer al.Close()
	al.Log(AccessLogEntry{
		Timestamp: time.Now().Format(time.RFC3339),
		Method:    "POST", Path: "/test", Status: 201,
	})
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int64(1), al.Stats().Written)
}

// TestAccessLogEntry_JSON 序列化格式
func TestAccessLogEntry_JSON(t *testing.T) {
	entry := AccessLogEntry{
		Timestamp:  "2024-01-01T00:00:00Z",
		Level:      "info",
		RequestID:  "req-123",
		ClientIP:   "10.0.0.1",
		Method:     "POST",
		Path:       "/test/_doc/1",
		Query:      "refresh=true",
		Route:      "/{index}/_doc/{id}",
		Status:     201,
		DurationMs: 42,
		BodySize:   100,
		UserAgent:  "go-es-client/1.0",
		Username:   "alice",
	}
	b, err := json.Marshal(entry)
	assert.NoError(t, err)
	// 必须有所有字段
	js := string(b)
	for _, f := range []string{`"@timestamp"`, `"request_id"`, `"client_ip"`, `"method"`, `"path"`, `"status"`, `"duration_ms"`, `"username"`} {
		assert.Contains(t, js, f)
	}
}

// TestAccessLogMiddleware 端到端: 通过中间件产生日志
func TestAccessLogMiddleware(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access.log")
	al := NewAccessLogger(AccessLogConfig{
		Enabled:    true,
		FilePath:   path,
		BufferSize: 100,
		SampleRate: 1.0,
	}, nil)
	defer al.Close()

	// 构造最小 server
	store, err := storage.Open("")
	assert.NoError(t, err)
	defer store.Close()
	realEng := search.New(store)
	s := &Server{
		store: store, engine: realEng,
		rbac:      newRBAC(),
		accessLog: al,
	}

	// 直接用 accessLog 中间件包一个简单 handler
	h := s.middlewareAccessLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]interface{}{"ok": true})
	}))

	// 发请求
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/_test/path", nil)
	h.ServeHTTP(rr, req)
	assert.Equal(t, 200, rr.Code)

	// 等异步写
	time.Sleep(100 * time.Millisecond)
	stats := al.Stats()
	assert.Equal(t, int64(1), stats.Written)
	content, err := os.ReadFile(path)
	assert.NoError(t, err)
	js := string(content)
	assert.Contains(t, js, `"path":"/_test/path"`)
	assert.Contains(t, js, `"status":200`)
}

// TestHandleAccessLogStats 端到端 stats 端点
func TestHandleAccessLogStats(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access.log")
	al := NewAccessLogger(AccessLogConfig{Enabled: true, FilePath: path, BufferSize: 100}, nil)
	defer al.Close()
	for i := 0; i < 5; i++ {
		al.Log(AccessLogEntry{Method: "GET", Path: "/x", Status: 200})
	}
	time.Sleep(50 * time.Millisecond)

	s := &Server{accessLog: al}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/_accesslog/stats", nil)
	s.handleAccessLogStats(rr, req)
	assert.Equal(t, 200, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, `"written"`)
	assert.Contains(t, body, `"enabled":true`)
}

// TestStatusRecorder 状态记录器
func TestStatusRecorder(t *testing.T) {
	w := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: w, status: 200}
	sr.WriteHeader(404)
	assert.Equal(t, 404, sr.status)
	_, _ = sr.Write([]byte("hello"))
	assert.Equal(t, int64(5), sr.bytes)
}
