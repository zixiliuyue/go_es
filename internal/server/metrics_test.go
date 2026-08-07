// Package server - Prometheus 指标 单元测试
package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/zixiliuyue/go_es/internal/search"
	"github.com/zixiliuyue/go_es/internal/storage"
)

// TestNewServerMetrics 注册所有指标不冲突
func TestNewServerMetrics(t *testing.T) {
	m := NewServerMetrics()
	assert.NotNil(t, m)
	assert.NotNil(t, m.Registry())
}

// TestCollect_WCStats 写协调器指标收集
func TestCollect_WCStats(t *testing.T) {
	store, err := storage.Open("")
	assert.NoError(t, err)
	defer store.Close()
	eng := search.New(store)
	wc := NewWriteCoordinator(WriteConfig{MaxConcurrent: 4})
	s := &Server{store: store, engine: eng, wc: wc, metrics: NewServerMetrics()}
	s.metrics.Collect(s)
	// 验证 wc_in_flight 没崩
}

// TestCollect_AccessLog 访问日志指标
func TestCollect_AccessLog(t *testing.T) {
	store, err := storage.Open("")
	assert.NoError(t, err)
	defer store.Close()
	eng := search.New(store)
	al := NewAccessLogger(AccessLogConfig{Enabled: true, BufferSize: 10}, nil)
	defer al.Close()
	s := &Server{store: store, engine: eng, accessLog: al, metrics: NewServerMetrics()}
	s.metrics.Collect(s)
}

// TestCollect_Segment segment 指标
func TestCollect_Segment(t *testing.T) {
	store, err := storage.Open("")
	assert.NoError(t, err)
	defer store.Close()
	eng := search.New(store)
	seg := NewSegmentManager(SegmentConfig{}, store, eng)
	s := &Server{store: store, engine: eng, seg: seg, metrics: NewServerMetrics()}
	s.metrics.Collect(s)
}

// TestCollect_NilServer 不崩
func TestCollect_NilServer(t *testing.T) {
	m := NewServerMetrics()
	// nil server
	m.Collect(nil)
	// 不应崩
}

// TestIncOptimisticConflict 优化锁冲突
func TestIncOptimisticConflict(t *testing.T) {
	m := NewServerMetrics()
	m.IncOptimisticConflict("write", "create")
	m.IncOptimisticConflict("write", "create")
	m.IncOptimisticConflict("write", "index")
	// 不崩
}

// TestIncRbacAuthFail / Forbidden
func TestIncRbac(t *testing.T) {
	m := NewServerMetrics()
	m.IncRbacAuthFail("no_user")
	m.IncRbacAuthFail("bad_pass")
	m.IncRbacForbidden("write", "test")
	m.IncRbacForbidden("delete", "test")
	// 不崩
}

// TestMetricsHandler_Renders 验证抓取端点输出
func TestMetricsHandler_Renders(t *testing.T) {
	store, err := storage.Open("")
	assert.NoError(t, err)
	defer store.Close()
	eng := search.New(store)
	s := &Server{
		store: store, engine: eng,
		metrics: NewServerMetrics(),
	}
	// 触发一次 ObserveRequest: 直接调用
	s.metrics.ObserveRequest("GET", "/test", 200, 0)
	s.metrics.ObserveRequest("POST", "/_doc", 201, 0)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	s.handleMetrics(rr, req)
	assert.Equal(t, 200, rr.Code)
	body := rr.Body.String()
	// 必须有基础指标
	assert.Contains(t, body, "go_es_http_requests_total")
	assert.Contains(t, body, "go_es_start_time_seconds")
	assert.Contains(t, body, "go_es_build_info")
	// 新增指标
	assert.Contains(t, body, "go_es_wc_in_flight")
	assert.Contains(t, body, "go_es_segment_total")
}

// TestMetricsHandler_ContentType
func TestMetricsHandler_ContentType(t *testing.T) {
	s := &Server{metrics: NewServerMetrics()}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	s.handleMetrics(rr, req)
	ct := rr.Header().Get("Content-Type")
	assert.True(t, strings.HasPrefix(ct, "text/plain") || strings.HasPrefix(ct, "application/openmetrics-text"),
		"Content-Type should be Prometheus text, got %s", ct)
}

// TestCollect_AllNilServer 各字段 nil
func TestCollect_AllNilServer(t *testing.T) {
	m := NewServerMetrics()
	s := &Server{metrics: m}
	m.Collect(s)
	// 不应崩
}
