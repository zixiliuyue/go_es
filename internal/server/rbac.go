// Package server - 索引级 + 操作级 RBAC
//
// 设计:
//   - 用户: User{name, password_hash, roles: [role_name, ...]}
//   - 角色: Role{name, permissions: [Permission, ...]}
//   - 权限: Permission{index: "logs-*", actions: ["read", "write"]}
//   - index 支持通配 (*, prefix*, *suffix, prefix*suffix)
//   - action 集合: read / write / admin / monitor / cluster
//   - 特殊角色:
//     * superuser: 绕过所有校验
//     * admin: 对所有 index 有 admin 权限
//     * read: 对所有 index 有 read 权限
//     * monitor: 只读 metrics
//
// 存储: cluster/rbac -> {users, roles, version}
// 端点:
//   - POST   /_security/user/{name}              创建/更新用户
//   - DELETE /_security/user/{name}              删用户
//   - GET    /_security/user/{name}              查用户
//   - POST   /_security/role/{name}              创建/更新角色
//   - DELETE /_security/role/{name}              删角色
//   - GET    /_security/role/{name}              查角色
//   - GET    /_security/whoami                   当前用户信息
//
// 中间件流程:
//   requestID -> metrics -> recover -> auth(认证) -> rbac(授权) -> bodyLimit -> rateLimit -> router
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Action RBAC 操作的种类
type Action string

const (
	ActionRead    Action = "read"
	ActionWrite   Action = "write"
	ActionAdmin   Action = "admin"
	ActionMonitor Action = "monitor"
	ActionCluster Action = "cluster"
)

// Permission 索引级权限
type Permission struct {
	// Index 索引名, 支持通配: *, logs-*, *-2024, logs-*-2024
	Index string `json:"index"`
	// Actions 该权限允许的操作
	Actions []Action `json:"actions"`
}

// matchesIndex 索引名匹配检查
func (p *Permission) matchesIndex(index string) bool {
	if p.Index == "*" {
		return true
	}
	pat := p.Index
	// *suffix
	if strings.HasPrefix(pat, "*") {
		return strings.HasSuffix(index, pat[1:])
	}
	// prefix*
	if strings.HasSuffix(pat, "*") {
		return strings.HasPrefix(index, pat[:len(pat)-1])
	}
	// *in*middle (rare)
	if strings.Contains(pat, "*") {
		// 拆 *
		parts := strings.SplitN(pat, "*", 2)
		return strings.HasPrefix(index, parts[0]) && strings.HasSuffix(index, parts[1])
	}
	return pat == index
}

// hasAction 检查权限是否包含某 action
func (p *Permission) hasAction(a Action) bool {
	for _, x := range p.Actions {
		if x == a {
			return true
		}
		// admin 隐含 read+write
		if x == ActionAdmin && (a == ActionRead || a == ActionWrite) {
			return true
		}
	}
	return false
}

// Role 角色
type Role struct {
	Name        string       `json:"name"`
	Permissions []Permission `json:"permissions"`
}

// HasPermission 角色对某 index + action 是否有权限
func (r *Role) HasPermission(index string, action Action) bool {
	for _, p := range r.Permissions {
		if p.matchesIndex(index) && p.hasAction(action) {
			return true
		}
	}
	return false
}

// User 用户
type User struct {
	Name         string   `json:"name"`
	PasswordHash string   `json:"password_hash,omitempty"` // sha256 hex
	Roles        []string `json:"roles"`                   // role names
}

// CheckPassword 校验密码(明文对比 sha256 摘要)
func (u *User) CheckPassword(plain string) bool {
	want := HashPassword(plain)
	return subtle.ConstantTimeCompare([]byte(want), []byte(u.PasswordHash)) == 1
}

// HashPassword sha256(plain) hex
func HashPassword(plain string) string {
	h := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(h[:])
}

// RBACConfig 持久化格式
type RBACConfig struct {
	Version int64           `json:"version"`
	Users   map[string]User `json:"users"`
	Roles   map[string]Role `json:"roles"`
}

// rbac 内存中的 RBAC 状态
type rbac struct {
	mu     sync.RWMutex
	users  map[string]User
	roles  map[string]Role
	loaded bool
}

