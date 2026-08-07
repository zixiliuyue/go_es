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
	// WriteCoordinator 写入事务合并 + 回压
	wc *WriteCoordinator
}

// ServerOptions 构造服务端的可选配置
type ServerOptions struct {
	// Auth 认证配置(Enabled=false 时不启用, 保持向后兼容)
	Auth AuthConfig
	// Limit 限流与体限制配置
	Limit LimitConfig
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
	}
	s.router = s.buildRouter()
	return s
}

// Shutdown 标记服务正在关闭并等待 inflight 任务排空(直到 ctx 结束或全部完成)
func (s *Server) Shutdown(ctx context.Context) { s.shutdown.MarkShuttingDown(ctx) }

// MarkStartupDone 启动完成后由 main 调用, 否则 /_health/startup 仍返回 503
func (s *Server) MarkStartupDone() { s.startupDone.Store(true) }

// Handler 返回 net/http.Handler
func (s *Server) Handler() http.Handler {
	router := http.HandlerFunc(s.router.ServeHTTP)
	// 链路顺序: auth (in chainMiddleware) -> rbac -> bodyLimit -> rateLimit -> router
	return s.guards.chainMiddleware(s.middlewareRBAC(router))
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

	// /_cat/nodes 与 /_cat/indices
	rt.addExact("GET", []string{"_cat", "nodes"}, s.handleCatNodes)
	rt.addExact("GET", []string{"_cat", "indices"}, s.handleCatIndices)

	// /_health/{liveness|readiness|startup}  K8s 探针端点
	rt.addExact("GET", []string{"_health", "liveness"}, s.handleLiveness)
	rt.addExact("GET", []string{"_health", "readiness"}, s.handleReadiness)
	rt.addExact("GET", []string{"_health", "startup"}, s.handleStartup)

	// /metrics  Prometheus 抓取端点
	rt.addExact("GET", []string{"metrics"}, s.handleMetrics)

	// /_ui/  与  /_ui/index.html  内置 Web 控制台
	rt.addExact("GET", []string{"_ui"}, s.handleUI)
	rt.addExact("GET", []string{"_ui", "index.html"}, s.handleUI)

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
	// /{index}/_inverted/info
	if len(rest) >= 2 && rest[0] == "_inverted" && rest[1] == "info" && method == http.MethodGet {
		s.handleInvertedInfo(w, r, index)
		return
	}
	http.NotFound(w, r)
}
