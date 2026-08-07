// Package server - 健康端点 单元测试
package server

import (
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/zixiliuyue/go_es/internal/search"
	"github.com/zixiliuyue/go_es/internal/storage"
)

// TestHealthState_String
func TestHealthState_String(t *testing.T) {
	assert.Equal(t, "starting", HealthStarting.String())
	assert.Equal(t, "ready", HealthReady.String())
	assert.Equal(t, "degraded", HealthDegraded.String())
	assert.Equal(t, "shutting_down", HealthShuttingDown.String())
}

// TestHealthChecker_SetGetState
func TestHealthChecker_SetGetState(t *testing.T) {
	hc := NewHealthChecker()
	assert.Equal(t, HealthStarting, hc.GetState())
	hc.SetState(HealthReady)
	assert.Equal(t, HealthReady, hc.GetState())
	hc.SetState(HealthShuttingDown)
	assert.Equal(t, HealthShuttingDown, hc.GetState())
}

// TestHealthChecker_IsReady
func TestHealthChecker_IsReady(t *testing.T) {
	hc := NewHealthChecker()
	assert.False(t, hc.IsReady(), "starting -> not ready")
	hc.SetState(HealthReady)
	assert.True(t, hc.IsReady())
	hc.SetState(HealthDegraded)
	assert.True(t, hc.IsReady(), "degraded also ready (K8s 仍可路由)")
	hc.SetState(HealthShuttingDown)
	assert.False(t, hc.IsReady())
}

// TestHealthChecker_CacheTTL
func TestHealthChecker_CacheTTL(t *testing.T) {
	hc := NewHealthChecker()
	hc.cacheTTL = 50 * time.Millisecond
	hc.cache = &HealthReport{}
	hc.cacheExpiry = time.Now().Add(20 * time.Millisecond)
	// 20ms 后过期
	time.Sleep(30 * time.Millisecond)
	hc.mu.Lock()
	cached := hc.cache != nil && time.Now().Before(hc.cacheExpiry)
	hc.mu.Unlock()
	assert.False(t, cached, "should expire after TTL")
}

// TestServer_Check_AllUp 全 up
func TestServer_Check_AllUp(t *testing.T) {
	store, err := storage.Open("")
	assert.NoError(t, err)
	defer store.Close()
	eng := search.New(store)
	realWC := NewWriteCoordinator(WriteConfig{MaxConcurrent: 1})
	al := NewAccessLogger(AccessLogConfig{Enabled: true, BufferSize: 10}, nil)
	defer al.Close()
	hc := NewHealthChecker()
	hc.SetState(HealthReady)
	s := &Server{
		store: store, engine: eng, wc: realWC, accessLog: al, rbac: newRBAC(),
		clusterName: "test_cluster",
		startedAt:   time.Now(),
		healthChecker: hc,
	}
	report := s.Check()
	assert.Equal(t, "ready", report.StateName)
	assert.False(t, report.Degraded)
	assert.Equal(t, 5, len(report.Components))
	for _, c := range report.Components {
		assert.Equal(t, "up", c.Status, "component %s should be up", c.Name)
	}
}

// TestServer_Check_NoStorage 没有 storage
func TestServer_Check_NoStorage(t *testing.T) {
	hc := NewHealthChecker()
	hc.SetState(HealthReady)
	s := &Server{startedAt: time.Now(), rbac: newRBAC(), healthChecker: hc}
	report := s.Check()
	// 没 storage, 但 engine/writeQueue/rbac/accessLog 都 nil -> 都 down
	// state 应该是 degraded
	assert.Equal(t, "degraded", report.StateName)
	hasDown := false
	for _, c := range report.Components {
		if c.Status == "down" {
			hasDown = true
		}
	}
	assert.True(t, hasDown, "should have at least one down component")
}

// TestServer_Check_StorageLatency storage latency
func TestServer_Check_StorageLatency(t *testing.T) {
	store, err := storage.Open("")
	assert.NoError(t, err)
	defer store.Close()
	eng := search.New(store)
	hc := NewHealthChecker()
	hc.SetState(HealthReady)
	s := &Server{store: store, engine: eng, rbac: newRBAC(), startedAt: time.Now(), healthChecker: hc}
	report := s.Check()
	var storageComp *HealthComponent
	for i := range report.Components {
		if report.Components[i].Name == "storage" {
			storageComp = &report.Components[i]
		}
	}
	assert.NotNil(t, storageComp)
	assert.Equal(t, "up", storageComp.Status)
}

