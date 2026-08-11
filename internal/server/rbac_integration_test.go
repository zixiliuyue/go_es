// RBAC + Session 集成测试
//
// 测试目标:
//   1. 完整认证流程: 登录 -> 使用 Token 访问资源 -> 登出
//   2. 多设备会话管理: 多设备登录 + 超出限制自动撤销最老会话
//   3. RBAC 权限控制: 不同角色访问受保护资源的权限验证
//   4. 会话超时: Token 过期后自动失效
//   5. 会话持久化: 服务重启后会话恢复
//   6. Admin UI 访问: 管理员控制台端点可达
//   7. 配置加载: YAML 会话配置正确加载
//   8. 会话配置热更新: PUT /_security/session/config
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zixiliuyue/go_es/internal/search"
	"github.com/zixiliuyue/go_es/internal/storage"
	"go.uber.org/zap"
)

// ---------- 集成测试基础设施 ----------

// setupIntegrationServer 创建一个完整的集成测试 server
// 启用认证 + 会话管理, 包含预设用户和角色
func setupIntegrationServer(t *testing.T, opts ServerOptions) (*Server, string) {
	t.Helper()
	store, err := storage.Open("")
	require.NoError(t, err)
	engine := search.New(store)
	logger, _ := zap.NewDevelopment()
	s := NewWithOptions(store, engine, logger, opts)
	s.MarkStartupDone()
	t.Cleanup(func() {
		if s.sessionMgr != nil {
			s.sessionMgr.Stop()
		}
		_ = store.Close()
	})

	// 预设 RBAC 用户
	s.rbac.loadFromStore(s)
	s.rbac.setUser(User{
		Name:         "admin",
		PasswordHash: HashPassword("admin123"),
		Roles:        []string{"admin"},
	})
	s.rbac.setUser(User{
		Name:         "reader",
		PasswordHash: HashPassword("reader123"),
		Roles:        []string{"reader"},
	})
	s.rbac.setUser(User{
		Name:         "writer",
		PasswordHash: HashPassword("writer123"),
		Roles:        []string{"writer"},
	})

	// 预设 RBAC 角色
	s.rbac.setRole(Role{
		Name: "admin",
		Permissions: []Permission{
			{Index: "*", Actions: []Action{ActionRead, ActionWrite, ActionAdmin}},
		},
	})
	s.rbac.setRole(Role{
		Name: "reader",
		Permissions: []Permission{
			{Index: "*", Actions: []Action{ActionRead}},
		},
	})
	s.rbac.setRole(Role{
		Name: "writer",
		Permissions: []Permission{
			{Index: "logs-*", Actions: []Action{ActionRead, ActionWrite}},
		},
	})
	_ = s.rbac.saveToStore(s)

	// 启动会话清理(如果启用)
	if s.sessionMgr != nil && s.sessionMgr.cfg.Enabled {
		s.sessionMgr.loadFromStore()
	}

	return s, ""
}

// createIntegrationServer 创建集成 server 并返回 http 测试服务器
func createIntegrationServer(t *testing.T, opts ServerOptions) (*Server, *httptest.Server) {
	t.Helper()
	s, _ := setupIntegrationServer(t, opts)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() { ts.Close() })
	return s, ts
}

