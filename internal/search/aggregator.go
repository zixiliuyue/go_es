// Package search - 服务端聚合分析
//
// 覆盖的聚合类型(ES 8 兼容子集):
//   - terms          桶聚合, 按字段值分组
//   - histogram      数值/日期直方图
//   - date_histogram 日期直方图(calendar_interval)
//   - range          自定义区间桶
//   - value_count    计数
//   - avg            平均值
//   - sum            求和
//   - min            最小值
//   - max            最大值
//   - stats          一次性返回 count/min/max/avg/sum
//   - cardinality    去重计数(精确实现, 非 HLL)
//
// 设计:
//   - 输入: 命中的 docID 列表(由 query 阶段算出) + 聚合定义
//   - 中间: 遍历 doc source 一次, 同步计算所有顶层聚合
//   - 嵌套: 父桶先求, 每个父桶再算子聚合(一次遍历/桶)
//   - 输出: ES 兼容 JSON 结构
package search

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// AggregationRequest 聚合定义.
// Key 是聚合名(客户端在响应中用此 key 找结果), Value 是聚合类型定义.
type AggregationRequest struct {
	Key  string
	Spec map[string]interface{}
}

// AggregationsResult 聚合结果. 与请求一一对应.
type AggregationsResult map[string]interface{}

// evalAggregations 求值一组聚合定义.
// indices: 命中的 (index, docID) 列表, 跨索引(每个 doc 携 index 信息)
// aggs: 聚合定义列表
func evalAggregations(indices []IndexedHit, aggs []AggregationRequest) (AggregationsResult, error) {
	out := make(AggregationsResult, len(aggs))
	for _, a := range aggs {
		v, err := evalOneAggregation(indices, a.Spec)
		if err != nil {
			return nil, fmt.Errorf("agg %q: %w", a.Key, err)
		}
		out[a.Key] = v
	}
	return out, nil
}

// EvalAggregations 是 evalAggregations 的导出别名, 给 server 层调用
func EvalAggregations(hits []IndexedHit, aggs []AggregationRequest) (AggregationsResult, error) {
	return evalAggregations(hits, aggs)
}

// IndexedHit 一条命中, 携 (index, docID) 便于跨索引聚合
type IndexedHit struct {
	Index string
	DocID string
}

// evalOneAggregation 解析一个聚合定义, 委派到具体类型
// spec 形如 {"terms": {"field": "color", "size": 10}} 或 {"avg": {"field": "price"}}
func evalOneAggregation(hits []IndexedHit, spec map[string]interface{}) (map[string]interface{}, error) {
	if len(spec) != 1 {
		return nil, fmt.Errorf("aggregation must have exactly one type key, got %d", len(spec))
	}
	for typ, def := range spec {
		defMap, ok := def.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("aggregation body must be an object")
		}
		switch typ {
		case "terms":
			return evalTerms(hits, defMap)
		case "histogram":
			return evalHistogram(hits, defMap, false)
		case "date_histogram":
			return evalHistogram(hits, defMap, true)
		case "range":
			return evalRangeAgg(hits, defMap)
		case "value_count":
			return evalValueCount(hits, defMap)
		case "avg":
			return evalSingleMetric(hits, defMap, "avg")
		case "sum":
			return evalSingleMetric(hits, defMap, "sum")
		case "min":
			return evalSingleMetric(hits, defMap, "min")
		case "max":
			return evalSingleMetric(hits, defMap, "max")
		case "stats":
			return evalStats(hits, defMap)
		case "cardinality":
			return evalCardinality(hits, defMap)
		}
		return nil, fmt.Errorf("unsupported aggregation type: %s", typ)
	}
	return nil, fmt.Errorf("empty aggregation spec")
}

// fieldValue 提取 doc 中某字段的值, 缺失返回 (nil, false)
func (h IndexedHit) fieldValue(field string) (interface{}, bool) {
	src, ok := engineGetSource(h.Index, h.DocID)
	if !ok {
		return nil, false
	}
	v, ok := src[field]
	return v, ok
}

