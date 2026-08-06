// Package search 包的查询 DSL 解析与执行
// 支持的 query 类型: match / term / terms / range / bool
// 任何未识别类型视为 match-all 或 no-match
package search

import (
	"fmt"
	"strings"
)

// Query 表示一个查询请求(只关心 query 字段;size/from/sort 在 server 层处理)
type Query struct {
	Match   map[string]interface{} `json:"match,omitempty"`
	Term    map[string]interface{} `json:"term,omitempty"`
	Terms   map[string]interface{} `json:"terms,omitempty"`
	Range   map[string]interface{} `json:"range,omitempty"`
	Bool    *BoolQuery             `json:"bool,omitempty"`
	MatchAll map[string]interface{} `json:"match_all,omitempty"`
}

// BoolQuery bool 组合查询
type BoolQuery struct {
	Must               []map[string]interface{} `json:"must,omitempty"`
	Filter             []map[string]interface{} `json:"filter,omitempty"`
	Should             []map[string]interface{} `json:"should,omitempty"`
	MustNot            []map[string]interface{} `json:"must_not,omitempty"`
	MinimumShouldMatch int                      `json:"minimum_should_match,omitempty"`
}

// Match 在某索引上根据 query 算出命中的 docID 集合
// 参数:
//
//	index: 索引名
//	q: 已经反序列化好的 Query 结构
//
// 返回:
//
//	[]string: 命中的 docID 列表(不保证排序,server 层再 sort)
//	error: 解析错误
func (e *Engine) Match(index string, q *Query) ([]string, error) {
	if q == nil {
		return e.allDocs(index), nil
	}

	// 优先处理 bool(ES 不允许与其它 leaf query 组合)
	if q.Bool != nil {
		set, err := e.evalBoolQuery(index, q.Bool)
		if err != nil {
			return nil, err
		}
		return setToSlice(set), nil
	}
	// match_all
	if q.MatchAll != nil {
		return e.allDocs(index), nil
	}

	set, err := e.evalQueryMap(index, map[string]interface{}{"match": q.Match, "term": q.Term, "terms": q.Terms, "range": q.Range})
	if err != nil {
		return nil, err
	}
	return setToSlice(set), nil
}

// allDocs 返回某索引全部 docID
func (e *Engine) allDocs(index string) []string {
	return e.AllIDs(index)
}

// evalQueryMap 在解析后查询 map 上递归求值
// 顶层 map 只会有一个 key
func (e *Engine) evalQueryMap(index string, m map[string]interface{}) (map[string]struct{}, error) {
	// 按 ES 惯例: 顶层只能有一种 query 类型
	// 我们取第一个非空字段
	for _, k := range []string{"match", "term", "terms", "range", "bool", "match_all"} {
		raw, ok := m[k]
		if !ok || raw == nil {
			continue
		}
		// 兼容"非 nil 但为空"的情况
		if mp, isMap := raw.(map[string]interface{}); isMap && len(mp) == 0 {
			continue
		}
		switch k {
		case "match":
			fields, _ := raw.(map[string]interface{})
			return e.evalMatch(index, fields)
		case "term":
			fields, _ := raw.(map[string]interface{})
			return e.evalTerm(index, fields)
		case "terms":
			fields, _ := raw.(map[string]interface{})
			return e.evalTerms(index, fields)
		case "range":
			fields, _ := raw.(map[string]interface{})
			return e.evalRange(index, fields)
		case "bool":
			switch v := raw.(type) {
			case *BoolQuery:
				return e.evalBoolQuery(index, v)
			case map[string]interface{}:
				return e.evalBool(index, v)
			}
		case "match_all":
			ids := e.allDocs(index)
			out := make(map[string]struct{}, len(ids))
			for _, id := range ids {
				out[id] = struct{}{}
			}
			return out, nil
		}
	}
	// 未知: 视为 no match
	return map[string]struct{}{}, nil
}

// evalMatch 处理 match 查询
// match 字段值可为字符串或 {"query": "...", "operator": "and"/"or"}
func (e *Engine) evalMatch(index string, fields map[string]interface{}) (map[string]struct{}, error) {
	if len(fields) == 0 {
		return nil, fmt.Errorf("match requires at least one field")
	}
	// 只取第一个字段
	for field, raw := range fields {
		value := extractQueryValue(raw)
		return e.matchDocsTokens(index, field, value), nil
	}
	return nil, nil
}

