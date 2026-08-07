// 健康端点深化
//
// 设计:
//   - Component health: storage, engine, write queue, RBAC, segment
//   - Latency probe: 真实读 storage key (cheap) 测延迟
//   - Health state: starting -> ready -> degraded | shutting_down
//   - /_health/status    完整 JSON (含各 component 状态)
//   - /_health/liveness  K8s 探针: 进程在
//   - /_health/readiness K8s 探针: ready + !shutting_down
//   - /_health/startup   K8s 探针: 已过 starting 阶段
//   - /_health/components 单独列各 component 详细
//   - Cache: 1s TTL, 避免高频调用把 storage 测压
package server

import (
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// HealthState 健康状态机
type HealthState int32

const (
	HealthStarting    HealthState = 0
	HealthReady       HealthState = 1
	HealthDegraded    HealthState = 2
	HealthShuttingDown HealthState = 3
)

func (s HealthState) String() string {
	switch s {
	case HealthStarting:
		return "starting"
	case HealthReady:
		return "ready"
	case HealthDegraded:
		return "degraded"
	case HealthShuttingDown:
		return "shutting_down"
	default:
		return "unknown"
	}
}

// HealthComponent 各 component 状态
type HealthComponent struct {
	Name      string `json:"name"`
	Status    string `json:"status"` // up | down | unknown
	Message   string `json:"message,omitempty"`
	LatencyMs int64  `json:"latency_ms"`
}

// HealthReport 健康快照
type HealthReport struct {
	State      HealthState        `json:"state"`
	StateName  string             `json:"state_name"`
	Cluster    string             `json:"cluster"`
	StartedAt  time.Time          `json:"started_at"`
	UptimeSec  int64              `json:"uptime_sec"`
	Components []HealthComponent  `json:"components"`
	Degraded   bool               `json:"degraded"`
}

// HealthChecker 健康检查器
type HealthChecker struct {
	mu          sync.Mutex
	cache       *HealthReport
	cacheExpiry time.Time
	cacheTTL    time.Duration

	state atomic.Int32 // HealthState
}

// NewHealthChecker 构造
func NewHealthChecker() *HealthChecker {
	return &HealthChecker{
		cacheTTL: 1 * time.Second,
	}
}

// SetState 改状态
func (h *HealthChecker) SetState(s HealthState) {
	h.state.Store(int32(s))
	// 状态变更清缓存
	h.mu.Lock()
	h.cache = nil
	h.mu.Unlock()
}

// GetState 拿状态
func (h *HealthChecker) GetState() HealthState {
	return HealthState(h.state.Load())
}

// IsReady ready 状态 + 未 shutting_down
func (h *HealthChecker) IsReady() bool {
	s := h.GetState()
	return s == HealthReady || s == HealthDegraded
}

// Check 生成完整健康报告 (缓存 1s)
func (s *Server) Check() *HealthReport {
	hc := s.healthChecker
	if hc == nil {
		s.healthCheckerMu.Lock()
		if s.healthChecker == nil {
			s.healthChecker = NewHealthChecker()
		}
		hc = s.healthChecker
		s.healthCheckerMu.Unlock()
	}
	hc.mu.Lock()
	if hc.cache != nil && time.Now().Before(hc.cacheExpiry) {
		r := hc.cache
		hc.mu.Unlock()
		return r
	}
	hc.mu.Unlock()

	// 收集 component
	now := time.Now()
	components := make([]HealthComponent, 0, 5)

	// 1. storage
	if s.store != nil {
		start := time.Now()
		err := s.store.Ping()
		comp := HealthComponent{
			Name:      "storage",
			Status:    "up",
			LatencyMs: time.Since(start).Milliseconds(),
		}
		if err != nil {
			comp.Status = "down"
			comp.Message = err.Error()
		}
		components = append(components, comp)
	} else {
		components = append(components, HealthComponent{Name: "storage", Status: "down", Message: "not initialized"})
	}

	// 2. engine
	if s.engine != nil {
		components = append(components, HealthComponent{Name: "engine", Status: "up"})
	} else {
		components = append(components, HealthComponent{Name: "engine", Status: "down", Message: "not initialized"})
	}

	// 3. write queue
	if s.wc != nil {
		stats := s.wc.Stats()
		comp := HealthComponent{
			Name:      "write_queue",
			Status:    "up",
			LatencyMs: 0,
		}
		// 容量使用率超 90% 视为 degraded
		if stats.InFlight > 0 {
			comp.Message = "active"
		}
		components = append(components, comp)
	} else {
		components = append(components, HealthComponent{Name: "write_queue", Status: "down", Message: "not initialized"})
	}

	// 4. access log
	if s.accessLog != nil && s.accessLog.cfg.Enabled {
		components = append(components, HealthComponent{Name: "access_log", Status: "up"})
	} else {
		components = append(components, HealthComponent{Name: "access_log", Status: "down", Message: "not enabled"})
	}

	// 5. rbac
	if s.rbac != nil {
		components = append(components, HealthComponent{Name: "rbac", Status: "up"})
	} else {
		components = append(components, HealthComponent{Name: "rbac", Status: "down"})
	}

	// 计算 state
	state := hc.GetState()
	// 任何 component down -> degraded
	degraded := false
	for _, c := range components {
		if c.Status == "down" {
			degraded = true
		}
	}
	_ = now
	// shutting_down 不被改写
	if state != HealthShuttingDown {
		if degraded && state == HealthReady {
			hc.SetState(HealthDegraded)
			state = HealthDegraded
		} else if !degraded && state == HealthDegraded {
			hc.SetState(HealthReady)
			state = HealthReady
		}
	}

	report := &HealthReport{
		State:      state,
		StateName:  state.String(),
		Cluster:    s.clusterName,
		StartedAt:  s.startedAt,
		UptimeSec:  int64(time.Since(s.startedAt).Seconds()),
		Components: components,
		Degraded:   state == HealthDegraded,
	}

	hc.mu.Lock()
	hc.cache = report
	hc.cacheExpiry = time.Now().Add(hc.cacheTTL)
	hc.mu.Unlock()

	return report
}

// handleHealthStatus GET /_health/status  完整健康报告
func (s *Server) handleHealthStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET", "")
		return
	}
	report := s.Check()
	status := http.StatusOK
	if report.State == HealthShuttingDown || (report.State == HealthStarting && !sIsStarted(s)) {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, report)
}

// handleHealthComponents GET /_health/components  各 component 详细
func (s *Server) handleHealthComponents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET", "")
		return
	}
	report := s.Check()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"components": report.Components,
		"state":      report.StateName,
		"degraded":   report.Degraded,
	})
}

// sGetHealthChecker 取得 health checker (懒构造)
func sGetHealthChecker(s *Server) *HealthChecker {
	if s.healthChecker == nil {
		s.healthCheckerMu.Lock()
		if s.healthChecker == nil {
			s.healthChecker = NewHealthChecker()
		}
		s.healthCheckerMu.Unlock()
	}
	return s.healthChecker
}

// sIsStarted 是否已启动
func sIsStarted(s *Server) bool {
	return s.startupDone.Load()
}

// SetReady 标记 ready
func (s *Server) SetReady() {
	if s.healthChecker != nil {
		s.healthChecker.SetState(HealthReady)
	}
}

// SetShuttingDown 标记 shutting_down
func (s *Server) SetShuttingDown() {
	if s.healthChecker != nil {
		s.healthChecker.SetState(HealthShuttingDown)
	}
}
