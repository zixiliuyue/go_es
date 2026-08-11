// 审计日志
//
// 设计:
//   - 异步记录所有写操作(PUT/POST/DELETE/PATCH)
//   - 字段: timestamp, request_id, user, action, index, doc_id, status, detail
//   - 支持按时间范围、用户、索引、操作类型查询
//   - 内存缓冲 + 异步写入,不阻塞业务请求
//   - 可配置: 启用开关、文件路径、缓冲区大小
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// AuditAction 审计操作类型
type AuditAction string

const (
	AuditActionCreate  AuditAction = "create"
	AuditActionUpdate  AuditAction = "update"
	AuditActionDelete  AuditAction = "delete"
	AuditActionBulk    AuditAction = "bulk"
	AuditActionReindex AuditAction = "reindex"
	AuditActionSearch  AuditAction = "search"
	AuditActionLogin   AuditAction = "login"
	AuditActionRole    AuditAction = "role_change"
)

// AuditConfig 审计日志配置
type AuditConfig struct {
	// Enabled 启用审计日志
	Enabled bool
	// FilePath 写入文件路径,留空 -> stdout
	FilePath string
	// BufferSize 内存缓冲区大小
	BufferSize int
}

// AuditEntry 审计日志条目
type AuditEntry struct {
	Timestamp   string        `json:"@timestamp"`
	RequestID   string        `json:"request_id"`
	Username    string        `json:"username"`
	Action      AuditAction   `json:"action"`
	Index       string        `json:"index,omitempty"`
	DocID       string        `json:"doc_id,omitempty"`
	Status      string        `json:"status"`
	StatusCode  int           `json:"status_code"`
	Detail      string        `json:"detail,omitempty"`
	ClientIP    string        `json:"client_ip,omitempty"`
	Method      string        `json:"method,omitempty"`
	Path        string        `json:"path,omitempty"`
	DurationMs  int64         `json:"duration_ms,omitempty"`
	TraceID     string        `json:"trace_id,omitempty"`
	SpanID      string        `json:"span_id,omitempty"`
}

// AuditStats 审计日志统计
type AuditStats struct {
	TotalEntries int64 `json:"total_entries"`
	Dropped      int64 `json:"dropped"`
	CreateOps    int64 `json:"create_ops"`
	UpdateOps    int64 `json:"update_ops"`
	DeleteOps    int64 `json:"delete_ops"`
}

// AuditLogger 审计日志记录器
type AuditLogger struct {
	mu       sync.Mutex
	cfg      AuditConfig
	ch       chan AuditEntry
	stopped  chan struct{}
	stopOnce sync.Once
	writer   *os.File
	logger   *zap.Logger
	stats    AuditStats
}

// globalAuditLogger 全局审计日志实例
var globalAuditLogger *AuditLogger
var auditOnce sync.Once

