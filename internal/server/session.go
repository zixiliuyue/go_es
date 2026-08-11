// Session 管理系统
//
// 设计:
//   - 基于 Token 的会话管理, 支持多设备登录
//   - Token 使用 HMAC-SHA256 签名, 防篡改
//   - 会话超时自动登出, 可配置默认超时
//   - 会话信息持久化到 storage
//   - 支持会话撤销(单个或全部)
//
// 端点:
//   - POST   /_security/login           用户登录
//   - POST   /_security/logout           用户登出
//   - POST   /_security/logout_all      登出所有设备
//   - GET    /_security/session         查看当前会话
//   - GET    /_security/sessions        列出当前用户所有会话
//   - GET    /_security/session/{token} 查看指定会话
//   - DELETE /_security/session/{token} 撤销指定会话
//   - DELETE /_security/sessions        撤销当前用户所有会话
//
// 中间件:
//   - middlewareSession 在 auth 之后校验 Session Token
//   - Token 来源: Authorization: Bearer <token>
package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
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

	"github.com/zixiliuyue/go_es/internal/storage"
)

// SessionConfig 会话管理配置
type SessionConfig struct {
	// Enabled 是否启用会话管理
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Timeout 会话超时时间(默认 24h)
	Timeout time.Duration `yaml:"timeout" json:"timeout"`
	// MaxSessions 每个用户最大会话数(0 = 不限)
	MaxSessions int `yaml:"max_sessions" json:"max_sessions"`
	// Secret 签名密钥(从配置文件读取, 用于 HMAC)
	Secret string `yaml:"secret" json:"-"`
	// CleanupInterval 过期会话清理间隔(默认 5min)
	CleanupInterval time.Duration `yaml:"cleanup_interval" json:"cleanup_interval"`
}

// DefaultSessionConfig 默认会话配置
func DefaultSessionConfig() SessionConfig {
	return SessionConfig{
		Enabled:         true,
		Timeout:         24 * time.Hour,
		MaxSessions:     5,
		Secret:          "go_es_session_default_secret_change_me",
		CleanupInterval: 5 * time.Minute,
	}
}

// Session 会话信息
type Session struct {
	// Token 会话唯一标识(token + 签名)
	Token string `json:"token"`
	// UserID 关联的用户
	UserID string `json:"user_id"`
	// DeviceID 设备标识(可选)
	DeviceID string `json:"device_id,omitempty"`
	// IP 登录 IP
	IP string `json:"ip"`
	// UserAgent 客户端 UA
	UserAgent string `json:"user_agent,omitempty"`
	// CreatedAt 创建时间
	CreatedAt time.Time `json:"created_at"`
	// ExpiresAt 过期时间
	ExpiresAt time.Time `json:"expires_at"`
	// LastAccessAt 最后访问时间
	LastAccessAt time.Time `json:"last_access_at"`
	// IsRevoked 是否已撤销
	IsRevoked bool `json:"is_revoked"`
}

// IsExpired 检查会话是否已过期
func (s *Session) IsExpired() bool {
	return time.Now().After(s.ExpiresAt)
}

// IsActive 会话是否活跃(未过期且未撤销)
func (s *Session) IsActive() bool {
	return !s.IsExpired() && !s.IsRevoked
}

// sessionManager 会话管理器
type sessionManager struct {
	mu     sync.RWMutex
	cfg    SessionConfig
	store  *storage.Store
	server *Server
	// sessions 内存索引: token -> *Session
	sessions map[string]*Session
	// userIndex 用户会话索引: userID -> []string (token列表)
	userIndex map[string][]string
	loaded   bool
	stopCh   chan struct{}
}

func newSessionManager(s *Server, cfg SessionConfig) *sessionManager {
	return &sessionManager{
		cfg:       cfg,
		store:     s.store,
		server:    s,
		sessions:  make(map[string]*Session),
		userIndex: make(map[string][]string),
		stopCh:    make(chan struct{}),
	}
}

