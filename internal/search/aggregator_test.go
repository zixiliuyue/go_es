// Package search - 聚合单元测试
// 覆盖 terms / histogram / range / value_count / avg / sum / min / max / stats / cardinality / date_histogram
package search

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// newAggTestEngine 构造一个测试引擎并注入 sample 数据
// 字段:
//   - color (keyword)    red, blue, red, green, blue, blue
//   - price (number)     10, 20, 30, 40, 50, 60
//   - ts (string epoch)  1672531200, 1672617600, 1672704000, ... (按 day)
//   - tag  (string)      hot, hot, warm, cold, ...
func newAggTestEngine(t *testing.T) *Engine {
	t.Helper()
	e := newTestEngine(t)
	// 6 条样本
	docs := []map[string]interface{}{
		{"id": "1", "color": "red", "price": 10.0, "ts": 1672531200, "tag": "hot"},
		{"id": "2", "color": "blue", "price": 20.0, "ts": 1672617600, "tag": "hot"},
		{"id": "3", "color": "red", "price": 30.0, "ts": 1672704000, "tag": "warm"},
		{"id": "4", "color": "green", "price": 40.0, "ts": 1672790400, "tag": "cold"},
		{"id": "5", "color": "blue", "price": 50.0, "ts": 1672876800, "tag": "cold"},
		{"id": "6", "color": "blue", "price": 60.0, "ts": 1672963200, "tag": "warm"},
	}
	for i, d := range docs {
		e.IndexDoc("idx", docID(i+1), d)
	}
	// 注入 source lookup(因为新测试用 New() 而非 newTestEngine(), 在 newTestEngine 内部已注入)
	// 此处再保险一次
	SetSourceLookup(func(index, id string) (map[string]interface{}, bool) {
		return e.GetSource(index, id)
	})
	return e
}

func docID(n int) string {
	// 简化的 ID, 与 newAggTestEngine 中索引顺序一致
	return []string{"1", "2", "3", "4", "5", "6"}[n-1]
}

func allHits() []IndexedHit {
	hits := make([]IndexedHit, 6)
	for i := 0; i < 6; i++ {
		hits[i] = IndexedHit{Index: "idx", DocID: docID(i + 1)}
	}
	return hits
}

func TestAggregator_Terms(t *testing.T) {
	e := newAggTestEngine(t)
	_ = e
	reqs := []AggregationRequest{{
		Key:  "by_color",
		Spec: map[string]interface{}{"terms": map[string]interface{}{"field": "color"}},
	}}
	res, err := EvalAggregations(allHits(), reqs)
	assert.NoError(t, err)
	out, ok := res["by_color"].(map[string]interface{})
	assert.True(t, ok)
	buckets := out["buckets"].([]map[string]interface{})
	// blue=3, red=2, green=1
	assert.Len(t, buckets, 3)
	assert.Equal(t, "blue", buckets[0]["key"])
	assert.EqualValues(t, 3, buckets[0]["doc_count"])
	assert.Equal(t, "red", buckets[1]["key"])
	assert.EqualValues(t, 2, buckets[1]["doc_count"])
}

func TestAggregator_TermsSize(t *testing.T) {
	e := newAggTestEngine(t)
	_ = e
	reqs := []AggregationRequest{{
		Key:  "by_color",
		Spec: map[string]interface{}{"terms": map[string]interface{}{"field": "color", "size": 2}},
	}}
	res, err := EvalAggregations(allHits(), reqs)
	assert.NoError(t, err)
	buckets := res["by_color"].(map[string]interface{})["buckets"].([]map[string]interface{})
	assert.Len(t, buckets, 2)
	assert.EqualValues(t, 1, res["by_color"].(map[string]interface{})["sum_other_doc_count"])
}

func TestAggregator_Avg(t *testing.T) {
	e := newAggTestEngine(t)
	_ = e
	reqs := []AggregationRequest{{
		Key:  "avg_price",
		Spec: map[string]interface{}{"avg": map[string]interface{}{"field": "price"}},
	}}
	res, err := EvalAggregations(allHits(), reqs)
	assert.NoError(t, err)
	v := res["avg_price"].(map[string]interface{})["value"]
	assert.InDelta(t, 35.0, v, 0.001) // (10+20+30+40+50+60)/6 = 35
}

func TestAggregator_Sum(t *testing.T) {
	e := newAggTestEngine(t)
	_ = e
	reqs := []AggregationRequest{{
		Key:  "sum_price",
		Spec: map[string]interface{}{"sum": map[string]interface{}{"field": "price"}},
	}}
	res, err := EvalAggregations(allHits(), reqs)
	assert.NoError(t, err)
	v := res["sum_price"].(map[string]interface{})["value"]
	assert.InDelta(t, 210.0, v, 0.001)
}

func TestAggregator_MinMax(t *testing.T) {
	e := newAggTestEngine(t)
	_ = e
	minReq := []AggregationRequest{{Key: "m", Spec: map[string]interface{}{"min": map[string]interface{}{"field": "price"}}}}
	maxReq := []AggregationRequest{{Key: "m", Spec: map[string]interface{}{"max": map[string]interface{}{"field": "price"}}}}
	rMin, _ := EvalAggregations(allHits(), minReq)
	rMax, _ := EvalAggregations(allHits(), maxReq)
	assert.InDelta(t, 10.0, rMin["m"].(map[string]interface{})["value"], 0.001)
	assert.InDelta(t, 60.0, rMax["m"].(map[string]interface{})["value"], 0.001)
}