// evalTerm 处理 term 查询
func (e *Engine) evalTerm(index string, fields map[string]interface{}) (map[string]struct{}, error) {
	if len(fields) == 0 {
		return nil, fmt.Errorf("term requires at least one field")
	}
	for field, raw := range fields {
		// term value 直接用,不做分词
		return e.termValue(index, field, raw), nil
	}
	return nil, nil
}

// evalTerms 处理 terms 查询(多值 term,任一匹配即命中)
func (e *Engine) evalTerms(index string, fields map[string]interface{}) (map[string]struct{}, error) {
	if len(fields) == 0 {
		return nil, fmt.Errorf("terms requires at least one field")
	}
	out := map[string]struct{}{}
	for field, raw := range fields {
		list, ok := raw.([]interface{})
		if !ok {
			continue
		}
		for _, v := range list {
			set := e.termValue(index, field, v)
			for id := range set {
				out[id] = struct{}{}
			}
		}
		return out, nil
	}
	return out, nil
}

// evalRange 处理 range 查询
// 支持 gte/lte/gt/lt,值可为数字或字符串(字符串比较用字典序)
// 走 sortedIndexCache, O(logN + K) 而非 O(N)
func (e *Engine) evalRange(index string, fields map[string]interface{}) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	if len(fields) == 0 {
		return nil, fmt.Errorf("range requires at least one field")
	}
	for field, raw := range fields {
		rng, _ := raw.(map[string]interface{})
		var gte, lte, gt, lt *string
		if s, ok := asStringKey(rng["gte"]); ok {
			gte = &s
		}
		if s, ok := asStringKey(rng["lte"]); ok {
			lte = &s
		}
		if s, ok := asStringKey(rng["gt"]); ok {
			gt = &s
		}
		if s, ok := asStringKey(rng["lt"]); ok {
			lt = &s
		}
		// 走 sortedCache; 若字段不索引(复杂类型), 回退到全表扫描
		ids := e.sortedCache.rangeQuery(index, field, gte, lte, gt, lt)
		if len(ids) == 0 && (gte == nil && lte == nil && gt == nil && lt == nil) {
			// 没有值, 视为不命中
			return out, nil
		}
		// 若 sortedCache 完全没有该 field, 退化到全表扫描
		e.mu.RLock()
		_, hasField := e.docs[index]
		e.mu.RUnlock()
		if !hasField {
			return out, nil
		}
		// 退化判断: cache 返回 0 且该 index 确实有 docs 且 range 是非空 -> 走扫描
		e.mu.RLock()
		docs := e.docs[index]
		e.mu.RUnlock()
		if len(ids) == 0 && len(docs) > 0 {
			// 可能是 field 不可索引(数组/嵌套), 走全表扫描
			for id, doc := range docs {
				v, ok := doc[field]
				if !ok {
					continue
				}
				if compareRange(v, rng["gte"], rng["lte"], rng["gt"], rng["lt"]) {
					ids[id] = struct{}{}
				}
			}
		}
		for id := range ids {
			out[id] = struct{}{}
		}
		return out, nil
	}
	return out, nil
}

// asStringKey 把任意 JSON 值转为可用于 sorted index 比较的字符串键
// 失败返回 ok=false
func asStringKey(v interface{}) (string, bool) {
	if v == nil {
		return "", false
	}
	if s, ok := valueOf(v); ok {
		return s, true
	}
	// 数组/对象等复杂类型, 走字符串化兜底
	return stringify(v), true
}

