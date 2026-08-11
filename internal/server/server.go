// Package server 提供本仓库自研 Elasticsearch 服务端
//
// 这是 go_es 项目从"客户端 SDK"扩展为"客户端 + 服务端"双端实现的服务端部分
// 目标是提供一个最小可运行、URL 与 ES 8 保持一致的服务端,
// 使 examples/main.go 以及单元测试能脱离真实 ES 完成端到端验证
//
// 当前覆盖的 API(以 examples/main.go 调用为准)
//   - 集群: GET /  GET /_cluster/health
//   - 索引: PUT/HEAD/DELETE /<index>  GET /<index>/_mapping
//   - 别名: POST /_aliases   GET /_alias/<name>   HEAD /_alias/<name>
//   - 文档: POST/PUT /<index>/_doc[/<id>]   GET/HEAD/DELETE /<index>/_doc/<id>
//   - 搜索: POST /<index>/_search  POST /_search
//   - 索引模板: PUT/GET/DELETE /_index_template/<name>
//   - 组件模板: PUT/DELETE /_component_template/<name>
//   - ILM: PUT/GET/DELETE /_ilm/policy/<name>  GET /<index>/_ilm/explain
//   - Ingest Pipeline: PUT/GET/DELETE /_ingest/pipeline/<name>  POST /_ingest/pipeline/_simulate
//   - _reindex: POST /_reindex
//   - 快照: PUT/GET/DELETE /_snapshot/<repo>  PUT/GET/DELETE /_snapshot/<repo>/<snap>
//
// 不覆盖的 API 会以 404 响应(本服务用 http.NotFound)。
// 由于仅追求"驱动 examples 跑通",端点集合并非全量 ES 8
package server

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zixiliuyue/go_es/internal/search"
	"github.com/zixiliuyue/go_es/internal/storage"
	"github.com/zixiliuyue/go_es/pkg/cache"
	"go.uber.org/zap"
)

// Server HTTP 服务端
type Server struct {
	store   *storage.Store
	engine  *search.Engine
	logger  *zap.Logger
	mu      sync.Mutex
	started bool
	// cluster 元信息
	clusterName string
	clusterUUID string
	router      *router
	// 可观测/守卫(新加, 向后兼容)
	metrics    *ServerMetrics
	guards     *guards
	shutdown   *ShutdownState
	startedAt  time.Time
	startupDone atomic.Bool
	// RBAC 授权(新加, 索引级 + 操作级)
	rbac *rbac
	// Session 会话管理
	sessionMgr *sessionManager
	// WriteCoordinator 写入事务合并 + 回压
	wc *WriteCoordinator
	// SegmentManager 倒排分段
	seg *SegmentManager
	// ILM 执行器
	ilmExecutor *ILMExecutor
	// AccessLogger 访问日志
	accessLog *AccessLogger
	// HealthChecker 健康检查
	healthChecker *HealthChecker
	healthCheckerMu sync.Mutex
	// 快照目录
	snapDir string
	// ConfigLoader 配置文件加载器(可选, 由 cmd/server 注入)
	configLoader *ConfigLoader
	// TracerProvider 追踪提供者
	tp *TracerProvider
	// Prometheus remote_write 客户端
	remoteWriter *RemoteWriter
	// OpenTelemetry 导出器
	otelExporter *OTelExporter
	// SearchCache 搜索结果 LRU 缓存 (#11)
	searchCache     *cache.Cache
	searchCacheCfg  SearchCacheConfig
}

// ServerOptions 构造服务端的可选配置
type ServerOptions struct {
	// Auth 认证配置(Enabled=false 时不启用, 保持向后兼容)
	Auth AuthConfig
	// Limit 限流与体限制配置
	Limit LimitConfig
	// ConfigLoader 配置文件加载器(可选, 用于 /_config/reload 端点)
	ConfigLoader *ConfigLoader
	// Session 会话管理配置
	Session SessionConfig
	// Tracing 分布式追踪配置
	Tracing TracingConfig
	// RemoteWrite Prometheus remote_write 配置
	RemoteWrite RemoteWriteConfig
	// OTelExport OpenTelemetry 导出配置
	OTelExport OTelExportConfig
	// SearchCache 搜索结果缓存配置
	SearchCache SearchCacheConfig
}