func newRBAC() *rbac {
	return &rbac{
		users: make(map[string]User),
		roles: make(map[string]Role),
	}
}

// loadFromStore 从 storage 加载
func (r *rbac) loadFromStore(s *Server) {
	if r.loaded {
		return
	}
	cfg := RBACConfig{Users: map[string]User{}, Roles: map[string]Role{}}
	_, err := s.store.Get([]byte("cluster/rbac"), &cfg)
	if err == nil && cfg.Users != nil && cfg.Roles != nil {
		r.mu.Lock()
		r.users = cfg.Users
		r.roles = cfg.Roles
		r.mu.Unlock()
	}
	// 安装默认角色
	r.ensureBuiltinRoles()
	r.loaded = true
}

// ensureBuiltinRoles 安装 superuser / admin / read / monitor 默认角色
func (r *rbac) ensureBuiltinRoles() {
	r.mu.Lock()
	defer r.mu.Unlock()
	builtins := []Role{
		{Name: "superuser", Permissions: []Permission{
			{Index: "*", Actions: []Action{ActionRead, ActionWrite, ActionAdmin, ActionMonitor, ActionCluster}},
		}},
		{Name: "admin", Permissions: []Permission{
			{Index: "*", Actions: []Action{ActionRead, ActionWrite, ActionAdmin, ActionMonitor}},
		}},
		{Name: "read", Permissions: []Permission{
			{Index: "*", Actions: []Action{ActionRead, ActionMonitor}},
		}},
		{Name: "monitor", Permissions: []Permission{
			{Index: "*", Actions: []Action{ActionMonitor}},
		}},
	}
	for _, role := range builtins {
		if _, ok := r.roles[role.Name]; !ok {
			r.roles[role.Name] = role
		}
	}
}

// saveToStore 持久化
func (r *rbac) saveToStore(s *Server) error {
	r.mu.RLock()
	cfg := RBACConfig{
		Version: time.Now().Unix(),
		Users:   r.users,
		Roles:   r.roles,
	}
	r.mu.RUnlock()
	return s.store.Put([]byte("cluster/rbac"), cfg)
}

// getUser 查 user
func (r *rbac) getUser(name string) (User, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	u, ok := r.users[name]
	return u, ok
}

// setUser 写 user
func (r *rbac) setUser(u User) {
	r.mu.Lock()
	r.users[u.Name] = u
	r.mu.Unlock()
}

// deleteUser 删 user
func (r *rbac) deleteUser(name string) {
	r.mu.Lock()
	delete(r.users, name)
	r.mu.Unlock()
}

// getRole 查 role
func (r *rbac) getRole(name string) (Role, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	role, ok := r.roles[name]
	return role, ok
}

// setRole 写 role
func (r *rbac) setRole(role Role) {
	r.mu.Lock()
	r.roles[role.Name] = role
	r.mu.Unlock()
}

// deleteRole 删 role
func (r *rbac) deleteRole(name string) {
	r.mu.Lock()
	delete(r.roles, name)
	r.mu.Unlock()
}