func TestAggregator_Stats(t *testing.T) {
	e := newAggTestEngine(t)
	_ = e
	reqs := []AggregationRequest{{
		Key:  "s",
		Spec: map[string]interface{}{"stats": map[string]interface{}{"field": "price"}},
	}}
	res, err := EvalAggregations(allHits(), reqs)
	assert.NoError(t, err)
	s := res["s"].(map[string]interface{})
	assert.EqualValues(t, 6, s["count"])
	assert.InDelta(t, 10.0, s["min"], 0.001)
	assert.InDelta(t, 60.0, s["max"], 0.001)
	assert.InDelta(t, 35.0, s["avg"], 0.001)
	assert.InDelta(t, 210.0, s["sum"], 0.001)
}

func TestAggregator_ValueCount(t *testing.T) {
	e := newAggTestEngine(t)
	_ = e
	reqs := []AggregationRequest{{
		Key:  "vc",
		Spec: map[string]interface{}{"value_count": map[string]interface{}{"field": "price"}},
	}}
	res, err := EvalAggregations(allHits(), reqs)
	assert.NoError(t, err)
	assert.EqualValues(t, 6, res["vc"].(map[string]interface{})["value"])
}

func TestAggregator_Cardinality(t *testing.T) {
	e := newAggTestEngine(t)
	_ = e
	reqs := []AggregationRequest{{
		Key:  "c",
		Spec: map[string]interface{}{"cardinality": map[string]interface{}{"field": "color"}},
	}}
	res, err := EvalAggregations(allHits(), reqs)
	assert.NoError(t, err)
	assert.EqualValues(t, 3, res["c"].(map[string]interface{})["value"])
}

func TestAggregator_Histogram(t *testing.T) {
	e := newAggTestEngine(t)
	_ = e
	reqs := []AggregationRequest{{
		Key: "h",
		Spec: map[string]interface{}{"histogram": map[string]interface{}{
			"field":     "price",
			"interval":  20.0,
		}},
	}}
	res, err := EvalAggregations(allHits(), reqs)
	assert.NoError(t, err)
	buckets := res["h"].(map[string]interface{})["buckets"].([]map[string]interface{})
	// 0-20: 10,20 -> 2
	// 20-40: 30,40 -> 2
	// 40-60: 50,60 -> 2
	// 60-80: empty (min_doc_count=0 时也会输出)
	assert.GreaterOrEqual(t, len(buckets), 3)
	total := int64(0)
	for _, b := range buckets {
		total += b["doc_count"].(int64)
	}
	assert.EqualValues(t, 6, total)
}

func TestAggregator_Range(t *testing.T) {
	e := newAggTestEngine(t)
	_ = e
	reqs := []AggregationRequest{{
		Key: "r",
		Spec: map[string]interface{}{"range": map[string]interface{}{
			"field": "price",
			"ranges": []interface{}{
				map[string]interface{}{"to": 30.0},
				map[string]interface{}{"from": 30.0, "to": 50.0},
				map[string]interface{}{"from": 50.0},
			},
		}},
	}}
	res, err := EvalAggregations(allHits(), reqs)
	assert.NoError(t, err)
	buckets := res["r"].(map[string]interface{})["buckets"].([]map[string]interface{})
	assert.Len(t, buckets, 3)
	assert.EqualValues(t, 2, buckets[0]["doc_count"]) // < 30: 10, 20
	assert.EqualValues(t, 2, buckets[1]["doc_count"]) // [30, 50): 30, 40
	assert.EqualValues(t, 2, buckets[2]["doc_count"]) // >= 50: 50, 60
}

func TestAggregator_DateHistogram(t *testing.T) {
	e := newAggTestEngine(t)
	_ = e
	reqs := []AggregationRequest{{
		Key: "d",
		Spec: map[string]interface{}{"date_histogram": map[string]interface{}{
			"field":    "ts",
			"interval": "day",
		}},
	}}
	res, err := EvalAggregations(allHits(), reqs)
	assert.NoError(t, err)
	buckets := res["d"].(map[string]interface{})["buckets"].([]map[string]interface{})
	// 6 天数据, 每条一天, 总和应 = 6
	total := int64(0)
	for _, b := range buckets {
		total += b["doc_count"].(int64)
	}
	assert.EqualValues(t, 6, total)
}

func TestAggregator_EmptySet(t *testing.T) {
	e := newAggTestEngine(t)
	_ = e
	reqs := []AggregationRequest{{
		Key:  "x",
		Spec: map[string]interface{}{"avg": map[string]interface{}{"field": "price"}},
	}}
	// 空 hits
	res, err := EvalAggregations([]IndexedHit{}, reqs)
	assert.NoError(t, err)
	assert.Nil(t, res["x"].(map[string]interface{})["value"])
}

func TestAggregator_MissingField(t *testing.T) {
	e := newAggTestEngine(t)
	_ = e
	// 全部 doc 都没有 field=missing
	reqs := []AggregationRequest{{
		Key:  "x",
		Spec: map[string]interface{}{"avg": map[string]interface{}{"field": "missing"}},
	}}
	res, err := EvalAggregations(allHits(), reqs)
	assert.NoError(t, err)
	assert.Nil(t, res["x"].(map[string]interface{})["value"])
}

func TestAggregator_InvalidSpec(t *testing.T) {
	e := newAggTestEngine(t)
	_ = e
	// 缺 field
	reqs := []AggregationRequest{{
		Key:  "x",
		Spec: map[string]interface{}{"terms": map[string]interface{}{}},
	}}
	_, err := EvalAggregations(allHits(), reqs)
	assert.Error(t, err)
}

func TestAggregator_UnsupportedType(t *testing.T) {
	e := newAggTestEngine(t)
	_ = e
	reqs := []AggregationRequest{{
		Key:  "x",
		Spec: map[string]interface{}{"unknown_type": map[string]interface{}{"field": "x"}},
	}}
	_, err := EvalAggregations(allHits(), reqs)
	assert.Error(t, err)
}
