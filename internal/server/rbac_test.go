// Package server - RBAC 单元测试
package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/zixiliuyue/go_es/internal/search"
	"github.com/zixiliuyue/go_es/internal/storage"
)

func newRBACTestServer(t *testing.T) *Server {
	t.Helper()
	store, err := storage.Open("")
	assert.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	engine := search.New(store)
	s := &Server{store: store, engine: engine, rbac: newRBAC()}
	s.rbac.loadFromStore(s)
	return s
}

// TestPermission_MatchIndex 索引通配
func TestPermission_MatchIndex(t *testing.T) {
	cases := []struct {
		pat, index string
		want       bool
	}{
		{"*", "logs", true},
		{"logs", "logs", true},
		{"logs", "users", false},
		{"logs-*", "logs-2024", true},
		{"logs-*", "metrics-2024", false},
		{"*-2024", "logs-2024", true},
		{"*-2024", "logs-2025", false},
		{"logs-*-bak", "logs-2024-bak", true},
		{"logs-*-bak", "logs-2024-old", false},
	}
	for _, c := range cases {
		p := &Permission{Index: c.pat}
		assert.Equal(t, c.want, p.matchesIndex(c.index), "pat=%s index=%s", c.pat, c.index)
	}
}

// TestPermission_HasAction
func TestPermission_HasAction(t *testing.T) {
	p := &Permission{Index: "*", Actions: []Action{ActionRead, ActionWrite}}
	assert.True(t, p.hasAction(ActionRead))
	assert.True(t, p.hasAction(ActionWrite))
	assert.False(t, p.hasAction(ActionAdmin))
	assert.False(t, p.hasAction(ActionMonitor))

	// admin 隐含 read+write
	p2 := &Permission{Index: "*", Actions: []Action{ActionAdmin}}
	assert.True(t, p2.hasAction(ActionRead))
	assert.True(t, p2.hasAction(ActionWrite))
	assert.True(t, p2.hasAction(ActionAdmin))
}

// TestRole_HasPermission
func TestRole_HasPermission(t *testing.T) {
	r := &Role{
		Name: "writer",
		Permissions: []Permission{
			{Index: "logs-*", Actions: []Action{ActionWrite, ActionRead}},
		},
	}
	assert.True(t, r.HasPermission("logs-2024", ActionRead))
	assert.True(t, r.HasPermission("logs-2024", ActionWrite))
	assert.False(t, r.HasPermission("metrics", ActionWrite))
	assert.False(t, r.HasPermission("logs-2024", ActionAdmin))
}

// TestRBAC_CheckPermission
func TestRBAC_CheckPermission(t *testing.T) {
	r := newRBAC()
	// user alice 持有 role "writer" (logs-*)
	r.setUser(User{Name: "alice", PasswordHash: "x", Roles: []string{"writer"}})
	r.setRole(Role{
		Name: "writer",
		Permissions: []Permission{
			{Index: "logs-*", Actions: []Action{ActionWrite, ActionRead}},
		},
	})

	// alice 对 logs-2024 有 read/write
	assert.True(t, r.checkPermission("alice", "logs-2024", ActionRead))
	assert.True(t, r.checkPermission("alice", "logs-2024", ActionWrite))
	// alice 对 metrics 无权
	assert.False(t, r.checkPermission("alice", "metrics", ActionWrite))
	// 匿名无权限
	assert.False(t, r.checkPermission("", "logs-2024", ActionRead))
	// 不存在的 user -> 后向兼容: 默认 superuser (return true)
	assert.True(t, r.checkPermission("bob", "logs-2024", ActionRead))
	// apikey 永远 superuser
	assert.True(t, r.checkPermission("apikey", "logs-2024", ActionRead))
}

// TestRBAC_RequestAction 推断 (index, action)
func TestRBAC_RequestAction(t *testing.T) {
	cases := []struct {
		method, path string
		wantIndex    string
		wantAction   Action
	}{
		{"GET", "/idx/_search", "idx", ActionRead},
		{"GET", "/idx/_doc/1", "idx", ActionRead},
		{"POST", "/idx/_doc/1", "idx", ActionWrite},
		{"PUT", "/idx/_doc/1", "idx", ActionWrite},
		{"DELETE", "/idx/_doc/1", "idx", ActionWrite},
		{"DELETE", "/idx", "idx", ActionAdmin},
		{"GET", "/_cluster/health", "", ActionCluster},
		{"GET", "/metrics", "", ActionMonitor},
		{"GET", "/_security/whoami", "", ActionRead},
		{"POST", "/_security/user/foo", "", ActionWrite},
	}
	for _, c := range cases {
		idx, act := requestAction(c.method, c.path)
		assert.Equal(t, c.wantIndex, idx, "%s %s", c.method, c.path)
		assert.Equal(t, c.wantAction, act, "%s %s", c.method, c.path)
	}
}

// TestUser_CheckPassword
func TestUser_CheckPassword(t *testing.T) {
	u := User{
		Name:         "alice",
		PasswordHash: HashPassword("secret"),
		Roles:        []string{"admin"},
	}
	assert.True(t, u.CheckPassword("secret"))
	assert.False(t, u.CheckPassword("wrong"))
}