// loadFromStore 从存储加载会话
func (sm *sessionManager) loadFromStore() {
	if sm.loaded {
		return
	}
	cfg := struct {
		Sessions []Session `json:"sessions"`
	}{}
	_, err := sm.store.Get([]byte("cluster/sessions"), &cfg)
	if err == nil {
		sm.mu.Lock()
		for i := range cfg.Sessions {
			s := &cfg.Sessions[i]
			if !s.IsExpired() {
				sm.sessions[s.Token] = s
				sm.userIndex[s.UserID] = append(sm.userIndex[s.UserID], s.Token)
			}
		}
		sm.loaded = true
		sm.mu.Unlock()
	}
}

// saveToStore 持久化会话 (外层必须持 RLock 或 Lock)
func (sm *sessionManager) saveToStore() error {
	sessions := make([]Session, 0, len(sm.sessions))
	for _, s := range sm.sessions {
		sessions = append(sessions, *s)
	}
	cfg := struct {
		Sessions []Session `json:"sessions"`
	}{Sessions: sessions}
	return sm.store.Put([]byte("cluster/sessions"), cfg)
}

// persistToStore 获取读锁并持久化(供无锁调用方使用)
func (sm *sessionManager) persistToStore() error {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.saveToStore()
}

// generateToken 生成会话 Token(HMAC-SHA256)
func (sm *sessionManager) generateToken(userID, deviceID string) string {
	// 随机部分 + 时间戳
	now := time.Now().UnixNano()
	raw := fmt.Sprintf("%s:%s:%d:%s", userID, deviceID, now, randomHex(16))
	mac := hmac.New(sha256.New, []byte(sm.cfg.Secret))
	mac.Write([]byte(raw))
	sig := hex.EncodeToString(mac.Sum(nil))
	return hex.EncodeToString([]byte(raw)) + "." + sig
}

// randomHex 生成随机 hex 字符串
func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// validateToken 校验 Token 有效性
func (sm *sessionManager) validateToken(token string) (string, error) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("invalid token format")
	}
	rawHex := parts[0]
	sig := parts[1]

	// 解码 hex 得到原始 payload
	rawBytes, err := hex.DecodeString(rawHex)
	if err != nil {
		return "", fmt.Errorf("invalid token encoding: %w", err)
	}

	// 重新计算签名(基于原始 payload 字节)
	mac := hmac.New(sha256.New, []byte(sm.cfg.Secret))
	mac.Write(rawBytes)
	expectedSig := hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(sig), []byte(expectedSig)) {
		return "", fmt.Errorf("invalid token signature")
	}

	// 解析 userID
	fields := strings.SplitN(string(rawBytes), ":", 2)
	if len(fields) < 1 {
		return "", fmt.Errorf("invalid token payload")
	}
	return fields[0], nil
}

// CreateSession 创建新会话
func (sm *sessionManager) CreateSession(userID string, req *http.Request) (*Session, error) {
	sm.loadFromStore()

	deviceID := getDeviceID(req)
	token := sm.generateToken(userID, deviceID)
	now := time.Now()

	s := &Session{
		Token:        token,
		UserID:       userID,
		DeviceID:     deviceID,
		IP:           clientIP(req),
		UserAgent:    req.Header.Get("User-Agent"),
		CreatedAt:    now,
		ExpiresAt:    now.Add(sm.cfg.Timeout),
		LastAccessAt: now,
	}

	sm.mu.Lock()
	// 检查最大会话数
	if sm.cfg.MaxSessions > 0 {
		userSessions := sm.userIndex[userID]
		if len(userSessions) >= sm.cfg.MaxSessions {
			// 撤销最老的会话
			oldest := sm.findOldestSession(userSessions)
			if oldest != nil {
				delete(sm.sessions, oldest.Token)
				filtered := make([]string, 0, len(userSessions)-1)
				for _, t := range userSessions {
					if t != oldest.Token {
						filtered = append(filtered, t)
					}
				}
				sm.userIndex[userID] = filtered
			}
		}
	}
	sm.sessions[token] = s
	sm.userIndex[userID] = append(sm.userIndex[userID], token)
	sm.mu.Unlock()

	_ = sm.persistToStore()
	return s, nil
}