// SearchCacheConfig 搜索结果缓存配置
//
// 字段:
//   - Enabled: 是否启用缓存 (默认 true)
//   - Capacity: 最大缓存条目数 (默认 1000)
//   - MaxSize: 单条缓存响应最大字节数 (默认 64KB, 超过不缓存)
type SearchCacheConfig struct {
	Enabled  bool
	Capacity int
	MaxSize  int
}

// DefaultSearchCacheConfig 返回默认搜索缓存配置
func DefaultSearchCacheConfig() SearchCacheConfig {
	return SearchCacheConfig{
		Enabled:  true,
		Capacity: 1000,
		MaxSize:  64 << 10, // 64KB
	}
}

// New 创建一个新的服务端
// 参数:
//
//	store: 存储后端
//	engine: 搜索引擎
//	logger: zap logger
//
// 返回:
//
//	*Server: 服务端实例
func New(store *storage.Store, engine *search.Engine, logger *zap.Logger) *Server {
	return NewWithOptions(store, engine, logger, ServerOptions{})
}

// NewWithOptions 创建带可选配置的服务端
func NewWithOptions(store *storage.Store, engine *search.Engine, logger *zap.Logger, opts ServerOptions) *Server {
	if logger == nil {
		logger, _ = zap.NewProduction()
	}
	metrics := NewServerMetrics()
	shutdown := &ShutdownState{}
	// Tracing 初始化
	tracingCfg := opts.Tracing
	if tracingCfg.ServiceName == "" {
		tracingCfg = DefaultTracingConfig()
	}
	tp := NewTracerProvider(tracingCfg)

	s := &Server{
		store:       store,
		engine:      engine,
		logger:      logger,
		clusterName: "go_es_cluster",
		clusterUUID: "go-es-self-hosted",
		metrics:     metrics,
		shutdown:    shutdown,
		guards:      newGuards(logger, metrics, shutdown, opts.Auth, opts.Limit),
		rbac:        newRBAC(),
		wc:          NewWriteCoordinator(WriteConfig{MaxConcurrent: 64, MaxBatchSize: 1000}),
		startedAt:   time.Now(),
		configLoader: opts.ConfigLoader,
		tp:          tp,
	}
	// 将 tracerProvider 注入到 guards
	s.guards.tracerProvider = tp
	logger.Info("distributed tracing initialized",
		zap.String("service_name", tracingCfg.ServiceName),
		zap.String("propagation", tracingCfg.Propagation),
		zap.Bool("enabled", tracingCfg.Enabled))
	// Session 会话管理初始化
	sessionCfg := opts.Session
	if sessionCfg.Timeout <= 0 {
		sessionCfg.Timeout = DefaultSessionConfig().Timeout
	}
	if sessionCfg.Secret == "" {
		sessionCfg.Secret = DefaultSessionConfig().Secret
	}
	s.sessionMgr = newSessionManager(s, sessionCfg)
	if sessionCfg.Enabled {
		s.sessionMgr.loadFromStore()
		s.sessionMgr.StartCleanupLoop()
	}
	// 将会话 Token 校验函数注入到 auth 中间件
	if s.sessionMgr != nil {
		s.guards.tokenValidator = func(token string) (string, bool) {
			user, err := s.sessionMgr.validateToken(token)
			return user, err == nil
		}
	}
	// SegmentManager 初始化(在 wc 之后, 因为它依赖 engine)
	s.seg = NewSegmentManager(SegmentConfig{
		MaxBufferDocs:        10000,
		MaxBufferBytes:       64 << 20,
		AutoFlushIntervalSec: 30,
	}, store, engine)
	// SearchCache 初始化 (#11)
	cacheCfg := opts.SearchCache
	if !cacheCfg.Enabled {
		cacheCfg = DefaultSearchCacheConfig()
		cacheCfg.Enabled = false
	}
	if cacheCfg.Capacity <= 0 {
		cacheCfg.Capacity = 1000
	}
	if cacheCfg.MaxSize <= 0 {
		cacheCfg.MaxSize = 64 << 10
	}
	s.searchCacheCfg = cacheCfg
	if cacheCfg.Enabled {
		s.searchCache = cache.New(cacheCfg.Capacity)
		logger.Info("search cache initialized",
			zap.Int("capacity", cacheCfg.Capacity),
			zap.Int("max_size", cacheCfg.MaxSize))
	} else {
		logger.Info("search cache disabled")
	}
	// ILM 执行器初始化(默认 30s 扫描间隔)
	s.ilmExecutor = NewILMExecutor(s, logger, 30*time.Second)
	s.ilmExecutor.Start()
	// AccessLogger 初始化 (默认开启, 写 stdout)
	s.accessLog = NewAccessLogger(AccessLogConfig{
		Enabled:    true,
		BufferSize: 10000,
		SampleRate: 1.0,
	}, logger)
	// AuditLogger 初始化 (默认关闭, 可通过 /_audit/config 启用)
	InitAuditLogger(AuditConfig{
		Enabled:    false,
		BufferSize: 10000,
	}, logger)
	// 启动状态 + 健康检查
	s.healthChecker = NewHealthChecker()
	s.healthChecker.SetState(HealthReady)
	// 简化: 启动即 ready
	s.startupDone.Store(true)
	// 初始化 exporters
	s.initExporters(opts, logger)
	s.router = s.buildRouter()
	return s
}

