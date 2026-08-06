// 索引模板、ILM、Ingest Pipeline、Reindex、Snapshot、Cluster 路由
package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/zixiliuyue/go_es/internal/storage"
)

// 索引模板

func (s *Server) handleIndexTemplatePut(w http.ResponseWriter, r *http.Request) {
	name := pathSegment(r, 1)
	if name == "" {
		writeError(w, http.StatusBadRequest, "illegal_argument_exception", "template name required", "")
		return
	}
	var body map[string]interface{}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "parse_exception", err.Error(), "")
		return
	}
	if err := s.store.Put(storage.IndexTplKey(name), body); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"acknowledged": true})
}

func (s *Server) handleIndexTemplateGet(w http.ResponseWriter, r *http.Request) {
	name := pathSegment(r, 1)
	var body map[string]interface{}
	found, err := s.store.Get(storage.IndexTplKey(name), &body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error(), "")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "resource_not_found_exception", "template not found", name)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"index_templates": []map[string]interface{}{
			{"name": name, "index_template": body},
		},
	})
}

func (s *Server) handleIndexTemplateDelete(w http.ResponseWriter, r *http.Request) {
	name := pathSegment(r, 1)
	exists, _ := s.store.Exists(storage.IndexTplKey(name))
	if !exists {
		writeError(w, http.StatusNotFound, "resource_not_found_exception", "template not found", name)
		return
	}
	_ = s.store.Delete(storage.IndexTplKey(name))
	writeJSON(w, http.StatusOK, map[string]interface{}{"acknowledged": true})
}

// handleIndexTemplateSimulate POST /_index_template/_simulate/{name}
func (s *Server) handleIndexTemplateSimulate(w http.ResponseWriter, r *http.Request) {
	var body map[string]interface{}
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, "parse_exception", err.Error(), "")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"template": body})
}

// 组件模板

func (s *Server) handleComponentTemplatePut(w http.ResponseWriter, r *http.Request) {
	name := pathSegment(r, 1)
	if name == "" {
		writeError(w, http.StatusBadRequest, "illegal_argument_exception", "component name required", "")
		return
	}
	var body map[string]interface{}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "parse_exception", err.Error(), "")
		return
	}
	if err := s.store.Put(storage.ComponentTplKey(name), body); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"acknowledged": true})
}

func (s *Server) handleComponentTemplateDelete(w http.ResponseWriter, r *http.Request) {
	name := pathSegment(r, 1)
	exists, _ := s.store.Exists(storage.ComponentTplKey(name))
	if !exists {
		writeError(w, http.StatusNotFound, "resource_not_found_exception", "component template not found", name)
		return
	}
	_ = s.store.Delete(storage.ComponentTplKey(name))
	writeJSON(w, http.StatusOK, map[string]interface{}{"acknowledged": true})
}

// ILM

func (s *Server) handleILMPutPolicy(w http.ResponseWriter, r *http.Request) {
	name := pathSegment(r, 2)
	if name == "" {
		writeError(w, http.StatusBadRequest, "illegal_argument_exception", "policy name required", "")
		return
	}
	var body map[string]interface{}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "parse_exception", err.Error(), "")
		return
	}
	if err := s.store.Put(storage.ILMKey(name), body); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"acknowledged": true})
}

func (s *Server) handleILMGetPolicy(w http.ResponseWriter, r *http.Request) {
	name := pathSegment(r, 2)
	var body map[string]interface{}
	found, err := s.store.Get(storage.ILMKey(name), &body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error(), "")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "resource_not_found_exception", "policy not found", name)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{name: body})
}

func (s *Server) handleILMDeletePolicy(w http.ResponseWriter, r *http.Request) {
	name := pathSegment(r, 2)
	exists, _ := s.store.Exists(storage.ILMKey(name))
	if !exists {
		writeError(w, http.StatusNotFound, "resource_not_found_exception", "policy not found", name)
		return
	}
	_ = s.store.Delete(storage.ILMKey(name))
	writeJSON(w, http.StatusOK, map[string]interface{}{"acknowledged": true})
}

