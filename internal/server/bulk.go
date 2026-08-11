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

	"go.uber.org/zap"
)

// handleBulk POST/PUT /_bulk
// 请求体格式: 每行一个 JSON,奇数行为 action(index/create/update/delete),
// 偶数行为 source 行(index/create 才有)
// 响应: { "took": ..., "errors": bool, "items": [...] }
// items 每个元素形如 {"index": {"_index": "...", "_id": "...", "status": 201, "result": "created", "_version": 1}}
//
// 实现:
//   - 解析 body 收集所有 WriteOp
//   - 调用 WriteCoordinator.SubmitBulk 一次事务合并
//   - 单 op 错误(409/parse)只影响该 op, 事务继续
func (s *Server) handleBulk(w http.ResponseWriter, r *http.Request) {
	took := time.Now()
	if r.Body == nil {
		writeError(w, http.StatusBadRequest, "parse_exception", "empty body", "")
		return
	}

	// 严格 ES 格式: items 是 []map[string]BulkItemInfo
	// esutil 期望 value 不是指针
	type BulkItemInfo struct {
		Index   string `json:"_index"`
		ID      string `json:"_id"`
		Version int    `json:"_version,omitempty"`
		Result  string `json:"result,omitempty"`
		Status  int    `json:"status"`
		Error   any    `json:"error,omitempty"`
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

	bodyBytes, _ := io.ReadAll(r.Body)
	s.logger.Info("bulk body", zap.Int("size", len(bodyBytes)),
		zap.String("first200", string(bodyBytes[:min(200, len(bodyBytes))])))

	scanner := bufio.NewScanner(bytes.NewReader(bodyBytes))
	scanner.Buffer(make([]byte, 1024*1024), 100*1024*1024)

	// 收集: parsed ops + 同步记录待插入的 BulkItem
	type pending struct {
		action, index, id string
		doc               map[string]interface{}
	}
	pendings := make([]pending, 0)
	var errItems []BulkItem // parse 错误的直接 append 到 errItems

	var p pending
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		// 第一行 (action)
		if p.action == "" {
			var m map[string]map[string]interface{}
			if err := json.Unmarshal(line, &m); err != nil {
				resp.Errors = true
				errItems = append(errItems, BulkItem{Index: &BulkItemInfo{
					Index: "", ID: "", Status: 400,
					Error: map[string]interface{}{"type": "parse_exception", "reason": err.Error()},
				}})
				continue
			}
			for action, body := range m {
				p.action = action
				if v, ok := body["_index"].(string); ok {
					p.index = v
				}
				if v, ok := body["_id"].(string); ok {
					p.id = v
				}
			}
			continue
		}
		// 第二行 (source / data)
		switch p.action {
		case "index", "create":
			var doc map[string]interface{}
			if err := json.Unmarshal(line, &doc); err != nil {
				resp.Errors = true
				errItems = append(errItems, BulkItem{Index: &BulkItemInfo{
					Index: p.index, ID: p.id, Status: 400,
					Error: map[string]interface{}{"type": "parse_exception", "reason": err.Error()},
				}})
				p = pending{}
				continue
			}
			if p.id == "" {
				p.id = generateID()
			}
			pendings = append(pendings, p)
			// 但要保留 doc 等待批量提交
			pendings[len(pendings)-1].doc = doc
		case "delete":
			// delete 立即收集, 不需要 source
			pendings = append(pendings, p)
		case "update":
			// 简化: update 不展开, 走 noop
			errItems = append(errItems, BulkItem{Update: &BulkItemInfo{
				Index: p.index, ID: p.id, Status: 200, Result: "noop",
			}})
		}
		p = pending{}
	}
	// 处理末尾的 delete (无 source 行, 上面未 append)
	if p.action == "delete" {
		pendings = append(pendings, p)
	}

	// 把 pendings 转为 WriteOp
	ops := make([]WriteOp, 0, len(pendings))
	for _, p := range pendings {
		kind := p.action
		// ES 语义: create -> 必须新建; 我们的 op_type=create
		op := WriteOp{Index: p.index, ID: p.id, Kind: kind, Doc: p.doc}
		// 简化: 走版本自增
		current, exists := s.readDocMeta(p.index, p.id)
		var currentPtr *DocMeta
		if exists {
			currentPtr = &current
		}
		meta, _ := NextMeta(currentPtr, exists, 0, 0, 0, "")
		op.VersionMeta = &meta
		ops = append(ops, op)
	}

	// 一次事务合并提交
	results := s.wc.SubmitBulk(s.store, s.engine, ops)

	// 解析结果回写
	for i, p := range pendings {
		r := results[i]
		var info BulkItemInfo
		info.Index = p.index
		info.ID = p.id
		if r.Meta != nil {
			info.Version = int(r.Meta.Version)
		}
		if r.Status == 200 || r.Status == 201 {
			info.Status = r.Status
			if p.action == "delete" {
				info.Result = "deleted"
			} else if r.Status == 201 {
				info.Result = "created"
			} else {
				info.Result = "updated"
			}
		} else if r.Status == 409 {
			resp.Errors = true
			info.Status = 409
			info.Error = r.ErrBody
		} else {
			resp.Errors = true
			info.Status = r.Status
			if r.Error != nil {
				info.Error = map[string]interface{}{"type": "internal_error", "reason": r.Error.Error()}
			}
		}
		switch p.action {
		case "index":
			resp.Items = append(resp.Items, BulkItem{Index: &info})
		case "create":
			resp.Items = append(resp.Items, BulkItem{Create: &info})
		case "delete":
			resp.Items = append(resp.Items, BulkItem{Delete: &info})
		case "update":
			resp.Items = append(resp.Items, BulkItem{Update: &info})
		}
	}
	// 加上 parse 错误的
	resp.Items = append(errItems, resp.Items...)

	// 失效缓存 (#11): 按涉及的索引逐个失效
	seen := make(map[string]struct{})
	for _, p := range pendings {
		if p.index != "" {
			seen[p.index] = struct{}{}
		}
	}
	for idx := range seen {
		s.invalidateCacheForIndex(idx)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	resp.Took = int(time.Since(took).Milliseconds())
	s.logger.Info("bulk handled", zap.Int("items", len(resp.Items)), zap.Bool("errors", resp.Errors))
	writeJSON(w, http.StatusOK, resp)
}