// Shutdown 标记服务正在关闭并等待 inflight 任务排空(直到 ctx 结束或全部完成)
func (s *Server) Shutdown(ctx context.Context) {
	s.shutdown.MarkShuttingDown(ctx)
	if s.ilmExecutor != nil {
		s.ilmExecutor.Stop()
	}
	if s.sessionMgr != nil {
		s.sessionMgr.Stop()
	}
	// 关闭 exporters, flush 剩余数据
	if s.remoteWriter != nil {
		s.remoteWriter.Close()
	}
	if s.otelExporter != nil {
		s.otelExporter.Close()
	}
}

// MarkStartupDone 启动完成后由 main 调用, 否则 /_health/startup 仍返回 503
func (s *Server) MarkStartupDone() { s.startupDone.Store(true) }

// TracerProvider 获取追踪提供者
func (s *Server) TracerProvider() *TracerProvider { return s.tp }

// Handler 返回 net/http.Handler
func (s *Server) Handler() http.Handler {
	router := http.HandlerFunc(s.router.ServeHTTP)
	// 链路顺序: session -> validation -> cors -> auditLog -> auth -> rbac -> accessLog -> bodyLimit -> rateLimit -> router
	var handler http.Handler = router
	// 审计日志最内层, 记录所有写操作
	handler = s.middlewareAuditLog(handler)
	// 慢查询日志检测
	handler = s.withSlowLog(handler)
	// 访问日志
	handler = s.middlewareAccessLog(handler)
	// RBAC 授权
	handler = s.middlewareRBAC(handler)
	// 会话 Token 校验(在 RBAC 之前, 这样可以覆盖 auth)
	handler = s.middlewareSession(handler)
	// 输入校验
	handler = s.middlewareValidation(handler)
	// CORS
	handler = s.middlewareCORS(handler)
	// 认证
	handler = s.guards.chainMiddleware(handler)
	return handler
}

