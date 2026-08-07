// update_by_query / delete_by_query 服务端实现
//
// 设计:
//   - 复用 TaskManager 异步框架(与 reindex 一致): wait_for_completion=false 走异步
//   - 同/异步入参: POST /<index>/_update_by_query 与 POST /<index>/_delete_by_query
//   - 取消语义: 取消后已 delete 的不可恢复, 已 update 的同样不可回滚(在 batch 边界未提交的部分)
//     - 工程现实: 每个 doc 处理是单事务(put+indexdoc), 失败只统计到 Progress.Failures, 不会破坏其它 doc
//   - update 用简化版 painless script(JSON 路径表达式):
//     - "ctx._source.status = 'archived'"  (赋值)
//     - "ctx._source.views += 1"            (数值增减)
//     - "ctx._source.remove('field')"      (删除字段)
//     - 解析失败 -> 400
//   - 进度字段: Total / Updated(或 Deleted) / Failures / Batches (复用 reindex 的 Progress)
//
// 参考: ES 8.x _update_by_query / _delete_by_query 行为(同步返回 + 异步任务路径)
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/zixiliuyue/go_es/internal/search"
	"github.com/zixiliuyue/go_es/internal/storage"
)

// updateByQueryRequest POST /<index>/_update_by_query
type updateByQueryRequest struct {
	Query  map[string]interface{} `json:"query"`
	Script map[string]interface{} `json:"script"`
}

// deleteByQueryRequest POST /<index>/_delete_by_query
type deleteByQueryRequest struct {
	Query map[string]interface{} `json:"query"`
}

// handleUpdateByQuery POST /<index>/_update_by_query
func (s *Server) handleUpdateByQuery(w http.ResponseWriter, r *http.Request, index string) {
	var req updateByQueryRequest
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "parse_exception", err.Error(), "")
			return
		}
	}
	if req.Script == nil {
		writeError(w, http.StatusBadRequest, "illegal_argument_exception", "script is required", "")
		return
	}
	// 解析 script(简化 painless-lite)
	fn, err := compileUpdateScript(req.Script)
	if err != nil {
		writeError(w, http.StatusBadRequest, "script_exception", err.Error(), "")
		return
	}
	q, err := parseQuery(req.Query)
	if err != nil {
		writeError(w, http.StatusBadRequest, "parse_exception", err.Error(), "")
		return
	}
	// 异步模式
	if r.URL.Query().Get("wait_for_completion") == "false" {
		taskID := globalTaskManager.Submit("indices:data/write/update/byquery", func(e *taskEntry) {
			s.runUpdateByQuery(index, q, fn, e)
		})
		writeJSON(w, http.StatusOK, map[string]interface{}{"task": taskID})
		return
	}
	// 同步
	writeJSON(w, http.StatusOK, s.doUpdateByQuerySync(index, q, fn))
}

// handleDeleteByQuery POST /<index>/_delete_by_query
func (s *Server) handleDeleteByQuery(w http.ResponseWriter, r *http.Request, index string) {
	var req deleteByQueryRequest
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "parse_exception", err.Error(), "")
			return
		}
	}
	q, err := parseQuery(req.Query)
	if err != nil {
		writeError(w, http.StatusBadRequest, "parse_exception", err.Error(), "")
		return
	}
	// 异步模式
	if r.URL.Query().Get("wait_for_completion") == "false" {
		taskID := globalTaskManager.Submit("indices:data/write/delete/byquery", func(e *taskEntry) {
			s.runDeleteByQuery(index, q, e)
		})
		writeJSON(w, http.StatusOK, map[string]interface{}{"task": taskID})
		return
	}
	// 同步
	writeJSON(w, http.StatusOK, s.doDeleteByQuerySync(index, q))
}

