// 新增可观测/安全模块单元测试: 慢查询日志、审计日志、输入校验、pprof
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/zixiliuyue/go_es/internal/search"
	"github.com/zixiliuyue/go_es/internal/storage"
	"go.uber.org/zap"
)

// newObsTestServer 构造测试用 server (返回 *Server, 非 httptest)
func newObsTestServer(t *testing.T) *Server {
	t.Helper()
	store, err := storage.Open("")
	assert.NoError(t, err)
	engine := search.New(store)
	logger, _ := zap.NewDevelopment()
	s := New(store, engine, logger)
	s.MarkStartupDone()
	t.Cleanup(func() { _ = store.Close() })
	return s
}

// ---------- slowlog ----------

func TestSlowLog_SetGetThreshold(t *testing.T) {
	SetSlowThreshold(0)
	assert.EqualValues(t, 500, GetSlowThreshold(), "0 -> 默认 500ms")

	SetSlowThreshold(123)
	assert.EqualValues(t, 123, GetSlowThreshold())

	SetSlowThreshold(-1)
	assert.EqualValues(t, 500, GetSlowThreshold(), "负数 -> 默认 500ms")
}

func TestSlowLog_RecordAndStats(t *testing.T) {
	ResetSlowStats()
	RecordSlowRequest(1500)
	RecordSlowRequest(2500)
	RecordSlowRequest(300) // 低于默认 500ms, 但仍被记录(RecordSlowRequest 不判断阈值)

	stats := SlowStats()
	assert.EqualValues(t, 3, stats.SlowCount)
	assert.EqualValues(t, 2500, stats.MaxDuration)
	assert.NotEmpty(t, stats.LastSlowTime)
	assert.True(t, stats.ThresholdMs > 0)
}

func TestSlowLog_Reset(t *testing.T) {
	RecordSlowRequest(1000)
	ResetSlowStats()
	stats := SlowStats()
	assert.EqualValues(t, 0, stats.SlowCount)
	assert.EqualValues(t, 0, stats.MaxDuration)
	assert.Empty(t, stats.LastSlowTime)
}