// buildRouter 构造路由表
// 路由策略: 系统保留前缀(以 _ 开头)优先精确匹配; 其它按"段[0] = index, 段[1:] = rest"兜底
func (s *Server) buildRouter() *router {
	rt := newRouter()
	// 根路径
	rt.addExact("GET", nil, s.handleRoot)
	// go-elasticsearch 客户端的 Ping() 发的是 HEAD /
	rt.addExact("HEAD", nil, s.handleRoot)

	// /_cluster/health
	rt.addExact("GET", []string{"_cluster", "health"}, s.handleClusterHealth)

	// /_settings (返回全部索引的 settings)
	rt.addExact("GET", []string{"_settings"}, s.handleAllSettings)

	// /_aliases (POST)
	rt.addExact("POST", []string{"_aliases"}, s.handleAliasesUpdate)

	// /_alias/{name}
	rt.addExact("GET", []string{"_alias", "{name}"}, s.handleAliasGetByPath)
	rt.addExact("HEAD", []string{"_alias", "{name}"}, s.handleAliasExistsByPath)
	// {name} 在手写路由下是"精确段 = 任意名字",我们直接把第二段取出

	// /_index_template/{name}
	rt.addExact("PUT", []string{"_index_template", "{name}"}, s.handleIndexTemplatePut)
	rt.addExact("GET", []string{"_index_template", "{name}"}, s.handleIndexTemplateGet)
	rt.addExact("DELETE", []string{"_index_template", "{name}"}, s.handleIndexTemplateDelete)
	rt.addExact("POST", []string{"_index_template", "_simulate", "{name}"}, s.handleIndexTemplateSimulate)

	// /_component_template/{name}
	rt.addExact("PUT", []string{"_component_template", "{name}"}, s.handleComponentTemplatePut)
	rt.addExact("DELETE", []string{"_component_template", "{name}"}, s.handleComponentTemplateDelete)

	// /_ilm/policy/{name}
	rt.addExact("PUT", []string{"_ilm", "policy", "{name}"}, s.handleILMPutPolicy)
	rt.addExact("GET", []string{"_ilm", "policy", "{name}"}, s.handleILMGetPolicy)
	rt.addExact("DELETE", []string{"_ilm", "policy", "{name}"}, s.handleILMDeletePolicy)

	// /_ingest/pipeline/{name}
	rt.addExact("PUT", []string{"_ingest", "pipeline", "{name}"}, s.handleIngestPut)
	rt.addExact("GET", []string{"_ingest", "pipeline", "{name}"}, s.handleIngestGet)
	rt.addExact("DELETE", []string{"_ingest", "pipeline", "{name}"}, s.handleIngestDelete)
	rt.addExact("POST", []string{"_ingest", "pipeline", "_simulate"}, s.handleIngestSimulate)

	// /_reindex
	rt.addExact("POST", []string{"_reindex"}, s.handleReindex)

	// /_tasks 与 /_tasks/{id}  异步任务 API
	rt.addExact("GET", []string{"_tasks"}, s.handleTaskList)
	rt.addExact("GET", []string{"_tasks", "{id}"}, s.handleTaskGet)
	rt.addExact("DELETE", []string{"_tasks", "{id}"}, s.handleTaskCancel)

	// /_bulk 与 /<index>/_bulk(esapi.BulkRequest.Do 走 <index>/_bulk)
	rt.addExact("POST", []string{"_bulk"}, s.handleBulk)
	rt.addExact("PUT", []string{"_bulk"}, s.handleBulk)

	// /_search
	rt.addExact("POST", []string{"_search"}, s.handleSearchAll)

	// /_snapshot/{repo}  与  /_snapshot/{repo}/{snap}
	rt.addExact("PUT", []string{"_snapshot", "{repo}"}, s.handleSnapshotRepoPut)
	rt.addExact("DELETE", []string{"_snapshot", "{repo}"}, s.handleSnapshotRepoDelete)
	rt.addExact("PUT", []string{"_snapshot", "{repo}", "{snap}"}, s.handleSnapshotCreate)
	rt.addExact("GET", []string{"_snapshot", "{repo}", "{snap}"}, s.handleSnapshotGet)
	rt.addExact("DELETE", []string{"_snapshot", "{repo}", "{snap}"}, s.handleSnapshotDelete)
	// /_snapshot/{repo}/{snap}/_restore  POST 恢复
	rt.addExact("POST", []string{"_snapshot", "{repo}", "{snap}", "_restore"}, s.handleSnapshotRestore)

	// /_cat/nodes 与 /_cat/indices
	rt.addExact("GET", []string{"_cat", "nodes"}, s.handleCatNodes)
	rt.addExact("GET", []string{"_cat", "indices"}, s.handleCatIndices)

	// /_health/{liveness|readiness|startup}  K8s 探针端点
	rt.addExact("GET", []string{"_health", "liveness"}, s.handleLiveness)
	rt.addExact("GET", []string{"_health", "readiness"}, s.handleReadiness)
	rt.addExact("GET", []string{"_health", "startup"}, s.handleStartup)
	// /_health/status 与 /_health/components  完整健康报告
	rt.addExact("GET", []string{"_health", "status"}, s.handleHealthStatus)
	rt.addExact("GET", []string{"_health", "components"}, s.handleHealthComponents)

	// /_accesslog/stats
	rt.addExact("GET", []string{"_accesslog", "stats"}, s.handleAccessLogStats)

	// /_slowlog/* 慢查询日志端点
	rt.addExact("GET", []string{"_slowlog", "stats"}, s.handleSlowLogStats)
	rt.addExact("PUT", []string{"_slowlog", "config"}, s.handleSlowLogConfig)
	rt.addExact("POST", []string{"_slowlog", "reset"}, s.handleSlowLogReset)

	// /_audit/* 审计日志端点
	rt.addExact("GET", []string{"_audit"}, s.handleAuditQuery)
	rt.addExact("GET", []string{"_audit", "stats"}, s.handleAuditStats)
	rt.addExact("PUT", []string{"_audit", "config"}, s.handleAuditConfig)

	// /_validation/* 输入校验端点
	rt.addExact("GET", []string{"_validation", "config"}, s.handleValidationConfig)
	rt.addExact("PUT", []string{"_validation", "config"}, s.handleValidationConfigUpdate)

	// /_debug/pprof/* 性能剖析端点
	rt.addExact("GET", []string{"_debug", "pprof"}, s.handlePprofIndex)
	rt.addExact("GET", []string{"_debug", "pprof", "cmdline"}, s.handlePprofCmdline)
	rt.addExact("GET", []string{"_debug", "pprof", "profile"}, s.handlePprofProfile)
	rt.addExact("GET", []string{"_debug", "pprof", "symbols"}, s.handlePprofSymbols)
	rt.addExact("GET", []string{"_debug", "pprof", "goroutine"}, s.handlePprofGoroutine)
	rt.addExact("GET", []string{"_debug", "pprof", "heap"}, s.handlePprofHeap)
	rt.addExact("GET", []string{"_debug", "pprof", "threadcreate"}, s.handlePprofThreadcreate)
	rt.addExact("GET", []string{"_debug", "pprof", "allocs"}, s.handlePprofAllocs)
	rt.addExact("GET", []string{"_debug", "pprof", "block"}, s.handlePprofBlock)
	rt.addExact("GET", []string{"_debug", "pprof", "mutex"}, s.handlePprofMutex)

	// /_config/reload 配置热加载端点
	rt.addExact("POST", []string{"_config", "reload"}, s.handleConfigReload)

	// /metrics  Prometheus 抓取端点
	rt.addExact("GET", []string{"metrics"}, s.handleMetrics)

	// /_ui/  与  /_ui/index.html  内置 Web 控制台
	rt.addExact("GET", []string{"_ui"}, s.handleUI)
	rt.addExact("GET", []string{"_ui", "index.html"}, s.handleUI)
	rt.addExact("GET", []string{"_ui", "admin.html"}, s.handleAdminUI)

	// /_security/*  RBAC API
	rt.addExact("GET", []string{"_security", "whoami"}, s.handleWhoAmI)
	rt.addExact("GET", []string{"_security", "user"}, s.handleListUsers)
	rt.addExact("GET", []string{"_security", "user", "{name}"}, s.handleGetUser)
	rt.addExact("POST", []string{"_security", "user", "{name}"}, s.handleCreateUser)
	rt.addExact("PUT", []string{"_security", "user", "{name}"}, s.handleCreateUser)
	rt.addExact("DELETE", []string{"_security", "user", "{name}"}, s.handleDeleteUser)
	rt.addExact("GET", []string{"_security", "role"}, s.handleListRoles)
	rt.addExact("GET", []string{"_security", "role", "{name}"}, s.handleGetRole)
	rt.addExact("POST", []string{"_security", "role", "{name}"}, s.handleCreateRole)
	rt.addExact("PUT", []string{"_security", "role", "{name}"}, s.handleCreateRole)
	rt.addExact("DELETE", []string{"_security", "role", "{name}"}, s.handleDeleteRole)

	// /_security/login 登录/登出/会话管理
	rt.addExact("POST", []string{"_security", "login"}, s.handleLogin)
	rt.addExact("POST", []string{"_security", "logout"}, s.handleLogout)
	rt.addExact("POST", []string{"_security", "logout_all"}, s.handleLogoutAll)
	rt.addExact("GET", []string{"_security", "session"}, s.handleGetCurrentSession)
	rt.addExact("GET", []string{"_security", "session", "stats"}, s.handleSessionStats)
	rt.addExact("GET", []string{"_security", "session", "config"}, s.handleSessionConfig)
	rt.addExact("PUT", []string{"_security", "session", "config"}, s.handleSessionConfig)
	rt.addExact("GET", []string{"_security", "sessions"}, s.handleListSessions)
	rt.addExact("DELETE", []string{"_security", "sessions"}, s.handleRevokeAllSessions)
	rt.addExact("GET", []string{"_security", "sessions", "all"}, s.handleListAllSessions)
	rt.addExact("GET", []string{"_security", "session", "{token}"}, s.handleGetSession)
	rt.addExact("DELETE", []string{"_security", "session", "{token}"}, s.handleRevokeSession)

	// /_security/permission 独立权限管理(细化 RBAC)
	rt.addExact("GET", []string{"_security", "permission"}, s.handleListPermissions)
	rt.addExact("GET", []string{"_security", "permission", "{name}"}, s.handleGetPermission)
	rt.addExact("POST", []string{"_security", "permission", "{name}"}, s.handleCreatePermission)
	rt.addExact("PUT", []string{"_security", "permission", "{name}"}, s.handleCreatePermission)
	rt.addExact("DELETE", []string{"_security", "permission", "{name}"}, s.handleDeletePermission)
	rt.addExact("POST", []string{"_security", "permission", "batch"}, s.handleBatchPermissions)

	// 兜底: {index}/...
	rt.addIndexDispatcher(s.dispatchIndex)
	return rt
}