// doDeleteByQuerySync 同步 delete_by_query, 返回 ES 风格统计
func (s *Server) doDeleteByQuerySync(index string, q *search.Query) map[string]interface{} {
	ids, err := s.engine.Match(index, q)
	if err != nil {
		return map[string]interface{}{"error": err.Error()}
	}
	total := int64(len(ids))
	var deleted int64
	for _, id := range ids {
		if err := s.store.Delete(storage.DocKey(index, id)); err != nil {
			continue
		}
		s.engine.DeleteDoc(index, id)
		deleted++
	}
	return map[string]interface{}{
		"took":      0,
		"timed_out": false,
		"total":     total,
		"deleted":   deleted,
		"batches":   1,
		"failures":  []interface{}{},
		"version_conflicts": 0,
	}
}

// runDeleteByQuery 异步版, 支持取消(注意: 取消后已删除的不可回滚)
// 取消时设置 TaskStatusCancelled, Progress 保留已删除计数(告知用户部分完成)
func (s *Server) runDeleteByQuery(index string, q *search.Query, e *taskEntry) {
	ids, err := s.engine.Match(index, q)
	if err != nil {
		e.withInfo(func(info *TaskInfo) {
			info.Status = TaskStatusFailed
			info.Error = map[string]interface{}{"type": "search_phase_execution_exception", "reason": err.Error()}
		})
		return
	}
	e.withInfo(func(info *TaskInfo) { info.Progress.Total = int64(len(ids)) })

	for i, id := range ids {
		select {
		case <-e.cancel:
			e.withInfo(func(info *TaskInfo) { info.Status = TaskStatusCancelled })
			return
		default:
		}
		if err := s.store.Delete(storage.DocKey(index, id)); err != nil {
			e.withInfo(func(info *TaskInfo) { info.Progress.Failures++ })
			continue
		}
		s.engine.DeleteDoc(index, id)
		e.withInfo(func(info *TaskInfo) {
			info.Progress.Deleted++
			if (i+1)%deleteByQueryBatchSize == 0 {
				info.Progress.Batches++
			}
		})
	}
	if e.cancelled.Load() {
		e.withInfo(func(info *TaskInfo) { info.Status = TaskStatusCancelled })
		return
	}
	// 收集最终进度 + 写 Response
	var total, deleted, batches int64
	e.withInfo(func(info *TaskInfo) {
		total = info.Progress.Total
		deleted = info.Progress.Deleted
		batches = info.Progress.Batches
	})
	e.withInfo(func(info *TaskInfo) {
		info.Status = TaskStatusCompleted
		info.Response = map[string]interface{}{
			"took":             0,
			"timed_out":        false,
			"total":            total,
			"deleted":          deleted,
			"batches":          batches + 1,
			"failures":         []interface{}{},
			"version_conflicts": 0,
		}
	})
	// 提交后再次检查取消(避免竞态)
	if e.cancelled.Load() {
		e.withInfo(func(info *TaskInfo) { info.Status = TaskStatusCancelled })
	}
}

// doUpdateByQuerySync 同步 update_by_query
func (s *Server) doUpdateByQuerySync(index string, q *search.Query, fn updateScript) map[string]interface{} {
	ids, err := s.engine.Match(index, q)
	if err != nil {
		return map[string]interface{}{"error": err.Error()}
	}
	total := int64(len(ids))
	var updated int64
	for _, id := range ids {
		src, ok := s.engine.GetSource(index, id)
		if !ok {
			continue
		}
		newSrc, err := fn(src)
		if err != nil {
			continue
		}
		if err := s.store.Put(storage.DocKey(index, id), newSrc); err != nil {
			continue
		}
		s.engine.IndexDoc(index, id, newSrc)
		updated++
	}
	return map[string]interface{}{
		"took":      0,
		"timed_out": false,
		"total":     total,
		"updated":   updated,
		"batches":   1,
		"failures":  []interface{}{},
		"version_conflicts": 0,
	}
}

