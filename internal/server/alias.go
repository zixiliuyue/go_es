// 别名相关路由
package server

import (
	"encoding/json"
	"net/http"

	"github.com/zixiliuyue/go_es/internal/storage"
)

// aliasesAction 别名动作,对应 _aliases 接口
type aliasesAction struct {
	Add    *aliasOp `json:"add,omitempty"`
	Remove *aliasOp `json:"remove,omitempty"`
}

type aliasOp struct {
	Index string `json:"index"`
	Alias string `json:"alias"`
}

// handleAliasesUpdate POST /_aliases
func (s *Server) handleAliasesUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Actions []aliasesAction `json:"actions"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "parse_exception", err.Error(), "")
		return
	}
	for _, act := range req.Actions {
		if act.Add != nil {
			if err := s.addAlias(act.Add.Index, act.Add.Alias); err != nil {
				writeError(w, http.StatusBadRequest, "illegal_argument_exception", err.Error(), "")
				return
			}
		}
		if act.Remove != nil {
			if err := s.removeAlias(act.Remove.Index, act.Remove.Alias); err != nil {
				writeError(w, http.StatusBadRequest, "illegal_argument_exception", err.Error(), "")
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"acknowledged": true})
}

// addAlias 添加索引到别名
func (s *Server) addAlias(index, alias string) error {
	if index == "" || alias == "" {
		return errBadInput("index and alias required")
	}
	exists, _ := s.store.Exists(storage.MetaKey(index))
	if !exists {
		return errBadInput("index does not exist: " + index)
	}
	var list []string
	found, _ := s.store.Get(storage.AliasKey(alias), &list)
	if !found {
		list = []string{}
	}
	for _, x := range list {
		if x == index {
			return nil
		}
	}
	list = append(list, index)
	return s.store.Put(storage.AliasKey(alias), list)
}

// removeAlias 从别名中移除索引
func (s *Server) removeAlias(index, alias string) error {
	if index == "" || alias == "" {
		return errBadInput("index and alias required")
	}
	var list []string
	found, _ := s.store.Get(storage.AliasKey(alias), &list)
	if !found {
		return nil
	}
	filtered := make([]string, 0, len(list))
	for _, x := range list {
		if x != index {
			filtered = append(filtered, x)
		}
	}
	if len(filtered) == 0 {
		_ = s.store.Delete(storage.AliasKey(alias))
	} else {
		return s.store.Put(storage.AliasKey(alias), filtered)
	}
	return nil
}

// handleAliasGetByPath GET /_alias/{name}
func (s *Server) handleAliasGetByPath(w http.ResponseWriter, r *http.Request) {
	name := pathSegment(r, 1)
	s.handleAliasGetForName(w, r, name)
}

// handleAliasExistsByPath HEAD /_alias/{name}
func (s *Server) handleAliasExistsByPath(w http.ResponseWriter, r *http.Request) {
	name := pathSegment(r, 1)
	exists, _ := s.store.Exists(storage.AliasKey(name))
	if exists {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusNotFound)
	}
}

// handleAliasGetForName 真正的别名查询逻辑
func (s *Server) handleAliasGetForName(w http.ResponseWriter, r *http.Request, name string) {
	if name == "" {
		writeError(w, http.StatusBadRequest, "illegal_argument_exception", "alias required", "")
		return
	}
	raw, found, err := s.store.GetRaw(storage.AliasKey(name))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error(), "")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "alias_not_found_exception",
			"alias does not exist", name)
		return
	}
	var list []string
	_ = json.Unmarshal(raw, &list)
	resp := map[string]interface{}{}
	for _, idx := range list {
		resp[idx] = map[string]interface{}{
			"aliases": map[string]interface{}{
				name: map[string]interface{}{},
			},
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// errBadInput 简单包装一个 error
type errBadInput string

func (e errBadInput) Error() string { return string(e) }
