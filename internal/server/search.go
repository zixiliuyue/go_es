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
	// totalHits 是分页前所有匹配的 doc 计数(跨索引), 用于 track_total_hits
	totalHitsAll := 0
	took := time.Now()
	// 判定是否需要 BM25 打分
	scored := isTextQuery(q)
	// highlight 提取 query tokens
	highlightTokens := extractQueryTokensFromQuery(viewForHighlight(q))
	// source 过滤是否启用(false=不显 _source)
	sourceEnabled := req.SourceFilter == nil || req.SourceFilter != false
	for _, idx := range indices {
		ids, err := s.engine.Match(idx, q)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "search_phase_execution_exception", err.Error(), "")
			return
		}
		// 累计分页前的总命中数
		totalHitsAll += len(ids)
		from := req.From
		size := req.Size
		if size == 0 {
			size = 10
		}
		// 如果有 BM25 打分且没显式 sort, 先按 score 降序
		if scored && len(req.Sort) == 0 {
			ids = sortByBM25Score(s.engine, idx, q, ids)
		} else {
			ids = s.applySort(idx, ids, req.Sort)
		}
		if from > len(ids) {
			continue
		}
		end := from + size
		if end > len(ids) {
			end = len(ids)
		}
		page := ids[from:end]
		for _, id := range page {
			src, _ := s.engine.GetSource(idx, id)
			h := hit{
				Index:  idx,
				ID:     id,
				Source: src,
			}
			// 计算 score: 文本查询走 BM25, 其它置 1.0
			if scored {
				h.Score = computeHitScore(s.engine, idx, q, id)
			} else {
				h.Score = 1.0
			}
			// _source 过滤
			if sourceEnabled && req.SourceFilter != nil && req.SourceFilter != true {
				h.Source = applySourceFilter(src, req.SourceFilter)
			} else if !sourceEnabled {
				h.Source = nil
			}
			// highlight(只有 source 存在时才有意义)
			if len(highlightTokens) > 0 && len(req.Highlight) > 0 && h.Source != nil {
				h.Highlight = applyHighlight(h.Source, req.Highlight, highlightTokens)
			}
			allHits = append(allHits, h)
		}
	}
	stdSort.Slice(allHits, func(i, j int) bool {
		// 文本查询且未指定 sort: 按 _score 降序, 相同分按 index+id 升序
		if scored {
			if allHits[i].Score != allHits[j].Score {
				return allHits[i].Score > allHits[j].Score
			}
		}
		if allHits[i].Index == allHits[j].Index {
			return allHits[i].ID < allHits[j].ID
		}
		return allHits[i].Index < allHits[j].Index
	})

	// total.value / total.relation 受 track_total_hits 控制
	exactTotal, totalCap := trackTotalHitsValue(req.TrackTotalHits)
	page := len(allHits)
	totalValue := int64(totalHitsAll)
	relation := "eq"
	if !exactTotal && int64(totalHitsAll) > int64(totalCap) {
		totalValue = int64(totalCap)
		relation = "gte"
	}
	_ = page
	resp := map[string]interface{}{
		"took": time.Since(took).Milliseconds(),
		"hits": map[string]interface{}{
			"total": map[string]interface{}{
				"value":    totalValue,
				"relation": relation,
			},
			"hits": allHits,
		},
	}
	// 聚合: 在已命中的 hits 上求值(每个 hit 携 (index, docID) 信息)
	if len(req.Aggregations) > 0 {
		indexedHits := make([]search.IndexedHit, 0, len(allHits))
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
	// suggest: 在 hits/aggregations 计算完后附加
	if len(req.Suggest) > 0 {
		// 跨索引合并: 把每个 idx 单独跑, 然后按 suggest 名字合并
		merged := make(map[string][]search.SuggestResult, len(req.Suggest))
		for _, idx := range indices {
			s.attachSuggestToResponseHelper(idx, req.Suggest, merged)
		}
		if len(merged) > 0 {
			resp["suggest"] = merged
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
	// _source 过滤: true=全显, false=不显, []string=只显指定字段, 默认 true
	SourceFilter interface{} `json:"_source,omitempty"`
	// highlight: {"fields": {"field_name": {}}, "pre_tags": [...], "post_tags": [...]}
	Highlight map[string]interface{} `json:"highlight,omitempty"`
	// track_total_hits: true=全量统计, false=截断, int=上限
	TrackTotalHits interface{} `json:"track_total_hits,omitempty"`
	// suggest: 形如 {"<name>": {"text": "...", "term": {...} | "completion": {...} | "prefix": {...}}}
	Suggest map[string]interface{} `json:"suggest,omitempty"`
}

// hit 单条命中
type hit struct {
	Index  string                 `json:"_index"`
	ID     string                 `json:"_id"`
	Score  float64                `json:"_score,omitempty"`
	Source map[string]interface{} `json:"_source,omitempty"`
	// 新增: 高亮片段, 与 ES 兼容: {"field_name": ["...<em>tok</em>..."]}
	Highlight map[string][]string `json:"highlight,omitempty"`
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
	if v, ok := raw["match_phrase"].(map[string]interface{}); ok {
		out.MatchPhrase = v
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

// isTextQuery 判定一个 query 是否走 BM25 打分
// true 条件: 顶层是 match, 或 bool 包了 match 子句
// false 条件: match_all / term / terms / range / 纯 filter(bool)
// 被 constant_score 包裹时降级为 false(布尔语义)
func isTextQuery(q *search.Query) bool {
	if q == nil {
		return false
	}
	if q.Match != nil {
		return true
	}
	if q.Bool != nil {
		// 任何 must/should 里出现 match 就视为文本查询
		for _, c := range q.Bool.Must {
			if clauseType(c) == "match" {
				return true
			}
		}
		for _, c := range q.Bool.Should {
			if clauseType(c) == "match" {
				return true
			}
		}
		for _, c := range q.Bool.Filter {
			if clauseType(c) == "match" {
				return true
			}
		}
	}
	return false
}

// clauseType 提取子句的类型 key (e.g. "match", "term", ...)
func clauseType(clause map[string]interface{}) string {
	for k := range clause {
		return k
	}
	return ""
}

// extractTextClauses 从 query 中提取 (field, queryText) 列表, 用于 BM25 打分
// 只看顶层 match, 或 bool.must/should 中所有 match 子句
func extractTextClauses(q *search.Query) []fieldQuery {
	if q == nil {
		return nil
	}
	if q.Match != nil {
		out := make([]fieldQuery, 0, len(q.Match))
		for field, val := range q.Match {
			if m, ok := val.(map[string]interface{}); ok {
				if s, ok := m["query"].(string); ok {
					out = append(out, fieldQuery{Field: field, Query: s})
					continue
				}
			}
			// 简化: 直接传字符串
			if s, ok := val.(string); ok {
				out = append(out, fieldQuery{Field: field, Query: s})
			}
		}
		return out
	}
	if q.Bool != nil {
		out := make([]fieldQuery, 0)
		for _, c := range q.Bool.Must {
			if m, ok := c["match"].(map[string]interface{}); ok {
				for field, val := range m {
					if s, ok := val.(string); ok {
						out = append(out, fieldQuery{Field: field, Query: s})
					} else if mm, ok := val.(map[string]interface{}); ok {
						if qs, ok := mm["query"].(string); ok {
							out = append(out, fieldQuery{Field: field, Query: qs})
						}
					}
				}
			}
		}
		return out
	}
	return nil
}

// fieldQuery (field, queryText) 元组
type fieldQuery struct {
	Field string
	Query string
}

// computeHitScore 算一个 hit 的 BM25 总分(对所有文本子句求和)
func computeHitScore(e *search.Engine, index string, q *search.Query, docID string) float64 {
	clauses := extractTextClauses(q)
	if len(clauses) == 0 {
		return 1.0
	}
	var total float64
	for _, c := range clauses {
		total += e.BM25FieldScore(index, c.Field, docID, c.Query)
	}
	if total == 0 {
		// 命中但无文本打分(全 term 子句等), 退回 1.0
		return 1.0
	}
	return total
}

// sortByBM25Score 按 BM25 得分降序排序 docID 列表
func sortByBM25Score(e *search.Engine, index string, q *search.Query, ids []string) []string {
	clauses := extractTextClauses(q)
	if len(clauses) == 0 {
		return ids
	}
	type scored struct {
		id    string
		score float64
	}
	out := make([]scored, len(ids))
	for i, id := range ids {
		var s float64
		for _, c := range clauses {
			s += e.BM25FieldScore(index, c.Field, id, c.Query)
		}
		out[i] = scored{id: id, score: s}
	}
	stdSort.SliceStable(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].id < out[j].id
	})
	res := make([]string, len(out))
	for i, s := range out {
		res[i] = s.id
	}
	return res
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
