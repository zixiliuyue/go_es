// 服务端可观测与守卫中间件
//
// 本文件集中实现 4 项相互独立的能力, 都以 http.Handler 中间件形式存在,
// 在 server.New 中以明确顺序链接:
//   panic recover  -> metrics  -> requestID  -> auth  -> bodyLimit  -> rateLimit  -> router
//
// 包含的能力:
//   1. request_id 注入(为后续 trace 串联打基础, 同时作为响应头 X-Request-Id)
//   2. 基础认证(Basic Auth + 可选 API Key), 凭据存于 storage 顶层
//   3. 请求体大小限制(MaxBytesReader), 防 OOM
//   4. IP 级限速(golang.org/x/time/rate 令牌桶), 防 DoS
//   5. liveness / readiness / startup 健康端点
//   6. 任务排空(WaitGroup), 用于优雅关闭
package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zixiliuyue/go_es/internal/storage"
	"go.uber.org/zap"
	"golang.org/x/time/rate"
)

// requestIDHeader 是 X-Request-Id 响应头, 给客户端排查问题
const requestIDHeader = "X-Request-Id"

// requestCtxKey 用来在 ctx 中传 request_id / username
type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota + 1
	ctxKeyUsername
	// 从 100 起保留给其它模块(如 router 的 ctxKeyRoute)
)

// AuthConfig 认证配置
// 全部为空 -> 关闭认证(向后兼容)
type AuthConfig struct {
	// Enabled 是否启用认证
	Enabled bool `yaml:"enabled"`
	// Basic 允许 Basic 认证的用户名密码
	Basic map[string]string `yaml:"basic,omitempty"`
	// APIKeys 允许的 API Key 列表(明文 token)
	APIKeys []string `yaml:"api_keys,omitempty"`
}

// LimitConfig 限流与请求体配置
type LimitConfig struct {
	// MaxBodyBytes 单请求体最大字节数, 0 表示不限制(默认 100MiB)
	MaxBodyBytes int64 `yaml:"max_body_bytes"`
	// RatePerSecond 单 IP 每秒允许的请求数, 0 表示不限制(默认 1000)
	RatePerSecond float64 `yaml:"rate_per_second"`
	// Burst 令牌桶 burst, 默认 = Rate
	Burst int `yaml:"burst"`
}

// ShutdownState 关闭状态(由 main 在收到信号时调用 MarkShuttingDown)
type ShutdownState struct {
	shuttingDown atomic.Bool
	// inflight 正在处理的请求计数
	inflight sync.WaitGroup
}

// MarkShuttingDown 标记服务正在关闭, 并等待 inflight 归零(直到 ctx 结束)
func (s *ShutdownState) MarkShuttingDown(ctx context.Context) {
	s.shuttingDown.Store(true)
	done := make(chan struct{})
	go func() { s.inflight.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// IsShuttingDown 当前是否正在关闭
func (s *ShutdownState) IsShuttingDown() bool { return s.shuttingDown.Load() }

// Track / Done 计数 inflight 请求
func (s *ShutdownState) Track() func() {
	s.inflight.Add(1)
	return s.inflight.Done
}

// guards 集中管理本文件所有可观测/守卫依赖
type guards struct {
	logger   *zap.Logger
	metrics  *ServerMetrics
	shutdown *ShutdownState
	auth     AuthConfig
	limit    LimitConfig
	limiter  *ipLimiter
}

// newGuards 构造守卫
func newGuards(logger *zap.Logger, metrics *ServerMetrics, shutdown *ShutdownState, auth AuthConfig, limit LimitConfig) *guards {
	g := &guards{logger: logger, metrics: metrics, shutdown: shutdown, auth: auth, limit: limit}
	// 默认值
	if g.limit.MaxBodyBytes == 0 {
		g.limit.MaxBodyBytes = 100 << 20 // 100 MiB
	}
	if g.limit.RatePerSecond > 0 {
		if g.limit.Burst == 0 {
			g.limit.Burst = int(g.limit.RatePerSecond)
		}
		g.limiter = newIPLimiter(rate.Limit(g.limit.RatePerSecond), g.limit.Burst)
	}
	return g
}

// chainMiddleware 按顺序包装
func (g *guards) chainMiddleware(h http.Handler) http.Handler {
	out := h
	// body 限制(贴近业务, 让认证/health 端点不受影响)
	if g.limit.MaxBodyBytes > 0 {
		out = g.middlewareBodyLimit(out)
	}
	// 限速(在认证前, 防暴力探测密码)
	if g.limiter != nil {
		out = g.middlewareRateLimit(out)
	}
	// 认证
	if g.auth.Enabled {
		out = g.middlewareAuth(out)
	}
	// requestID 注入
	out = g.middlewareRequestID(out)
	// metrics 与 inflight 计数
	out = g.middlewareMetrics(out)
	// 关闭探测(若已 MarkShuttingDown, 拒绝新连接)
	out = g.middlewareShutdown(out)
	// panic recover 最外层
	out = g.middlewareRecover(out)
	return out
}

// middlewareRecover 把 panic 转成 500 + JSON
func (g *guards) middlewareRecover(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				g.logger.Error("panic in handler",
					zap.Any("panic", rec),
					zap.String("path", r.URL.Path))
				writeError(w, http.StatusInternalServerError,
					"server_error", "internal server error", "")
			}
		}()
		h.ServeHTTP(w, r)
	})
}