// engineGetSource 是 Engine.GetSource 的薄包装, 避免 engine 包在 test stub 时强耦合
// (实际就是 Engine.GetSource)
var engineGetSource = func(index, id string) (map[string]interface{}, bool) {
	// 真实实现在 engine.go 中注册, 见 SetSourceLookup
	return nil, false
}

// SetSourceLookup 注入 source 查询函数(由 server 层在 init 时调用, 避免循环 import)
func SetSourceLookup(fn func(index, id string) (map[string]interface{}, bool)) {
	engineGetSource = fn
}

// ---------- 公共: 收集 doc 字段值 ----------

// collectFieldValues 遍历 hits 收集所有非空 field 值
func collectFieldValues(hits []IndexedHit, field string) []interface{} {
	out := make([]interface{}, 0, len(hits))
	for _, h := range hits {
		if v, ok := h.fieldValue(field); ok && v != nil {
			out = append(out, v)
		}
	}
	return out
}

// ---------- terms 聚合 ----------

// evalTerms 按字段值分组计数
// 期望 {"field": "...", "size": int, "order": {...}}
// 缺省 size = 10, 按 _count desc 排序
func evalTerms(hits []IndexedHit, def map[string]interface{}) (map[string]interface{}, error) {
	field, _ := def["field"].(string)
	if field == "" {
		return nil, fmt.Errorf("terms aggregation requires field")
	}
	size := 10
	if s, ok := def["size"].(float64); ok && int(s) > 0 {
		size = int(s)
	}
	if s, ok := def["size"].(int); ok && s > 0 {
		size = s
	}
	// 计数
	counts := make(map[string]int64)
	for _, h := range hits {
		v, ok := h.fieldValue(field)
		if !ok {
			continue
		}
		key := stringify(v)
		counts[key]++
	}
	// 排序
	type kv struct {
		key string
		c   int64
	}
	pairs := make([]kv, 0, len(counts))
	for k, c := range counts {
		pairs = append(pairs, kv{k, c})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].c != pairs[j].c {
			return pairs[i].c > pairs[j].c
		}
		return pairs[i].key < pairs[j].key
	})
	// 截断
	truncated := false
	if size > 0 && size < len(pairs) {
		truncated = true
		pairs = pairs[:size]
	}
	// 拼 buckets
	other := int64(0)
	if truncated {
		// 重新算 other: 取被截断的桶
		kept := make(map[string]struct{}, size)
		for _, p := range pairs {
			kept[p.key] = struct{}{}
		}
		for k, c := range counts {
			if _, ok := kept[k]; !ok {
				other += c
			}
		}
	}
	buckets := make([]map[string]interface{}, 0, len(pairs))
	for _, p := range pairs {
		buckets = append(buckets, map[string]interface{}{
			"key":      p.key,
			"doc_count": p.c,
		})
	}
	out := map[string]interface{}{
		"doc_count_error_upper_bound": 0,
		"sum_other_doc_count":         other,
		"buckets":                     buckets,
	}
	// 处理子聚合
	if sub, ok := def["aggs"].(map[string]interface{}); ok {
		// 子聚合只在每个父桶上求值(对桶内 doc 集合)
		attachSubAggsTerms(hits, field, buckets, sub)
	}
	return out, nil
}

// attachSubAggsTerms 对 terms 的每个 bucket 跑子聚合
func attachSubAggsTerms(hits []IndexedHit, field string, buckets []map[string]interface{}, subs map[string]interface{}) {
	subReqs, err := parseAggDefs(subs)
	if err != nil {
		// 失败时静默忽略, ES 也只返回 partial result
		return
	}
	for _, b := range buckets {
		key, _ := b["key"].(string)
		// 收集此 bucket 内的 hit
		subset := make([]IndexedHit, 0)
		for _, h := range hits {
			v, ok := h.fieldValue(field)
			if !ok {
				continue
			}
			if stringify(v) == key {
				subset = append(subset, h)
			}
		}
		res, err := evalAggregations(subset, subReqs)
		if err != nil {
			continue
		}
		b["<aggregation_name_placeholder>"] = nil
		for k, v := range res {
			b[k] = v
		}
		// 删除占位 key(若 subReqs 为空, b 里可能多个 nil)
		delete(b, "<aggregation_name_placeholder>")
	}
}

