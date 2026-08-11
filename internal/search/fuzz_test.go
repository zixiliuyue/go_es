// Package search fuzz 测试
//
// 使用 Go 1.18+ 原生 fuzz 能力, 对核心解析路径做随机输入测试:
//   - parseQueryString / parseSimpleQueryString / stripSQSReserved: 纯字符串解析
//   - evalMultiMatch / evalQueryString / evalSimpleQueryString: Engine 方法, 接受任意 map
//   - Match: 完整查询路径, fuzz []byte → JSON → Query → Match
//
// 运行方式:
//
//	# 快速跑已有 corpus + 种子(不做 fuzz 探索)
//	go test -count=1 ./internal/search/
//
//	# 启动 fuzz 探索(每个 target 30s)
//	go test -fuzz=FuzzParseQueryString -fuzztime=30s ./internal/search/
//	go test -fuzz=FuzzParseSimpleQueryString -fuzztime=30s ./internal/search/
//	go test -fuzz=FuzzStripSQSReserved -fuzztime=30s ./internal/search/
//	go test -fuzz=FuzzEngineMatch -fuzztime=30s ./internal/search/
//	go test -fuzz=FuzzEvalMultiMatch -fuzztime=30s ./internal/search/
//
// crash 自动保存到 testdata/fuzz/<TestName>/<hash>, CI 可复现
package search

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/zixiliuyue/go_es/internal/storage"
)

// newFuzzEngine 创建一个用于 fuzz 的内存 Engine, 预置 3 条文档
// 预置数据让 fuzz 输入可以命中真实倒排路径(而非空索引快速返回)
func newFuzzEngine() *Engine {
	store, _ := storage.Open("")
	e := New(store)
	// 预置 3 条文档, 覆盖 text / keyword / number / bool 字段
	e.IndexDoc("fuzz_idx", "1", map[string]interface{}{
		"title":   "hello world",
		"content": "the quick brown fox",
		"count":   json.Number("42"),
		"active":  true,
	})
	e.IndexDoc("fuzz_idx", "2", map[string]interface{}{
		"title":   "hello go",
		"content": "jumps over the lazy dog",
		"count":   json.Number("7"),
		"active":  false,
	})
	e.IndexDoc("fuzz_idx", "3", map[string]interface{}{
		"title":   "world peace",
		"content": "quick brown dog",
		"count":   json.Number("100"),
		"active":  true,
	})
	return e
}

// mustDecodeJSONUseNumber 模拟 server 层 decodeJSON 的行为(UseNumber)
// fuzz []byte → map[string]interface{}
func mustDecodeJSONUseNumber(t *testing.T, data []byte) map[string]interface{} {
	t.Helper()
	if len(data) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var m map[string]interface{}
	if err := dec.Decode(&m); err != nil {
		// 解析失败是正常 fuzz 输入, 返回 nil(不 panic 即通过)
		return nil
	}
	return m
}

// ================================================================
// P0: 纯字符串解析函数(无 I/O 依赖, 最快发现 panic)
// ================================================================

// FuzzParseQueryString: 对 Lucene 简化解析器做 fuzz
// 输入: 任意字符串 q + defaultField
// 预期: 不 panic(返回 clauses 或 error 都是合法的)
func FuzzParseQueryString(f *testing.F) {
	// 种子 corpus: 覆盖正常 / 空格 / +/- / 引号 / field:value / OR / 特殊字符
	seeds := []struct {
		q, field string
	}{
		{"hello", "title"},
		{"+hello -world", "title"},
		{"\"hello world\"", "title"},
		{"title:hello", "_all"},
		{"a,b:hello", "_all"},
		{"hello OR world", "title"},
		{"", "title"},
		{"+", ""},
		{"-", ""},
		{"\"unterminated", "_all"},
		{"field:\"value", "_all"},
		{"日本語テスト", "content"},
		{"a b c d e f g h i j k l m n o p", "title"},
		{"(((())))", "title"},
		{"+\"phrase one\" -\"phrase two\" field:value", "default"},
	}
	for _, s := range seeds {
		f.Add(s.q, s.field)
	}

	f.Fuzz(func(t *testing.T, q, defaultField string) {
		// 限制输入长度, 避免超长字符串导致测试超时
		if len(q) > 10000 {
			t.Skip("input too long, skip")
		}
		clauses, err := parseQueryString(q, defaultField)
		// err 非 nil 是合法的(如 "unterminated phrase"), 只要不 panic 即可
		_ = err
		_ = clauses
	})
}