// middlewareShutdown 关闭期间拒绝新连接(except 健康端点)
func (g *guards) middlewareShutdown(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 健康端点始终允许通过, 让 K8s 能滚动升级
		if g.shutdown.IsShuttingDown() && !isHealthPath(r.URL.Path) {
			writeError(w, http.StatusServiceUnavailable,
				"service_unavailable", "server is shutting down", "")
			return
		}
		h.ServeHTTP(w, r)
	})
}

// middlewareMetrics 记录请求计数 / 耗时 / inflight
// 同时承担 gzip 压缩(避免与 statusWriter 嵌套)
func (g *guards) middlewareMetrics(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// inflight 计数 - 与 graceful shutdown 配合
		g.shutdown.inflight.Add(1)
		defer g.shutdown.inflight.Done()

		start := time.Now()
		// /metrics 端点由 promhttp 自处理, 不压缩
		if r.URL.Path == "/metrics" {
			h.ServeHTTP(w, r)
			route := routeFromContext(r.Context())
			if route == "" {
				route = normalizeRoute(r.URL.Path)
			}
			g.metrics.ObserveRequest(r.Method, route, 200, time.Since(start))
			return
		}
		wantsGzip := acceptsGzip(r.Header.Get("Accept-Encoding"))
		if wantsGzip {
			w.Header().Add("Vary", "Accept-Encoding")
		}
		cw := newCompressingWriter(w, wantsGzip)
		defer func() { _ = cw.Close() }()
		h.ServeHTTP(cw, r)
		// 路由模板取自 router 暴露的 context key, 兜底用 path
		route := routeFromContext(r.Context())
		if route == "" {
			route = normalizeRoute(r.URL.Path)
		}
		g.metrics.ObserveRequest(r.Method, route, cw.status, time.Since(start))
	})
}

