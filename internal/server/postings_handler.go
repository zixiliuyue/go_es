// postings-snapshot 端点 (#7)
//
//   POST /<index>/_postings/flush     强制 flush postings snapshot (冷启动加速)
//   GET  /<index>/_postings/snapshot  查看 snapshot 诊断信息 (版本/字段数/是否匹配)
package server

import (
	"net/http"
)

// handlePostingsFlush POST /<index>/_postings/flush
// 把 index 的内存倒排 + BM25 统计全量写入 postings-snapshot, 下次冷启动走快路径
func (s *Server) handlePostingsFlush(w http.ResponseWriter, r *http.Request, index string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST", "")
		return
	}
	fields, tookMs, err := s.engine.FlushPostingsSnapshot(index)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "postings_flush_failed", err.Error(), "")
		return
	}
	info, _ := s.engine.GetPostingsSnapshotInfo(index)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"index":          index,
		"fields_flushed": fields,
		"took_ms":        tookMs,
		"snapshot":       info,
	})
}

// handlePostingsSnapshotInfo GET /<index>/_postings/snapshot
// 返回 snapshot 诊断信息: 是否存在 / 版本号 / 是否匹配 / 字段数
func (s *Server) handlePostingsSnapshotInfo(w http.ResponseWriter, r *http.Request, index string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET", "")
		return
	}
	info, err := s.engine.GetPostingsSnapshotInfo(index)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "postings_info_failed", err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, info)
}
