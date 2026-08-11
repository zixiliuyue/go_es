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
	MaxDuration  int64         `json:"max_duration_ms"`
	LastSlowTime string        `json:"last_slow_time,omitempty"`
	ThresholdMs  int64         `json:"threshold_ms"`
}

// slowLogState 慢查询状态(线程安全)
type slowLogState struct {
	slowCount   int64
	maxDuration int64
	lastSlowMs  int64
	lastSlowNS  int64
	thresholdMs int64
}

// globalSlowLog 全局慢查询状态
var globalSlowLog = &slowLogState{
	thresholdMs: 500, // 默认 500ms
}

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
	lastNS := atomic.LoadInt64(&globalSlowLog.lastSlowNS)
	lastTime := ""
	if lastNS > 0 {
		lastTime = time.Unix(0, lastNS).UTC().Format(time.RFC3339Nano)
	}
	return SlowLogStats{
		SlowCount:    atomic.LoadInt64(&globalSlowLog.slowCount),
		MaxDuration:  atomic.LoadInt64(&globalSlowLog.maxDuration),
		LastSlowTime: lastTime,
		ThresholdMs:  atomic.LoadInt64(&globalSlowLog.thresholdMs),
	}
}

// ResetSlowStats 重置慢查询统计
func ResetSlowStats() {
	atomic.StoreInt64(&globalSlowLog.slowCount, 0)
	atomic.StoreInt64(&globalSlowLog.maxDuration, 0)
	atomic.StoreInt64(&globalSlowLog.lastSlowMs, 0)
	atomic.StoreInt64(&globalSlowLog.lastSlowNS, 0)
}

// withSlowLog 包装 handler,添加慢查询检测
// 如果请求耗时超过阈值,在日志中标记为 warn 级别
func (s *Server) withSlowLog(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		durationMs := time.Since(start).Milliseconds()

		if IsSlowRequest(durationMs) {
			RecordSlowRequest(durationMs)
			// 通过访问日志输出慢请求警告
			rid := requestIDFromCtx(r.Context())
			user := getUsernameFromCtx(r.Context())
			route := routeFromContext(r.Context())
			if route == "" {
				route = normalizeRoute(r.URL.Path)
			}
			traceID, spanID := TraceInfoFromContext(r.Context())
			s.logger.Warn("slow request detected",
				zap.String("request_id", rid),
				zap.String("method", r.Method),
				zap.String("path", r.URL.Path),
				zap.String("route", route),
				zap.Int64("duration_ms", durationMs),
				zap.Int64("threshold_ms", GetSlowThreshold()),
				zap.String("username", user),
				zap.String("client_ip", clientIP(r)),
				zap.String("trace_id", traceID),
				zap.String("span_id", spanID),
			)
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
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "parse_exception", err.Error(), "")
		return
	}
	if req.ThresholdMs <= 0 || req.ThresholdMs > 60000 {
		writeError(w, http.StatusBadRequest, "illegal_argument_exception", "threshold_ms must be 1-60000", "")
		return
	}
	SetSlowThreshold(req.ThresholdMs)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"threshold_ms": GetSlowThreshold(),
		"updated":      true,
	})
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