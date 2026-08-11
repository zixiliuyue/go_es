// 慢查询日志
//
// 设计:
//   - 扩展 AccessLogConfig 添加 SlowThresholdMs 配置
//   - 超过阈值的请求自动标记为 warn 级别
//   - 支持动态配置阈值(通过配置热更新)
//   - 统计慢查询计数,暴露到 /accesslog/stats
package server

import (
	"net/http"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// SlowLogStats 慢查询统计
type SlowLogStats struct {
	SlowCount    int64         `json:"slow_count"`
	Error5xxCount int64        `json:"error_5xx_count"`
	MaxDuration  int64         `json:"max_duration_ms"`
	LastSlowTime string        `json:"last_slow_time,omitempty"`
	LastError5xxTime string    `json:"last_error_5xx_time,omitempty"`
	ThresholdMs  int64         `json:"threshold_ms"`
	Log5xx       bool          `json:"log_5xx"`
}

// slowLogState 慢查询状态(线程安全)
type slowLogState struct {
	slowCount     int64
	error5xxCount int64
	maxDuration   int64
	lastSlowMs    int64
	lastSlowNS    int64
	lastErr5xxNS  int64
	thresholdMs   int64
	// log5xx 是否对 5xx 响应独立输出 WARN 日志, 默认 true
	log5xx atomic.Bool
}

// globalSlowLog 全局慢查询状态
var globalSlowLog = func() *slowLogState {
	s := &slowLogState{
		thresholdMs: 500, // 默认 500ms
	}
	s.log5xx.Store(true) // 默认开启 5xx 日志
	return s
}()

// SetSlowThreshold 设置慢查询阈值(毫秒)
func SetSlowThreshold(ms int64) {
	if ms <= 0 {
		ms = 500
	}
	atomic.StoreInt64(&globalSlowLog.thresholdMs, ms)
}

// GetSlowThreshold 获取慢查询阈值
func GetSlowThreshold() int64 {
	return atomic.LoadInt64(&globalSlowLog.thresholdMs)
}

// SetSlowLog5xx 设置是否记录 5xx 错误日志
func SetSlowLog5xx(enabled bool) {
	globalSlowLog.log5xx.Store(enabled)
}

// GetSlowLog5xx 获取是否记录 5xx 错误日志
func GetSlowLog5xx() bool {
	return globalSlowLog.log5xx.Load()
}

// RecordError5xx 记录一次 5xx 错误
func RecordError5xx() {
	atomic.AddInt64(&globalSlowLog.error5xxCount, 1)
	atomic.StoreInt64(&globalSlowLog.lastErr5xxNS, time.Now().UnixNano())
}

// ApplySlowLogConfig 把 YAML 中的 SlowLogConfig 应用到全局状态.
// 适用于: 启动期初始加载 + 热更新 onChange.
// 零值跳过(由各 Setter 内部处理默认兜底, 如 threshold<=0 -> 500)
func ApplySlowLogConfig(cfg SlowLogConfig) {
	if cfg.ThresholdMs != 0 {
		SetSlowThreshold(cfg.ThresholdMs)
	}
	if cfg.Log5xx != nil {
		SetSlowLog5xx(*cfg.Log5xx)
	}
}

// RecordSlowRequest 记录一个慢请求
func RecordSlowRequest(durationMs int64) {
	atomic.AddInt64(&globalSlowLog.slowCount, 1)
	// 更新最大耗时
	for {
		old := atomic.LoadInt64(&globalSlowLog.maxDuration)
		if durationMs <= old {
			break
		}
		if atomic.CompareAndSwapInt64(&globalSlowLog.maxDuration, old, durationMs) {
			break
		}
	}
	atomic.StoreInt64(&globalSlowLog.lastSlowMs, durationMs)
	atomic.StoreInt64(&globalSlowLog.lastSlowNS, time.Now().UnixNano())
}

// IsSlowRequest 判断是否为慢请求
func IsSlowRequest(durationMs int64) bool {
	threshold := atomic.LoadInt64(&globalSlowLog.thresholdMs)
	return durationMs >= threshold
}

// SlowStats 获取慢查询统计
func SlowStats() SlowLogStats {
	lastSlowNS := atomic.LoadInt64(&globalSlowLog.lastSlowNS)
	lastSlowTime := ""
	if lastSlowNS > 0 {
		lastSlowTime = time.Unix(0, lastSlowNS).UTC().Format(time.RFC3339Nano)
	}
	lastErr5xxNS := atomic.LoadInt64(&globalSlowLog.lastErr5xxNS)
	lastErr5xxTime := ""
	if lastErr5xxNS > 0 {
		lastErr5xxTime = time.Unix(0, lastErr5xxNS).UTC().Format(time.RFC3339Nano)
	}
	return SlowLogStats{
		SlowCount:       atomic.LoadInt64(&globalSlowLog.slowCount),
		Error5xxCount:   atomic.LoadInt64(&globalSlowLog.error5xxCount),
		MaxDuration:     atomic.LoadInt64(&globalSlowLog.maxDuration),
		LastSlowTime:    lastSlowTime,
		LastError5xxTime: lastErr5xxTime,
		ThresholdMs:     atomic.LoadInt64(&globalSlowLog.thresholdMs),
		Log5xx:          GetSlowLog5xx(),
	}
}

// ResetSlowStats 重置慢查询统计
func ResetSlowStats() {
	atomic.StoreInt64(&globalSlowLog.slowCount, 0)
	atomic.StoreInt64(&globalSlowLog.error5xxCount, 0)
	atomic.StoreInt64(&globalSlowLog.maxDuration, 0)
	atomic.StoreInt64(&globalSlowLog.lastSlowMs, 0)
	atomic.StoreInt64(&globalSlowLog.lastSlowNS, 0)
	atomic.StoreInt64(&globalSlowLog.lastErr5xxNS, 0)
}

// withSlowLog 包装 handler,添加慢查询检测与 5xx 错误日志
// 触发 WARN 日志的条件(满足任一):
//   1. 请求耗时 >= slow_request_threshold_ms (默认 500ms) -> 慢请求
//   2. 响应状态码 >= 500 且 log_5xx = true (默认 true) -> 服务端错误
//
// 日志字段均包含完整 req/res 摘要: method, path, route, status, duration_ms,
// username, client_ip, request_id, trace_id, span_id, 便于排障串联.
func (s *Server) withSlowLog(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// 包装 ResponseWriter 以捕获 status code
		sw := &slowLogResponseWriter{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(sw, r)
		durationMs := time.Since(start).Milliseconds()

		rid := requestIDFromCtx(r.Context())
		user := getUsernameFromCtx(r.Context())
		route := routeFromContext(r.Context())
		if route == "" {
			route = normalizeRoute(r.URL.Path)
		}
		traceID, spanID := TraceInfoFromContext(r.Context())
		clientAddr := clientIP(r)
		status := sw.status

		isSlow := IsSlowRequest(durationMs)
		is5xx := status >= 500 && GetSlowLog5xx()

		// 公共字段(慢请求 / 5xx 都复用)
		commonFields := []zap.Field{
			zap.String("request_id", rid),
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path),
			zap.String("route", route),
			zap.Int("status", status),
			zap.Int64("duration_ms", durationMs),
			zap.String("username", user),
			zap.String("client_ip", clientAddr),
			zap.String("trace_id", traceID),
			zap.String("span_id", spanID),
		}

		// 条件1: 慢请求
		if isSlow {
			RecordSlowRequest(durationMs)
			fields := append([]zap.Field{
				zap.Int64("threshold_ms", GetSlowThreshold()),
			}, commonFields...)
			s.logger.Warn("slow request detected", fields...)
		}

		// 条件2: 5xx 服务端错误(独立输出,避免与慢请求重复计数但日志可能双发——故意的,
		// 因为排查 5xx 与排查 slow request 的视角不同)
		if is5xx {
			RecordError5xx()
			s.logger.Warn("server error 5xx response", commonFields...)
		}
	})
}

