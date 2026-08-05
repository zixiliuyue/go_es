// 索引模板、ILM、Ingest Pipeline、Reindex、Snapshot、Cluster 路由
package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
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
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"took":      0,
		"timed_out": false,
		"total":     total,
		"created":   created,
		"updated":   0,
		"batches":   1,
		"failures":  []interface{}{},
	})
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