// findOldestSession 从 token 列表中找到最老的会话
func (sm *sessionManager) findOldestSession(tokens []string) *Session {
	var oldest *Session
	for _, t := range tokens {
		if s, ok := sm.sessions[t]; ok {
			if oldest == nil || s.CreatedAt.Before(oldest.CreatedAt) {
				oldest = s
			}
		}
	}
	return oldest
}

// GetSession 根据 Token 获取会话
func (sm *sessionManager) GetSession(token string) (*Session, error) {
	sm.loadFromStore()

	userID, err := sm.validateToken(token)
	if err != nil {
		return nil, err
	}

	sm.mu.RLock()
	s, ok := sm.sessions[token]
	sm.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("session not found")
	}
	if s.IsExpired() {
		sm.RevokeSession(token)
		return nil, fmt.Errorf("session expired")
	}
	if s.IsRevoked {
		return nil, fmt.Errorf("session revoked")
	}
	if s.UserID != userID {
		return nil, fmt.Errorf("session user mismatch")
	}

	return s, nil
}

// TouchSession 更新会话最后访问时间
func (sm *sessionManager) TouchSession(token string) {
	sm.mu.Lock()
	if s, ok := sm.sessions[token]; ok {
		s.LastAccessAt = time.Now()
	}
	sm.mu.Unlock()
}

// RevokeSession 撤销指定会话
func (sm *sessionManager) RevokeSession(token string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if s, ok := sm.sessions[token]; ok {
		s.IsRevoked = true
	}
	// 从用户索引中移除
	userID := sm.findUserForToken(token)
	if userID != "" {
		tokens := sm.userIndex[userID]
		filtered := make([]string, 0, len(tokens))
		for _, t := range tokens {
			if t != token {
				filtered = append(filtered, t)
			}
		}
		sm.userIndex[userID] = filtered
	}
	delete(sm.sessions, token)
	_ = sm.saveToStore()
}

// RevokeAllUserSessions 撤销指定用户所有会话
func (sm *sessionManager) RevokeAllUserSessions(userID string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	tokens := sm.userIndex[userID]
	for _, t := range tokens {
		delete(sm.sessions, t)
	}
	delete(sm.userIndex, userID)
	_ = sm.saveToStore()
}

// findUserForToken 查找 Token 对应的用户
func (sm *sessionManager) findUserForToken(token string) string {
	for uid, tokens := range sm.userIndex {
		for _, t := range tokens {
			if t == token {
				return uid
			}
		}
	}
	return ""
}

// ListUserSessions 列出指定用户所有活跃会话
func (sm *sessionManager) ListUserSessions(userID string) []*Session {
	sm.loadFromStore()
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	tokens := sm.userIndex[userID]
	sessions := make([]*Session, 0)
	for _, t := range tokens {
		if s, ok := sm.sessions[t]; ok && s.IsActive() {
			sessions = append(sessions, s)
		}
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].CreatedAt.After(sessions[j].CreatedAt)
	})
	return sessions
}

// ListAllSessions 列出所有活跃会话(管理员用)
func (sm *sessionManager) ListAllSessions() []*Session {
	sm.loadFromStore()
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	sessions := make([]*Session, 0)
	for _, s := range sm.sessions {
		if s.IsActive() {
			sessions = append(sessions, s)
		}
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].CreatedAt.After(sessions[j].CreatedAt)
	})
	return sessions
}

// CleanupExpired 清理过期会话
func (sm *sessionManager) CleanupExpired() int {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	count := 0
	for token, s := range sm.sessions {
		if s.IsExpired() {
			userID := sm.findUserForToken(token)
			if userID != "" {
				tokens := sm.userIndex[userID]
				filtered := make([]string, 0, len(tokens))
				for _, t := range tokens {
					if t != token {
						filtered = append(filtered, t)
					}
				}
				sm.userIndex[userID] = filtered
			}
			delete(sm.sessions, token)
			count++
		}
	}
	if count > 0 {
		_ = sm.saveToStore()
	}
	return count
}

// StartCleanupLoop 启动后台清理协程
func (sm *sessionManager) StartCleanupLoop() {
	if sm.cfg.CleanupInterval <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(sm.cfg.CleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-sm.stopCh:
				return
			case <-ticker.C:
				sm.CleanupExpired()
			}
		}
	}()
}