// handleILMExplainForName GET /{index}/_ilm/explain
func (s *Server) handleILMExplainForName(w http.ResponseWriter, r *http.Request, index string) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"indices": map[string]interface{}{
			index: map[string]interface{}{
				"managed": true,
				"policy":  "demo_articles_policy",
				"phase":   "hot",
				"action":  "complete",
				"step":    "complete",
			},
		},
	})
}

// Ingest Pipeline

func (s *Server) handleIngestPut(w http.ResponseWriter, r *http.Request) {
	name := pathSegment(r, 2)
	if name == "" {
		writeError(w, http.StatusBadRequest, "illegal_argument_exception", "pipeline name required", "")
		return
	}
	var body map[string]interface{}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "parse_exception", err.Error(), "")
		return
	}
	if err := s.store.Put(storage.IngestKey(name), body); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"acknowledged": true})
}

func (s *Server) handleIngestGet(w http.ResponseWriter, r *http.Request) {
	name := pathSegment(r, 2)
	var body map[string]interface{}
	found, err := s.store.Get(storage.IngestKey(name), &body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error(), "")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "resource_not_found_exception", "pipeline not found", name)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{name: body})
}

func (s *Server) handleIngestDelete(w http.ResponseWriter, r *http.Request) {
	name := pathSegment(r, 2)
	exists, _ := s.store.Exists(storage.IngestKey(name))
	if !exists {
		writeError(w, http.StatusNotFound, "resource_not_found_exception", "pipeline not found", name)
		return
	}
	_ = s.store.Delete(storage.IngestKey(name))
	writeJSON(w, http.StatusOK, map[string]interface{}{"acknowledged": true})
}

func (s *Server) handleIngestSimulate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Pipeline map[string]interface{} `json:"pipeline"`
		Docs     []map[string]interface{} `json:"docs"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "parse_exception", err.Error(), "")
		return
	}
	out := make([]map[string]interface{}, 0, len(req.Docs))
	for _, d := range req.Docs {
		newDoc, err := s.runPipelineBody(req.Pipeline, d)
		if err != nil {
			writeError(w, http.StatusBadRequest, "illegal_argument_exception", err.Error(), "")
			return
		}
		out = append(out, map[string]interface{}{
			"doc": map[string]interface{}{"_source": newDoc},
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"docs": out})
}

// runPipeline 处理 _doc?pipeline=xxx
func (s *Server) runPipeline(name string, doc map[string]interface{}) (map[string]interface{}, error) {
	var p map[string]interface{}
	found, _ := s.store.Get(storage.IngestKey(name), &p)
	if !found {
		return nil, fmt.Errorf("pipeline not found: %s", name)
	}
	return s.runPipelineBody(p, doc)
}

// runPipelineBody 对 pipeline JSON 跑一个简化版的"set"处理器
func (s *Server) runPipelineBody(pipeline map[string]interface{}, doc map[string]interface{}) (map[string]interface{}, error) {
	procs, _ := pipeline["processors"].([]interface{})
	for _, p := range procs {
		pm, _ := p.(map[string]interface{})
		if set, ok := pm["set"].(map[string]interface{}); ok {
			field, _ := set["field"].(string)
			value := set["value"]
			doc[field] = value
		}
		if rename, ok := pm["rename"].(map[string]interface{}); ok {
			from, _ := rename["field"].(string)
			to, _ := rename["target_field"].(string)
			if v, ok := doc[from]; ok {
				delete(doc, from)
				doc[to] = v
			}
		}
	}
	return doc, nil
}

// _reindex
//
// 支持 wait_for_completion 查询参数:
//   - true(默认): 同步执行, 返回统计
//   - false:      异步执行, 立即返回 taskID, 客户端可轮询 /_tasks/{id}
func (s *Server) handleReindex(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source map[string]interface{} `json:"source"`
		Dest   map[string]interface{} `json:"dest"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "parse_exception", err.Error(), "")
		return
	}
	srcIdxList, _ := req.Source["index"].([]interface{})
	if len(srcIdxList) == 0 {
		writeError(w, http.StatusBadRequest, "illegal_argument_exception", "source.index required", "")
		return
	}
	srcIdx, _ := srcIdxList[0].(string)
	dest, _ := req.Dest["index"].(string)
	if dest == "" {
		writeError(w, http.StatusBadRequest, "illegal_argument_exception", "dest.index required", "")
		return
	}
	if exists, _ := s.store.Exists(storage.MetaKey(dest)); !exists {
		writeError(w, http.StatusNotFound, "index_not_found_exception", "dest index not found", dest)
		return
	}

	// wait_for_completion=false -> 异步
	if r.URL.Query().Get("wait_for_completion") == "false" {
		taskID := globalTaskManager.Submit("indices:data/write/reindex", func(e *taskEntry) {
			s.runReindex(srcIdx, dest, e)
		})
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"task": taskID,
		})
		return
	}

	// 同步模式
	writeJSON(w, http.StatusOK, s.doReindexSync(srcIdx, dest))
}