// evalBool 处理 bool 查询
// must/filter: 与;should: 或;must_not: 非
func (e *Engine) evalBool(index string, b map[string]interface{}) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	// 初始为全集
	for _, id := range e.allDocs(index) {
		out[id] = struct{}{}
	}

	// must
	if raw, ok := b["must"]; ok {
		clauses, _ := raw.([]interface{})
		must, err := e.unionClauses(index, clauses)
		if err != nil {
			return nil, err
		}
		out = intersect(out, must)
	}
	// filter(语义同 must,这里也按与)
	if raw, ok := b["filter"]; ok {
		clauses, _ := raw.([]interface{})
		filter, err := e.unionClauses(index, clauses)
		if err != nil {
			return nil, err
		}
		out = intersect(out, filter)
	}
	// must_not
	if raw, ok := b["must_not"]; ok {
		clauses, _ := raw.([]interface{})
		notSet, err := e.unionClauses(index, clauses)
		if err != nil {
			return nil, err
		}
		out = diff(out, notSet)
	}
	// should(任一命中即留下;若 has must/filter,只在该范围内扩展)
	if raw, ok := b["should"]; ok {
		clauses, _ := raw.([]interface{})
		should, err := e.unionClauses(index, clauses)
		if err != nil {
			return nil, err
		}
		out = intersect(out, should)
	}
	return out, nil
}

// evalBoolQuery 接受 *BoolQuery 的入口,内部转为 map 再调 evalBool
func (e *Engine) evalBoolQuery(index string, b *BoolQuery) (map[string]struct{}, error) {
	m := map[string]interface{}{}
	if len(b.Must) > 0 {
		clauses := make([]interface{}, len(b.Must))
		for i, c := range b.Must {
			clauses[i] = c
		}
		m["must"] = clauses
	}
	if len(b.Filter) > 0 {
		clauses := make([]interface{}, len(b.Filter))
		for i, c := range b.Filter {
			clauses[i] = c
		}
		m["filter"] = clauses
	}
	if len(b.Should) > 0 {
		clauses := make([]interface{}, len(b.Should))
		for i, c := range b.Should {
			clauses[i] = c
		}
		m["should"] = clauses
	}
	if len(b.MustNot) > 0 {
		clauses := make([]interface{}, len(b.MustNot))
		for i, c := range b.MustNot {
			clauses[i] = c
		}
		m["must_not"] = clauses
	}
	return e.evalBool(index, m)
}

// unionClauses 把一组 clause 求并集
func (e *Engine) unionClauses(index string, clauses []interface{}) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	for _, c := range clauses {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		set, err := e.evalQueryMap(index, m)
		if err != nil {
			return nil, err
		}
		for id := range set {
			out[id] = struct{}{}
		}
	}
	return out, nil
}

// 工具:集合运算

func intersect(a, b map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{})
	for k := range a {
		if _, ok := b[k]; ok {
			out[k] = struct{}{}
		}
	}
	return out
}

func diff(a, b map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{})
	for k := range a {
		if _, ok := b[k]; !ok {
			out[k] = struct{}{}
		}
	}
	return out
}

func setToSlice(s map[string]struct{}) []string {
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	return out
}

// extractQueryValue 兼容 "value" 或 {"query": "value"} 形式
func extractQueryValue(raw interface{}) string {
	switch x := raw.(type) {
	case string:
		return x
	case map[string]interface{}:
		if v, ok := x["value"]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
		if v, ok := x["query"]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return fmt.Sprintf("%v", raw)
}

// compareRange 比较某字段值是否落在 [gte,lte]/(gt,lt) 区间
// 数字按数值比;字符串按字典序
func compareRange(v interface{}, gte, lte, gt, lt interface{}) bool {
	if gte != nil && !compareGTE(v, gte) {
		return false
	}
	if lte != nil && !compareLTE(v, lte) {
		return false
	}
	if gt != nil && !compareGT(v, gt) {
		return false
	}
	if lt != nil && !compareLT(v, lt) {
		return false
	}
	return true
}

func compareGTE(v, bound interface{}) bool { return !compareLT(v, bound) }
func compareLTE(v, bound interface{}) bool { return !compareGT(v, bound) }
func compareGT(v, bound interface{}) bool {
	c := cmpAny(v, bound)
	return c > 0
}
func compareLT(v, bound interface{}) bool {
	c := cmpAny(v, bound)
	return c < 0
}

// cmpAny 通用比较:数字按数值,其它按字符串
func cmpAny(a, b interface{}) int {
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		switch {
		case af < bf:
			return -1
		case af > bf:
			return 1
		default:
			return 0
		}
	}
	as := fmt.Sprintf("%v", a)
	bs := fmt.Sprintf("%v", b)
	return strings.Compare(as, bs)
}

func toFloat(v interface{}) (float64, bool) {
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