// TestRBAC_Persistence 保存到 store 后从 store 恢复
func TestRBAC_Persistence(t *testing.T) {
	// 自己构造一个有 store 的 server
	store, err := storage.Open("")
	assert.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	engine := search.New(store)
	s := &Server{store: store, engine: engine, rbac: newRBAC()}
	r := s.rbac
	r.setUser(User{Name: "alice", PasswordHash: "x", Roles: []string{"writer"}})
	r.setRole(Role{
		Name: "writer",
		Permissions: []Permission{
			{Index: "logs-*", Actions: []Action{ActionWrite, ActionRead}},
		},
	})
	err = r.saveToStore(s)
	assert.NoError(t, err)

	// 新 rbac 实例, 加载
	r2 := newRBAC()
	r2.loadFromStore(s)
	_, ok := r2.getUser("alice")
	assert.True(t, ok, "alice should be persisted")
	_, ok = r2.getRole("writer")
	assert.True(t, ok, "writer role should be persisted")
}

// TestHandleListRoles 验证 list roles 返回 4 个内置角色
func TestHandleListRoles(t *testing.T) {
	s := newRBACTestServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/_security/role", nil)
	s.handleListRoles(rr, req)
	assert.Equal(t, 200, rr.Code)
	body := rr.Body.String()
	for _, r := range []string{"superuser", "admin", "read", "monitor"} {
		assert.Contains(t, body, r, "should contain built-in role %s", r)
	}
}

// TestHandleCreateUser 测试创建用户
func TestHandleCreateUser(t *testing.T) {
	s := newRBACTestServer(t)
	rr := httptest.NewRecorder()
	body := `{"password":"hello","roles":["admin"]}`
	req := httptest.NewRequest("POST", "/_security/user/tester", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.handleCreateUser(rr, req)
	assert.Equal(t, 200, rr.Code)
	// 验证
	u, ok := s.rbac.getUser("tester")
	assert.True(t, ok)
	assert.Equal(t, "tester", u.Name)
	assert.True(t, u.CheckPassword("hello"))
	assert.Equal(t, []string{"admin"}, u.Roles)
}

// TestHandleCreateUser_MissingPassword -> 400
func TestHandleCreateUser_MissingPassword(t *testing.T) {
	s := newRBACTestServer(t)
	rr := httptest.NewRecorder()
	body := `{"roles":["admin"]}`
	req := httptest.NewRequest("POST", "/_security/user/tester2", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.handleCreateUser(rr, req)
	assert.Equal(t, 400, rr.Code)
}

// TestHandleCreateRole 测试创建自定义角色
func TestHandleCreateRole(t *testing.T) {
	s := newRBACTestServer(t)
	rr := httptest.NewRecorder()
	body := `{"permissions":[{"index":"logs-*","actions":["read","write"]}]}`
	req := httptest.NewRequest("POST", "/_security/role/limited", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.handleCreateRole(rr, req)
	assert.Equal(t, 200, rr.Code)
	role, ok := s.rbac.getRole("limited")
	assert.True(t, ok)
	assert.Equal(t, "limited", role.Name)
}

// TestHandleCreateRole_BuiltinProtect 不能覆盖内置角色
func TestHandleCreateRole_BuiltinProtect(t *testing.T) {
	s := newRBACTestServer(t)
	rr := httptest.NewRecorder()
	body := `{"permissions":[{"index":"*","actions":[]}]}`
	req := httptest.NewRequest("POST", "/_security/role/superuser", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.handleCreateRole(rr, req)
	assert.Equal(t, 400, rr.Code, "should not allow overriding built-in role")
}

// TestHandleDeleteRole_BuiltinProtect 不能删内置角色
func TestHandleDeleteRole_BuiltinProtect(t *testing.T) {
	s := newRBACTestServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/_security/role/superuser", nil)
	s.handleDeleteRole(rr, req)
	// pathSegment(r, 3) 返回 "superuser" 因为 path 是 /_security/role/superuser
	// 但 /_security/role 长度是 3, index 3 越界 -> 返回 ""
	// 实际: 路由会传 path 三段, parts[2] = "superuser" -> 应改为 pathSegment(r, 2)
	assert.Equal(t, 400, rr.Code)
}

// TestRBAC_EndToEnd 测试 rbac.users 控制访问
func TestRBAC_EndToEnd(t *testing.T) {
	r := newRBAC()
	// user restricted 只有 read 权限在 idx1
	r.setUser(User{Name: "restricted", PasswordHash: "x", Roles: []string{"readonly"}})
	r.setRole(Role{
		Name: "readonly",
		Permissions: []Permission{
			{Index: "idx1", Actions: []Action{ActionRead}},
		},
	})
	// 在 idx1 上 read -> OK
	assert.True(t, r.checkPermission("restricted", "idx1", ActionRead))
	// 在 idx1 上 write -> NO
	assert.False(t, r.checkPermission("restricted", "idx1", ActionWrite))
	// 在 idx2 上 read -> NO
	assert.False(t, r.checkPermission("restricted", "idx2", ActionRead))
	// 在 idx2 上 write -> NO
	assert.False(t, r.checkPermission("restricted", "idx2", ActionWrite))
}
