// 手写路由分发器, 替代 net/http.ServeMux
//
// 背景: Go 1.22+ 的 ServeMux 严格按 pattern 解析, 不允许 GET /_alias/{name}
// 与 GET /{index}/_mapping 同时存在(因为它们都会匹配 /_alias/_mapping)。
// ES 自身的 URL 模型允许同一 URL 段在不同上下文下具有不同语义
// (例如 /_alias 和 /_cluster 都是"系统保留索引"),
// 因此我们用一段简化的分发逻辑:
//
//  1. 解析 METHOD 与 URL 段
//  2. 系统保留前缀优先: /_cluster/... /_aliases /_alias/... /_index_template/...
//     /_component_template/... /_ilm/policy/... /_ingest/pipeline/...
//     /_snapshot/... /_reindex /_search /_go_es/...
//  3. 其它按 "{index}/..." 形式分派到索引/文档/搜索
//
// 这种"按段优先"的分发比 ServeMux 更贴合 ES,但仍然不解析 ?query=1 等
// 任何 URL 细节(URL 解析由 std http.Request.URL 提供)
//
// 同时 router 在分发时把"路由模板"(如 /_alias/{name})写入 context,
// 供 metrics 中间件做低基数打标.
package server

import (
	"context"
	"net/http"
	"strings"
)

// routeCtxKey 用于在 context 中传路由模板
// 直接使用 guards.go 中声明的 ctxKey 类型, 这里只定义常量
const ctxKeyRoute ctxKey = 100

// withRoute 把路由模板注入到 context
func withRoute(parent context.Context, route string) context.Context {
	return context.WithValue(parent, ctxKeyRoute, route)
}

// routeKey 路由查找键: METHOD + 段列表
// 仅作类型文档,真实 key 是用 makeKey 拼出的字符串(map 键需可比较)

// router 内部路由器
type router struct {
	// exact 精确路径(method+parts)
	exact map[string]routeEntry
	// indexDispatch 兜底函数
	indexDispatch func(w http.ResponseWriter, r *http.Request, index string, rest []string)
}

// routeEntry 记录精确路由的 handler 与模板字符串
type routeEntry struct {
	handler http.HandlerFunc
	// template 路由模板字符串, 如 /_alias/{name} 或 nil(未知)
	template string
}

// newRouter 创建一个空 router
func newRouter() *router {
	return &router{exact: make(map[string]routeEntry)}
}

// addExact 注册一个精确路由
// parts 中可以包含以 {xxx} 包裹的"变量占位段",例如 {"_alias", "{name}"}
// 匹配时该段会与 URL 的实际段等长且相等即视为命中
// 变量名(去掉 {})会出现在 matchedVars 中,业务 handler 通过 r.PathValue 等
// 不能直接拿到(因为我们没走 ServeMux),需要从 handler 内部自行取 r.URL.Path
func (rt *router) addExact(method string, parts []string, h http.HandlerFunc) {
	tpl := "/" + strings.Join(parts, "/")
	rt.exact[makeKey(method, parts)] = routeEntry{handler: h, template: tpl}
}

// makeKey 把 method+parts 拼成可比较的字符串 key
func makeKey(method string, parts []string) string {
	return method + " " + strings.Join(parts, "/")
}

// addIndexDispatcher 注册"按 index 派发"的兜底函数
func (rt *router) addIndexDispatcher(fn func(w http.ResponseWriter, r *http.Request, index string, rest []string)) {
	rt.indexDispatch = fn
}

// ServeHTTP 是 router 自身的 http.Handler
func (rt *router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// go-elasticsearch v8 客户端在建立连接时会做"产品嗅探":
	// 期望服务端在响应中带 X-Elastic-Product: Elasticsearch 头
	// 我们用自研服务端冒充 ES 时,需要补上此头才能让 SDK 客户端接受
	w.Header().Set("X-Elastic-Product", "Elasticsearch")

	path := strings.Trim(r.URL.Path, "/")
	if path == "" {
		// 根路径
		if e, ok := rt.exact[makeKey(r.Method, nil)]; ok {
			r = r.WithContext(withRoute(r.Context(), e.template))
			e.handler(w, r)
			return
		}
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(path, "/")
	// 先尝试精确匹配(支持 {var} 通配段)
	if e, h := rt.lookup(r.Method, parts); h != nil {
		r = r.WithContext(withRoute(r.Context(), e.template))
		h(w, r)
		return
	}
	if len(parts) >= 1 && rt.indexDispatch != nil {
		// 兜底: 第一个段视为 index, 剩余为 rest
		// 模板用 {index}/<rest> 表示
		tpl := "{index}"
		if len(parts) > 1 {
			tpl = "{index}/" + strings.Join(parts[1:], "/")
		}
		r = r.WithContext(withRoute(r.Context(), "/"+tpl))
		rt.indexDispatch(w, r, parts[0], parts[1:])
		return
	}
	http.NotFound(w, r)
}

// lookup 在路由表中查找 method+parts
// 其中 parts 中的 {xxx} 段视为通配(任意单段)
// 优先匹配不含变量的精确路由, 再匹配含变量的路由
func (rt *router) lookup(method string, parts []string) (routeEntry, http.HandlerFunc) {
	var bestEntry routeEntry
	var bestHandler http.HandlerFunc
	bestHasVar := false

	for key, e := range rt.exact {
		kmethod, kparts := splitKey(key)
		if kmethod != method {
			continue
		}
		if len(kparts) != len(parts) {
			continue
		}
		ok := true
		hasVar := false
		for i := range kparts {
			if isVar(kparts[i]) {
				hasVar = true
				continue
			}
			if kparts[i] != parts[i] {
				ok = false
				break
			}
		}
		if ok {
			if !hasVar {
				// 精确匹配优先, 立即返回
				return e, e.handler
			}
			if !bestHasVar {
				bestEntry = e
				bestHandler = e.handler
				bestHasVar = true
			}
		}
	}
	return bestEntry, bestHandler
}

// splitKey 把 makeKey 拼出的字符串拆回 (method, parts)
func splitKey(key string) (string, []string) {
	idx := strings.IndexByte(key, ' ')
	if idx < 0 {
		return key, nil
	}
	return key[:idx], strings.Split(key[idx+1:], "/")
}

// isVar 判读一个段是否"变量占位"(用 {name} 表示)
func isVar(seg string) bool {
	return len(seg) >= 2 && seg[0] == '{' && seg[len(seg)-1] == '}'
}
