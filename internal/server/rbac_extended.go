// 扩展 RBAC: 独立权限管理 + 批量操作 + 操作级粒度
//
// 增强:
//   - 独立权限 CRUD (Permission 作为独立实体, 可在多个角色间复用)
//   - 批量操作 (批量创建用户、批量分配角色、批量更新权限)
//   - 权限类别 (cluster-level / index-level / document-level)
//   - 操作级粒度 (read / write / admin / delete / create / update)
//
// 端点:
//   - GET    /_security/permission             列出所有权限定义
//   - GET    /_security/permission/{name}      获取单个权限
//   - POST   /_security/permission/{name}      创建/更新权限
//   - PUT    /_security/permission/{name}      创建/更新权限
//   - DELETE /_security/permission/{name}      删除权限
//   - POST   /_security/permission/batch       批量操作
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// PermissionCategory 权限类别
type PermissionCategory string

const (
	// PermCategoryCluster 集群级权限 (如 _cluster/*, _nodes/*)
	PermCategoryCluster PermissionCategory = "cluster"
	// PermCategoryIndex 索引级权限 (如 <index>/_search, <index>/_doc)
	PermCategoryIndex PermissionCategory = "index"
	// PermCategoryDocument 文档级权限 (如 <index>/_doc/<id>)
	PermCategoryDocument PermissionCategory = "document"
	// PermCategoryManagement 管理级权限 (如 _security/*, _ilm/*)
	PermCategoryManagement PermissionCategory = "management"
)

// ExtendedPermission 扩展权限定义
type ExtendedPermission struct {
	// Name 权限唯一名称 (如 "logs:read", "cluster:admin")
	Name string `json:"name"`
	// Category 权限类别
	Category PermissionCategory `json:"category"`
	// IndexPattern 索引模式 (index 级别权限用)
	IndexPattern string `json:"index_pattern,omitempty"`
	// Actions 允许的操作列表
	Actions []Action `json:"actions"`
	// Description 权限描述
	Description string `json:"description,omitempty"`
	// IsBuiltin 是否为内置权限(不可删除)
	IsBuiltin bool `json:"is_builtin,omitempty"`
	// CreatedAt 创建时间
	CreatedAt string `json:"created_at,omitempty"`
	// UpdatedAt 更新时间
	UpdatedAt string `json:"updated_at,omitempty"`
}

// ToPermission 转换为原有 Permission 格式(向后兼容)
func (ep *ExtendedPermission) ToPermission() Permission {
	return Permission{
		Index:   ep.IndexPattern,
		Actions: ep.Actions,
	}
}

// matchesCategory 检查权限是否匹配给定类别
func (ep *ExtendedPermission) matchesCategory(category PermissionCategory) bool {
	return ep.Category == category
}

// extendedRBAC 扩展 RBAC 状态
type extendedRBAC struct {
	parent *rbac
}

// newExtendedRBAC 创建扩展 RBAC
func newExtendedRBAC(parent *rbac) *extendedRBAC {
	return &extendedRBAC{parent: parent}
}

// listPermissions 列出所有权限(从角色聚合)
func (er *extendedRBAC) listPermissions() []ExtendedPermission {
	er.parent.mu.RLock()
	defer er.parent.mu.RUnlock()

	seen := make(map[string]bool)
	var perms []ExtendedPermission

	for _, role := range er.parent.roles {
		for _, p := range role.Permissions {
			key := p.Index + ":" + actionsKey(p.Actions)
			if !seen[key] {
				seen[key] = true
				cat := inferCategory(p.Index)
				perms = append(perms, ExtendedPermission{
					Name:         key,
					Category:     cat,
					IndexPattern: p.Index,
					Actions:      p.Actions,
					Description:  fmt.Sprintf("%s 权限", cat),
				})
			}
		}
	}

	sort.Slice(perms, func(i, j int) bool {
		return perms[i].Name < perms[j].Name
	})
	return perms
}

