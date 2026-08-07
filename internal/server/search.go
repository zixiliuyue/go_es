// 搜索路由: 解析 ES 请求体 -> 调用 internal/search 引擎 -> 拼装 ES 响应
package server

import (
	"fmt"
	"net/http"
	stdSort "sort"
	"strings"
	"time"

	"github.com/zixiliuyue/go_es/internal/search"
)

// handleSearchAll POST /_search
func (s *Server) handleSearchAll(w http.ResponseWriter, r *http.Request) {
	s.doSearch(w, r, nil)
}

// handleSearchForName POST /{index}/_search
// 兼容保留: index 段视为精确名
func (s *Server) handleSearchForName(w http.ResponseWriter, r *http.Request, index string) {
	s.doSearch(w, r, []string{index})
}

// handleSearchForNamePattern POST /{index}/_search
// index 段支持通配: *, prefix*, idx1,idx2, -foo 等
func (s *Server) handleSearchForNamePattern(w http.ResponseWriter, r *http.Request, pattern string) {
	indices := s.getIndicesByPattern(pattern)
	if len(indices) == 0 {
		var req searchRequest
		if r.ContentLength > 0 {
			_ = decodeJSON(r, &req)
		}
		writeJSON(w, http.StatusOK, buildEmptyResponse(req))
		return
	}
	s.doSearch(w, r, indices)
}

// doSearch 实际搜索逻辑
func (s *Server) doSearch(w http.ResponseWriter, r *http.Request, indices []string) {
	var req searchRequest
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "parse_exception", err.Error(), "")
			return
		}
	}
	if indices == nil {
		indices = s.listAllIndexes()
	}
	if len(indices) == 0 {
		writeJSON(w, http.StatusOK, buildEmptyResponse(req))
		return
	}

	q, err := parseQuery(req.Query)
	if err != nil {
		writeError(w, http.StatusBadRequest, "parsing_exception", err.Error(), "")
		return
	}

	allHits := make([]hit, 0)
	took := time.Now()
	for _, idx := range indices {
		ids, err := s.engine.Match(idx, q)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "search_phase_execution_exception", err.Error(), "")
			return
		}
		from := req.From
		size := req.Size
		if size == 0 {
			size = 10
		}
		ids = s.applySort(idx, ids, req.Sort)
		if from > len(ids) {
			continue
		}
		end := from + size
		if end > len(ids) {
			end = len(ids)
		}
		page := ids[from:end]
		for _, id := range page {
			if src, ok := s.engine.GetSource(idx, id); ok {
				allHits = append(allHits, hit{
					Index:  idx,
					ID:     id,
					Source: src,
				})
			}
		}
	}
	stdSort.Slice(allHits, func(i, j int) bool {
		if allHits[i].Index == allHits[j].Index {
			return allHits[i].ID < allHits[j].ID
		}
		return allHits[i].Index < allHits[j].Index
	})

	total := len(allHits)
	resp := map[string]interface{}{
		"took": time.Since(took).Milliseconds(),
		"hits": map[string]interface{}{
			"total": map[string]interface{}{
				"value":    total,
				"relation": "eq",
			},
			"hits": allHits,
		},
	}
	// 聚合: 在已命中的 hits 上求值(每个 hit 携 (index, docID) 信息)
	if len(req.Aggregations) > 0 {
		indexedHits := make([]search.IndexedHit, 0, total)
		for _, h := range allHits {
			indexedHits = append(indexedHits, search.IndexedHit{Index: h.Index, DocID: h.ID})
		}
		aggReqs, err := parseAggregationRequests(req.Aggregations)
		if err != nil {
			writeError(w, http.StatusBadRequest, "parsing_exception", err.Error(), "")
			return
		}
		aggRes, err := search.EvalAggregations(indexedHits, aggReqs)
		if err != nil {
			writeError(w, http.StatusBadRequest, "aggregation_execution_exception", err.Error(), "")
			return
		}
		if len(aggRes) > 0 {
			resp["aggregations"] = map[string]interface{}(aggRes)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// searchRequest 顶层搜索请求结构
type searchRequest struct {
	From  int                      `json:"from,omitempty"`
	Size  int                      `json:"size,omitempty"`
	Sort  []map[string]string      `json:"sort,omitempty"`
	Query map[string]interface{}   `json:"query,omitempty"`
	// 新增: 聚合定义, 形如 {"name1": {"terms": {...}}, "name2": {"avg": {...}}}
	// 与 ES 兼容: 顶层 key 是聚合名, value 是 {"<agg_type>": <def>}
	Aggregations map[string]interface{} `json:"aggs,omitempty"`
}

// hit 单条命中
type hit struct {
	Index  string                 `json:"_index"`
	ID     string                 `json:"_id"`
	Score  float64                `json:"_score,omitempty"`
	Source map[string]interface{} `json:"_source,omitempty"`
}

// parseQuery 把 search.Request.Query 转为 *search.Query
func parseQuery(raw map[string]interface{}) (*search.Query, error) {
	if raw == nil {
		return &search.Query{}, nil
	}
	out := &search.Query{}
	if v, ok := raw["match"].(map[string]interface{}); ok {
		out.Match = v
	}
	if v, ok := raw["term"].(map[string]interface{}); ok {
		out.Term = v
	}
	if v, ok := raw["terms"].(map[string]interface{}); ok {
		out.Terms = v
	}
	if v, ok := raw["range"].(map[string]interface{}); ok {
		out.Range = v
	}
	if v, ok := raw["match_all"].(map[string]interface{}); ok {
		out.MatchAll = v
	}
	if v, ok := raw["bool"].(map[string]interface{}); ok {
		bq := &search.BoolQuery{}
		if x, ok := v["must"].([]interface{}); ok {
			for _, c := range x {
				if m, ok := c.(map[string]interface{}); ok {
					bq.Must = append(bq.Must, m)
				}
			}
		}
		if x, ok := v["filter"].([]interface{}); ok {
			for _, c := range x {
				if m, ok := c.(map[string]interface{}); ok {
					bq.Filter = append(bq.Filter, m)
				}
			}
		}
		if x, ok := v["should"].([]interface{}); ok {
			for _, c := range x {
				if m, ok := c.(map[string]interface{}); ok {
					bq.Should = append(bq.Should, m)
				}
			}
		}
		if x, ok := v["must_not"].([]interface{}); ok {
			for _, c := range x {
				if m, ok := c.(map[string]interface{}); ok {
					bq.MustNot = append(bq.MustNot, m)
				}
			}
		}
		out.Bool = bq
	}
	return out, nil
}

// applySort 对 IDs 按 sort 字段做简单排序
func (s *Server) applySort(index string, ids []string, sortClauses []map[string]string) []string {
	if len(sortClauses) == 0 {
		return ids
	}
	for _, m := range sortClauses {
		for field, dir := range m {
			stdSort.Slice(ids, func(i, j int) bool {
				si, oki := s.engine.GetSource(index, ids[i])
				sj, okj := s.engine.GetSource(index, ids[j])
				if !oki || !okj {
					return false
				}
				vi, oki := si[field]
				vj, okj := sj[field]
				if !oki || !okj {
					return false
				}
				cmp := compareValues(vi, vj)
				if dir == "desc" {
					return cmp > 0
				}
				return cmp < 0
			})
		}
	}
	return ids
}

// compareValues 通用大小比较
func compareValues(a, b interface{}) int {
	switch x := a.(type) {
	case float64:
		if y, ok := b.(float64); ok {
			switch {
			case x < y:
				return -1
			case x > y:
				return 1
			default:
				return 0
			}
		}
	case string:
		if y, ok := b.(string); ok {
			return strings.Compare(x, y)
		}
	}
	return 0
}

// parseAggregationRequests 把 search 请求体里的 aggs map 转为 search.AggregationRequest 列表.
// 输入形如: {"g1": {"terms": {...}}, "g2": {"avg": {...}}}
// 输出: [{Key: "g1", Spec: {"terms": {...}}}, ...]
func parseAggregationRequests(raw map[string]interface{}) ([]search.AggregationRequest, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]search.AggregationRequest, 0, len(raw))
	for name, def := range raw {
		spec, ok := def.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("aggregation %q must be an object", name)
		}
		out = append(out, search.AggregationRequest{Key: name, Spec: spec})
	}
	return out, nil
}