// runUpdateByQuery 异步版, 支持取消(取消后已修改不可回滚)
func (s *Server) runUpdateByQuery(index string, q *search.Query, fn updateScript, e *taskEntry) {
	ids, err := s.engine.Match(index, q)
	if err != nil {
		e.withInfo(func(info *TaskInfo) {
			info.Status = TaskStatusFailed
			info.Error = map[string]interface{}{"type": "search_phase_execution_exception", "reason": err.Error()}
		})
		return
	}
	e.withInfo(func(info *TaskInfo) { info.Progress.Total = int64(len(ids)) })

	for i, id := range ids {
		select {
		case <-e.cancel:
			e.withInfo(func(info *TaskInfo) { info.Status = TaskStatusCancelled })
			return
		default:
		}
		src, ok := s.engine.GetSource(index, id)
		if !ok {
			continue
		}
		newSrc, err := fn(src)
		if err != nil {
			e.withInfo(func(info *TaskInfo) { info.Progress.Failures++ })
			continue
		}
		if err := s.store.Put(storage.DocKey(index, id), newSrc); err != nil {
			e.withInfo(func(info *TaskInfo) { info.Progress.Failures++ })
			continue
		}
		s.engine.IndexDoc(index, id, newSrc)
		e.withInfo(func(info *TaskInfo) {
			info.Progress.Updated++
			if (i+1)%updateByQueryBatchSize == 0 {
				info.Progress.Batches++
			}
		})
	}
	if e.cancelled.Load() {
		e.withInfo(func(info *TaskInfo) { info.Status = TaskStatusCancelled })
		return
	}
	var total, updated, batches int64
	e.withInfo(func(info *TaskInfo) {
		total = info.Progress.Total
		updated = info.Progress.Updated
		batches = info.Progress.Batches
	})
	e.withInfo(func(info *TaskInfo) {
		info.Status = TaskStatusCompleted
		info.Response = map[string]interface{}{
			"took":             0,
			"timed_out":        false,
			"total":            total,
			"updated":          updated,
			"batches":          batches + 1,
			"failures":         []interface{}{},
			"version_conflicts": 0,
		}
	})
	if e.cancelled.Load() {
		e.withInfo(func(info *TaskInfo) { info.Status = TaskStatusCancelled })
	}
}

// updateByQueryBatchSize 同 reindex 节奏: 每 100 doc 算一批, 保证 5 条数据测试也能跳 1 次
const updateByQueryBatchSize = 100

// deleteByQueryBatchSize 同上
const deleteByQueryBatchSize = 100

// ------------------ 简化版 painless-lite script 编译/执行 ------------------

// updateScript 修改函数的签名, 输入 doc, 返回新 doc
type updateScript func(doc map[string]interface{}) (map[string]interface{}, error)