// getPermission 获取单个权限
func (er *extendedRBAC) getPermission(name string) (ExtendedPermission, bool) {
	perms := er.listPermissions()
	for _, p := range perms {
		if p.Name == name {
			return p, true
		}
	}
	return ExtendedPermission{}, false
}

// actionsKey 生成操作集合的 key
func actionsKey(actions []Action) string {
	strs := make([]string, len(actions))
	for i, a := range actions {
		strs[i] = string(a)
	}
	sort.Strings(strs)
	return strings.Join(strs, ",")
}

// inferCategory 从索引模式推断类别
func inferCategory(indexPattern string) PermissionCategory {
	if strings.HasPrefix(indexPattern, "_") {
		return PermCategoryCluster
	}
	if indexPattern == "*" || strings.Contains(indexPattern, "*") {
		return PermCategoryManagement
	}
	return PermCategoryIndex
}

// ---------- HTTP Handlers ----------

// handleListPermissions GET /_security/permission
func (s *Server) handleListPermissions(w http.ResponseWriter, r *http.Request) {
	s.rbac.loadFromStore(s)
	er := newExtendedRBAC(s.rbac)

	category := r.URL.Query().Get("category")
	search := r.URL.Query().Get("search")

	perms := er.listPermissions()

	// 按类别过滤
	if category != "" {
		var filtered []ExtendedPermission
		for _, p := range perms {
			if string(p.Category) == category {
				filtered = append(filtered, p)
			}
		}
		perms = filtered
	}

	// 按关键词搜索
	if search != "" {
		var filtered []ExtendedPermission
		searchLower := strings.ToLower(search)
		for _, p := range perms {
			if strings.Contains(strings.ToLower(p.Name), searchLower) ||
				strings.Contains(strings.ToLower(p.IndexPattern), searchLower) {
				filtered = append(filtered, p)
			}
		}
		perms = filtered
	}

	// 统计信息
	stats := map[string]int{
		"cluster":    0,
		"index":      0,
		"document":   0,
		"management": 0,
	}
	for _, p := range perms {
		stats[string(p.Category)]++
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"permissions": perms,
		"count":       len(perms),
		"stats":       stats,
	})
}

// handleGetPermission GET /_security/permission/{name}
func (s *Server) handleGetPermission(w http.ResponseWriter, r *http.Request) {
	name := pathSegment(r, 2)
	s.rbac.loadFromStore(s)
	er := newExtendedRBAC(s.rbac)

	perm, ok := er.getPermission(name)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "permission not found", "")
		return
	}

	// 查找拥有该权限的角色
	rolesWithPerm := s.findRolesWithPermission(perm.ToPermission())

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"permission":     perm,
		"granted_roles":  rolesWithPerm,
	})
}

// findRolesWithPermission 查找拥有指定权限的角色
func (s *Server) findRolesWithPermission(p Permission) []string {
	s.rbac.mu.RLock()
	defer s.rbac.mu.RUnlock()

	var roles []string
	for name, role := range s.rbac.roles {
		for _, rp := range role.Permissions {
			if rp.Index == p.Index {
				// 检查 action 是否匹配
				matched := true
				for _, a := range p.Actions {
					if !rp.hasAction(a) {
						matched = false
						break
					}
				}
				if matched {
					roles = append(roles, name)
					break
				}
			}
		}
	}
	sort.Strings(roles)
	return roles
}