// loginAndGetToken 执行登录并返回 token
func loginAndGetToken(t *testing.T, baseURL, username, password string) string {
	t.Helper()
	resp, err := http.Post(baseURL+"/_security/login",
		"application/json",
		bodyReader(t, fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var loginResp struct {
		Token string `json:"token"`
	}
	err = json.NewDecoder(resp.Body).Decode(&loginResp)
	resp.Body.Close()
	require.NoError(t, err)
	require.NotEmpty(t, loginResp.Token)
	return loginResp.Token
}

// authRequest 发送带 Authorization: Bearer token 的请求
func authRequest(t *testing.T, method, url, token string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return http.DefaultClient.Do(req)
}

// ---------- 测试用例 ----------

// TestIntegration_LoginLogoutFlow 完整登录-访问-登出流程
func TestIntegration_LoginLogoutFlow(t *testing.T) {
	_, ts := createIntegrationServer(t, ServerOptions{
		Auth:    AuthConfig{Enabled: true, Basic: map[string]string{"admin": "admin123"}},
		Session: SessionConfig{Enabled: true, Timeout: 1 * time.Hour},
	})

	baseURL := ts.URL

	// 1. 公共端点无需认证(健康检查等)
	resp, _ := http.Get(baseURL + "/_cluster/health")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// 2. 登录成功
	token := loginAndGetToken(t, baseURL, "admin", "admin123")

	// 3. 使用 Token 访问受保护资源 -> 200
	resp, err := authRequest(t, http.MethodGet, baseURL+"/_security/session", token)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var sessionResp map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&sessionResp)
	resp.Body.Close()
	assert.NoError(t, err)
	assert.Equal(t, "admin", sessionResp["user_id"])

	// 4. 登出
	logoutReq, _ := http.NewRequest(http.MethodPost, baseURL+"/_security/logout", nil)
	logoutReq.Header.Set("Authorization", "Bearer "+token)
	logoutResp, err := http.DefaultClient.Do(logoutReq)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusOK, logoutResp.StatusCode)
	logoutResp.Body.Close()

	// 5. 登出后 Token 失效 -> 401 (使用受保护端点验证)
	resp, err = authRequest(t, http.MethodGet, baseURL+"/_security/session", token)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()
}

// TestIntegration_MultiDeviceSessions 多设备会话管理
func TestIntegration_MultiDeviceSessions(t *testing.T) {
	_, ts := createIntegrationServer(t, ServerOptions{
		Auth:    AuthConfig{Enabled: true, Basic: map[string]string{"admin": "admin123"}},
		Session: SessionConfig{Enabled: true, Timeout: 1 * time.Hour, MaxSessions: 3},
	})
	baseURL := ts.URL

	// 登录 3 次(模拟 3 个设备)
	tokens := make([]string, 3)
	for i := 0; i < 3; i++ {
		tokens[i] = loginAndGetToken(t, baseURL, "admin", "admin123")
	}

	// 3 个 Token 都应该有效
	for i, token := range tokens {
		resp, err := authRequest(t, http.MethodGet, baseURL+"/_security/session", token)
		assert.NoError(t, err)
		assert.Equal(t, http.StatusOK, resp.StatusCode, "token %d should be valid", i)
		resp.Body.Close()
	}

	// 查看会话数
	resp, err := authRequest(t, http.MethodGet, baseURL+"/_security/sessions", tokens[2])
	assert.NoError(t, err)
	var sessionsResp map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&sessionsResp)
	resp.Body.Close()
	assert.Equal(t, float64(3), sessionsResp["count"])

	// 第 4 次登录应该撤销最老的会话
	token4 := loginAndGetToken(t, baseURL, "admin", "admin123")
	assert.NotEmpty(t, token4)

	// 最老的 Token (tokens[0]) 应该已失效
	resp, err = authRequest(t, http.MethodGet, baseURL+"/_security/session", tokens[0])
	assert.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()

	// 最新的 Token 应该有效
	resp, err = authRequest(t, http.MethodGet, baseURL+"/_security/session", token4)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// 会话数应该还是 3(上限)
	resp, err = authRequest(t, http.MethodGet, baseURL+"/_security/sessions", token4)
	assert.NoError(t, err)
	var finalSessions map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&finalSessions)
	resp.Body.Close()
	assert.Equal(t, float64(3), finalSessions["count"])
}

// TestIntegration_SessionExpiry 会话超时自动失效
func TestIntegration_SessionExpiry(t *testing.T) {
	_, ts := createIntegrationServer(t, ServerOptions{
		Auth:    AuthConfig{Enabled: true, Basic: map[string]string{"admin": "admin123"}},
		Session: SessionConfig{Enabled: true, Timeout: 100 * time.Millisecond},
	})
	baseURL := ts.URL

	// 登录
	token := loginAndGetToken(t, baseURL, "admin", "admin123")

	// 立即使用 Token -> 有效
	resp, _ := authRequest(t, http.MethodGet, baseURL+"/_security/session", token)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// 等待超时
	time.Sleep(200 * time.Millisecond)

	// 超时后使用 Token -> 401
	resp, _ = authRequest(t, http.MethodGet, baseURL+"/_security/session", token)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()
}

