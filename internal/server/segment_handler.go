// segment 端点
//
//   POST /<index>/_segment/flush  强制 flush 当前 buffer
//   GET  /<index>/_segment/list   列出所有 active segments
//   GET  /<index>/_segment/stats  segment 统计
//   POST /<index>/_segment/merge  合并小 segment
package server

import (
	"net/http"
	"strconv"
)

// handleSegmentFlush POST /<index>/_segment/flush
// 强制把当前 buffer 写入不可变 segment
func (s *Server) handleSegmentFlush(w http.ResponseWriter, r *http.Request, index string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST", "")
		return
	}
	if s.seg == nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable", "segment manager not initialized", "")
		return
	}
	created, err := s.seg.FlushNow(index)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "flush_failed", err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"index":            index,
		"segments_created": created,
		"stats":            s.seg.Stats(),
	})
}

// handleSegmentList GET /<index>/_segment/list
// 列出该 index 下的所有 active segments
func (s *Server) handleSegmentList(w http.ResponseWriter, r *http.Request, index string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET", "")
		return
	}
	if s.seg == nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable", "segment manager not initialized", "")
		return
	}
	segs := s.seg.ListSegments(index)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"index":    index,
		"segments": segs,
		"count":    len(segs),
	})
}

// handleSegmentStats GET /<index>/_segment/stats
// 返回 segment 统计
func (s *Server) handleSegmentStats(w http.ResponseWriter, r *http.Request, index string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET", "")
		return
	}
	if s.seg == nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable", "segment manager not initialized", "")
		return
	}
	segs := s.seg.ListSegments(index)
	var totalBytes int64
	for _, seg := range segs {
		totalBytes += seg.SizeBytes
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"index":          index,
		"segment_count":  len(segs),
		"total_bytes":    totalBytes,
		"global_stats":   s.seg.Stats(),
	})
}

// handleSegmentMerge POST /<index>/_segment/merge?max_segments=N
// 合并小 segment 为更大的 segment
func (s *Server) handleSegmentMerge(w http.ResponseWriter, r *http.Request, index string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST", "")
		return
	}
	if s.seg == nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable", "segment manager not initialized", "")
		return
	}
	maxSegs := 2 // 默认合并到 2 个 segment
	if qs := r.URL.Query().Get("max_segments"); qs != "" {
		if v, err := strconv.Atoi(qs); err == nil && v > 0 {
			maxSegs = v
		}
	}
	merged, created, err := s.seg.MergeSegments(index, maxSegs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "merge_failed", err.Error(), "")
		return
	}
	segs := s.seg.ListSegments(index)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"index":           index,
		"segments_merged": merged,
		"segments_created": created,
		"remaining_count": len(segs),
		"segments":        segs,
		"stats":           s.seg.Stats(),
	})
}