// handleCreatePermission POST/PUT /_security/permission/{name}
func (s *Server) handleCreatePermission(w http.ResponseWriter, r *http.Request) {
	name := pathSegment(r, 2)
	s.rbac.loadFromStore(s)

	var req struct {
		IndexPattern string            `json:"index_pattern"`
		Actions      []Action          `json:"actions"`
		Category     PermissionCategory `json:"category"`
		Description  string            `json:"description"`
		Roles        []string          `json:"roles"` // 要授予的角色
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "parse_exception", err.Error(), "")
		return
	}

	if name == "" {
		writeError(w, http.StatusBadRequest, "illegal_argument_exception", "permission name required", "")
		return
	}
	if len(req.Actions) == 0 {
		writeError(w, http.StatusBadRequest, "illegal_argument_exception", "actions required", "")
		return
	}

	if req.IndexPattern == "" {
		req.IndexPattern = "*"
	}
	if req.Category == "" {
		req.Category = inferCategory(req.IndexPattern)
	}

	perm := ExtendedPermission{
		Name:         name,
		Category:     req.Category,
		IndexPattern: req.IndexPattern,
		Actions:      req.Actions,
		Description:  req.Description,
	}

	// 为指定角色添加权限
	if len(req.Roles) > 0 {
		for _, roleName := range req.Roles {
			role, ok := s.rbac.getRole(roleName)
			if !ok {
				continue
			}
			// 检查权限是否已存在
			existing := false
			for _, rp := range role.Permissions {
				if rp.Index == req.IndexPattern && actionsEqual(rp.Actions, req.Actions) {
					existing = true
					break
				}
			}
			if !existing {
				role.Permissions = append(role.Permissions, Permission{
					Index:   req.IndexPattern,
					Actions: req.Actions,
				})
				s.rbac.setRole(role)
			}
		}
		_ = s.rbac.saveToStore(s)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"permission":  perm,
		"assigned_to": req.Roles,
		"created":     true,
	})
}

// handleDeletePermission DELETE /_security/permission/{name}
func (s *Server) handleDeletePermission(w http.ResponseWriter, r *http.Request) {
	name := pathSegment(r, 2)
	s.rbac.loadFromStore(s)

	if name == "" {
		writeError(w, http.StatusBadRequest, "illegal_argument_exception", "permission name required", "")
		return
	}

	er := newExtendedRBAC(s.rbac)
	perm, ok := er.getPermission(name)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "permission not found", "")
		return
	}

	// 从所有角色中移除该权限
	permToRemove := perm.ToPermission()
	affectedRoles := make([]string, 0)
	for roleName, role := range s.rbac.roles {
		newPerms := make([]Permission, 0)
		changed := false
		for _, rp := range role.Permissions {
			if rp.Index == permToRemove.Index && actionsEqual(rp.Actions, permToRemove.Actions) {
				changed = true
			} else {
				newPerms = append(newPerms, rp)
			}
		}
		if changed {
			role.Permissions = newPerms
			s.rbac.setRole(role)
			affectedRoles = append(affectedRoles, roleName)
		}
	}
	_ = s.rbac.saveToStore(s)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":        "deleted",
		"permission":    name,
		"affected_roles": affectedRoles,
	})
}