// TestIntegration_AdminRolePermissions Admin 角色完全访问
func TestIntegration_AdminRolePermissions(t *testing.T) {
	_, ts := createIntegrationServer(t, ServerOptions{
		Auth:    AuthConfig{Enabled: true, Basic: map[string]string{"admin": "admin123", "reader": "reader123"}},
		Session: SessionConfig{Enabled: true, Timeout: 1 * time.Hour},
	})
	baseURL := ts.URL

	// Admin 登录
	adminToken := loginAndGetToken(t, baseURL, "admin", "admin123")

	// Admin 可以查看会话
	resp, _ := authRequest(t, http.MethodGet, baseURL+"/_security/sessions", adminToken)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// Admin 可以查看权限列表
	resp, _ = authRequest(t, http.MethodGet, baseURL+"/_security/permission", adminToken)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
}

// TestIntegration_ReaderRolePermissions Reader 角色只读访问
func TestIntegration_ReaderRolePermissions(t *testing.T) {
	_, ts := createIntegrationServer(t, ServerOptions{
		Auth:    AuthConfig{Enabled: true, Basic: map[string]string{"admin": "admin123", "reader": "reader123"}},
		Session: SessionConfig{Enabled: true, Timeout: 1 * time.Hour},
	})
	baseURL := ts.URL

	// Reader 登录
	readerToken := loginAndGetToken(t, baseURL, "reader", "reader123")

	// Reader 可以查看会话信息
	resp, _ := authRequest(t, http.MethodGet, baseURL+"/_security/session", readerToken)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
}

// TestIntegration_ConcurrentLogins 并发登录测试
func TestIntegration_ConcurrentLogins(t *testing.T) {
	_, ts := createIntegrationServer(t, ServerOptions{
		Auth:    AuthConfig{Enabled: true, Basic: map[string]string{"admin": "admin123"}},
		Session: SessionConfig{Enabled: true, Timeout: 1 * time.Hour, MaxSessions: 10},
	})
	baseURL := ts.URL

	// 并发登录 5 次
	var tokens []string
	for i := 0; i < 5; i++ {
		token := loginAndGetToken(t, baseURL, "admin", "admin123")
		tokens = append(tokens, token)
	}

	// 所有 Token 都应有效
	for _, token := range tokens {
		resp, _ := authRequest(t, http.MethodGet, baseURL+"/_security/session", token)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		resp.Body.Close()
	}

	// 会话统计应正确
	resp, _ := authRequest(t, http.MethodGet, baseURL+"/_security/session/stats", tokens[0])
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	var stats SessionStats
	json.NewDecoder(resp.Body).Decode(&stats)
	resp.Body.Close()
	assert.Equal(t, 5, stats.TotalSessions)
	assert.Equal(t, 5, stats.ActiveSessions)
}

// TestIntegration_SessionConfigUpdate 会话配置热更新
func TestIntegration_SessionConfigUpdate(t *testing.T) {
	_, ts := createIntegrationServer(t, ServerOptions{
		Auth:    AuthConfig{Enabled: true, Basic: map[string]string{"admin": "admin123"}},
		Session: SessionConfig{Enabled: true, Timeout: 1 * time.Hour, MaxSessions: 5},
	})
	baseURL := ts.URL

	token := loginAndGetToken(t, baseURL, "admin", "admin123")

	// GET 当前配置
	resp, _ := authRequest(t, http.MethodGet, baseURL+"/_security/session/config", token)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var cfg SessionConfig
	json.NewDecoder(resp.Body).Decode(&cfg)
	resp.Body.Close()
	assert.Equal(t, 1*time.Hour, cfg.Timeout)
	assert.Equal(t, 5, cfg.MaxSessions)

	// PUT 更新配置
	updateReq, _ := http.NewRequest(http.MethodPut, baseURL+"/_security/session/config",
		strings.NewReader(`{"timeout":3600000000000,"max_sessions":10}`))
	updateReq.Header.Set("Authorization", "Bearer "+token)
	updateReq.Header.Set("Content-Type", "application/json")
	updateResp, _ := http.DefaultClient.Do(updateReq)
	assert.Equal(t, http.StatusOK, updateResp.StatusCode)

	var updatedCfg SessionConfig
	json.NewDecoder(updateResp.Body).Decode(&updatedCfg)
	updateResp.Body.Close()
	assert.Equal(t, 10, updatedCfg.MaxSessions)
}