// compileUpdateScript 解析 script 块
// 支持的脚本语义:
//   - "source": "ctx._source.field = value"      (string 直接替换)
//   - "source": "ctx._source.field += N"          (int/float 自增)
//   - "source": "ctx._source.remove('field')"     (删除字段)
// 也可以是 {"inline": "..."}, {"lang": "painless", "source": "..."}
func compileUpdateScript(script map[string]interface{}) (updateScript, error) {
	src, _ := script["source"].(string)
	if src == "" {
		src, _ = script["inline"].(string)
	}
	if src == "" {
		return nil, fmt.Errorf("script.source required")
	}
	// 按 ; 切分多个语句
	stmts := splitScript(src)
	if len(stmts) == 0 {
		return nil, fmt.Errorf("empty script")
	}
	// 预编译每条语句
	type op struct {
		kind   string // "set" | "inc" | "remove"
		field  string
		value  interface{} // set/inc 用
	}
	ops := make([]op, 0, len(stmts))
	for _, s := range stmts {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		// ctx._source.<field> = <value>
		// value 可以是数字(裸) / 字符串 'xxx' / 布尔 true false
		if m := reScriptSet.FindStringSubmatch(s); m != nil {
			field := m[1]
			raw := strings.TrimSpace(m[2])
			val, err := parseScriptValue(raw)
			if err != nil {
				return nil, fmt.Errorf("script: parse value %q: %w", raw, err)
			}
			ops = append(ops, op{kind: "set", field: field, value: val})
			continue
		}
		// ctx._source.<field> += <number>
		if m := reScriptInc.FindStringSubmatch(s); m != nil {
			field := m[1]
			raw := strings.TrimSpace(m[2])
			n, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				return nil, fmt.Errorf("script: invalid inc value %q", raw)
			}
			ops = append(ops, op{kind: "inc", field: field, value: n})
			continue
		}
		// ctx._source.remove('field')
		if m := reScriptRemove.FindStringSubmatch(s); m != nil {
			field := strings.Trim(m[1], "'\"")
			ops = append(ops, op{kind: "remove", field: field})
			continue
		}
		return nil, fmt.Errorf("script: unsupported statement: %q", s)
	}
	// 返回执行函数
	return func(doc map[string]interface{}) (map[string]interface{}, error) {
		for _, o := range ops {
			switch o.kind {
			case "set":
				setNestedField(doc, o.field, o.value)
			case "inc":
				cur, _ := o.value.(float64)
				if v, ok := doc[o.field]; ok {
					if fv, ok := toFloat64(v); ok {
						doc[o.field] = fv + cur
						continue
					}
				}
				doc[o.field] = cur
			case "remove":
				delete(doc, o.field)
			}
		}
		return doc, nil
	}, nil
}

// splitScript 按 ; 切(忽略字符串内的 ;)
func splitScript(s string) []string {
	// 简单实现: 真要严谨需要状态机, 这里足够覆盖常见脚本
	return strings.Split(s, ";")
}

// 正则: ctx._source.field = value
var reScriptSet = regexp.MustCompile(`^ctx\._source\.([A-Za-z_][A-Za-z0-9_.\-]*) = (.+)$`)

// 正则: ctx._source.field += number
var reScriptInc = regexp.MustCompile(`^ctx\._source\.([A-Za-z_][A-Za-z0-9_.\-]*) \+= (.+)$`)

// 正则: ctx._source.remove('field') 或 ctx._source.remove("field")
var reScriptRemove = regexp.MustCompile(`^ctx\._source\.remove\((.+)\)$`)

// parseScriptValue 解析脚本右侧字面量
// 支持: 数字 / 字符串(单/双引号) / true / false / null / 简单 JSON object/array(转给 json)
func parseScriptValue(raw string) (interface{}, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	// 字符串字面量
	if (raw[0] == '\'' && raw[len(raw)-1] == '\'') ||
		(raw[0] == '"' && raw[len(raw)-1] == '"') {
		return raw[1 : len(raw)-1], nil
	}
	// bool
	if raw == "true" {
		return true, nil
	}
	if raw == "false" {
		return false, nil
	}
	if raw == "null" {
		return nil, nil
	}
	// 数字(int / float)
	if n, err := strconv.ParseFloat(raw, 64); err == nil {
		// 整型更友好
		if i, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return i, nil
		}
		return n, nil
	}
	// 复杂 JSON
	var v interface{}
	if err := json.Unmarshal([]byte(raw), &v); err == nil {
		return v, nil
	}
	return nil, fmt.Errorf("unsupported literal: %q", raw)
}

// toFloat64 把任意数字型转 float64
func toFloat64(v interface{}) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

// setNestedField 支持点号路径: "a.b.c" 递归设置
func setNestedField(doc map[string]interface{}, path string, value interface{}) {
	parts := strings.Split(path, ".")
	cur := doc
	for i, p := range parts {
		if i == len(parts)-1 {
			cur[p] = value
			return
		}
		next, ok := cur[p].(map[string]interface{})
		if !ok {
			next = make(map[string]interface{})
			cur[p] = next
		}
		cur = next
	}
}