// parseAggDefs 把 {"name1": spec1, "name2": spec2} 展开为 []AggregationRequest
func parseAggDefs(m map[string]interface{}) ([]AggregationRequest, error) {
	out := make([]AggregationRequest, 0, len(m))
	for k, v := range m {
		spec, ok := v.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("agg %q body must be object", k)
		}
		out = append(out, AggregationRequest{Key: k, Spec: spec})
	}
	return out, nil
}

// ---------- histogram 聚合 ----------

// evalHistogram 数值或日期直方图
// def 必填: field, interval
// date: true 时按日历间隔, interval 接受 "day"/"hour"/"minute" 或 "Nd"/"Nh"/"Nm"
// date: false 时 interval 为数字, 按 fixed_interval
func evalHistogram(hits []IndexedHit, def map[string]interface{}, date bool) (map[string]interface{}, error) {
	field, _ := def["field"].(string)
	if field == "" {
		return nil, fmt.Errorf("histogram aggregation requires field")
	}
	var interval float64
	if date {
		// date_histogram: 简化版仅支持 "day" / "hour" / "minute"
		s, _ := def["interval"].(string)
		if s == "" {
			s, _ = def["calendar_interval"].(string)
		}
		interval = dateIntervalSeconds(s)
		if interval == 0 {
			return nil, fmt.Errorf("date_histogram requires interval like day/hour/minute, got %q", s)
		}
		// 转毫秒
		interval *= 1000
	} else {
		if iv, ok := def["interval"].(float64); ok {
			interval = iv
		} else if iv, ok := def["interval"].(int); ok {
			interval = float64(iv)
		} else if iv, ok := def["interval"].(json.Number); ok {
			if f, err := iv.Float64(); err == nil {
				interval = f
			} else {
				return nil, fmt.Errorf("histogram requires numeric interval")
			}
		} else {
			return nil, fmt.Errorf("histogram requires numeric interval")
		}
		if interval <= 0 {
			return nil, fmt.Errorf("histogram interval must be > 0")
		}
	}
	minDocCount := int64(0)
	if md, ok := def["min_doc_count"].(float64); ok {
		minDocCount = int64(md)
	}
	// 收集每条 doc 的桶 key
	counts := make(map[string]int64)
	for _, h := range hits {
		v, ok := h.fieldValue(field)
		if !ok {
			continue
		}
		var bucket float64
		if date {
			ts, ok := toMillis(v)
			if !ok {
				continue
			}
			bucket = float64(ts) - float64(int64(float64(ts)/interval)*int64(interval))
			// 实际 bucket key 为 floor
			_ = bucket
			key := fmt.Sprintf("%d", (int64(ts)/int64(interval))*int64(interval))
			counts[key]++
		} else {
			f, ok := aggToFloat(v)
			if !ok {
				continue
			}
			key := fmt.Sprintf("%d", (int64(f/interval))*int64(interval))
			counts[key]++
		}
	}
	// 排序 key
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	buckets := make([]map[string]interface{}, 0, len(keys))
	for _, k := range keys {
		if counts[k] < minDocCount {
			continue
		}
		// key 转回 int / 时间
		var keyVal interface{} = k
		if date {
			keyVal = parseInt(k) * 1000 * 1000 // ns(ES 习惯, 此处给 ms)
		} else {
			keyVal = parseFloat(k)
		}
		buckets = append(buckets, map[string]interface{}{
			"key":       keyVal,
			"key_as_string": nil,
			"doc_count": counts[k],
		})
	}
	return map[string]interface{}{
		"buckets": buckets,
	}, nil
}

// dateIntervalSeconds 把 "day"/"hour"/"minute" 转秒
func dateIntervalSeconds(s string) float64 {
	switch strings.ToLower(s) {
	case "minute", "1m":
		return 60
	case "hour", "1h":
		return 3600
	case "day", "1d":
		return 86400
	}
	return 0
}