// listAllIndexes 通过扫描 meta/ 前缀得到所有索引
func (s *Server) listAllIndexes() []string {
	out := make([]string, 0)
	_ = s.store.Scan([]byte("meta/"), func(_, v []byte) error {
		var meta IndexMeta
		if err := jsonUnmarshal(v, &meta); err == nil {
			out = append(out, meta.Name)
		}
		return nil
	})
	return out
}

// buildEmptyResponse 当没有任何索引时的返回结构
func buildEmptyResponse(req searchRequest) map[string]interface{} {
	resp := map[string]interface{}{
		"took": 0,
		"hits": map[string]interface{}{
			"total": map[string]interface{}{"value": 0, "relation": "eq"},
			"hits":  []hit{},
		},
	}
	// 聚合: 即使无命中, 也返回空聚合结果(与 ES 行为一致)
	if len(req.Aggregations) > 0 {
		empty := make(map[string]interface{}, len(req.Aggregations))
		for name, def := range req.Aggregations {
			if spec, ok := def.(map[string]interface{}); ok {
				empty[name] = emptyAggResult(spec)
			}
		}
		resp["aggregations"] = empty
	}
	return resp
}

// emptyAggResult 给定聚合定义, 返回无命中时的合理空结构
func emptyAggResult(spec map[string]interface{}) map[string]interface{} {
	if len(spec) != 1 {
		return map[string]interface{}{}
	}
	for typ := range spec {
		switch typ {
		case "terms", "histogram", "date_histogram", "range":
			return map[string]interface{}{"buckets": []interface{}{}}
		case "value_count", "avg", "sum", "min", "max", "cardinality":
			return map[string]interface{}{"value": nil}
		case "stats":
			return map[string]interface{}{"count": 0, "min": nil, "max": nil, "avg": nil, "sum": 0.0}
		}
	}
	return map[string]interface{}{}
}
