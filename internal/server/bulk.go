// Bulk 路由: 解析 NDJSON 格式的请求体,逐条写入对应索引
// 响应格式严格对齐 ES,确保 esutil.BulkIndexer 正确解析每条 item
package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/zixiliuyue/go_es/internal/storage"
	"go.uber.org/zap"
)

// handleBulk POST/PUT /_bulk
// 请求体格式: 每行一个 JSON,奇数行为 action(index/create/update/delete),
// 偶数行为 source 行(index/create 才有)
// 响应: { "took": ..., "errors": bool, "items": [...] }
// items 每个元素形如 {"index": {"_index": "...", "_id": "...", "status": 201, "result": "created", "_version": 1}}
func (s *Server) handleBulk(w http.ResponseWriter, r *http.Request) {
	took := time.Now()
	if r.Body == nil {
		writeError(w, http.StatusBadRequest, "parse_exception", "empty body", "")
		return
	}

	// 严格 ES 格式: items 是 []map[string]BulkItemInfo
	// esutil 期望 value 不是指针
	type BulkItemInfo struct {
		Index    string `json:"_index"`
		ID       string `json:"_id"`
		Version  int    `json:"_version,omitempty"`
		Result   string `json:"result,omitempty"`
		Status   int    `json:"status"`
		Error    any    `json:"error,omitempty"`
	}
	type BulkItem struct {
		Index  *BulkItemInfo `json:"index,omitempty"`
		Create *BulkItemInfo `json:"create,omitempty"`
		Delete *BulkItemInfo `json:"delete,omitempty"`
		Update *BulkItemInfo `json:"update,omitempty"`
	}
	resp := struct {
		Took   int        `json:"took"`
		Errors bool       `json:"errors"`
		Items  []BulkItem `json:"items"`
	}{}

	scanner := bufio.NewScanner(r.Body)
	scanner.Buffer(make([]byte, 1024*1024), 100*1024*1024)

	var pendingIndex string
	var pendingAction string
	var pendingID string

	mkInfo := func(idx, id string, status int, result string, version int, errObj any) BulkItemInfo {
		return BulkItemInfo{Index: idx, ID: id, Status: status, Result: result, Version: version, Error: errObj}
	}

	// 先把 body 读出来打印大小
	bodyBytes, _ := io.ReadAll(r.Body)
	s.logger.Info("bulk body", zap.Int("size", len(bodyBytes)), zap.String("first200", string(bodyBytes[:min(200, len(bodyBytes))])))
	scanner = bufio.NewScanner(bytes.NewReader(bodyBytes))
	scanner.Buffer(make([]byte, 1024*1024), 100*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if len(line) < 200 {
			s.logger.Info("bulk line", zap.String("line", string(line)), zap.String("pendingAction", pendingAction), zap.String("pendingIndex", pendingIndex))
		} else {
			s.logger.Info("bulk line (long)", zap.Int("len", len(line)), zap.String("pendingAction", pendingAction), zap.String("pendingIndex", pendingIndex))
		}
		if pendingIndex == "" && pendingAction == "" {
			// 解析 action 行
			var m map[string]map[string]interface{}
			if err := json.Unmarshal(line, &m); err != nil {
				resp.Errors = true
				continue
			}
			for action, body := range m {
				pendingAction = action
				if v, ok := body["_index"].(string); ok {
					pendingIndex = v
				}
				if v, ok := body["_id"].(string); ok {
					pendingID = v
				}
			}
			continue
		}
		// 解析 source 行
		switch pendingAction {
		case "index", "create":
			var doc map[string]interface{}
			if err := json.Unmarshal(line, &doc); err != nil {
				resp.Errors = true
				resp.Items = append(resp.Items, BulkItem{
					Index: &BulkItemInfo{
						Index: pendingIndex, ID: pendingID, Status: 400,
						Error: map[string]interface{}{"type": "parse_exception", "reason": err.Error()},
					},
				})
			} else {
				if pendingID == "" {
					pendingID = generateID()
				}
				if err := s.store.Put(storage.DocKey(pendingIndex, pendingID), doc); err != nil {
					resp.Errors = true
					resp.Items = append(resp.Items, BulkItem{
						Index: &BulkItemInfo{
							Index: pendingIndex, ID: pendingID, Status: 500,
							Error: map[string]interface{}{"type": "internal_error", "reason": err.Error()},
						},
					})
				} else {
					s.engine.IndexDoc(pendingIndex, pendingID, doc)
					resp.Items = append(resp.Items, BulkItem{
						Index: &BulkItemInfo{
							Index: pendingIndex, ID: pendingID, Status: 201, Result: "created", Version: 1,
						},
					})
				}
			}
		case "delete":
			_ = s.store.Delete(storage.DocKey(pendingIndex, pendingID))
			s.engine.DeleteDoc(pendingIndex, pendingID)
			resp.Items = append(resp.Items, BulkItem{
				Delete: &BulkItemInfo{
					Index: pendingIndex, ID: pendingID, Status: 200, Result: "deleted",
				},
			})
		case "update":
			resp.Items = append(resp.Items, BulkItem{
				Update: &BulkItemInfo{
					Index: pendingIndex, ID: pendingID, Status: 200, Result: "noop",
				},
			})
		}
		_ = mkInfo
		pendingIndex = ""
		pendingAction = ""
		pendingID = ""
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	resp.Took = int(time.Since(took).Milliseconds())
	s.logger.Info("bulk handled", zap.Int("items", len(resp.Items)), zap.Bool("errors", resp.Errors))
	writeJSON(w, http.StatusOK, resp)
}