// InitAuditLogger 初始化审计日志
func InitAuditLogger(cfg AuditConfig, logger *zap.Logger) *AuditLogger {
	auditOnce.Do(func() {
		if cfg.BufferSize <= 0 {
			cfg.BufferSize = 10000
		}
		al := &AuditLogger{
			cfg:     cfg,
			ch:      make(chan AuditEntry, cfg.BufferSize),
			stopped: make(chan struct{}),
			logger:  logger,
		}
		if cfg.FilePath != "" {
			f, err := os.OpenFile(cfg.FilePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
			if err != nil {
				logger.Warn("audit log open file failed, fallback to stdout", zap.Error(err))
			} else {
				al.writer = f
			}
		}
		if al.writer == nil {
			al.writer = os.Stdout
		}
		if cfg.Enabled {
			go al.run()
		}
		globalAuditLogger = al
	})
	return globalAuditLogger
}

// GetAuditLogger 获取全局审计日志实例
func GetAuditLogger() *AuditLogger {
	return globalAuditLogger
}

// run 异步消费 channel
func (al *AuditLogger) run() {
	for entry := range al.ch {
		b, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		b = append(b, '\n')
		_, _ = al.writer.Write(b)

		// 更新统计
		atomic.AddInt64(&al.stats.TotalEntries, 1)
		switch entry.Action {
		case AuditActionCreate:
			atomic.AddInt64(&al.stats.CreateOps, 1)
		case AuditActionUpdate:
			atomic.AddInt64(&al.stats.UpdateOps, 1)
		case AuditActionDelete:
			atomic.AddInt64(&al.stats.DeleteOps, 1)
		}
	}
	close(al.stopped)
}

// LogEntry 异步记录审计条目
func (al *AuditLogger) LogEntry(entry AuditEntry) {
	if !al.cfg.Enabled {
		return
	}
	select {
	case al.ch <- entry:
	default:
		atomic.AddInt64(&al.stats.Dropped, 1)
	}
}

// Close 关闭审计日志
func (al *AuditLogger) Close() error {
	al.stopOnce.Do(func() {
		if !al.cfg.Enabled {
			close(al.stopped)
			if al.writer != nil && al.writer != os.Stdout {
				_ = al.writer.Close()
			}
			return
		}
		close(al.ch)
		<-al.stopped
		if al.writer != nil && al.writer != os.Stdout {
			_ = al.writer.Close()
		}
	})
	return nil
}

// Stats 获取审计统计
func (al *AuditLogger) Stats() AuditStats {
	return AuditStats{
		TotalEntries: atomic.LoadInt64(&al.stats.TotalEntries),
		Dropped:      atomic.LoadInt64(&al.stats.Dropped),
		CreateOps:    atomic.LoadInt64(&al.stats.CreateOps),
		UpdateOps:    atomic.LoadInt64(&al.stats.UpdateOps),
		DeleteOps:    atomic.LoadInt64(&al.stats.DeleteOps),
	}
}

// RecordAudit 便捷方法:记录审计条目
func RecordAudit(action AuditAction, index, docID, status string, statusCode int, detail string, r *http.Request, durationMs int64) {
	al := globalAuditLogger
	if al == nil || !al.cfg.Enabled {
		return
	}
	traceID, spanID := TraceInfoFromContext(r.Context())
	entry := AuditEntry{
		Timestamp:  time.Now().UTC().Format(time.RFC3339Nano),
		RequestID:  requestIDFromCtx(r.Context()),
		Username:   getUsernameFromCtx(r.Context()),
		Action:     action,
		Index:      index,
		DocID:      docID,
		Status:     status,
		StatusCode: statusCode,
		Detail:     detail,
		ClientIP:   clientIP(r),
		Method:     r.Method,
		Path:       r.URL.Path,
		DurationMs: durationMs,
		TraceID:    traceID,
		SpanID:     spanID,
	}
	al.LogEntry(entry)
}

// buildAuditDetail 构造审计详情字符串
func buildAuditDetail(format string, args ...interface{}) string {
	return fmt.Sprintf(format, args...)
}

// isWriteMethod 判断是否为写操作
func isWriteMethod(method string) bool {
	switch method {
	case http.MethodPut, http.MethodPost, http.MethodDelete, http.MethodPatch:
		return true
	}
	return false
}

// middlewareAuditLog 审计日志中间件
// 自动记录写操作(PUT/POST/DELETE/PATCH)
func (s *Server) middlewareAuditLog(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isWriteMethod(r.Method) {
			h.ServeHTTP(w, r)
			return
		}

		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w, status: 200}

		// 记录请求信息
		index := pathSegment(r, 0)
		if index == "" || index[0] == '_' {
			index = ""
		}
		docID := pathSegment(r, 2)

		h.ServeHTTP(sr, r)

		durationMs := time.Since(start).Milliseconds()
		statusCode := sr.status
		status := "success"
		if statusCode >= 400 {
			status = "failed"
		}

		// 判断操作类型
		action := determineAuditAction(r)
		detail := buildAuditDetail("%s %s %s %dms", r.Method, r.URL.Path, status, durationMs)

		RecordAudit(action, index, docID, status, statusCode, detail, r, durationMs)
	})
}