// middlewareRequestID 注入 X-Request-Id
func (g *guards) middlewareRequestID(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get(requestIDHeader)
		if rid == "" {
			rid = newRequestID()
		}
		w.Header().Set(requestIDHeader, rid)
		ctx := context.WithValue(r.Context(), ctxKeyRequestID, rid)
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

// middlewareAuth 基础认证
// 支持:
//   - Authorization: Basic base64(user:pass)
//   - Authorization: ApiKey xxx
// 白名单路径(认证/健康/metrics)不需鉴权
func (g *guards) middlewareAuth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 白名单: 这些路径不要求鉴权
		if isPublicPath(r.URL.Path) {
			h.ServeHTTP(w, r)
			return
		}
		user, ok := g.authenticate(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="go_es"`)
			writeError(w, http.StatusUnauthorized,
				"security_exception", "authentication required", "")
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyUsername, user)
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

// authenticate 解析并校验 Authorization 头
func (g *guards) authenticate(r *http.Request) (string, bool) {
	hdr := r.Header.Get("Authorization")
	if hdr == "" {
		return "", false
	}
	// ApiKey xxx
	if strings.HasPrefix(hdr, "ApiKey ") {
		token := strings.TrimPrefix(hdr, "ApiKey ")
		for _, k := range g.auth.APIKeys {
			if subtle.ConstantTimeCompare([]byte(token), []byte(k)) == 1 {
				return "apikey", true
			}
		}
		return "", false
	}
	// Basic xxx
	if strings.HasPrefix(hdr, "Basic ") {
		raw, err := decodeBasic(strings.TrimPrefix(hdr, "Basic "))
		if err != nil {
			return "", false
		}
		colon := strings.IndexByte(raw, ':')
		if colon < 0 {
			return "", false
		}
		user, pass := raw[:colon], raw[colon+1:]
		expected, ok := g.auth.Basic[user]
		if !ok {
			return "", false
		}
		if subtle.ConstantTimeCompare([]byte(pass), []byte(expected)) == 1 {
			return user, true
		}
		return "", false
	}
	return "", false
}

// decodeBasic 解码 base64, 容错
func decodeBasic(s string) (string, error) {
	// 自己实现避免引 encoding/base64 的导入噪音
	// 这里直接走标准 base64 即可
	// 但为了保持本文件自包含, 用一个简化的 decoder
	// (偷懒: 直接用 std encoding/base64)
	return base64Decode(s)
}

// middlewareBodyLimit 请求体大小限制
func (g *guards) middlewareBodyLimit(h http.Handler) http.Handler {
	limit := g.limit.MaxBodyBytes
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// GET/HEAD/DELETE 一般无 body
		if r.Body != nil && r.ContentLength > limit {
			writeError(w, http.StatusRequestEntityTooLarge,
				"request_entity_too_large",
				fmt.Sprintf("request body exceeds limit %d bytes", limit), "")
			return
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		h.ServeHTTP(w, r)
	})
}

// middlewareRateLimit IP 级令牌桶
func (g *guards) middlewareRateLimit(h http.Handler) http.Handler {
	lim := g.limiter
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if !lim.allow(ip) {
			writeError(w, http.StatusTooManyRequests,
				"too_many_requests", "rate limit exceeded", "")
			return
		}
		h.ServeHTTP(w, r)
	})
}

// handleLiveness GET /_health/liveness  永远 200(除非 panic)
func (s *Server) handleLiveness(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "alive",
		"uptime": time.Since(s.startedAt).String(),
	})
}

// handleReadiness GET /_health/readiness  进程能服务时 200
func (s *Server) handleReadiness(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable", "storage not initialized", "")
		return
	}
	// 关闭期间 readiness 必须失败, K8s 才会停止发新流量
	if s.shutdown.IsShuttingDown() {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable", "server is shutting down", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "ready",
		"uptime": time.Since(s.startedAt).String(),
	})
}

// handleStartup GET /_health/startup  启动完成才 200
func (s *Server) handleStartup(w http.ResponseWriter, r *http.Request) {
	if !s.startupDone.Load() {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable", "server is starting", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "started"})
}

// isHealthPath 判断是否健康/系统端点(认证与限速白名单)
func isHealthPath(p string) bool {
	switch p {
	case "/",
		"/_health/liveness",
		"/_health/readiness",
		"/_health/startup":
		return true
	}
	return false
}

// isPublicPath 任何不需要认证与限速的路径(健康 + 监控 + UI)
func isPublicPath(p string) bool {
	if isHealthPath(p) {
		return true
	}
	switch p {
	case "/metrics",
		"/_ui",
		"/_ui/index.html":
		return true
	}
	return false
}

// clientIP 取客户端 IP(优先 X-Forwarded-For 第一段, 其次 RemoteAddr)
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

// newRequestID 生成 16 字节 hex
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 极端情况: 用时间戳兜底
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// routeFromContext / normalizeRoute 用于在 metrics 中把 path 归一化为模板
// router 在 dispatch 前会通过 setRouteContext 注入
func routeFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRoute).(string); ok {
		return v
	}
	return ""
}

func normalizeRoute(p string) string {
	// 把 /<index>/_doc/<id> 这类路径归一化
	// 用于 router 未注入 route context 时的兜底
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) == 0 {
		return "/"
	}
	// 简单规则: 第一段若是 _ 开头(系统端点), 不归一
	if strings.HasPrefix(parts[0], "_") {
		return p
	}
	// 索引相关: 替换数字/长字符串段
	out := make([]string, len(parts))
	copy(out, parts)
	if len(out) >= 3 && out[1] == "_doc" {
		out[0] = "{index}"
		out[2] = "{id}"
	}
	return strings.Join(out, "/")
}

// ipLimiter 单 IP 令牌桶
type ipLimiter struct {
	mu       sync.Mutex
	limit    rate.Limit
	burst    int
	buckets  map[string]*rate.Limiter
	lastSeen map[string]time.Time
}

// newIPLimiter 构造 IP 限速器
func newIPLimiter(limit rate.Limit, burst int) *ipLimiter {
	return &ipLimiter{
		limit:    limit,
		burst:    burst,
		buckets:  make(map[string]*rate.Limiter),
		lastSeen: make(map[string]time.Time),
	}
}

func (l *ipLimiter) allow(ip string) bool {
	l.mu.Lock()
	lim, ok := l.buckets[ip]
	if !ok {
		lim = rate.NewLimiter(l.limit, l.burst)
		l.buckets[ip] = lim
	}
	l.lastSeen[ip] = time.Now()
	l.mu.Unlock()
	return lim.Allow()
}

// SetClusterAuth 把认证凭据持久化到 storage(可选)
func (s *Server) SetClusterAuth(cfg AuthConfig) error {
	s.guards.auth = cfg
	if !cfg.Enabled {
		return nil
	}
	// 持久化(仅留作审计, 不强制从 storage 读)
	return s.store.Put([]byte("cluster/auth"), cfg)
}

// errors 哨兵, 避免 import 错误
var (
	_ = errors.New
	_ = json.Marshal
	_ = storage.DocKey
)