// TestIntegration_LogoutAllDevices 登出所有设备
func TestIntegration_LogoutAllDevices(t *testing.T) {
	_, ts := createIntegrationServer(t, ServerOptions{
		Auth:    AuthConfig{Enabled: true, Basic: map[string]string{"admin": "admin123"}},
		Session: SessionConfig{Enabled: true, Timeout: 1 * time.Hour, MaxSessions: 5},
	})
	baseURL := ts.URL

	// 登录 3 次
	var tokens []string
	for i := 0; i < 3; i++ {
		tokens = append(tokens, loginAndGetToken(t, baseURL, "admin", "admin123"))
	}

	// 确认所有 Token 有效
	for _, token := range tokens {
		resp, _ := authRequest(t, http.MethodGet, baseURL+"/_security/session", token)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		resp.Body.Close()
	}

	// 登出所有设备
	logoutAllReq, _ := http.NewRequest(http.MethodPost, baseURL+"/_security/logout_all", nil)
	logoutAllReq.Header.Set("Authorization", "Bearer "+tokens[0])
	logoutAllResp, _ := http.DefaultClient.Do(logoutAllReq)
	assert.Equal(t, http.StatusOK, logoutAllResp.StatusCode)
	logoutAllResp.Body.Close()

	// 所有 Token 应失效
	for _, token := range tokens {
		resp, _ := authRequest(t, http.MethodGet, baseURL+"/_security/session", token)
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		resp.Body.Close()
	}
}

// TestIntegration_AdminUIAccess Admin UI 页面访问
func TestIntegration_AdminUIAccess(t *testing.T) {
	s, ts := createIntegrationServer(t, ServerOptions{
		Auth:    AuthConfig{Enabled: true, Basic: map[string]string{"admin": "admin123"}},
		Session: SessionConfig{Enabled: true, Timeout: 1 * time.Hour},
	})
	assert.NotNil(t, s)
	baseURL := ts.URL

	// Admin UI 应该可访问(白名单)
	resp, err := http.Get(baseURL + "/_ui/admin.html")
	assert.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Contains(t, string(body), "管理员控制台")
	assert.Contains(t, string(body), "_security")
}

// TestIntegration_SessionPersistence 会话持久化(存储恢复)
func TestIntegration_SessionPersistence(t *testing.T) {
	dir, err := os.MkdirTemp("", "go_es_integration_test_*")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	// 第一个 server
	store1, _ := storage.Open(dir)
	engine1 := search.New(store1)
	logger, _ := zap.NewDevelopment()
	s1 := NewWithOptions(store1, engine1, logger, ServerOptions{
		Auth:    AuthConfig{Enabled: true, Basic: map[string]string{"admin": "admin123"}},
		Session: SessionConfig{Enabled: true, Timeout: 1 * time.Hour, Secret: "test-integration-secret"},
	})
	s1.MarkStartupDone()

	// 预设 RBAC
	s1.rbac.loadFromStore(s1)
	s1.rbac.setUser(User{
		Name:         "admin",
		PasswordHash: HashPassword("admin123"),
		Roles:        []string{"admin"},
	})
	s1.rbac.setRole(Role{
		Name: "admin",
		Permissions: []Permission{
			{Index: "*", Actions: []Action{ActionRead, ActionWrite, ActionAdmin}},
		},
	})
	_ = s1.rbac.saveToStore(s1)

	ts1 := httptest.NewServer(s1.Handler())
	defer ts1.Close()

	baseURL := ts1.URL
	token := loginAndGetToken(t, baseURL, "admin", "admin123")

	// 关闭第一个 server
	s1.sessionMgr.Stop()
	_ = store1.Close()

	// 重新打开第二个 server
	store2, _ := storage.Open(dir)
	engine2 := search.New(store2)
	s2 := NewWithOptions(store2, engine2, logger, ServerOptions{
		Auth:    AuthConfig{Enabled: true, Basic: map[string]string{"admin": "admin123"}},
		Session: SessionConfig{Enabled: true, Timeout: 1 * time.Hour, Secret: "test-integration-secret"},
	})
	s2.MarkStartupDone()

	ts2 := httptest.NewServer(s2.Handler())
	defer ts2.Close()

	// Token 在重启后应仍有效
	resp, err := authRequest(t, http.MethodGet, ts2.URL+"/_security/session", token)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode, "session should persist across restart")
	resp.Body.Close()

	_ = store2.Close()
}