// TestServer_Check_ShuttingDown shutting_down
func TestServer_Check_ShuttingDown(t *testing.T) {
	store, err := storage.Open("")
	assert.NoError(t, err)
	defer store.Close()
	eng := search.New(store)
	hc := NewHealthChecker()
	s := &Server{store: store, engine: eng, rbac: newRBAC(), startedAt: time.Now(), healthChecker: hc}
	hc.SetState(HealthShuttingDown)
	report := s.Check()
	assert.Equal(t, "shutting_down", report.StateName)
}

// TestServer_Check_Cache 缓存命中
func TestServer_Check_Cache(t *testing.T) {
	store, err := storage.Open("")
	assert.NoError(t, err)
	defer store.Close()
	eng := search.New(store)
	hc := NewHealthChecker()
	hc.SetState(HealthReady)
	s := &Server{store: store, engine: eng, rbac: newRBAC(), startedAt: time.Now(), healthChecker: hc}
	r1 := s.Check()
	r2 := s.Check()
	assert.Same(t, r1, r2, "should return cached report")
}

// TestHandleHealthStatus 端到端
func TestHandleHealthStatus(t *testing.T) {
	store, err := storage.Open("")
	assert.NoError(t, err)
	defer store.Close()
	eng := search.New(store)
	hc := NewHealthChecker()
	hc.SetState(HealthReady)
	al := NewAccessLogger(AccessLogConfig{Enabled: true, BufferSize: 10}, nil)
	defer al.Close()
	s := &Server{
		store: store, engine: eng, rbac: newRBAC(), wc: NewWriteCoordinator(WriteConfig{}),
		accessLog: al, clusterName: "go_es", startedAt: time.Now(),
		healthChecker: hc,
	}
	s.SetReady()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/_health/status", nil)
	s.handleHealthStatus(rr, req)
	assert.Equal(t, 200, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, `"state_name":"ready"`)
	assert.Contains(t, body, `"components"`)
	assert.Contains(t, body, `"cluster":"go_es"`)
}

// TestHandleHealthComponents 端到端
func TestHandleHealthComponents(t *testing.T) {
	store, err := storage.Open("")
	assert.NoError(t, err)
	defer store.Close()
	eng := search.New(store)
	hc := NewHealthChecker()
	hc.SetState(HealthReady)
	al := NewAccessLogger(AccessLogConfig{Enabled: true, BufferSize: 10}, nil)
	defer al.Close()
	s := &Server{
		store: store, engine: eng, rbac: newRBAC(), wc: NewWriteCoordinator(WriteConfig{}),
		accessLog: al, startedAt: time.Now(), healthChecker: hc,
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/_health/components", nil)
	s.handleHealthComponents(rr, req)
	assert.Equal(t, 200, rr.Code)
	assert.Contains(t, rr.Body.String(), `"components"`)
}

// TestHandleHealthStatus_Starting state 启动期间返回 503
func TestHandleHealthStatus_Starting(t *testing.T) {
	hc := NewHealthChecker()
	hc.SetState(HealthStarting)
	var sd atomic.Bool // not set, so sIsStarted returns false
	s := &Server{
		startedAt: time.Now(), rbac: newRBAC(), healthChecker: hc,
		startupDone: sd,
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/_health/status", nil)
	s.handleHealthStatus(rr, req)
	assert.Equal(t, 503, rr.Code)
}

// TestSetReady / SetShuttingDown
func TestServer_StateMethods(t *testing.T) {
	s := &Server{startedAt: time.Now(), rbac: newRBAC(), healthChecker: NewHealthChecker()}
	// 初始 starting
	assert.Equal(t, HealthStarting, s.healthChecker.GetState())
	// 标记启动完成 -> ready
	s.startupDone.Store(true)
	s.SetReady()
	assert.Equal(t, HealthReady, s.healthChecker.GetState())
	// shutting down
	s.SetShuttingDown()
	assert.Equal(t, HealthShuttingDown, s.healthChecker.GetState())
}

// 并发安全
func TestHealthChecker_Concurrent(t *testing.T) {
	hc := NewHealthChecker()
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				hc.SetState(HealthReady)
				hc.GetState()
				hc.IsReady()
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, HealthReady, hc.GetState())
}