// FuzzParseSimpleQueryString: 对 simple_query_string 解析器做 fuzz
// 输入: 任意字符串 q + defaultField
// 预期: 不 panic(此函数不返回 error)
func FuzzParseSimpleQueryString(f *testing.F) {
	seeds := []struct {
		q, field string
	}{
		{"hello world", "title"},
		{"+hello -world", "title"},
		{"hello|world", "title"},
		{"(hello AND world) OR test", "title"},
		{"field:value", "_all"},
		{"", "title"},
		{"日本語", "content"},
		{"a:b:c:d:e", "title"},
		{"*** +++ ---", "title"},
		{"\"quoted\" unquoted", "title"},
	}
	for _, s := range seeds {
		f.Add(s.q, s.field)
	}

	f.Fuzz(func(t *testing.T, q, defaultField string) {
		if len(q) > 10000 {
			t.Skip("input too long, skip")
		}
		clauses := parseSimpleQueryString(q, defaultField)
		_ = clauses
	})
}

// FuzzStripSQSReserved: 对 stripSQSReserved 做 fuzz
// 输入: 任意字符串
// 预期: 不 panic
func FuzzStripSQSReserved(f *testing.F) {
	seeds := []string{
		"hello world",
		"no special chars",
		"|<>(){}[]^~*?\\/",
		"",
		"日本語|<>test",
		"   \t\n  ",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, q string) {
		if len(q) > 10000 {
			t.Skip("input too long, skip")
		}
		result := stripSQSReserved(q)
		_ = result
	})
}

// ================================================================
// P1: Engine 方法, 接受任意 map (经 JSON 解码)
// ================================================================

// FuzzEvalMultiMatch: 对 evalMultiMatch 做 fuzz
// 输入: 任意 JSON bytes → map → evalMultiMatch
// 预期: 不 panic(返回结果或 error 均合法)
func FuzzEvalMultiMatch(f *testing.F) {
	e := newFuzzEngine()

	// 种子 corpus
	seeds := [][]byte{
		[]byte(`{"query":"hello","fields":["title"]}`),
		[]byte(`{"query":"hello","fields":["title","content"],"type":"best_fields"}`),
		[]byte(`{"query":"world","fields":["title"],"type":"phrase"}`),
		[]byte(`{"query":"","fields":[]}`),
		[]byte(`{}`),
		[]byte(`{"query":123}`),
		[]byte(`{"query":"hello","fields":"not_an_array"}`),
		[]byte(`{"query":"hello","type":"unknown_type"}`),
		[]byte(`{"query":null,"fields":null}`),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 10000 {
			t.Skip("input too long, skip")
		}
		spec := mustDecodeJSONUseNumber(t, data)
		if spec == nil {
			return
		}
		result, err := e.evalMultiMatch("fuzz_idx", spec)
		_ = err
		_ = result
	})
}

// FuzzEvalQueryString: 对 evalQueryString 做 fuzz
// 输入: 任意 JSON bytes → map → evalQueryString
// 预期: 不 panic
func FuzzEvalQueryString(f *testing.F) {
	e := newFuzzEngine()

	seeds := [][]byte{
		[]byte(`{"query":"hello","default_field":"title"}`),
		[]byte(`{"query":"+hello -world","default_field":"title"}`),
		[]byte(`{"query":"title:hello"}`),
		[]byte(`{"query":"\"hello world\""}`),
		[]byte(`{"query":""}`),
		[]byte(`{}`),
		[]byte(`{"query":null}`),
		[]byte(`{"query":123,"default_field":"title"}`),
		[]byte(`{"query":"hello OR world AND test"}`),
		[]byte(`{"query":"日本語テスト","default_field":"content"}`),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 10000 {
			t.Skip("input too long, skip")
		}
		spec := mustDecodeJSONUseNumber(t, data)
		if spec == nil {
			return
		}
		result, err := e.evalQueryString("fuzz_idx", spec)
		_ = err
		_ = result
	})
}