// Stop 停止清理协程
func (sm *sessionManager) Stop() {
	select {
	case <-sm.stopCh:
	default:
		close(sm.stopCh)
	}
}

// Stats 会话统计信息
type SessionStats struct {
	TotalSessions    int              `json:"total_sessions"`
	ActiveSessions    int              `json:"active_sessions"`
	ExpiredSessions  int              `json:"expired_sessions"`
	RevokedSessions  int              `json:"revoked_sessions"`
	UsersCount       int              `json:"users_count"`
	Config           SessionConfig    `json:"config"`
}

// GetStats 获取会话统计
func (sm *sessionManager) GetStats() SessionStats {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	stats := SessionStats{
		TotalSessions: len(sm.sessions),
		Config:        sm.cfg,
	}
	userSet := make(map[string]struct{})
	for _, s := range sm.sessions {
		if s.IsActive() {
			stats.ActiveSessions++
		}
		if s.IsExpired() {
			stats.ExpiredSessions++
		}
		if s.IsRevoked {
			stats.RevokedSessions++
		}
		userSet[s.UserID] = struct{}{}
	}
	stats.UsersCount = len(userSet)
	return stats
}

// getDeviceID 从请求中提取或生成设备标识
func getDeviceID(r *http.Request) string {
	// 优先使用 X-Device-Id 头
	if id := r.Header.Get("X-Device-Id"); id != "" {
		return id
	}
	// 否则基于 IP + UA 生成
	ip := clientIP(r)
	ua := r.Header.Get("User-Agent")
	h := sha256.New()
	h.Write([]byte(ip + ":" + ua))
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// ---------- HTTP Handlers ----------

// handleLogin POST /_security/login
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST", "")
		return
	}

	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "parse_exception", err.Error(), "")
		return
	}

	if req.Username == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "illegal_argument_exception", "username and password required", "")
		return
	}

	// 验证用户凭据
	s.rbac.loadFromStore(s)
	user, ok := s.rbac.getUser(req.Username)
	if !ok {
		// 用户不在 RBAC 中, 检查是否在 Basic Auth 配置中
		if !s.checkBasicCredentials(req.Username, req.Password) {
			writeError(w, http.StatusUnauthorized, "security_exception", "invalid username or password", "")
			return
		}
	} else if !user.CheckPassword(req.Password) {
		writeError(w, http.StatusUnauthorized, "security_exception", "invalid username or password", "")
		return
	}

	// 创建会话
	if s.sessionMgr != nil && s.sessionMgr.cfg.Enabled {
		session, err := s.sessionMgr.CreateSession(req.Username, r)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "session_create_failed", err.Error(), "")
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status":     "ok",
			"user_id":    req.Username,
			"roles":      getUserRoles(s, req.Username),
			"token":      session.Token,
			"expires_at": session.ExpiresAt.Format(time.RFC3339),
			"session_id": session.Token[:12],
		})
		return
	}

	// 会话管理未启用时, 返回 404
	writeError(w, http.StatusNotFound, "session_manager_disabled", "session management is not enabled", "")
}

// handleLogout POST /_security/logout
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST", "")
		return
	}

	token := extractSessionToken(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "security_exception", "no active session", "")
		return
	}

	if s.sessionMgr != nil {
		s.sessionMgr.RevokeSession(token)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "logged_out"})
}

// handleLogoutAll POST /_security/logout_all
func (s *Server) handleLogoutAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST", "")
		return
	}

	token := extractSessionToken(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "security_exception", "no active session", "")
		return
	}

	user := ""
	if s.sessionMgr != nil {
		if u, err := s.sessionMgr.validateToken(token); err == nil {
			user = u
			s.sessionMgr.RevokeAllUserSessions(user)
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":            "all_logged_out",
		"user_id":           user,
		"sessions_revoked":  true,
	})
}