// ---------- range 聚合 ----------

// rangeToFloat 兼容 float64/int/json.Number 的 toFloat(本地版, 避免改函数名影响其它)
func rangeToFloat(v interface{}) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		if err == nil {
			return f, true
		}
	}
	return 0, false
}

// evalRangeAgg 自定义区间桶
// def: {"field": "...", "ranges": [{"to": 100}, {"from": 100, "to": 200}, {"from": 200}]}
func evalRangeAgg(hits []IndexedHit, def map[string]interface{}) (map[string]interface{}, error) {
	field, _ := def["field"].(string)
	if field == "" {
		return nil, fmt.Errorf("range aggregation requires field")
	}
	ranges, _ := def["ranges"].([]interface{})
	if len(ranges) == 0 {
		return nil, fmt.Errorf("range aggregation requires ranges array")
	}
	type rng struct {
		from, to *float64
		key      string
	}
	parsed := make([]rng, 0, len(ranges))
	for _, r := range ranges {
		rm, ok := r.(map[string]interface{})
		if !ok {
			continue
		}
		var f, t *float64
		if v, ok := rangeToFloat(rm["from"]); ok {
			f = &v
		}
		if v, ok := rangeToFloat(rm["to"]); ok {
			t = &v
		}
		k, _ := rm["key"].(string)
		parsed = append(parsed, rng{from: f, to: t, key: k})
	}
	buckets := make([]map[string]interface{}, len(parsed))
	for i, r := range parsed {
		var count int64
		for _, h := range hits {
			v, ok := h.fieldValue(field)
			if !ok {
				continue
			}
			f, ok := rangeToFloat(v)
			if !ok {
				continue
			}
			if r.from != nil && f < *r.from {
				continue
			}
			if r.to != nil && f >= *r.to {
				continue
			}
			count++
		}
		b := map[string]interface{}{"doc_count": count}
		if r.from != nil {
			b["from"] = *r.from
		}
		if r.to != nil {
			b["to"] = *r.to
		}
		if r.key != "" {
			b["key"] = r.key
		}
		buckets[i] = b
	}
	return map[string]interface{}{"buckets": buckets}, nil
}

// ---------- value_count 聚合 ----------

func evalValueCount(hits []IndexedHit, def map[string]interface{}) (map[string]interface{}, error) {
	field, _ := def["field"].(string)
	if field == "" {
		return nil, fmt.Errorf("value_count requires field")
	}
	var n int64
	for _, h := range hits {
		if _, ok := h.fieldValue(field); ok {
			n++
		}
	}
	return map[string]interface{}{"value": n}, nil
}

// ---------- avg / sum / min / max 单指标 ----------

// evalSingleMetric 求单值(avg/sum/min/max),统一处理避免重复
func evalSingleMetric(hits []IndexedHit, def map[string]interface{}, kind string) (map[string]interface{}, error) {
	field, _ := def["field"].(string)
	if field == "" {
		return nil, fmt.Errorf("%s requires field", kind)
	}
	values := collectFieldValues(hits, field)
	n := float64(len(values))
	if n == 0 {
		// ES 行为: 空集合返回 null
		return map[string]interface{}{"value": nil}, nil
	}
	switch kind {
	case "avg":
		sum := 0.0
		for _, v := range values {
			f, ok := aggToFloat(v)
			if !ok {
				continue
			}
			sum += f
		}
		return map[string]interface{}{"value": sum / n}, nil
	case "sum":
		s := 0.0
		for _, v := range values {
			f, ok := aggToFloat(v)
			if !ok {
				continue
			}
			s += f
		}
		return map[string]interface{}{"value": s}, nil
	case "min":
		m, _ := aggToFloat(values[0])
		for _, v := range values[1:] {
			f, ok := aggToFloat(v)
			if !ok {
				continue
			}
			if f < m {
				m = f
			}
		}
		return map[string]interface{}{"value": m}, nil
	case "max":
		m, _ := aggToFloat(values[0])
		for _, v := range values[1:] {
			f, ok := aggToFloat(v)
			if !ok {
				continue
			}
			if f > m {
				m = f
			}
		}
		return map[string]interface{}{"value": m}, nil
	}
	return nil, fmt.Errorf("unknown single metric: %s", kind)
}