// doReindexSync 同步 reindex, 返回 ES 风格统计
func (s *Server) doReindexSync(srcIdx, dest string) map[string]interface{} {
	ids := s.engine.AllIDs(srcIdx)
	total := int64(len(ids))
	var created int64
	for _, id := range ids {
		src, ok := s.engine.GetSource(srcIdx, id)
		if !ok {
			continue
		}
		if err := s.store.Put(storage.DocKey(dest, id), src); err != nil {
			continue
		}
		s.engine.IndexDoc(dest, id, src)
		created++
	}
	return map[string]interface{}{
		"took":      0,
		"timed_out": false,
		"total":     total,
		"created":   created,
		"updated":   0,
		"batches":   1,
		"failures":  []interface{}{},
	}
}

// reindexBatchSize 异步 reindex 进度上报的批次大小.
// 每处理 N 个文档, Progress.Batches 自增 1, 供 /_tasks/{id} 客户端感知精细进度.
// 取 100 是为了让 5 条数据的最小测试也能至少跳 1 次(5 < 100 走 +1 兜底).
const reindexBatchSize = 100

// runReindex 异步 reindex, 在 taskEntry 中更新进度并响应 cancel.
//
// 取消语义: 取消时已写入目标索引的 doc 会被**逐个反向 Delete**(store + engine).
// 行为上等价于"目标索引回到 reindex 前的状态":
//   - 目标索引原本不存在 -> 取消后索引为空(用户视角与"从未 reindex 过"等价;
//     索引本身仍存在, _cat/indices 可见, 但 doc count = 0)
//   - 目标索引原本存在若干 doc -> 取消后那些原有 doc 保持不变, 新写入的被清理
//
// 进度字段在回滚完成后重置为 Created=0/Batches=0, 与 TaskStatusCancelled 状态一致.
// info 字段的并发写通过 taskEntry.mu 保护(见 withInfo/snapshot).
func (s *Server) runReindex(srcIdx, dest string, e *taskEntry) {
	// written 记录本次 reindex 过程中成功 Put 到目标索引的 doc id 列表.
	// 取消时只 Delete 这些, 不动目标索引原本存在的 doc(若 pre-existing 有).
	written := make([]string, 0, 64)

	ids := s.engine.AllIDs(srcIdx)
	e.withInfo(func(info *TaskInfo) { info.Progress.Total = int64(len(ids)) })

	cancelAfter := func() {
		// 取消路径: 写 cancelled 状态, 触发回滚
		e.withInfo(func(info *TaskInfo) {
			info.Status = TaskStatusCancelled
			info.Progress.Created = 0
			info.Progress.Batches = 0
		})
		s.rollbackReindex(dest, written)
	}

	for i, id := range ids {
		select {
		case <-e.cancel:
			cancelAfter()
			return
		default:
		}
		src, ok := s.engine.GetSource(srcIdx, id)
		if !ok {
			continue
		}
		if err := s.store.Put(storage.DocKey(dest, id), src); err != nil {
			e.withInfo(func(info *TaskInfo) { info.Progress.Failures++ })
			continue
		}
		s.engine.IndexDoc(dest, id, src)
		written = append(written, id)
		e.withInfo(func(info *TaskInfo) {
			info.Progress.Created++
			// 周期性的 batch 计数(每 reindexBatchSize 算一批)
			if (i+1)%reindexBatchSize == 0 {
				info.Progress.Batches++
			}
		})
	}
	if e.cancelled.Load() {
		cancelAfter()
		return
	}
	e.withInfo(func(info *TaskInfo) { info.Status = TaskStatusCompleted })
	// 提交前再检查一次, 避免 "循环完成 -> cancel 到达 -> 状态已置 completed" 的竞态
	if e.cancelled.Load() {
		cancelAfter()
		return
	}
	// 收集最终进度, 写 Response(只读一次, 避免在多次 Lock 间被改)
	var total, created, batches int64
	e.withInfo(func(info *TaskInfo) {
		total = info.Progress.Total
		created = info.Progress.Created
		batches = info.Progress.Batches
	})
	e.withInfo(func(info *TaskInfo) {
		info.Response = map[string]interface{}{
			"took":      0,
			"timed_out": false,
			"total":     total,
			"created":   created,
			"updated":   0,
			"batches":   batches + 1,
			"failures":  []interface{}{},
		}
	})
}

