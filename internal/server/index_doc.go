// 索引与文档相关路由
package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/zixiliuyue/go_es/internal/storage"
)

// IndexMeta 索引元信息(对应 meta/<index>)
type IndexMeta struct {
	Name       string                 `json:"name"`
	CreatedAt  int64                  `json:"created_at"`
	Mapping    map[string]interface{} `json:"mapping,omitempty"`
	Settings   map[string]interface{} `json:"settings,omitempty"`
	Aliases    map[string]interface{} `json:"aliases,omitempty"`
	DocCount   int64                  `json:"doc_count"`
	StoreBytes int64                  `json:"store_bytes"`
}

// handleIndexCreateForName PUT /{index}
func (s *Server) handleIndexCreateForName(w http.ResponseWriter, r *http.Request, index string) {
	if index == "" {
		writeError(w, http.StatusBadRequest, "illegal_argument_exception", "index required", "")
		return
	}
	exists, err := s.store.Exists(storage.MetaKey(index))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error(), "")
		return
	}
	if exists {
		writeError(w, http.StatusBadRequest, "resource_already_exists_exception",
			"index already exists", index)
		return
	}
	var body map[string]interface{}
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, "parse_exception", err.Error(), "")
			return
		}
	}
	meta := IndexMeta{
		Name:      index,
		CreatedAt: time.Now().Unix(),
		Mapping:   mapOf(body, "mappings"),
		Settings:  mapOf(body, "settings"),
		Aliases:   mapOf(body, "aliases"),
	}
	if err := s.store.Put(storage.MetaKey(index), meta); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"acknowledged":       true,
		"shards_acknowledged": true,
		"index":              index,
	})
}

// handleIndexExistsForName HEAD/GET /{index}
func (s *Server) handleIndexExistsForName(w http.ResponseWriter, r *http.Request, index string) {
	if index == "" {
		writeError(w, http.StatusBadRequest, "illegal_argument_exception", "index required", "")
		return
	}
	found, err := s.store.Exists(storage.MetaKey(index))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "search_phase_execution_exception", err.Error(), "")
		return
	}
	if !found {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleIndexDeleteForName DELETE /{index}
func (s *Server) handleIndexDeleteForName(w http.ResponseWriter, r *http.Request, index string) {
	if exists, _ := s.store.Exists(storage.MetaKey(index)); !exists {
		writeError(w, http.StatusNotFound, "index_not_found_exception",
			"index does not exist", index)
		return
	}
	if err := s.store.Delete(storage.MetaKey(index)); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error(), "")
		return
	}
	_ = s.store.DeletePrefix(storage.DocPrefix(index))
	// 清掉指向此 index 的别名
	_ = s.store.Scan([]byte("alias/"), func(k, v []byte) error {
		var list []string
		if err := json.Unmarshal(v, &list); err != nil {
			return nil
		}
		filtered := make([]string, 0, len(list))
		for _, x := range list {
			if x != index {
				filtered = append(filtered, x)
			}
		}
		if len(filtered) == 0 {
			_ = s.store.Delete(k)
		} else {
			_ = s.store.PutRaw(k, mustJSON(filtered))
		}
		return nil
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{"acknowledged": true})
}

// handleIndexMappingForName GET /{index}/_mapping
func (s *Server) handleIndexMappingForName(w http.ResponseWriter, r *http.Request, index string) {
	var meta IndexMeta
	found, err := s.store.Get(storage.MetaKey(index), &meta)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error(), "")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "index_not_found_exception",
			"index does not exist", index)
		return
	}
	mapping := meta.Mapping
	if mapping == nil {
		mapping = map[string]interface{}{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		index: map[string]interface{}{
			"mappings": mapping,
		},
	})
}

// 文档接口

// handleDocIndexForName PUT/POST /{index}/_doc/{id}
func (s *Server) handleDocIndexForName(w http.ResponseWriter, r *http.Request, index, id string) {
	if index == "" || id == "" {
		writeError(w, http.StatusBadRequest, "illegal_argument_exception", "index and id required", "")
		return
	}
	exists, _ := s.store.Exists(storage.MetaKey(index))
	if !exists {
		writeError(w, http.StatusNotFound, "index_not_found_exception",
			"index does not exist", index)
		return
	}
	var doc map[string]interface{}
	if err := decodeJSON(r, &doc); err != nil {
		writeError(w, http.StatusBadRequest, "parse_exception", err.Error(), "")
		return
	}
	if p := r.URL.Query().Get("pipeline"); p != "" {
		processed, perr := s.runPipeline(p, doc)
		if perr != nil {
			writeError(w, http.StatusBadRequest, "illegal_argument_exception", perr.Error(), "")
			return
		}
		doc = processed
	}
	if err := s.store.Put(storage.DocKey(index, id), doc); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error(), "")
		return
	}
	s.engine.IndexDoc(index, id, doc)
	resp := map[string]interface{}{
		"_index":   index,
		"_id":      id,
		"_version": 1,
		"result":   "created",
		"created":  true,
	}
	if r.URL.Query().Get("refresh") == "true" || r.URL.Query().Get("refresh") == "wait_for" {
		resp["_refresh"] = true
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleDocIndexAutoIDForName POST /{index}/_doc
func (s *Server) handleDocIndexAutoIDForName(w http.ResponseWriter, r *http.Request, index string) {
	if index == "" {
		writeError(w, http.StatusBadRequest, "illegal_argument_exception", "index required", "")
		return
	}
	exists, _ := s.store.Exists(storage.MetaKey(index))
	if !exists {
		writeError(w, http.StatusNotFound, "index_not_found_exception",
			"index does not exist", index)
		return
	}
	var doc map[string]interface{}
	if err := decodeJSON(r, &doc); err != nil {
		writeError(w, http.StatusBadRequest, "parse_exception", err.Error(), "")
		return
	}
	id := generateID()
	if err := s.store.Put(storage.DocKey(index, id), doc); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error(), "")
		return
	}
	s.engine.IndexDoc(index, id, doc)
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"_index":   index,
		"_id":      id,
		"_version": 1,
		"result":   "created",
		"created":  true,
	})
}