// ---------- stats 聚合 ----------

func evalStats(hits []IndexedHit, def map[string]interface{}) (map[string]interface{}, error) {
	field, _ := def["field"].(string)
	if field == "" {
		return nil, fmt.Errorf("stats requires field")
	}
	values := collectFieldValues(hits, field)
	n := float64(len(values))
	if n == 0 {
		return map[string]interface{}{
			"count": 0, "min": nil, "max": nil, "avg": nil, "sum": 0.0,
		}, nil
	}
	first, _ := aggToFloat(values[0])
	mn, mx := first, first
	sum := 0.0
	for _, v := range values {
		f, ok := aggToFloat(v)
		if !ok {
			continue
		}
		sum += f
		if f < mn {
			mn = f
		}
		if f > mx {
			mx = f
		}
	}
	return map[string]interface{}{
		"count": int64(n),
		"min":   mn,
		"max":   mx,
		"avg":   sum / n,
		"sum":   sum,
	}, nil
}

// ---------- cardinality 聚合 ----------

// evalCardinality 精确实现(非 HLL), 直接 set 去重
func evalCardinality(hits []IndexedHit, def map[string]interface{}) (map[string]interface{}, error) {
	field, _ := def["field"].(string)
	if field == "" {
		return nil, fmt.Errorf("cardinality requires field")
	}
	set := make(map[string]struct{})
	for _, h := range hits {
		if v, ok := h.fieldValue(field); ok {
			set[stringify(v)] = struct{}{}
		}
	}
	return map[string]interface{}{"value": int64(len(set))}, nil
}

// ---------- 工具函数 ----------

// toFloat 数字转 float64, 字符串数字也尽量转, 否则返回 false
// 注意: 与 query.go::toFloat 同名冲突, 这里用 aggToFloat 区分
// 同时支持 json.Number(server 的 decodeJSON 启用了 UseNumber)
func aggToFloat(v interface{}) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case string:
		var f float64
		if _, err := fmt.Sscanf(x, "%g", &f); err == nil {
			return f, true
		}
	case json.Number:
		f, err := x.Float64()
		if err == nil {
			return f, true
		}
	}
	return 0, false
}

// toMillis 把字符串/数字时间戳转毫秒数
// 接受 int / int64 / float64 / float32 / string(RFC3339 或纯数字) / json.Number
func toMillis(v interface{}) (int64, bool) {
	switch x := v.(type) {
	case float64:
		// 可能是秒(10位) 或毫秒(13位)
		if x < 1e12 {
			return int64(x * 1000), true
		}
		return int64(x), true
	case float32:
		xf := float64(x)
		if xf < 1e12 {
			return int64(xf * 1000), true
		}
		return int64(xf), true
	case int:
		if x < 1e12 {
			return int64(x) * 1000, true
		}
		return int64(x), true
	case int64:
		if x < 1e12 {
			return x * 1000, true
		}
		return x, true
	case json.Number:
		if f, err := x.Float64(); err == nil {
			if f < 1e12 {
				return int64(f * 1000), true
			}
			return int64(f), true
		}
	case string:
		// 试 RFC3339
		t, err := time.Parse(time.RFC3339, x)
		if err == nil {
			return t.UnixMilli(), true
		}
		var n int64
		if _, err := fmt.Sscanf(x, "%d", &n); err == nil {
			if n < 1e12 {
				return n * 1000, true
			}
			return n, true
		}
	}
	return 0, false
}

func parseInt(s string) int64 {
	var n int64
	_, _ = fmt.Sscanf(s, "%d", &n)
	return n
}

func parseFloat(s string) float64 {
	var f float64
	_, _ = fmt.Sscanf(s, "%g", &f)
	return f
}