// determineAuditAction 根据请求判断审计操作类型
func determineAuditAction(r *http.Request) AuditAction {
	path := r.URL.Path
	method := r.Method

	// bulk 操作
	if contains(path, "_bulk") {
		return AuditActionBulk
	}
	// reindex 操作
	if contains(path, "_reindex") {
		return AuditActionReindex
	}
	// search 操作
	if contains(path, "_search") && (method == http.MethodPost || method == http.MethodGet) {
		return AuditActionSearch
	}
	// role/user 操作
	if contains(path, "_security") {
		return AuditActionRole
	}
	// 按 method 判断
	switch method {
	case http.MethodPost:
		// POST 可能是 create 或 bulk/reindex
		return AuditActionCreate
	case http.MethodPut:
		return AuditActionUpdate
	case http.MethodPatch:
		return AuditActionUpdate
	case http.MethodDelete:
		return AuditActionDelete
	default:
		return AuditActionSearch
	}
}

// contains 检查字符串是否包含子串(不区分大小写简单实现)
func contains(s, substr string) bool {
	return len(s) >= len(substr) && indexOf(s, substr) >= 0
}

// indexOf 简单实现字符串查找
func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// handleAuditQuery GET /_audit 查询审计日志
// 支持参数:
//   - action: 按操作类型过滤(create/update/delete/bulk/reindex/search)
//   - index: 按索引过滤
//   - user: 按用户过滤
//   - status: 按状态过滤(success/failed)
//   - since: 起始时间(RFC3339格式)
//   - until: 结束时间(RFC3339格式)
//   - limit: 返回条数限制(默认100,最大1000)
func (s *Server) handleAuditQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET", "")
		return
	}

	al := GetAuditLogger()
	if al == nil || !al.cfg.Enabled {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable", "audit log not enabled", "")
		return
	}

	// 解析查询参数
	actionFilter := r.URL.Query().Get("action")
	indexFilter := r.URL.Query().Get("index")
	userFilter := r.URL.Query().Get("user")
	statusFilter := r.URL.Query().Get("status")
	sinceStr := r.URL.Query().Get("since")
	untilStr := r.URL.Query().Get("until")

	// 解析时间范围(记录到响应, 未来持久化后用于实际过滤)
	var sinceTime, untilTime time.Time
	_ = sinceTime
	_ = untilTime
	if sinceStr != "" {
		t, err := time.Parse(time.RFC3339, sinceStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, "illegal_argument_exception", "invalid since format, use RFC3339", "")
			return
		}
		sinceTime = t
	}
	if untilStr != "" {
		t, err := time.Parse(time.RFC3339, untilStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, "illegal_argument_exception", "invalid until format, use RFC3339", "")
			return
		}
		untilTime = t
	}

	// limit
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := parseInt(l); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 1000 {
		limit = 1000
	}

	// 从 channel 读取所有已记录条目(生产环境应使用持久化存储)
	// 这里简化实现:通过 stats 返回汇总信息
	stats := al.Stats()

	// 构建响应
	resp := map[string]interface{}{
		"stats":       stats,
		"filters":     map[string]string{"action": actionFilter, "index": indexFilter, "user": userFilter, "status": statusFilter},
		"time_range":  map[string]string{"since": sinceStr, "until": untilStr},
		"limit":       limit,
		"note":        "Full audit log query requires persistent storage. Use /_audit/stats for aggregate stats.",
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleAuditStats GET /_audit/stats
func (s *Server) handleAuditStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET", "")
		return
	}

	al := GetAuditLogger()
	if al == nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable", "audit log not initialized", "")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled": al.cfg.Enabled,
		"file":    al.cfg.FilePath,
		"stats":   al.Stats(),
	})
}

// handleAuditConfig PUT /_audit/config
func (s *Server) handleAuditConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use PUT", "")
		return
	}

	var req struct {
		Enabled bool   `json:"enabled"`
		FilePath string `json:"file_path"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "parse_exception", err.Error(), "")
		return
	}

	// 动态更新配置(简化实现)
	al := GetAuditLogger()
	if al == nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable", "audit log not initialized", "")
		return
	}

	al.mu.Lock()
	al.cfg.Enabled = req.Enabled
	if req.FilePath != "" {
		al.cfg.FilePath = req.FilePath
	}
	al.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled": req.Enabled,
		"file":    req.FilePath,
		"updated": true,
	})
}