// handleGetCurrentSession GET /_security/session
func (s *Server) handleGetCurrentSession(w http.ResponseWriter, r *http.Request) {
	token := extractSessionToken(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "security_exception", "no active session", "")
		return
	}

	if s.sessionMgr == nil {
		writeError(w, http.StatusNotFound, "session_manager_disabled", "", "")
		return
	}

	session, err := s.sessionMgr.GetSession(token)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "security_exception", err.Error(), "")
		return
	}

	writeJSON(w, http.StatusOK, formatSession(session))
}

// handleListSessions GET /_security/sessions
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	user := getUsernameFromCtx(r.Context())
	if user == "" {
		writeError(w, http.StatusUnauthorized, "security_exception", "not authenticated", "")
		return
	}

	if s.sessionMgr == nil {
		writeError(w, http.StatusNotFound, "session_manager_disabled", "", "")
		return
	}

	sessions := s.sessionMgr.ListUserSessions(user)
	formatted := make([]map[string]interface{}, 0, len(sessions))
	for _, s := range sessions {
		formatted = append(formatted, formatSession(s))
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"sessions": formatted,
		"count":    len(formatted),
	})
}

// handleGetSession GET /_security/session/{token}
func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	token := pathSegment(r, 3)
	if token == "" {
		writeError(w, http.StatusBadRequest, "illegal_argument_exception", "token required", "")
		return
	}

	if s.sessionMgr == nil {
		writeError(w, http.StatusNotFound, "session_manager_disabled", "", "")
		return
	}

	session, err := s.sessionMgr.GetSession(token)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error(), "")
		return
	}

	writeJSON(w, http.StatusOK, formatSession(session))
}

// handleRevokeSession DELETE /_security/session/{token}
func (s *Server) handleRevokeSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use DELETE", "")
		return
	}

	token := pathSegment(r, 3)
	if token == "" {
		writeError(w, http.StatusBadRequest, "illegal_argument_exception", "token required", "")
		return
	}

	if s.sessionMgr == nil {
		writeError(w, http.StatusNotFound, "session_manager_disabled", "", "")
		return
	}

	s.sessionMgr.RevokeSession(token)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "session_revoked",
		"token":  token[:12] + "...",
	})
}

// handleRevokeAllSessions DELETE /_security/sessions
func (s *Server) handleRevokeAllSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use DELETE", "")
		return
	}

	user := getUsernameFromCtx(r.Context())
	if user == "" {
		writeError(w, http.StatusUnauthorized, "security_exception", "not authenticated", "")
		return
	}

	if s.sessionMgr == nil {
		writeError(w, http.StatusNotFound, "session_manager_disabled", "", "")
		return
	}

	count := len(s.sessionMgr.ListUserSessions(user))
	s.sessionMgr.RevokeAllUserSessions(user)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":         "all_sessions_revoked",
		"user_id":        user,
		"revoked_count":  count,
	})
}

// handleSessionStats GET /_security/session/stats
func (s *Server) handleSessionStats(w http.ResponseWriter, r *http.Request) {
	if s.sessionMgr == nil {
		writeJSON(w, http.StatusOK, SessionStats{Config: DefaultSessionConfig()})
		return
	}
	stats := s.sessionMgr.GetStats()
	// 不返回 secret
	stats.Config.Secret = ""
	writeJSON(w, http.StatusOK, stats)
}

// handleSessionConfig GET/PUT /_security/session/config
func (s *Server) handleSessionConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if s.sessionMgr == nil {
			writeJSON(w, http.StatusOK, DefaultSessionConfig())
			return
		}
		cfg := s.sessionMgr.cfg
		cfg.Secret = "" // 不返回密钥
		writeJSON(w, http.StatusOK, cfg)

	case http.MethodPut:
		if s.sessionMgr == nil {
			writeError(w, http.StatusNotFound, "session_manager_disabled", "", "")
			return
		}
		var newCfg SessionConfig
		if err := decodeJSON(r, &newCfg); err != nil {
			writeError(w, http.StatusBadRequest, "parse_exception", err.Error(), "")
			return
		}
		// 只更新允许的字段
		if newCfg.Timeout > 0 {
			s.sessionMgr.cfg.Timeout = newCfg.Timeout
		}
		if newCfg.MaxSessions > 0 {
			s.sessionMgr.cfg.MaxSessions = newCfg.MaxSessions
		}
		if newCfg.CleanupInterval > 0 {
			s.sessionMgr.cfg.CleanupInterval = newCfg.CleanupInterval
		}
		s.sessionMgr.cfg.Enabled = newCfg.Enabled
		cfg := s.sessionMgr.cfg
		cfg.Secret = ""
		writeJSON(w, http.StatusOK, cfg)

	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET or PUT", "")
	}
}