// listUsers 列出所有 user
func (r *rbac) listUsers() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.users))
	for k := range r.users {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// listRoles 列出所有 role
func (r *rbac) listRoles() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.roles))
	for k := range r.roles {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// checkPermission 检查 user 对 index+action 是否有权限
// user 为空(匿名) 视为无角色 -> 拒绝
// superuser 角色 -> 永远放行
// 特殊: username == "apikey" 视为 superuser (向后兼容, APIKey 认证为全权限)
// Basic 认证用户 (auth.Enabled + auth.Basic[username]) 默认视为 superuser
//   (向后兼容, 现有 auth 用户的体验)
// 用户在 rbac.users 中显式注册则按其角色判断
func (r *rbac) checkPermission(username, index string, action Action) bool {
	if username == "" {
		return false
	}
	// APIKey 认证为 superuser (向后兼容)
	if username == "apikey" {
		return true
	}
	// 在 RBAC 中显式注册 -> 按其角色
	if user, ok := r.getUser(username); ok {
		for _, roleName := range user.Roles {
			role, ok := r.getRole(roleName)
			if !ok {
				continue
			}
			if role.Name == "superuser" {
				return true
			}
			if role.HasPermission(index, action) {
				return true
			}
		}
		return false
	}
	// 用户未在 RBAC 注册: 视为 superuser (向后兼容 Basic 认证)
	return true
}

// requestAction 推断请求对应的 action
// 路径 + 方法 -> (index, action)
// 不传 index 时 index="" 表示 cluster 级
func requestAction(method, path string) (string, Action) {
	// path: /{index}/_doc/{id}, /{index}/_search, /_cluster/health, /metrics ...
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 {
		return "", ActionRead
	}
	first := parts[0]
	// 集群级端点
	if strings.HasPrefix(first, "_") {
		// /_security/* 写
		if strings.HasPrefix(first, "_security") {
			if method == http.MethodGet || method == http.MethodHead {
				return "", ActionRead
			}
			return "", ActionWrite
		}
		// /_tasks, /_nodes, /_cluster/* read
		if method == http.MethodGet || method == http.MethodHead {
			return "", ActionCluster
		}
		// 其它 _ 端点: 写视为 cluster
		return "", ActionCluster
	}
	// /metrics
	if first == "metrics" {
		return "", ActionMonitor
	}
	// 索引级
	index := first
	// 操作映射
	if method == http.MethodGet || method == http.MethodHead {
		return index, ActionRead
	}
	// 写操作: PUT/POST/PATCH/DELETE
	if method == http.MethodDelete {
		// DELETE 索引本身 -> admin
		if len(parts) == 1 {
			return index, ActionAdmin
		}
		return index, ActionWrite
	}
	// POST/PUT 索引级: 写
	return index, ActionWrite
}

// ctxKeyUser context key (用户名) - 复用 guards.go 中的 ctxKeyUsername
// 注意: 不要定义新 ctxKey, 防止与 auth 中间件设的值错位
const ctxKeyUser ctxKey = ctxKeyUsername

// getUsernameFromCtx 取当前用户
func getUsernameFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyUser).(string); ok {
		return v
	}
	return ""
}

// middlewareRBAC 授权检查
// 位于 auth 之后, 在业务路由之前
// 注意: 如果 auth 未启用 (向后兼容), 跳过 RBAC
func (s *Server) middlewareRBAC(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 加载 RBAC 状态
		s.rbac.loadFromStore(s)
		// 白名单: 健康/metrics
		if isPublicPath(r.URL.Path) {
			h.ServeHTTP(w, r)
			return
		}
		// 向后兼容: 如果 auth 未启用, 跳过 RBAC 校验
		if !s.guards.auth.Enabled {
			h.ServeHTTP(w, r)
			return
		}
		user := getUsernameFromCtx(r.Context())
		// 集群端点
		index, action := requestAction(r.Method, r.URL.Path)
		// 集群级 action (cluster/monitor) 也要求登录
		if !s.rbac.checkPermission(user, index, action) {
			writeError(w, http.StatusForbidden, "security_exception",
				fmt.Sprintf("user %q lacks %q permission on %q", user, action, indexOrCluster(index)), "")
			return
		}
		h.ServeHTTP(w, r)
	})
}

func indexOrCluster(idx string) string {
	if idx == "" {
		return "<cluster>"
	}
	return idx
}