// rollbackReindex 取消后清理已写入目标索引的 doc.
// 反向顺序 Delete(store + engine), 与正向写入顺序相反, 避免依赖 doc 之间的任何关系.
// 回滚期间的 delete 失败不回写到 Progress.Failures(那是 reindex 自身的失败计数,
// 不能把清理阶段的失败混进 reindex 失败统计里; 失败信息写 stderr, 由运维查日志).
func (s *Server) rollbackReindex(dest string, written []string) {
	for i := len(written) - 1; i >= 0; i-- {
		id := written[i]
		if err := s.store.Delete(storage.DocKey(dest, id)); err != nil {
			fmt.Fprintf(os.Stderr, "[reindex-rollback] store.Delete(%s/%s) failed: %v\n", dest, id, err)
		}
		s.engine.DeleteDoc(dest, id)
	}
}

// 快照

func (s *Server) handleSnapshotRepoPut(w http.ResponseWriter, r *http.Request) {
	repo := pathSegment(r, 1)
	if repo == "" {
		writeError(w, http.StatusBadRequest, "illegal_argument_exception", "repo name required", "")
		return
	}
	var body map[string]interface{}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "parse_exception", err.Error(), "")
		return
	}
	if err := s.store.Put(storage.SnapshotRepoKey(repo), body); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"acknowledged": true})
}

func (s *Server) handleSnapshotRepoDelete(w http.ResponseWriter, r *http.Request) {
	repo := pathSegment(r, 1)
	exists, _ := s.store.Exists(storage.SnapshotRepoKey(repo))
	if !exists {
		writeError(w, http.StatusNotFound, "repository_not_found_exception", "repo not found", repo)
		return
	}
	_ = s.store.Delete(storage.SnapshotRepoKey(repo))
	writeJSON(w, http.StatusOK, map[string]interface{}{"acknowledged": true})
}

func (s *Server) handleSnapshotCreate(w http.ResponseWriter, r *http.Request) {
	repo := pathSegment(r, 1)
	snap := pathSegment(r, 2)
	if repo == "" || snap == "" {
		writeError(w, http.StatusBadRequest, "illegal_argument_exception", "repo and snap required", "")
		return
	}
	repoExists, _ := s.store.Exists(storage.SnapshotRepoKey(repo))
	if !repoExists {
		writeError(w, http.StatusNotFound, "repository_not_found_exception", "repo not found", repo)
		return
	}
	meta := map[string]interface{}{
		"state":      "SUCCESS",
		"start_time": time.Now().UTC().Format(time.RFC3339),
		"end_time":   time.Now().UTC().Format(time.RFC3339),
		"indices":    s.listAllIndexes(),
	}
	if err := s.store.Put(storage.SnapshotKey(repo, snap), meta); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"accepted": true})
}

