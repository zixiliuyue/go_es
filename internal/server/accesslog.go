// 结构化访问日志
//
// 设计:
//   - 每个 HTTP 请求一条 JSON 行
//   - 字段: timestamp, request_id, client_ip, method, path, query, status,
//     duration_ms, body_size, user_agent, username, route, response_size
//   - 异步写入 (buffered chan + flush goroutine)
//   - 文件或 stdout 两种 sink
//   - 采样 (sample_rate 0-1)
//   - 不阻塞: chan 满时丢弃并自增 drop 计数
package server

import (
	"encoding/json"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// AccessLogConfig 访问日志配置
type AccessLogConfig struct {
	// Enabled 启用访问日志 (默认 true)
	Enabled bool
	// FilePath 写入文件路径, 留空 -> stdout
	FilePath string
	// BufferSize 内存 buffer 大小 (行数), 满则丢弃
	BufferSize int
	// SampleRate 采样率 0-1, 默认 1 (全采样)
	SampleRate float64
	// Fields 包含/排除字段控制 (暂不实现, 留作扩展)
}

// AccessLogEntry 访问日志单行
type AccessLogEntry struct {
	Timestamp    string `json:"@timestamp"`
	Level        string `json:"level"`
	RequestID    string `json:"request_id"`
	ClientIP     string `json:"client_ip"`
	Method       string `json:"method"`
	Path         string `json:"path"`
	Query        string `json:"query,omitempty"`
	Route        string `json:"route,omitempty"`
	Status       int    `json:"status"`
	DurationMs   int64  `json:"duration_ms"`
	BodySize     int64  `json:"body_size"`
	ResponseSize int64  `json:"response_size,omitempty"`
	UserAgent    string `json:"user_agent,omitempty"`
	Username     string `json:"username,omitempty"`
	TraceID      string `json:"trace_id,omitempty"`
	SpanID       string `json:"span_id,omitempty"`
}

// AccessLogger 访问日志器
type AccessLogger struct {
	mu       sync.Mutex
	cfg      AccessLogConfig
	ch       chan AccessLogEntry
	stopped  chan struct{}
	stopOnce sync.Once
	writer   *os.File
	logger   *zap.Logger

	// 统计
	stats AccessLogStats
}

// AccessLogStats 访问日志统计
type AccessLogStats struct {
	Written int64 `json:"written"`
	Dropped int64 `json:"dropped"`
	Bytes   int64 `json:"bytes"`
}

// NewAccessLogger 构造
func NewAccessLogger(cfg AccessLogConfig, logger *zap.Logger) *AccessLogger {
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = 10000
	}
	if cfg.SampleRate <= 0 {
		cfg.SampleRate = 1.0
	}
	al := &AccessLogger{
		cfg:     cfg,
		ch:      make(chan AccessLogEntry, cfg.BufferSize),
		stopped: make(chan struct{}),
		logger:  logger,
	}
	if cfg.FilePath != "" {
		f, err := os.OpenFile(cfg.FilePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			logger.Warn("access log open file failed, fallback to stdout", zap.Error(err))
		} else {
			al.writer = f
		}
	}
	if al.writer == nil {
		al.writer = os.Stdout
	}
	if al.cfg.Enabled {
		go al.run()
	}
	return al
}

// run 异步消费 channel
func (al *AccessLogger) run() {
	for entry := range al.ch {
		b, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		b = append(b, '\n')
		_, _ = al.writer.Write(b)
		atomic.AddInt64(&al.stats.Written, 1)
		atomic.AddInt64(&al.stats.Bytes, int64(len(b)))
	}
	close(al.stopped)
}

// Log 异步写一条日志 (chan 满则丢弃并计数)
func (al *AccessLogger) Log(entry AccessLogEntry) {
	if !al.cfg.Enabled {
		return
	}
	// 采样
	if al.cfg.SampleRate < 1.0 {
		if rand.Float64() > al.cfg.SampleRate {
			return
		}
	}
	select {
	case al.ch <- entry:
	default:
		atomic.AddInt64(&al.stats.Dropped, 1)
	}
}

// Close 关闭 (flush)
func (al *AccessLogger) Close() error {
	al.stopOnce.Do(func() {
		// Enabled=false 时不启动 run goroutine, 也不需要 close ch
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

// Stats 统计
func (al *AccessLogger) Stats() AccessLogStats {
	return AccessLogStats{
		Written: atomic.LoadInt64(&al.stats.Written),
		Dropped: atomic.LoadInt64(&al.stats.Dropped),
		Bytes:   atomic.LoadInt64(&al.stats.Bytes),
	}
}

// middlewareAccessLog 访问日志中间件
// 在所有其它中间件之后, router 之前
func (s *Server) middlewareAccessLog(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.accessLog == nil || !s.accessLog.cfg.Enabled {
			h.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		// 用 statusRecorder 拿 status code
		sr := &statusRecorder{ResponseWriter: w, status: 200}
		// 拿 body size (Content-Length)
		var reqSize int64
		if r.ContentLength > 0 {
			reqSize = r.ContentLength
		}
		h.ServeHTTP(sr, r)
		// 构造日志
		rid := requestIDFromCtx(r.Context())
		user := getUsernameFromCtx(r.Context())
		ua := r.Header.Get("User-Agent")
		query := r.URL.RawQuery
		route := routeFromContext(r.Context())
		if route == "" {
			route = normalizeRoute(r.URL.Path)
		}
		traceID, spanID := TraceInfoFromContext(r.Context())
		entry := AccessLogEntry{
			Timestamp:    time.Now().UTC().Format(time.RFC3339Nano),
			Level:        "info",
			RequestID:    rid,
			ClientIP:     clientIP(r),
			Method:       r.Method,
			Path:         r.URL.Path,
			Query:        query,
			Route:        route,
			Status:       sr.status,
			DurationMs:   time.Since(start).Milliseconds(),
			BodySize:     reqSize,
			ResponseSize: sr.bytes,
			UserAgent:    ua,
			Username:     user,
			TraceID:      traceID,
			SpanID:       spanID,
		}
		s.accessLog.Log(entry)
	})
}

// statusRecorder 记录 status code 与 bytes
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

// requestIDFromCtx 取 request_id
func requestIDFromCtx(ctx interface{ Value(interface{}) interface{} }) string {
	// 简单实现, 与 guards 中的 ctxKeyRequestID 共享
	if v := ctx.Value(ctxKeyRequestID); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// handleAccessLogStats GET /_accesslog/stats
func (s *Server) handleAccessLogStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET", "")
		return
	}
	if s.accessLog == nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable", "access log not initialized", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"stats":   s.accessLog.Stats(),
		"enabled": s.accessLog.cfg.Enabled,
		"file":    s.accessLog.cfg.FilePath,
	})
}