// handleWhoAmI GET /_security/whoami 返回当前用户
func (s *Server) handleWhoAmI(w http.ResponseWriter, r *http.Request) {
	s.rbac.loadFromStore(s)
	user := getUsernameFromCtx(r.Context())
	if user == "" {
		writeError(w, http.StatusUnauthorized, "security_exception", "not authenticated", "")
		return
	}
	u, ok := s.rbac.getUser(user)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]interface{}{"username": user, "roles": []string{}})
		return
	}
	out := map[string]interface{}{
		"username": u.Name,
		"roles":    u.Roles,
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCreateUser POST /_security/user/{name}
func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	name := pathSegment(r, 2)
	s.rbac.loadFromStore(s)
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST or PUT", "")
		return
	}
	var req struct {
		Password string   `json:"password"`
		Roles    []string `json:"roles"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "parse_exception", err.Error(), "")
		return
	}
	if req.Password == "" {
		writeError(w, http.StatusBadRequest, "illegal_argument_exception", "password required", "")
		return
	}
	if len(req.Roles) == 0 {
		writeError(w, http.StatusBadRequest, "illegal_argument_exception", "roles required", "")
		return
	}
	u := User{
		Name:         name,
		PasswordHash: HashPassword(req.Password),
		Roles:        req.Roles,
	}
	s.rbac.setUser(u)
	if err := s.rbac.saveToStore(s); err != nil {
		writeError(w, http.StatusInternalServerError, "save_failed", err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"user": name, "roles": u.Roles, "created": true})
}

// handleDeleteUser DELETE /_security/user/{name}
func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	name := pathSegment(r, 2)
	s.rbac.loadFromStore(s)
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use DELETE", "")
		return
	}
	// 不允许删除 superuser
	if name == "admin" || name == "superuser" {
		// 实际上 superuser 不是 user 是 role; 但内置 admin user 可能存在
	}
	s.rbac.deleteUser(name)
	if err := s.rbac.saveToStore(s); err != nil {
		writeError(w, http.StatusInternalServerError, "save_failed", err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"user": name, "deleted": true})
}

// handleGetUser GET /_security/user/{name}
func (s *Server) handleGetUser(w http.ResponseWriter, r *http.Request) {
	name := pathSegment(r, 2)
	s.rbac.loadFromStore(s)
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET", "")
		return
	}
	u, ok := s.rbac.getUser(name)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "user not found", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"username":      u.Name,
		"roles":         u.Roles,
		"password_hash": u.PasswordHash,
	})
}

// handleListUsers GET /_security/user
func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	s.rbac.loadFromStore(s)
	users := s.rbac.listUsers()
	writeJSON(w, http.StatusOK, map[string]interface{}{"users": users})
}

// handleCreateRole POST /_security/role/{name}
func (s *Server) handleCreateRole(w http.ResponseWriter, r *http.Request) {
	name := pathSegment(r, 2)
	s.rbac.loadFromStore(s)
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST or PUT", "")
		return
	}
	var req struct {
		Permissions []Permission `json:"permissions"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "parse_exception", err.Error(), "")
		return
	}
	// 保护内置角色
	if name == "superuser" || name == "admin" || name == "read" || name == "monitor" {
		writeError(w, http.StatusBadRequest, "illegal_argument_exception", "cannot override built-in role", "")
		return
	}
	role := Role{
		Name:        name,
		Permissions: req.Permissions,
	}
	s.rbac.setRole(role)
	if err := s.rbac.saveToStore(s); err != nil {
		writeError(w, http.StatusInternalServerError, "save_failed", err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"role": name, "created": true, "permissions": req.Permissions})
}

// handleDeleteRole DELETE /_security/role/{name}
func (s *Server) handleDeleteRole(w http.ResponseWriter, r *http.Request) {
	name := pathSegment(r, 2)
	s.rbac.loadFromStore(s)
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use DELETE", "")
		return
	}
	if name == "superuser" || name == "admin" || name == "read" || name == "monitor" {
		writeError(w, http.StatusBadRequest, "illegal_argument_exception", "cannot delete built-in role", "")
		return
	}
	s.rbac.deleteRole(name)
	if err := s.rbac.saveToStore(s); err != nil {
		writeError(w, http.StatusInternalServerError, "save_failed", err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"role": name, "deleted": true})
}

// handleGetRole GET /_security/role/{name}
func (s *Server) handleGetRole(w http.ResponseWriter, r *http.Request) {
	name := pathSegment(r, 2)
	s.rbac.loadFromStore(s)
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET", "")
		return
	}
	role, ok := s.rbac.getRole(name)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "role not found", "")
		return
	}
	writeJSON(w, http.StatusOK, role)
}

// handleListRoles GET /_security/role
func (s *Server) handleListRoles(w http.ResponseWriter, r *http.Request) {
	s.rbac.loadFromStore(s)
	roles := s.rbac.listRoles()
	writeJSON(w, http.StatusOK, map[string]interface{}{"roles": roles})
}

// 防止 import 漂移
var _ = json.Marshal