// FuzzEvalSimpleQueryString: 对 evalSimpleQueryString 做 fuzz
// 输入: 任意 JSON bytes → map → evalSimpleQueryString
// 预期: 不 panic
func FuzzEvalSimpleQueryString(f *testing.F) {
	e := newFuzzEngine()

	seeds := [][]byte{
		[]byte(`{"query":"hello world"}`),
		[]byte(`{"query":"+hello -world","default_field":"title"}`),
		[]byte(`{"query":"hello|world"}`),
		[]byte(`{"query":"(hello) AND (world)"}`),
		[]byte(`{"query":""}`),
		[]byte(`{}`),
		[]byte(`{"query":null}`),
		[]byte(`{"query":123}`),
		[]byte(`{"query":"*** +++ ---","fields":["title"]}`),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 10000 {
			t.Skip("input too long, skip")
		}
		spec := mustDecodeJSONUseNumber(t, data)
		if spec == nil {
			return
		}
		result, err := e.evalSimpleQueryString("fuzz_idx", spec)
		_ = err
		_ = result
	})
}

// ================================================================
// P2: Engine.Match 完整查询路径
// fuzz []byte → JSON → Query → Match(index, q)
// ================================================================

// FuzzEngineMatch: 对完整搜索路径做 fuzz
// 输入: 任意 JSON bytes → 构造 Query → Engine.Match
// 预期: 不 panic(返回结果或 error 均合法)
func FuzzEngineMatch(f *testing.F) {
	e := newFuzzEngine()

	seeds := [][]byte{
		// match
		[]byte(`{"match":{"title":"hello"}}`),
		[]byte(`{"match":{"title":{"query":"hello","operator":"and"}}}`),
		// match_phrase
		[]byte(`{"match_phrase":{"title":"hello world"}}`),
		// multi_match
		[]byte(`{"multi_match":{"query":"hello","fields":["title","content"]}}`),
		[]byte(`{"multi_match":{"query":"world","fields":["title"],"type":"phrase"}}`),
		// query_string
		[]byte(`{"query_string":{"query":"hello OR world","default_field":"title"}}`),
		[]byte(`{"query_string":{"query":"+hello -world"}}`),
		// simple_query_string
		[]byte(`{"simple_query_string":{"query":"hello world"}}`),
		// term
		[]byte(`{"term":{"active":true}}`),
		[]byte(`{"term":{"count":42}}`),
		// range
		[]byte(`{"range":{"count":{"gte":10,"lte":100}}}`),
		// bool
		[]byte(`{"bool":{"must":[{"match":{"title":"hello"}}],"filter":[{"term":{"active":true}}]}}`),
		[]byte(`{"bool":{"should":[{"match":{"title":"hello"}},{"match":{"title":"world"}}],"minimum_should_match":1}}`),
		// match_all
		[]byte(`{"match_all":{}}`),
		// 空 / 无效
		[]byte(`{}`),
		[]byte(`{"unknown_query_type":{"foo":"bar"}}`),
		[]byte(`{"match":null}`),
		[]byte(`{"match":{}}`),
		[]byte(`{"bool":{"must":"not_an_array"}}`),
		[]byte(`{"range":{"count":{"gte":"not_a_number"}}}`),
		[]byte(`{"term":{"count":{"value":"not_a_number"}}}`),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 10000 {
			t.Skip("input too long, skip")
		}
		// 模拟 server 层: JSON → map → parseQuery → Match
		raw := mustDecodeJSONUseNumber(t, data)
		if raw == nil {
			return
		}
		q := fuzzBuildQuery(raw)
		if q == nil {
			return
		}
		result, err := e.Match("fuzz_idx", q)
		_ = err
		_ = result
	})
}

// fuzzBuildQuery 复制 server.parseQuery 的逻辑(不 import server 包避免循环依赖)
// 将 map[string]interface{} 转为 *Query
func fuzzBuildQuery(raw map[string]interface{}) *Query {
	if raw == nil {
		return &Query{}
	}
	out := &Query{}
	if v, ok := raw["match"].(map[string]interface{}); ok {
		out.Match = v
	}
	if v, ok := raw["match_phrase"].(map[string]interface{}); ok {
		out.MatchPhrase = v
	}
	if v, ok := raw["multi_match"].(map[string]interface{}); ok {
		out.MultiMatch = v
	}
	if v, ok := raw["query_string"].(map[string]interface{}); ok {
		out.QueryString = v
	}
	if v, ok := raw["simple_query_string"].(map[string]interface{}); ok {
		out.SimpleQueryString = v
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
		bq := &BoolQuery{}
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
		if x, ok := v["minimum_should_match"].(json.Number); ok {
			if n, err := x.Int64(); err == nil {
				bq.MinimumShouldMatch = int(n)
			}
		}
		if x, ok := v["minimum_should_match"].(float64); ok {
			bq.MinimumShouldMatch = int(x)
		}
		out.Bool = bq
	}
	return out
}
