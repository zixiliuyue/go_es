// 倒排索引重建端点
//
//   POST /<index>/_inverted/rebuild  -> 强制重建
//   GET  /<index>/_inverted/info     -> 倒排信息
package server

import (
	"net/http"

	"github.com/zixiliuyue/go_es/internal/search"
)

// handleRebuildInverted POST /<index>/_inverted/rebuild
// 强制重建某索引的倒排 + scorer
func (s *Server) handleRebuildInverted(w http.ResponseWriter, r *http.Request, index string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST", "")
		return
	}
	stats, err := s.engine.RebuildInverted(index)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "index_rebuild_failed", err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"_shards": map[string]interface{}{"total": 1, "successful": 1, "failed": 0},
		"stats":   stats,
		"took":    stats.DurationMs,
	})
}

// handleInvertedInfo GET /<index>/_inverted/info
// 返回索引的倒排信息(doc 数, 字段数, token 数, version, doc-tf 持久化状态)
func (s *Server) handleInvertedInfo(w http.ResponseWriter, r *http.Request, index string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET", "")
		return
	}
	info, err := s.engine.GetInvertedInfo(index)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "index_info_failed", err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// 防止 search 包未使用告警
var _ = search.Query{}