// dispatchIndex 在"段[0] = index, 段[1:] = rest"形式下的分发
func (s *Server) dispatchIndex(w http.ResponseWriter, r *http.Request, index string, rest []string) {
	method := r.Method
	// index 自身操作
	if len(rest) == 0 {
		switch method {
		case http.MethodPut:
			s.handleIndexCreateForName(w, r, index)
		case http.MethodHead, http.MethodGet:
			s.handleIndexExistsForName(w, r, index)
		case http.MethodDelete:
			s.handleIndexDeleteForName(w, r, index)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	// /{index}/_doc/{id}
	if len(rest) >= 2 && rest[0] == "_doc" {
		switch method {
		case http.MethodPut, http.MethodPost:
			s.handleDocIndexForName(w, r, index, rest[1])
		case http.MethodGet:
			s.handleDocGetForName(w, r, index, rest[1])
		case http.MethodHead:
			s.handleDocExistsForName(w, r, index, rest[1])
		case http.MethodDelete:
			s.handleDocDeleteForName(w, r, index, rest[1])
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	// /{index}/_doc
	if len(rest) == 1 && rest[0] == "_doc" && method == http.MethodPost {
		s.handleDocIndexAutoIDForName(w, r, index)
		return
	}
	// /{index}/_bulk(esapi.BulkRequest.Do 走 <index>/_bulk)
	if len(rest) == 1 && rest[0] == "_bulk" && (method == http.MethodPost || method == http.MethodPut) {
		s.handleBulk(w, r)
		return
	}
	// /{index}/_update/{id}
	if len(rest) == 2 && rest[0] == "_update" && method == http.MethodPost {
		s.handleUpdateForName(w, r, index, rest[1])
		return
	}
	// /{index}/_search
	// {index} 段支持通配: *, prefix*, *suffix, prefix*suffix, idx1,idx2, -foo
	if len(rest) >= 1 && rest[0] == "_search" && method == http.MethodPost {
		// 走专门的 handler, 让它用 getIndicesByPattern 展开
		s.handleSearchForNamePattern(w, r, index)
		return
	}
	// /{index}/_suggest
	if len(rest) >= 1 && rest[0] == "_suggest" && method == http.MethodPost {
		s.handleSuggest(w, r, index)
		return
	}
	// /{index}/_update_by_query
	if len(rest) >= 1 && rest[0] == "_update_by_query" && method == http.MethodPost {
		s.handleUpdateByQuery(w, r, index)
		return
	}
	// /{index}/_delete_by_query
	if len(rest) >= 1 && rest[0] == "_delete_by_query" && method == http.MethodPost {
		s.handleDeleteByQuery(w, r, index)
		return
	}
	// /{index}/_mapping
	if len(rest) >= 1 && rest[0] == "_mapping" && method == http.MethodGet {
		s.handleIndexMappingForName(w, r, index)
		return
	}
	// /{index}/_settings
	if len(rest) >= 1 && rest[0] == "_settings" && method == http.MethodGet {
		s.handleIndexSettingsForName(w, r, index)
		return
	}
	// /{index}/_ilm/explain
	if len(rest) >= 2 && rest[0] == "_ilm" && rest[1] == "explain" && method == http.MethodGet {
		s.handleILMExplainForName(w, r, index)
		return
	}
	// /{index}/_inverted/rebuild
	if len(rest) >= 2 && rest[0] == "_inverted" && rest[1] == "rebuild" && method == http.MethodPost {
		s.handleRebuildInverted(w, r, index)
		return
	}
	// /{index}/_segment/flush
	if len(rest) >= 2 && rest[0] == "_segment" && rest[1] == "flush" && method == http.MethodPost {
		s.handleSegmentFlush(w, r, index)
		return
	}
	// /{index}/_segment/list
	if len(rest) >= 2 && rest[0] == "_segment" && rest[1] == "list" && method == http.MethodGet {
		s.handleSegmentList(w, r, index)
		return
	}
	// /{index}/_segment/stats
	if len(rest) >= 2 && rest[0] == "_segment" && rest[1] == "stats" && method == http.MethodGet {
		s.handleSegmentStats(w, r, index)
		return
	}
	// /{index}/_inverted/info
	if len(rest) >= 2 && rest[0] == "_inverted" && rest[1] == "info" && method == http.MethodGet {
		s.handleInvertedInfo(w, r, index)
		return
	}
	http.NotFound(w, r)
}

// initExporters 根据配置初始化 remote_write 与 OTel 导出器
func (s *Server) initExporters(opts ServerOptions, logger *zap.Logger) {
	// 设置全局 TracerProvider (用于 Span.End() 回调)
	SetGlobalTracerProvider(s.tp)

	// Prometheus remote_write
	if opts.RemoteWrite.Enabled {
		rw, err := NewRemoteWriter(opts.RemoteWrite, logger)
		if err != nil {
			logger.Error("初始化 Prometheus remote_write 客户端失败", zap.Error(err))
		} else {
			s.remoteWriter = rw
			logger.Info("Prometheus remote_write 客户端初始化完成",
				zap.String("endpoint", opts.RemoteWrite.Endpoint))
		}
	}
	// OpenTelemetry 导出器
	if opts.OTelExport.Enabled {
		exp, err := NewOTelExporter(opts.OTelExport, logger)
		if err != nil {
			logger.Error("初始化 OpenTelemetry 导出器失败", zap.Error(err))
		} else {
			s.otelExporter = exp
			logger.Info("OpenTelemetry 导出器初始化完成",
				zap.Bool("metrics", opts.OTelExport.Metrics.Enabled),
				zap.Bool("traces", opts.OTelExport.Traces.Enabled),
				zap.Bool("logs", opts.OTelExport.Logs.Enabled))

			// 将 OTelTracesExporter 注册到 TracerProvider, 实现 span 自动导出
			if exp.TracesExporter() != nil {
				s.tp.SetSpanExporter(exp.TracesExporter())
				logger.Info("OTel traces exporter 已注册到 TracerProvider")
			}
		}
	}
}

// StartExporters 启动所有 exporter 的后台协程(在 HTTP server 启动后调用)
func (s *Server) StartExporters(ctx context.Context) {
	if s.remoteWriter != nil {
		s.remoteWriter.Start(ctx)
	}
	if s.otelExporter != nil {
		s.otelExporter.Start(ctx)
	}
}

// RemoteWriter 返回 remote_write 客户端
func (s *Server) RemoteWriter() *RemoteWriter { return s.remoteWriter }

// OTelExporter 返回 OTel 导出器
func (s *Server) OTelExporter() *OTelExporter { return s.otelExporter }

// invalidateCacheForIndex 按索引名失效所有关联的搜索缓存条目 (#11)
//
// 调用时机: 文档写入/更新/删除、bulk 操作、reindex 完成后
// 若缓存未启用或索引为空, 本函数为 no-op
func (s *Server) invalidateCacheForIndex(index string) {
	if s.searchCache == nil || index == "" {
		return
	}
	s.searchCache.InvalidateIndex(index)
}

// invalidateCacheAll 失效全部搜索缓存条目 (#11)
//
// 调用时机: 索引删除等全局级变更
func (s *Server) invalidateCacheAll() {
	if s.searchCache == nil {
		return
	}
	s.searchCache.InvalidateAll()
}
