// RBAC 扩展 + Session 管理单元测试
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/zixiliuyue/go_es/internal/search"
	"github.com/zixiliuyue/go_es/internal/storage"
	"go.uber.org/zap"
)

// newSecTestServer 构造安全测试用 server(带 Session 配置)
func newSecTestServer(t *testing.T, opts ServerOptions) *Server {
	t.Helper()
	store, err := storage.Open("")
	assert.NoError(t, err)
	engine := search.New(store)
	logger, _ := zap.NewDevelopment()
	s := NewWithOptions(store, engine, logger, opts)
	s.MarkStartupDone()
	t.Cleanup(func() { _ = store.Close() })
	return s
}

// ---------- Session 管理测试 ----------

func TestSessionConfig_Defaults(t *testing.T) {
	cfg := DefaultSessionConfig()
	assert.True(t, cfg.Enabled)
	assert.Equal(t, 24*time.Hour, cfg.Timeout)
	assert.Equal(t, 5, cfg.MaxSessions)
	assert.True(t, cfg.CleanupInterval > 0)
}

func TestSessionManager_CreateAndGet(t *testing.T) {
	s := newSecTestServer(t, ServerOptions{
		Session: SessionConfig{Enabled: true, Timeout: 1 * time.Hour, MaxSessions: 3},
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.RemoteAddr = "192.168.1.100:12345"
	req.Header.Set("User-Agent", "test-browser/1.0")

	session, err := s.sessionMgr.CreateSession("testuser", req)
	assert.NoError(t, err)
	assert.NotEmpty(t, session.Token)
	assert.Equal(t, "testuser", session.UserID)
	assert.Equal(t, "192.168.1.100", session.IP)
	assert.Equal(t, "test-browser/1.0", session.UserAgent)
	assert.True(t, session.IsActive())
	assert.False(t, session.IsExpired())

	// 获取会话
	got, err := s.sessionMgr.GetSession(session.Token)
	assert.NoError(t, err)
	assert.Equal(t, session.Token, got.Token)
	assert.Equal(t, "testuser", got.UserID)
}

func TestSessionManager_TokenValidation(t *testing.T) {
	s := newSecTestServer(t, ServerOptions{
		Session: SessionConfig{Enabled: true, Secret: "test-secret-key"},
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	session, err := s.sessionMgr.CreateSession("user1", req)
	assert.NoError(t, err)

	// 有效 token
	userID, err := s.sessionMgr.validateToken(session.Token)
	assert.NoError(t, err)
	assert.Equal(t, "user1", userID)

	// 无效 token
	_, err = s.sessionMgr.validateToken("invalid.token.here")
	assert.Error(t, err)

	// 空 token
	_, err = s.sessionMgr.validateToken("")
	assert.Error(t, err)
}

func TestSessionManager_MultiDevice(t *testing.T) {
	s := newSecTestServer(t, ServerOptions{
		Session: SessionConfig{Enabled: true, Timeout: 1 * time.Hour, MaxSessions: 3},
	})

	// 创建 3 个会话(不同 IP)
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "192.168.1." + string(rune('0'+i)) + ":12345"
		_, err := s.sessionMgr.CreateSession("user1", req)
		assert.NoError(t, err)
	}

	// 第 4 个会话应该自动撤销最老的
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.168.1.99:12345"
	_, err := s.sessionMgr.CreateSession("user1", req)
	assert.NoError(t, err)

	// 现在应该还有 3 个会话
	sessions := s.sessionMgr.ListUserSessions("user1")
	assert.Len(t, sessions, 3)
}

func TestSessionManager_Revoke(t *testing.T) {
	s := newSecTestServer(t, ServerOptions{
		Session: SessionConfig{Enabled: true},
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	session, err := s.sessionMgr.CreateSession("user1", req)
	assert.NoError(t, err)

	// 撤销
	s.sessionMgr.RevokeSession(session.Token)

	// 不应再获取(要么 session not found, 要么 revoked)
	_, err = s.sessionMgr.GetSession(session.Token)
	assert.Error(t, err)
	// RevokeSession 先设 IsRevoked, 然后 delete, 所以可能是 "revoked" 或 "not found"
	assert.True(t, err.Error() == "session revoked" || err.Error() == "session not found")
}

func TestSessionManager_RevokeAll(t *testing.T) {
	s := newSecTestServer(t, ServerOptions{
		Session: SessionConfig{Enabled: true},
	})

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0." + string(rune('0'+i)) + ":12345"
		_, _ = s.sessionMgr.CreateSession("user1", req)
	}

	sessions := s.sessionMgr.ListUserSessions("user1")
	assert.Len(t, sessions, 3)

	s.sessionMgr.RevokeAllUserSessions("user1")
	sessions = s.sessionMgr.ListUserSessions("user1")
	assert.Len(t, sessions, 0)
}

func TestSessionManager_Expiry(t *testing.T) {
	s := newSecTestServer(t, ServerOptions{
		Session: SessionConfig{Enabled: true, Timeout: 1 * time.Millisecond},
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	session, err := s.sessionMgr.CreateSession("user1", req)
	assert.NoError(t, err)

	// 等待过期
	time.Sleep(10 * time.Millisecond)

	_, err = s.sessionMgr.GetSession(session.Token)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "expired")
}

func TestSessionManager_Stats(t *testing.T) {
	s := newSecTestServer(t, ServerOptions{
		Session: SessionConfig{Enabled: true, Timeout: 1 * time.Hour},
	})

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		_, _ = s.sessionMgr.CreateSession("user"+string(rune('0'+i)), req)
	}

	stats := s.sessionMgr.GetStats()
	assert.Equal(t, 3, stats.TotalSessions)
	assert.Equal(t, 3, stats.ActiveSessions)
	assert.Equal(t, 3, stats.UsersCount)
}

func TestSessionManager_Cleanup(t *testing.T) {
	s := newSecTestServer(t, ServerOptions{
		Session: SessionConfig{Enabled: true, Timeout: 1 * time.Millisecond},
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	_, _ = s.sessionMgr.CreateSession("user1", req)

	// 等待过期
	time.Sleep(10 * time.Millisecond)

	count := s.sessionMgr.CleanupExpired()
	assert.Equal(t, 1, count)

	stats := s.sessionMgr.GetStats()
	assert.Equal(t, 0, stats.TotalSessions)
}

// ---------- Session HTTP 端点测试 ----------

func TestSessionEndpoint_Login(t *testing.T) {
	s := newSecTestServer(t, ServerOptions{
		Auth:    AuthConfig{Enabled: true, Basic: map[string]string{"testuser": "testpass"}},
		Session: SessionConfig{Enabled: true},
	})

	// 创建用户
	s.rbac.loadFromStore(s)
	s.rbac.setUser(User{
		Name:         "testuser",
		PasswordHash: HashPassword("testpass"),
		Roles:        []string{"admin"},
	})
	_ = s.rbac.saveToStore(s)

	req := httptest.NewRequest(http.MethodPost, "/_security/login",
		bodyReader(t, `{"username":"testuser","password":"testpass"}`))
	w := httptest.NewRecorder()
	s.handleLogin(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "ok", resp["status"])
	assert.NotEmpty(t, resp["token"])
}

func TestSessionEndpoint_LoginWrongPassword(t *testing.T) {
	s := newSecTestServer(t, ServerOptions{
		Auth:    AuthConfig{Enabled: true, Basic: map[string]string{"testuser": "testpass"}},
		Session: SessionConfig{Enabled: true},
	})

	s.rbac.loadFromStore(s)
	s.rbac.setUser(User{
		Name:         "testuser",
		PasswordHash: HashPassword("testpass"),
		Roles:        []string{"admin"},
	})
	_ = s.rbac.saveToStore(s)

	req := httptest.NewRequest(http.MethodPost, "/_security/login",
		bodyReader(t, `{"username":"testuser","password":"wrongpass"}`))
	w := httptest.NewRecorder()
	s.handleLogin(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestSessionEndpoint_LoginMissingFields(t *testing.T) {
	s := newSecTestServer(t, ServerOptions{
		Session: SessionConfig{Enabled: true},
	})

	req := httptest.NewRequest(http.MethodPost, "/_security/login",
		bodyReader(t, `{"username":"test"}`))
	w := httptest.NewRecorder()
	s.handleLogin(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSessionEndpoint_MethodNotAllowed(t *testing.T) {
	s := newSecTestServer(t, ServerOptions{
		Session: SessionConfig{Enabled: true},
	})

	req := httptest.NewRequest(http.MethodGet, "/_security/login", nil)
	w := httptest.NewRecorder()
	s.handleLogin(w, req)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

func TestSessionEndpoint_Logout(t *testing.T) {
	s := newSecTestServer(t, ServerOptions{
		Auth:    AuthConfig{Enabled: true, Basic: map[string]string{"user1": "pass1"}},
		Session: SessionConfig{Enabled: true},
	})

	s.rbac.loadFromStore(s)
	s.rbac.setUser(User{
		Name:         "user1",
		PasswordHash: HashPassword("pass1"),
		Roles:        []string{"read"},
	})
	_ = s.rbac.saveToStore(s)

	// 先登录
	loginReq := httptest.NewRequest(http.MethodPost, "/_security/login",
		bodyReader(t, `{"username":"user1","password":"pass1"}`))
	loginW := httptest.NewRecorder()
	s.handleLogin(loginW, loginReq)
	assert.Equal(t, http.StatusOK, loginW.Code)

	var loginResp map[string]interface{}
	json.Unmarshal(loginW.Body.Bytes(), &loginResp)
	token := loginResp["token"].(string)

	// 登出
	logoutReq := httptest.NewRequest(http.MethodPost, "/_security/logout", nil)
	logoutReq.Header.Set("Authorization", "Bearer "+token)
	logoutW := httptest.NewRecorder()
	s.handleLogout(logoutW, logoutReq)
	assert.Equal(t, http.StatusOK, logoutW.Code)

	// 验证已撤销
	_, err := s.sessionMgr.GetSession(token)
	assert.Error(t, err)
}

func TestSessionEndpoint_GetCurrentSession(t *testing.T) {
	s := newSecTestServer(t, ServerOptions{
		Session: SessionConfig{Enabled: true},
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	session, _ := s.sessionMgr.CreateSession("user1", req)

	getReq := httptest.NewRequest(http.MethodGet, "/_security/session", nil)
	getReq.Header.Set("Authorization", "Bearer "+session.Token)
	w := httptest.NewRecorder()
	s.handleGetCurrentSession(w, getReq)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "user1", resp["user_id"])
}

func TestSessionEndpoint_ListSessions(t *testing.T) {
	s := newSecTestServer(t, ServerOptions{
		Session: SessionConfig{Enabled: true},
	})

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		s.sessionMgr.CreateSession("user1", req)
	}

	req := httptest.NewRequest(http.MethodGet, "/_security/sessions", nil)
	ctx := context.WithValue(req.Context(), ctxKeyUsername, "user1")
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	s.handleListSessions(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, float64(2), resp["count"])
}

func TestSessionEndpoint_SessionStats(t *testing.T) {
	s := newSecTestServer(t, ServerOptions{
		Session: SessionConfig{Enabled: true},
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	s.sessionMgr.CreateSession("user1", req)

	getReq := httptest.NewRequest(http.MethodGet, "/_security/session/stats", nil)
	w := httptest.NewRecorder()
	s.handleSessionStats(w, getReq)
	assert.Equal(t, http.StatusOK, w.Code)

	var stats SessionStats
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &stats))
	assert.Equal(t, 1, stats.TotalSessions)
	assert.Equal(t, 1, stats.ActiveSessions)
}

func TestSessionEndpoint_SessionConfig(t *testing.T) {
	s := newSecTestServer(t, ServerOptions{
		Session: SessionConfig{Enabled: true, Timeout: 2 * time.Hour},
	})

	// GET
	req := httptest.NewRequest(http.MethodGet, "/_security/session/config", nil)
	w := httptest.NewRecorder()
	s.handleSessionConfig(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var cfg SessionConfig
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &cfg))
	assert.Equal(t, 2*time.Hour, cfg.Timeout)

	// PUT
	putReq := httptest.NewRequest(http.MethodPut, "/_security/session/config",
		bodyReader(t, `{"timeout":3600000000000,"max_sessions":10}`))
	putW := httptest.NewRecorder()
	s.handleSessionConfig(putW, putReq)
	assert.Equal(t, http.StatusOK, putW.Code)
}

// ---------- 扩展 RBAC 权限测试 ----------

func TestRBACExtended_ListPermissions(t *testing.T) {
	s := newSecTestServer(t, ServerOptions{})
	s.rbac.loadFromStore(s)

	// 先创建一些带权限的角色
	s.rbac.setRole(Role{
		Name: "testrole",
		Permissions: []Permission{
			{Index: "logs-*", Actions: []Action{ActionRead}},
			{Index: "metrics-*", Actions: []Action{ActionRead, ActionMonitor}},
		},
	})
	_ = s.rbac.saveToStore(s)

	req := httptest.NewRequest(http.MethodGet, "/_security/permission", nil)
	w := httptest.NewRecorder()
	s.handleListPermissions(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Permissions []ExtendedPermission `json:"permissions"`
		Count       int                   `json:"count"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.Count >= 2)
}

func TestRBACExtended_GetPermission(t *testing.T) {
	s := newSecTestServer(t, ServerOptions{})
	s.rbac.loadFromStore(s)

	s.rbac.setRole(Role{
		Name: "testrole",
		Permissions: []Permission{
			{Index: "logs-*", Actions: []Action{ActionRead}},
		},
	})
	_ = s.rbac.saveToStore(s)

	req := httptest.NewRequest(http.MethodGet, "/_security/permission/logs-*:read", nil)
	w := httptest.NewRecorder()
	s.handleGetPermission(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Contains(t, resp, "permission")
	assert.Contains(t, resp, "granted_roles")
}

func TestRBACExtended_GetPermissionNotFound(t *testing.T) {
	s := newSecTestServer(t, ServerOptions{})

	req := httptest.NewRequest(http.MethodGet, "/_security/permission/nonexistent", nil)
	w := httptest.NewRecorder()
	s.handleGetPermission(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestRBACExtended_CreatePermission(t *testing.T) {
	s := newSecTestServer(t, ServerOptions{})
	s.rbac.loadFromStore(s)

	req := httptest.NewRequest(http.MethodPost, "/_security/permission/testperm",
		bodyReader(t, `{"index_pattern":"myindex-*","actions":["read","write"],"description":"测试权限"}`))
	w := httptest.NewRecorder()
	s.handleCreatePermission(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "testperm", resp["permission"].(map[string]interface{})["name"])
}

func TestRBACExtended_DeletePermission(t *testing.T) {
	s := newSecTestServer(t, ServerOptions{})
	s.rbac.loadFromStore(s)

	// 先创建权限
	s.rbac.setRole(Role{
		Name: "testrole",
		Permissions: []Permission{
			{Index: "logs-*", Actions: []Action{ActionRead}},
		},
	})
	_ = s.rbac.saveToStore(s)

	// 删除
	req := httptest.NewRequest(http.MethodDelete, "/_security/permission/logs-*:read", nil)
	w := httptest.NewRecorder()
	s.handleDeletePermission(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRBACExtended_BatchAssign(t *testing.T) {
	s := newSecTestServer(t, ServerOptions{})
	s.rbac.loadFromStore(s)

	// 准备角色
	s.rbac.setRole(Role{
		Name: "source_role",
		Permissions: []Permission{
			{Index: "src-*", Actions: []Action{ActionRead}},
		},
	})
	s.rbac.setRole(Role{
		Name: "target_role",
	})
	_ = s.rbac.saveToStore(s)

	req := httptest.NewRequest(http.MethodPost, "/_security/permission/batch",
		bodyReader(t, `{"action":"assign","roles":["source_role"],"target_roles":["target_role"]}`))
	w := httptest.NewRecorder()
	s.handleBatchPermissions(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, float64(1), resp["assigned"])
}

func TestRBACExtended_BatchRevoke(t *testing.T) {
	s := newSecTestServer(t, ServerOptions{})
	s.rbac.loadFromStore(s)

	s.rbac.setRole(Role{
		Name: "source_role",
		Permissions: []Permission{
			{Index: "src-*", Actions: []Action{ActionRead}},
		},
	})
	s.rbac.setRole(Role{
		Name: "target_role",
		Permissions: []Permission{
			{Index: "src-*", Actions: []Action{ActionRead}},
		},
	})
	_ = s.rbac.saveToStore(s)

	req := httptest.NewRequest(http.MethodPost, "/_security/permission/batch",
		bodyReader(t, `{"action":"revoke","roles":["source_role"],"target_roles":["target_role"]}`))
	w := httptest.NewRecorder()
	s.handleBatchPermissions(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRBACExtended_ActionsEqual(t *testing.T) {
	assert.True(t, actionsEqual([]Action{ActionRead, ActionWrite}, []Action{ActionWrite, ActionRead}))
	assert.True(t, actionsEqual([]Action{ActionRead}, []Action{ActionRead}))
	assert.False(t, actionsEqual([]Action{ActionRead}, []Action{ActionWrite}))
	assert.False(t, actionsEqual([]Action{ActionRead}, []Action{ActionRead, ActionWrite}))
}

func TestRBACExtended_InferCategory(t *testing.T) {
	assert.Equal(t, PermCategoryCluster, inferCategory("_cluster/health"))
	assert.Equal(t, PermCategoryIndex, inferCategory("logs-2024"))
	assert.Equal(t, PermCategoryManagement, inferCategory("*"))
	assert.Equal(t, PermCategoryManagement, inferCategory("logs-*"))
}

// ---------- 辅助 ----------

func TestExtractSessionToken(t *testing.T) {
	// Bearer token
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer my-secret-token")
	assert.Equal(t, "my-secret-token", extractSessionToken(req))

	// Cookie
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.AddCookie(&http.Cookie{Name: "go_es_session", Value: "cookie-token"})
	assert.Equal(t, "cookie-token", extractSessionToken(req2))

	// 无 token
	req3 := httptest.NewRequest(http.MethodGet, "/", nil)
	assert.Empty(t, extractSessionToken(req3))
}

func TestSessionIsPublicPath(t *testing.T) {
	assert.True(t, isAuthPath("/_security/login"))
	assert.True(t, isAuthPath("/_security/logout"))
	assert.True(t, isAuthPath("/_security/logout_all"))
	assert.False(t, isAuthPath("/_security/session"))
	assert.False(t, isAuthPath("/_search"))
}

// ---------- Token 格式测试 ----------

func TestTokenGeneration_Format(t *testing.T) {
	s := newSecTestServer(t, ServerOptions{
		Session: SessionConfig{Enabled: true, Secret: "my-secret"},
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	session, err := s.sessionMgr.CreateSession("user1", req)
	assert.NoError(t, err)

	// Token 应该包含 "." 分隔的两部分
	assert.Contains(t, session.Token, ".")
	parts := splitToken(session.Token)
	assert.Len(t, parts, 2)
}

func splitToken(token string) []string {
	for i := 0; i < len(token); i++ {
		if token[i] == '.' {
			return []string{token[:i], token[i+1:]}
		}
	}
	return []string{token}
}

// TestSessionDisabled_DefaultNoPanic 会话管理未启用时不应 panic
func TestSessionDisabled_DefaultNoPanic(t *testing.T) {
	// 不设置 SessionConfig
	s := newSecTestServer(t, ServerOptions{})
	assert.NotNil(t, s.sessionMgr)
	assert.False(t, s.sessionMgr.cfg.Enabled)

	// middlewareSession 不应 panic
	h := s.middlewareSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()
	assert.NotPanics(t, func() { h.ServeHTTP(w, req) })
	assert.Equal(t, http.StatusOK, w.Code)
}

// TestSessionWithBasicAuth 会话管理关闭时, Basic Auth 仍可工作
func TestSessionWithBasicAuth(t *testing.T) {
	s := newSecTestServer(t, ServerOptions{
		Auth:    AuthConfig{Enabled: true, Basic: map[string]string{"admin": "admin"}},
		Session: SessionConfig{Enabled: false},
	})

	// 模拟 Basic Auth 请求(通过中间件链)
	req := httptest.NewRequest(http.MethodGet, "/_search", nil)
	req.SetBasicAuth("admin", "admin")
	w := httptest.NewRecorder()

	handler := s.Handler()
	handler.ServeHTTP(w, req)
	// 应该能通过认证(虽然 /_search 没有实现, 但认证应该通过)
	// 302 or other non-401
	assert.NotEqual(t, http.StatusUnauthorized, w.Code)
}

// ---------- Web UI Admin 页面 ----------

func TestAdminUI_HandleRequest(t *testing.T) {
	s := newSecTestServer(t, ServerOptions{})
	req := httptest.NewRequest(http.MethodGet, "/_ui/admin.html", nil)
	w := httptest.NewRecorder()
	s.handleAdminUI(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "管理员控制台")
	assert.Contains(t, w.Body.String(), "security")
}

// ---------- 完整流程: 登录 -> 使用 Token -> 登出 ----------

func TestFullAuthFlow_LoginTokenLogout(t *testing.T) {
	s := newSecTestServer(t, ServerOptions{
		Auth:    AuthConfig{Enabled: true, Basic: map[string]string{"user": "pass"}},
		Session: SessionConfig{Enabled: true, Timeout: 1 * time.Hour},
	})

	s.rbac.loadFromStore(s)
	s.rbac.setUser(User{
		Name:         "user",
		PasswordHash: HashPassword("pass"),
		Roles:        []string{"admin"},
	})
	_ = s.rbac.saveToStore(s)

	// 1. 登录
	loginReq := httptest.NewRequest(http.MethodPost, "/_security/login",
		bodyReader(t, `{"username":"user","password":"pass"}`))
	loginW := httptest.NewRecorder()
	s.handleLogin(loginW, loginReq)
	assert.Equal(t, http.StatusOK, loginW.Code)

	var loginResp map[string]interface{}
	json.Unmarshal(loginW.Body.Bytes(), &loginResp)
	token := loginResp["token"].(string)
	assert.NotEmpty(t, token)

	// 2. 使用 Token 获取当前会话
	sessionReq := httptest.NewRequest(http.MethodGet, "/_security/session", nil)
	sessionReq.Header.Set("Authorization", "Bearer "+token)
	sessionW := httptest.NewRecorder()
	s.handleGetCurrentSession(sessionW, sessionReq)
	assert.Equal(t, http.StatusOK, sessionW.Code)

	// 3. 登出
	logoutReq := httptest.NewRequest(http.MethodPost, "/_security/logout", nil)
	logoutReq.Header.Set("Authorization", "Bearer "+token)
	logoutW := httptest.NewRecorder()
	s.handleLogout(logoutW, logoutReq)
	assert.Equal(t, http.StatusOK, logoutW.Code)

	// 4. Token 已失效
	checkReq := httptest.NewRequest(http.MethodGet, "/_security/session", nil)
	checkReq.Header.Set("Authorization", "Bearer "+token)
	checkW := httptest.NewRecorder()
	s.handleGetCurrentSession(checkW, checkReq)
	assert.Equal(t, http.StatusUnauthorized, checkW.Code)
}

// ---------- 会话管理器持久化测试 ----------

func TestSessionPersistence_LoadAfterRestart(t *testing.T) {
	dir, err := os.MkdirTemp("", "go_es_session_test_*")
	assert.NoError(t, err)
	defer os.RemoveAll(dir)

	// 第一个 server
	store1, _ := storage.Open(dir)
	engine1 := search.New(store1)
	logger, _ := zap.NewDevelopment()
	s1 := NewWithOptions(store1, engine1, logger, ServerOptions{
		Session: SessionConfig{Enabled: true, Timeout: 1 * time.Hour, Secret: "test"},
	})
	s1.MarkStartupDone()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	session, _ := s1.sessionMgr.CreateSession("user1", req)
	token := session.Token

	// 模拟重启: 关闭并重新打开
	_ = store1.Close()

	store2, _ := storage.Open(dir)
	engine2 := search.New(store2)
	s2 := NewWithOptions(store2, engine2, logger, ServerOptions{
		Session: SessionConfig{Enabled: true, Timeout: 1 * time.Hour, Secret: "test"},
	})
	s2.MarkStartupDone()

	// Token 应该仍然有效
	got, err := s2.sessionMgr.GetSession(token)
	assert.NoError(t, err)
	assert.Equal(t, "user1", got.UserID)

	_ = store2.Close()
}