// handleDocGetForName GET /{index}/_doc/{id}
func (s *Server) handleDocGetForName(w http.ResponseWriter, r *http.Request, index, id string) {
	raw, found, err := s.store.GetRaw(storage.DocKey(index, id))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error(), "")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "document_missing_exception",
			"document not found", id)
		return
	}
	var doc map[string]interface{}
	_ = json.Unmarshal(raw, &doc)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"_index":   index,
		"_id":      id,
		"_version": 1,
		"_source":  doc,
		"found":    true,
	})
}

// handleUpdateForName POST /{index}/_update/{id}
// 简化版:仅支持 {"doc": {...}} 形式(覆盖式合并)
func (s *Server) handleUpdateForName(w http.ResponseWriter, r *http.Request, index, id string) {
	if index == "" || id == "" {
		writeError(w, http.StatusBadRequest, "illegal_argument_exception", "index and id required", "")
		return
	}
	exists, _ := s.store.Exists(storage.DocKey(index, id))
	if !exists {
		writeError(w, http.StatusNotFound, "document_missing_exception",
			"document not found", id)
		return
	}
	var req struct {
		Doc map[string]interface{} `json:"doc"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "parse_exception", err.Error(), "")
		return
	}
	raw, _, _ := s.store.GetRaw(storage.DocKey(index, id))
	existing := map[string]interface{}{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &existing)
	}
	for k, v := range req.Doc {
		existing[k] = v
	}
	if err := s.store.Put(storage.DocKey(index, id), existing); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error(), "")
		return
	}
	s.engine.IndexDoc(index, id, existing)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"_index":   index,
		"_id":      id,
		"_version": 2,
		"result":   "updated",
	})
}

// handleDocExistsForName HEAD /{index}/_doc/{id}
func (s *Server) handleDocExistsForName(w http.ResponseWriter, r *http.Request, index, id string) {
	exists, _ := s.store.Exists(storage.DocKey(index, id))
	if exists {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusNotFound)
	}
}

// handleDocDeleteForName DELETE /{index}/_doc/{id}
func (s *Server) handleDocDeleteForName(w http.ResponseWriter, r *http.Request, index, id string) {
	exists, _ := s.store.Exists(storage.DocKey(index, id))
	if !exists {
		writeError(w, http.StatusNotFound, "document_missing_exception",
			"document not found", id)
		return
	}
	if err := s.store.Delete(storage.DocKey(index, id)); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error(), "")
		return
	}
	s.engine.DeleteDoc(index, id)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"_index":   index,
		"_id":      id,
		"_version": 2,
		"result":   "deleted",
	})
}

// mapOf 取出嵌套 map 的子字段
func mapOf(m map[string]interface{}, key string) map[string]interface{} {
	if m == nil {
		return nil
	}
	v, ok := m[key]
	if !ok {
		return nil
	}
	out, _ := v.(map[string]interface{})
	return out
}

// mustJSON 序列化为 []byte,出错时返回空
func mustJSON(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}

// generateID 产生一个 UUID 风格(简化版)的随机 ID
func generateID() string {
	now := time.Now().UnixNano()
	globalCounter.counterMu.Lock()
	seq := globalCounter.counter
	globalCounter.counter++
	globalCounter.counterMu.Unlock()
	return strings.ReplaceAll(time.Unix(0, now).UTC().Format("20060102T150405.000"), ".", "") + "-" + itoa(seq)
}

var globalCounter struct {
	counterMu sync.Mutex
	counter   int64
}

// itoa 把 int64 转为字符串(避免再导入 strconv)
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		pos--
		b[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

// pathSegment 从 r.URL.Path 中取出第 n 段(0-based)
func pathSegment(r *http.Request, n int) string {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if n < 0 || n >= len(parts) {
		return ""
	}
	return parts[n]
}