func TestSlowLog_StatsEndpoint(t *testing.T) {
	s := newObsTestServer(t)
	RecordSlowRequest(1200)

	req := httptest.NewRequest(http.MethodGet, "/_slowlog/stats", nil)
	w := httptest.NewRecorder()
	s.handleSlowLogStats(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var body SlowLogStats
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.EqualValues(t, 1, body.SlowCount)
	assert.EqualValues(t, 1200, body.MaxDuration)
}

func TestSlowLog_ConfigAndResetEndpoints(t *testing.T) {
	s := newObsTestServer(t)

	// 无效阈值(超过上限)
	req := httptest.NewRequest(http.MethodPut, "/_slowlog/config",
		bodyReader(t, `{"threshold_ms":60001}`))
	w := httptest.NewRecorder()
	s.handleSlowLogConfig(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	// 无效阈值(JSON 格式错误, 把数字写成字符串)
	req = httptest.NewRequest(http.MethodPut, "/_slowlog/config",
		bodyReader(t, `{"threshold_ms":"notanumber"}`))
	w = httptest.NewRecorder()
	s.handleSlowLogConfig(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code, "threshold_ms 非数字应报 parse 错误")

	// 有效阈值
	req = httptest.NewRequest(http.MethodPut, "/_slowlog/config",
		bodyReader(t, `{"threshold_ms":2000}`))
	w = httptest.NewRecorder()
	s.handleSlowLogConfig(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.EqualValues(t, 2000, GetSlowThreshold())

	// reset
	req = httptest.NewRequest(http.MethodPost, "/_slowlog/reset", nil)
	w = httptest.NewRecorder()
	s.handleSlowLogReset(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

// ---------- #15 新增: Log5xx / Error5xx / ApplySlowLogConfig / 中间件 ----------

func TestSlowLog_Log5xx_SetGet(t *testing.T) {
	// 默认 true
	assert.True(t, GetSlowLog5xx(), "Log5xx 默认应为 true")

	SetSlowLog5xx(false)
	assert.False(t, GetSlowLog5xx())

	SetSlowLog5xx(true)
	assert.True(t, GetSlowLog5xx())
}

func TestSlowLog_RecordError5xxAndStats(t *testing.T) {
	ResetSlowStats()
	RecordError5xx()
	RecordError5xx()
	RecordSlowRequest(2000)

	stats := SlowStats()
	assert.EqualValues(t, 1, stats.SlowCount, "慢请求应只有 1 条")
	assert.EqualValues(t, 2, stats.Error5xxCount, "5xx 错误应计数 2")
	assert.NotEmpty(t, stats.LastError5xxTime, "LastError5xxTime 应非空")
	assert.Equal(t, GetSlowLog5xx(), stats.Log5xx, "stats.Log5xx 应与全局一致")
}

func TestSlowLog_Reset_Incl5xx(t *testing.T) {
	RecordSlowRequest(1000)
	RecordError5xx()
	ResetSlowStats()
	stats := SlowStats()
	assert.EqualValues(t, 0, stats.SlowCount)
	assert.EqualValues(t, 0, stats.Error5xxCount)
	assert.Empty(t, stats.LastSlowTime)
	assert.Empty(t, stats.LastError5xxTime)
}

func TestSlowLog_ApplySlowLogConfig(t *testing.T) {
	// 先设置一个基线
	SetSlowThreshold(500)
	SetSlowLog5xx(true)

	// 情况1: 传 ThresholdMs=1000 + Log5xx=false
	bFalse := false
	ApplySlowLogConfig(SlowLogConfig{ThresholdMs: 1000, Log5xx: &bFalse})
	assert.EqualValues(t, 1000, GetSlowThreshold())
	assert.False(t, GetSlowLog5xx())

	// 情况2: 传零值(不修改)
	ApplySlowLogConfig(SlowLogConfig{ThresholdMs: 0, Log5xx: nil})
	assert.EqualValues(t, 1000, GetSlowThreshold(), "0 表示未设置, 不应修改阈值")
	assert.False(t, GetSlowLog5xx(), "nil 指针表示未设置, 不应修改 Log5xx")

	// 情况3: 负数 -> 内部 SetSlowThreshold 兜底为默认
	ApplySlowLogConfig(SlowLogConfig{ThresholdMs: -1})
	assert.EqualValues(t, 500, GetSlowThreshold(), "负数阈值应兜底为默认 500")
}

func TestSlowLog_ConfigEndpoint_Log5xx(t *testing.T) {
	s := newObsTestServer(t)
	// 先还原
	SetSlowLog5xx(true)
	SetSlowThreshold(500)

	// 只传 log_5xx: false
	req := httptest.NewRequest(http.MethodPut, "/_slowlog/config",
		bodyReader(t, `{"log_5xx":false}`))
	w := httptest.NewRecorder()
	s.handleSlowLogConfig(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.False(t, GetSlowLog5xx(), "log_5xx 应设为 false")
	// 阈值不应变
	assert.EqualValues(t, 500, GetSlowThreshold())

	// 同时传 threshold_ms + log_5xx
	req = httptest.NewRequest(http.MethodPut, "/_slowlog/config",
		bodyReader(t, `{"threshold_ms":300,"log_5xx":true}`))
	w = httptest.NewRecorder()
	s.handleSlowLogConfig(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.EqualValues(t, 300, GetSlowThreshold())
	assert.True(t, GetSlowLog5xx())

	// 响应体字段校验
	var resp map[string]any
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["log_5xx"])
	assert.Equal(t, true, resp["updated"])
}

func TestSlowLog_ConfigEndpoint_MethodNotAllowed(t *testing.T) {
	s := newObsTestServer(t)
	// stats 用 POST -> 405
	req := httptest.NewRequest(http.MethodPost, "/_slowlog/stats", nil)
	w := httptest.NewRecorder()
	s.handleSlowLogStats(w, req)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)

	// config 用 GET -> 405
	req = httptest.NewRequest(http.MethodGet, "/_slowlog/config", nil)
	w = httptest.NewRecorder()
	s.handleSlowLogConfig(w, req)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)

	// reset 用 GET -> 405
	req = httptest.NewRequest(http.MethodGet, "/_slowlog/reset", nil)
	w = httptest.NewRecorder()
	s.handleSlowLogReset(w, req)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

func TestSlowLog_Middleware_SlowRequest(t *testing.T) {
	s := newObsTestServer(t)
	ResetSlowStats()
	// 设置极低阈值, 让任何请求都算"慢"
	SetSlowThreshold(1)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	h := s.withSlowLog(inner)
	req := httptest.NewRequest(http.MethodGet, "/test/slow", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	stats := SlowStats()
	assert.True(t, stats.SlowCount >= 1, "应至少记录 1 条慢请求")
}

func TestSlowLog_Middleware_5xxResponse(t *testing.T) {
	s := newObsTestServer(t)
	ResetSlowStats()
	// 确保 Log5xx 打开
	SetSlowLog5xx(true)
	// 设置一个很大的阈值, 避免触发"慢请求"条件(只验证 5xx 分支)
	SetSlowThreshold(60000)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"boom"}`)
	})
	h := s.withSlowLog(inner)
	req := httptest.NewRequest(http.MethodPost, "/idx/_doc/1", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	stats := SlowStats()
	assert.EqualValues(t, 1, stats.Error5xxCount, "5xx 响应应计数 1")
	assert.EqualValues(t, 0, stats.SlowCount, "阈值很大, 不应触发慢请求计数")
}

func TestSlowLog_Middleware_5xx_Disabled(t *testing.T) {
	s := newObsTestServer(t)
	ResetSlowStats()
	// 关闭 Log5xx
	SetSlowLog5xx(false)
	SetSlowThreshold(60000)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	h := s.withSlowLog(inner)
	req := httptest.NewRequest(http.MethodGet, "/some/path", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	stats := SlowStats()
	assert.EqualValues(t, 0, stats.Error5xxCount, "Log5xx=false 时不应记录 5xx")
}

func TestSlowLog_Middleware_NormalFastRequest(t *testing.T) {
	s := newObsTestServer(t)
	ResetSlowStats()
	// 默认阈值 500ms, 快请求不触发慢请求; Log5xx=true 但 200 不触发 5xx
	SetSlowThreshold(500)
	SetSlowLog5xx(true)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	h := s.withSlowLog(inner)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	stats := SlowStats()
	assert.EqualValues(t, 0, stats.SlowCount, "快请求不触发慢请求")
	assert.EqualValues(t, 0, stats.Error5xxCount, "200 不触发 5xx")
}

func TestSlowLog_Middleware_4xxNot5xx(t *testing.T) {
	s := newObsTestServer(t)
	ResetSlowStats()
	SetSlowThreshold(60000)
	SetSlowLog5xx(true)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})
	h := s.withSlowLog(inner)
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	stats := SlowStats()
	assert.EqualValues(t, 0, stats.Error5xxCount, "4xx 不应被计为 5xx")
}

// 确保 slowLogResponseWriter 在 Write 没显式 WriteHeader 时默认 200
func TestSlowLog_ResponseWriter_Write_Default200(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &slowLogResponseWriter{ResponseWriter: rec, status: http.StatusOK}
	_, _ = sw.Write([]byte("hi"))
	assert.Equal(t, http.StatusOK, sw.status)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// 确保 Stats 端点返回新增字段: error_5xx_count / last_error_5xx_time / log_5xx
func TestSlowLog_StatsEndpoint_InclNewFields(t *testing.T) {
	s := newObsTestServer(t)
	ResetSlowStats()
	SetSlowLog5xx(true)
	RecordError5xx()
	RecordSlowRequest(1000)

	req := httptest.NewRequest(http.MethodGet, "/_slowlog/stats", nil)
	w := httptest.NewRecorder()
	s.handleSlowLogStats(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]any
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Contains(t, body, "error_5xx_count")
	assert.Contains(t, body, "last_error_5xx_time")
	assert.Contains(t, body, "log_5xx")
	assert.EqualValues(t, 1, body["error_5xx_count"])
	assert.Equal(t, true, body["log_5xx"])
}

// ---------- auditlog ----------

func TestAuditLogger_InitAndClose(t *testing.T) {
	// 重置全局状态, 避免被其他测试预先初始化
	globalAuditLogger = nil
	auditOnce = sync.Once{}
	al := InitAuditLogger(AuditConfig{Enabled: true, BufferSize: 1000}, zap.NewNop())
	assert.NotNil(t, al)
	assert.True(t, al.cfg.Enabled)

	al.LogEntry(AuditEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Action:    AuditActionCreate,
		Status:    "success",
	})

	// 触发关闭, 等待异步写入完成
	assert.NoError(t, al.Close())
	stats := al.Stats()
	assert.True(t, stats.TotalEntries >= 1)
	assert.EqualValues(t, 1, stats.CreateOps)
}

func TestAuditLogger_Disabled(t *testing.T) {
	// 用一个全新的全局实例(避免与前一个测试共享)
	globalAuditLogger = nil
	auditOnce = sync.Once{}
	al := InitAuditLogger(AuditConfig{Enabled: false}, zap.NewNop())
	assert.False(t, al.cfg.Enabled)

	// disabled 时直接返回, 不应 panic 也不应写入
	al.LogEntry(AuditEntry{Action: AuditActionDelete, Status: "success"})
	stats := al.Stats()
	assert.EqualValues(t, 0, stats.DeleteOps)
}

func TestAuditLogger_StatsEndpoint(t *testing.T) {
	s := newObsTestServer(t)
	// 确保全局状态干净
	globalAuditLogger = nil
	auditOnce = sync.Once{}
	al := InitAuditLogger(AuditConfig{Enabled: true}, zap.NewNop())
	defer al.Close()

	req := httptest.NewRequest(http.MethodGet, "/_audit/stats", nil)
	w := httptest.NewRecorder()
	s.handleAuditStats(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["enabled"])
}

func TestAuditLogger_QueryAndConfigEndpoints(t *testing.T) {
	s := newObsTestServer(t)
	// 确保全局状态干净
	globalAuditLogger = nil
	auditOnce = sync.Once{}
	al := InitAuditLogger(AuditConfig{Enabled: true}, zap.NewNop())
	defer al.Close()

	// GET /_audit
	req := httptest.NewRequest(http.MethodGet, "/_audit?action=create&limit=10", nil)
	w := httptest.NewRecorder()
	s.handleAuditQuery(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// 非法 limit
	req = httptest.NewRequest(http.MethodGet, "/_audit?limit=abc", nil)
	w = httptest.NewRecorder()
	s.handleAuditQuery(w, req)
	assert.Equal(t, http.StatusOK, w.Code) // 非法 limit 被忽略, 默认 100

	// PUT /_audit/config
	req = httptest.NewRequest(http.MethodPut, "/_audit/config",
		bodyReader(t, `{"enabled":false}`))
	w = httptest.NewRecorder()
	s.handleAuditConfig(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	al.mu.Lock()
	assert.False(t, al.cfg.Enabled)
	al.mu.Unlock()
}

// ---------- validation ----------

func TestValidateIndexName(t *testing.T) {
	SetValidationConfig(DefaultValidationConfig())

	ok, msg := ValidateIndexName("my-index")
	assert.True(t, ok)
	assert.Empty(t, msg)

	ok, msg = ValidateIndexName("")
	assert.False(t, ok)
	assert.Contains(t, msg, "required")

	ok, msg = ValidateIndexName("_system")
	assert.False(t, ok)
	assert.Contains(t, msg, "underscore")

	ok, msg = ValidateIndexName("bad name!")
	assert.False(t, ok)

	// 超长名
	longName := string(make([]byte, IndexNameMaxLength+1))
	for i := range longName {
		longName = longName[:i] + "a" + longName[i+1:]
	}
	ok, msg = ValidateIndexName(longName)
	assert.False(t, ok)
	assert.Contains(t, msg, "exceeds maximum length")
}

func TestValidateFromSize(t *testing.T) {
	ok, msg := ValidateFromSize(0, 10)
	assert.True(t, ok, msg)

	ok, msg = ValidateFromSize(-1, 10)
	assert.False(t, ok)

	ok, msg = ValidateFromSize(5000, 6000)
	assert.False(t, ok)
	assert.Contains(t, msg, "exceeds maximum limit")

	ok, msg = ValidateFromSize(0, 0)
	assert.False(t, ok)
	assert.Contains(t, msg, "positive")
}

func TestValidation_Endpoints(t *testing.T) {
	s := newObsTestServer(t)

	// GET config
	req := httptest.NewRequest(http.MethodGet, "/_validation/config", nil)
	w := httptest.NewRecorder()
	s.handleValidationConfig(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// PUT config
	req = httptest.NewRequest(http.MethodPut, "/_validation/config",
		bodyReader(t, `{"enabled":true,"max_index_name_length":100,"max_from_size":5000}`))
	w = httptest.NewRecorder()
	s.handleValidationConfigUpdate(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	cfg := GetValidationConfig()
	assert.Equal(t, 100, cfg.MaxIndexNameLength)
	assert.Equal(t, 5000, cfg.MaxFromSize)
}

func TestCORS_DefaultDisabled(t *testing.T) {
	cfg := DefaultCORSConfig()
	assert.False(t, cfg.Enabled)
}

// ---------- pprof ----------

func TestPprofIndex(t *testing.T) {
	s := newObsTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/_debug/pprof", nil)
	w := httptest.NewRecorder()
	s.handlePprofIndex(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "pprof endpoints")
	assert.Contains(t, body, "goroutine")
	assert.Contains(t, body, "heap")
}

func TestPprofProfile(t *testing.T) {
	s := newObsTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/_debug/pprof/profile?seconds=1", nil)
	w := httptest.NewRecorder()
	s.handlePprofProfile(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestPprofSymbol(t *testing.T) {
	s := newObsTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/_debug/pprof/symbol", nil)
	w := httptest.NewRecorder()
	s.handlePprofSymbols(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestPprofHandlers_NoPanic(t *testing.T) {
	s := newObsTestServer(t)
	// goroutine / heap / allocs / block / mutex / threadcreate
	handlers := []struct {
		name string
		fn   func(http.ResponseWriter, *http.Request)
	}{
		{"goroutine", s.handlePprofGoroutine},
		{"heap", s.handlePprofHeap},
		{"threadcreate", s.handlePprofThreadcreate},
		{"allocs", s.handlePprofAllocs},
		{"block", s.handlePprofBlock},
		{"mutex", s.handlePprofMutex},
	}
	for _, h := range handlers {
		t.Run(h.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/_debug/pprof/"+h.name, nil)
			w := httptest.NewRecorder()
			assert.NotPanics(t, func() { h.fn(w, req) })
			assert.Equal(t, http.StatusOK, w.Code)
		})
	}
}

func TestConfigReload_WithConfigLoader(t *testing.T) {
	f, err := os.CreateTemp("", "go_es_config_*.yaml")
	assert.NoError(t, err)
	defer os.Remove(f.Name())
	_, _ = f.WriteString("log_level: info\naddr: ':9999'\n")
	_ = f.Close()

	loader := NewConfigLoader(f.Name())
	assert.NoError(t, loader.Load())

	s := newObsTestServer(t)
	s.configLoader = loader

	req := httptest.NewRequest(http.MethodPost, "/_config/reload", nil)
	w := httptest.NewRecorder()
	s.handleConfigReload(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "reloaded", resp["status"])
	assert.Equal(t, ":9999", resp["addr"])
}

func TestConfigReload_WithoutLoader(t *testing.T) {
	s := newObsTestServer(t) // configLoader = nil

	req := httptest.NewRequest(http.MethodPost, "/_config/reload", nil)
	w := httptest.NewRecorder()
	s.handleConfigReload(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRuntimeStats(t *testing.T) {
	stats := GetRuntimeStats()
	assert.NotEmpty(t, stats["go_version"])
	assert.NotEmpty(t, stats["num_cpu"])
	assert.Contains(t, stats, "goroutines")
	assert.Contains(t, stats, "memory")
	mem, ok := stats["memory"].(map[string]interface{})
	assert.True(t, ok)
	assert.Contains(t, mem, "alloc")
	assert.Contains(t, mem, "gc_pause_ns")

	// 启动 goroutine 数量合理
	assert.GreaterOrEqual(t, stats["goroutines"], 1)
	assert.Less(t, stats["goroutines"], runtime.NumGoroutine()+1000)
}

func TestRuntimeStats_Endpoint(t *testing.T) {
	s := newObsTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/_stats", nil)
	w := httptest.NewRecorder()
	s.handleRuntimeStats(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

// bodyReader 辅助函数: 将 JSON 字符串包装成 io.Reader
func bodyReader(t *testing.T, s string) *bodyReaderWrapper {
	t.Helper()
	return &bodyReaderWrapper{data: []byte(s), pos: 0}
}

type bodyReaderWrapper struct {
	data []byte
	pos  int
}

func (b *bodyReaderWrapper) Read(p []byte) (int, error) {
	if b.pos >= len(b.data) {
		return 0, io.EOF
	}
	n := copy(p, b.data[b.pos:])
	b.pos += n
	return n, nil
}