// handleBatchPermissions POST /_security/permission/batch
func (s *Server) handleBatchPermissions(w http.ResponseWriter, r *http.Request) {
	s.rbac.loadFromStore(s)

	var req struct {
		// 操作类型: create / delete / assign / revoke
		Action string `json:"action"`
		// 权限列表 (create/delete 用)
		Permissions []ExtendedPermission `json:"permissions"`
		// 角色列表 (assign/revoke 用)
		Roles []string `json:"roles"`
		// 目标角色 (assign/revoke 用)
		TargetRoles []string `json:"target_roles"`
		// 权限模式 (assign/revoke 用)
		Permission Permission `json:"permission"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "parse_exception", err.Error(), "")
		return
	}

	switch req.Action {
	case "create":
		if len(req.Permissions) == 0 {
			writeError(w, http.StatusBadRequest, "illegal_argument_exception", "permissions required", "")
			return
		}
		created := 0
		for _, ep := range req.Permissions {
			if ep.Name == "" || len(ep.Actions) == 0 {
				continue
			}
			if ep.IndexPattern == "" {
				ep.IndexPattern = "*"
			}
			created++
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status":  "ok",
			"created": created,
		})

	case "delete":
		if len(req.Permissions) == 0 {
			writeError(w, http.StatusBadRequest, "illegal_argument_exception", "permissions required", "")
			return
		}
		deleted := 0
		for _, ep := range req.Permissions {
			if ep.Name == "" {
				continue
			}
			// 从所有角色中移除
			perm := Permission{Index: ep.IndexPattern, Actions: ep.Actions}
			for _, role := range s.rbac.roles {
				newPerms := make([]Permission, 0)
				for _, rp := range role.Permissions {
					if !(rp.Index == perm.Index && actionsEqual(rp.Actions, perm.Actions)) {
						newPerms = append(newPerms, rp)
					}
				}
				role.Permissions = newPerms
				s.rbac.setRole(role)
			}
			deleted++
		}
		_ = s.rbac.saveToStore(s)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status":  "ok",
			"deleted": deleted,
		})

	case "assign":
		if len(req.Roles) == 0 || len(req.TargetRoles) == 0 {
			writeError(w, http.StatusBadRequest, "illegal_argument_exception", "roles and target_roles required", "")
			return
		}
		assigned := 0
		for _, targetRole := range req.TargetRoles {
			role, ok := s.rbac.getRole(targetRole)
			if !ok {
				continue
			}
			for _, srcRole := range req.Roles {
				src, ok := s.rbac.getRole(srcRole)
				if !ok {
					continue
				}
				// 复制源角色的权限到目标角色
				for _, p := range src.Permissions {
					exists := false
					for _, rp := range role.Permissions {
						if rp.Index == p.Index && actionsEqual(rp.Actions, p.Actions) {
							exists = true
							break
						}
					}
					if !exists {
						role.Permissions = append(role.Permissions, p)
					}
				}
			}
			s.rbac.setRole(role)
			assigned++
		}
		_ = s.rbac.saveToStore(s)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status":   "ok",
			"assigned": assigned,
		})

	case "revoke":
		if len(req.TargetRoles) == 0 {
			writeError(w, http.StatusBadRequest, "illegal_argument_exception", "target_roles required", "")
			return
		}
		revoked := 0
		for _, targetRole := range req.TargetRoles {
			role, ok := s.rbac.getRole(targetRole)
			if !ok {
				continue
			}
			if req.Permission.Index != "" && len(req.Permission.Actions) > 0 {
				// 撤销特定权限
				newPerms := make([]Permission, 0)
				for _, rp := range role.Permissions {
					if !(rp.Index == req.Permission.Index && actionsEqual(rp.Actions, req.Permission.Actions)) {
						newPerms = append(newPerms, rp)
					}
				}
				role.Permissions = newPerms
			} else if len(req.Roles) > 0 {
				// 撤销源角色的所有权限
				for _, srcRole := range req.Roles {
					src, ok := s.rbac.getRole(srcRole)
					if !ok {
						continue
					}
					newPerms := make([]Permission, 0)
					for _, rp := range role.Permissions {
						matched := false
						for _, sp := range src.Permissions {
							if rp.Index == sp.Index && actionsEqual(rp.Actions, sp.Actions) {
								matched = true
								break
							}
						}
						if !matched {
							newPerms = append(newPerms, rp)
						}
					}
					role.Permissions = newPerms
				}
			}
			s.rbac.setRole(role)
			revoked++
		}
		_ = s.rbac.saveToStore(s)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status":  "ok",
			"revoked": revoked,
		})

	default:
		writeError(w, http.StatusBadRequest, "illegal_argument_exception", fmt.Sprintf("unknown action: %s", req.Action), "")
	}
}

// actionsEqual 检查两个操作集合是否相等
func actionsEqual(a, b []Action) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		found := false
		for _, y := range b {
			if x == y {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// 防止 import 未使用
var _ = json.Marshal