// handleSlowLogStats GET /_slowlog/stats
func (s *Server) handleSlowLogStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET", "")
		return
	}
	writeJSON(w, http.StatusOK, SlowStats())
}

// handleSlowLogConfig PUT /_slowlog/config
func (s *Server) handleSlowLogConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use PUT", "")
		return
	}
	var req struct {
		ThresholdMs int64 `json:"threshold_ms"`
		Log5xx      *bool `json:"log_5xx,omitempty"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "parse_exception", err.Error(), "")
		return
	}
	updated := false
	if req.ThresholdMs > 0 {
		if req.ThresholdMs > 60000 {
			writeError(w, http.StatusBadRequest, "illegal_argument_exception", "threshold_ms must be 1-60000", "")
			return
		}
		SetSlowThreshold(req.ThresholdMs)
		updated = true
	}
	if req.Log5xx != nil {
		SetSlowLog5xx(*req.Log5xx)
		updated = true
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"threshold_ms": GetSlowThreshold(),
		"log_5xx":      GetSlowLog5xx(),
		"updated":      updated,
	})
}

// slowLogResponseWriter 包装 ResponseWriter 以捕获 status code
type slowLogResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *slowLogResponseWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *slowLogResponseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// handleSlowLogReset POST /_slowlog/reset
func (s *Server) handleSlowLogReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST", "")
		return
	}
	ResetSlowStats()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"reset": true,
	})
}