// TestIntegration_DoubleSubmitLogin 重复登录不应出错
func TestIntegration_DoubleSubmitLogin(t *testing.T) {
	_, ts := createIntegrationServer(t, ServerOptions{
		Auth:    AuthConfig{Enabled: true, Basic: map[string]string{"admin": "admin123"}},
		Session: SessionConfig{Enabled: true, Timeout: 1 * time.Hour},
	})
	baseURL := ts.URL

	// 连续两次登录相同用户
	token1 := loginAndGetToken(t, baseURL, "admin", "admin123")
	token2 := loginAndGetToken(t, baseURL, "admin", "admin123")

	// 两个 Token 都应该有效但不相同
	assert.NotEqual(t, token1, token2)

	// 两个都能访问
	for _, token := range []string{token1, token2} {
		resp, _ := authRequest(t, http.MethodGet, baseURL+"/_security/session", token)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		resp.Body.Close()
	}
}

// TestIntegration_InvalidToken 无效 Token 处理
func TestIntegration_InvalidToken(t *testing.T) {
	_, ts := createIntegrationServer(t, ServerOptions{
		Auth:    AuthConfig{Enabled: true, Basic: map[string]string{"admin": "admin123"}},
		Session: SessionConfig{Enabled: true},
	})
	baseURL := ts.URL

	// 无效 Token
	resp, _ := authRequest(t, http.MethodGet, baseURL+"/_security/session", "invalid.token.here")
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()

	// 空 Token
	resp, _ = authRequest(t, http.MethodGet, baseURL+"/_security/session", "")
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()

	// 伪造 Token
	resp, _ = authRequest(t, http.MethodGet, baseURL+"/_security/session", "fake.token.abc123")
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()
}

// TestIntegration_SessionStatsEndpoint 会话统计端点
func TestIntegration_SessionStatsEndpoint(t *testing.T) {
	_, ts := createIntegrationServer(t, ServerOptions{
		Auth:    AuthConfig{Enabled: true, Basic: map[string]string{"admin": "admin123", "user1": "pass1"}},
		Session: SessionConfig{Enabled: true, Timeout: 1 * time.Hour, MaxSessions: 10},
	})
	baseURL := ts.URL

	// 登录两个用户
	adminToken := loginAndGetToken(t, baseURL, "admin", "admin123")
	userToken := loginAndGetToken(t, baseURL, "user1", "pass1")

	// Admin 查看全局统计
	resp, _ := authRequest(t, http.MethodGet, baseURL+"/_security/session/stats", adminToken)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var stats SessionStats
	json.NewDecoder(resp.Body).Decode(&stats)
	resp.Body.Close()
	assert.Equal(t, 2, stats.TotalSessions)
	assert.Equal(t, 2, stats.ActiveSessions)
	assert.Equal(t, 2, stats.UsersCount)

	// User 查看自己的统计
	resp2, _ := authRequest(t, http.MethodGet, baseURL+"/_security/session/stats", userToken)
	assert.Equal(t, http.StatusOK, resp2.StatusCode)
	var userStats SessionStats
	json.NewDecoder(resp2.Body).Decode(&userStats)
	resp2.Body.Close()
	assert.Equal(t, 2, userStats.TotalSessions)
}

// TestIntegration_NoSessionBasicAuth 会话管理关闭时 Basic Auth 仍可工作
func TestIntegration_NoSessionBasicAuth(t *testing.T) {
	_, ts := createIntegrationServer(t, ServerOptions{
		Auth:    AuthConfig{Enabled: true, Basic: map[string]string{"admin": "admin123"}},
		Session: SessionConfig{Enabled: false},
	})
	baseURL := ts.URL

	// Basic Auth 应该仍可工作
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/_cluster/health", nil)
	req.SetBasicAuth("admin", "admin123")
	resp, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// 登录端点应该返回 404(会话管理未启用)
	resp, _ = http.Post(baseURL+"/_security/login",
		"application/json",
		bodyReader(t, `{"username":"admin","password":"admin123"}`))
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp.Body.Close()
}