// handleListAllSessions GET /_security/sessions/all (管理员)
func (s *Server) handleListAllSessions(w http.ResponseWriter, r *http.Request) {
	if s.sessionMgr == nil {
		writeError(w, http.StatusNotFound, "session_manager_disabled", "", "")
		return
	}
	sessions := s.sessionMgr.ListAllSessions()
	formatted := make([]map[string]interface{}, 0, len(sessions))
	for _, s := range sessions {
		formatted = append(formatted, formatSession(s))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"sessions": formatted,
		"count":    len(formatted),
	})
}

// ---------- 辅助函数 ----------

// extractSessionToken 从请求中提取 Session Token
func extractSessionToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	// 也支持 Cookie
	if cookie, err := r.Cookie("go_es_session"); err == nil {
		return cookie.Value
	}
	return ""
}

// formatSession 格式化会话为 API 响应
func formatSession(s *Session) map[string]interface{} {
	return map[string]interface{}{
		"token":         s.Token[:20] + "...", // 只返回部分 token
		"session_id":    s.Token[:12],
		"user_id":       s.UserID,
		"device_id":     s.DeviceID,
		"ip":            s.IP,
		"user_agent":    truncateStr(s.UserAgent, 100),
		"created_at":    s.CreatedAt.Format(time.RFC3339),
		"expires_at":    s.ExpiresAt.Format(time.RFC3339),
		"last_access_at": s.LastAccessAt.Format(time.RFC3339),
		"is_active":     s.IsActive(),
		"is_expired":    s.IsExpired(),
		"is_revoked":    s.IsRevoked,
	}
}

// truncateStr 截断字符串
func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// checkBasicCredentials 检查 Basic Auth 凭据(兼容模式)
func (s *Server) checkBasicCredentials(username, password string) bool {
	if !s.guards.auth.Enabled {
		return false
	}
	expected, ok := s.guards.auth.Basic[username]
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(password), []byte(expected)) == 1
}

// getUserRoles 获取用户的所有角色
func getUserRoles(s *Server, username string) []string {
	if s.rbac == nil {
		return nil
	}
	s.rbac.loadFromStore(s)
	user, ok := s.rbac.getUser(username)
	if !ok {
		return nil
	}
	return user.Roles
}

// middlewareSession 会话 Token 校验中间件
func (s *Server) middlewareSession(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 白名单路径跳过
		if isPublicPath(r.URL.Path) || isAuthPath(r.URL.Path) {
			h.ServeHTTP(w, r)
			return
		}

		// 如果 Session 管理未启用, 跳过
		if s.sessionMgr == nil || !s.sessionMgr.cfg.Enabled {
			h.ServeHTTP(w, r)
			return
		}

		token := extractSessionToken(r)
		if token == "" {
			// 没有 token, 让下游 auth 中间件处理(可能是 Basic Auth)
			h.ServeHTTP(w, r)
			return
		}

		session, err := s.sessionMgr.GetSession(token)
		if err != nil {
			// Token 无效, 让下游处理(可能已有 Basic Auth)
			h.ServeHTTP(w, r)
			return
		}

		// 更新最后访问时间
		s.sessionMgr.TouchSession(token)

		// 将用户信息注入 context
		ctx := context.WithValue(r.Context(), ctxKeyUsername, session.UserID)
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

// isAuthPath 检查是否为认证相关路径
func isAuthPath(p string) bool {
	switch p {
	case "/_security/login",
		"/_security/logout",
		"/_security/logout_all":
		return true
	}
	return false
}

// 防止 import 未使用
var (
	_ = json.Marshal
	_ = sync.RWMutex{}
)