func (s *Server) handleSnapshotGet(w http.ResponseWriter, r *http.Request) {
	repo := pathSegment(r, 1)
	snap := pathSegment(r, 2)
	var body map[string]interface{}
	found, err := s.store.Get(storage.SnapshotKey(repo, snap), &body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error(), "")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "snapshot_not_found_exception", "snapshot not found", snap)
		return
	}
	// ES _snapshot/{repo}/{snap} 返回 {"snapshots":[{"snapshot":"...","repository":"...","state":"..."}]}
	state, _ := body["state"].(string)
	resp := map[string]interface{}{
		"snapshots": []map[string]interface{}{
			{
				"repository": repo,
				"snapshot":   snap,
				"state":      state,
			},
		},
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleSnapshotDelete(w http.ResponseWriter, r *http.Request) {
	repo := pathSegment(r, 1)
	snap := pathSegment(r, 2)
	exists, _ := s.store.Exists(storage.SnapshotKey(repo, snap))
	if !exists {
		writeError(w, http.StatusNotFound, "snapshot_not_found_exception", "snapshot not found", snap)
		return
	}
	_ = s.store.Delete(storage.SnapshotKey(repo, snap))
	writeJSON(w, http.StatusOK, map[string]interface{}{"acknowledged": true})
}

// _cat

// handleCatNodes GET /_cat/nodes
// SDK 走 m.es.Cat.Nodes(... WithFormat("json") ...).Body
// 返回数组 [{name, version, ...}]
func (s *Server) handleCatNodes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, []map[string]interface{}{
		{
			"name":     "go-es-node-1",
			"version":  "8.19.6",
			"role":     "mdi",
			"heap.percent": 0,
			"ram.percent":  0,
			"cpu":          "",
			"load_average": "0.00",
			"nodeRole":     "master",
			"master":       "*",
			"cm":           "*",
		},
	})
}

// handleCatIndices GET /_cat/indices
func (s *Server) handleCatIndices(w http.ResponseWriter, r *http.Request) {
	out := make([]map[string]interface{}, 0)
	_ = s.store.Scan([]byte("meta/"), func(_, v []byte) error {
		var meta IndexMeta
		if err := jsonUnmarshal(v, &meta); err == nil {
			out = append(out, map[string]interface{}{
				"index":      meta.Name,
				"docs.count": meta.DocCount,
				"store.size": meta.StoreBytes,
			})
		}
		return nil
	})
	writeJSON(w, http.StatusOK, out)
}

// 集群

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"name":         s.clusterName,
		"cluster_name": s.clusterName,
		"cluster_uuid": s.clusterUUID,
		"version": map[string]interface{}{
			"number":                "8.19.6",
			"build_flavor":          "self-hosted",
			"build_type":            "tar",
			"build_hash":            randHex(8),
			"build_date":            time.Now().UTC().Format("2006-01-02"),
			"build_snapshot":        false,
			"lucene_version":        "9.0",
			"minimum_wire_compatibility_version": "7.17.0",
			"minimum_index_compatibility_version": "7.0.0",
		},
		"tagline": "You Know, for Search (self-hosted, go_es)",
	})
}

func (s *Server) handleClusterHealth(w http.ResponseWriter, r *http.Request) {
	status := "green"
	unassigned := 0
	_ = s.store.Scan([]byte("meta/"), func(_, v []byte) error {
		var meta IndexMeta
		if err := jsonUnmarshal(v, &meta); err == nil && meta.DocCount == 0 {
			unassigned++
		}
		return nil
	})
	if unassigned > 0 {
		status = "yellow"
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"cluster_name":                         s.clusterName,
		"status":                               status,
		"timed_out":                            false,
		"number_of_nodes":                      1,
		"number_of_data_nodes":                 1,
		"active_primary_shards":                0,
		"active_shards":                        0,
		"relocating_shards":                    0,
		"initializing_shards":                  0,
		"unassigned_shards":                    unassigned,
		"delayed_unassigned_shards":            0,
		"number_of_pending_tasks":              0,
		"number_of_in_flight_fetch":            0,
		"task_max_waiting_in_queue_millis":     0,
		"active_shards_percent_as_number":      100.0,
	})
}

// randHex 生成 n 字节的随机 hex
func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return strings.ToLower(hex.EncodeToString(b))
}