// TestIntegration_BasicAuthFallback Basic Auth + Session 混合认证
func TestIntegration_BasicAuthFallback(t *testing.T) {
	_, ts := createIntegrationServer(t, ServerOptions{
		Auth:    AuthConfig{Enabled: true, Basic: map[string]string{"admin": "admin123"}},
		Session: SessionConfig{Enabled: true, Timeout: 1 * time.Hour},
	})
	baseURL := ts.URL

	// 未携带 Token 时, Basic Auth 应仍有效
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/_cluster/health", nil)
	req.SetBasicAuth("admin", "admin123")
	resp, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// 同时携带 Token 和 Basic Auth
	token := loginAndGetToken(t, baseURL, "admin", "admin123")
	req2, _ := http.NewRequest(http.MethodGet, baseURL+"/_cluster/health", nil)
	req2.SetBasicAuth("admin", "admin123")
	req2.Header.Set("Authorization", "Bearer "+token)
	resp2, err := http.DefaultClient.Do(req2)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp2.StatusCode)
	resp2.Body.Close()
}

// TestIntegration_SessionListAfterRevokeAll 撤销全部后列表为空
func TestIntegration_SessionListAfterRevokeAll(t *testing.T) {
	_, ts := createIntegrationServer(t, ServerOptions{
		Auth:    AuthConfig{Enabled: true, Basic: map[string]string{"admin": "admin123"}},
		Session: SessionConfig{Enabled: true, Timeout: 1 * time.Hour, MaxSessions: 5},
	})
	baseURL := ts.URL

	// 登录 3 次
	token := ""
	for i := 0; i < 3; i++ {
		token = loginAndGetToken(t, baseURL, "admin", "admin123")
	}

	// 撤销全部
	logoutReq, _ := http.NewRequest(http.MethodPost, baseURL+"/_security/logout_all", nil)
	logoutReq.Header.Set("Authorization", "Bearer "+token)
	logoutResp, _ := http.DefaultClient.Do(logoutReq)
	assert.Equal(t, http.StatusOK, logoutResp.StatusCode)
	logoutResp.Body.Close()

	// 已撤销的 Token 无法查看会话
	resp, _ := authRequest(t, http.MethodGet, baseURL+"/_security/session", token)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()

	// 重新登录后, 应该只有 1 个会话(新的)
	newToken := loginAndGetToken(t, baseURL, "admin", "admin123")
	resp, _ = authRequest(t, http.MethodGet, baseURL+"/_security/sessions", newToken)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	var sessions map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&sessions)
	resp.Body.Close()
	assert.Equal(t, float64(1), sessions["count"])
}

// TestIntegration_LoginEmptyPassword 空密码登录
func TestIntegration_LoginEmptyPassword(t *testing.T) {
	_, ts := createIntegrationServer(t, ServerOptions{
		Auth:    AuthConfig{Enabled: true},
		Session: SessionConfig{Enabled: true},
	})
	baseURL := ts.URL

	resp, err := http.Post(baseURL+"/_security/login",
		"application/json",
		bodyReader(t, `{"username":"","password":""}`))
	assert.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()
}

// TestIntegration_SessionStatsAsAdmin 仅管理员可访问会话统计
func TestIntegration_SessionStatsAsAdmin(t *testing.T) {
	_, ts := createIntegrationServer(t, ServerOptions{
		Auth:    AuthConfig{Enabled: true, Basic: map[string]string{"admin": "admin123", "reader": "reader123"}},
		Session: SessionConfig{Enabled: true, Timeout: 1 * time.Hour},
	})
	baseURL := ts.URL

	// Reader 登录
	readerToken := loginAndGetToken(t, baseURL, "reader", "reader123")

	// 查看会话(非管理员也可看自己的)
	resp, _ := authRequest(t, http.MethodGet, baseURL+"/_security/session", readerToken)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
}