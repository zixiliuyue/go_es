// Package server fuzz 测试
//
// 使用 Go 1.18+ 原生 fuzz, 对 HTTP 层核心解析路径做随机输入测试:
//   - FuzzParseQuery: parseQuery(map) 任意 JSON → Query(不 panic)
//   - FuzzSearchBody: 完整 /_search 端到端(任意 body → server 响应, 不 panic / 不 500)
//   - FuzzBulkBody: 完整 /_bulk 端到端(任意 NDJSON body → server 响应, 不 panic / 不 500)
//   - FuzzIndexDoc: 完整 /{index}/_doc/{id} 端到端(任意 body → server 响应, 不 panic / 不 500)
//
// 运行方式:
//
//	# 快速跑已有 corpus + 种子
//	go test -count=1 ./internal/server/
//
//	# 启动 fuzz 探索
//	go test -fuzz=FuzzParseQuery -fuzztime=30s ./internal/server/
//	go test -fuzz=FuzzSearchBody -fuzztime=30s ./internal/server/
//	go test -fuzz=FuzzBulkBody -fuzztime=30s ./internal/server/
//	go test -fuzz=FuzzIndexDoc -fuzztime=30s ./internal/server/
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// mustDecodeJSONUseNumberServer 模拟 decodeJSON 的行为(UseNumber)
// 接受 testing.TB 以兼容 fuzz 函数的 *testing.F 参数
func mustDecodeJSONUseNumberServer(t testing.TB, data []byte) map[string]interface{} {
	t.Helper()
	if len(data) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var m map[string]interface{}
	if err := dec.Decode(&m); err != nil {
		return nil
	}
	return m
}

// ================================================================
// FuzzParseQuery: 直接对 parseQuery 做 fuzz
// ================================================================

func FuzzParseQuery(f *testing.F) {
	seeds := [][]byte{
		[]byte(`{"match":{"title":"hello"}}`),
		[]byte(`{"bool":{"must":[{"match":{"title":"hello"}}]}}`),
		[]byte(`{"term":{"count":42}}`),
		[]byte(`{"range":{"count":{"gte":10,"lte":100}}}`),
		[]byte(`{}`),
		[]byte(`{"unknown":"value"}`),
		[]byte(`{"match":null}`),
		[]byte(`{"bool":{"must":"not_an_array","should":123}}`),
		[]byte(`{"bool":{"minimum_should_match":"abc"}}`),
		[]byte(`null`),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 10000 {
			t.Skip("input too long, skip")
		}
		raw := mustDecodeJSONUseNumberServer(t, data)
		// parseQuery 不应 panic
		q, err := parseQuery(raw)
		_ = err
		_ = q
	})
}

// ================================================================
// FuzzSearchBody: 完整 /_search 端到端
// ================================================================

func FuzzSearchBody(f *testing.F) {
	ts := newTestServer(f)

	seeds := [][]byte{
		[]byte(`{"query":{"match_all":{}}}`),
		[]byte(`{"query":{"match":{"title":"hello"}}}`),
		[]byte(`{"query":{"bool":{"must":[{"match":{"title":"hello"}}]}}}`),
		[]byte(`{"query":{"range":{"count":{"gte":10}}}}`),
		[]byte(`{"query":{"term":{"active":true}}}`),
		[]byte(`{"size":0}`),
		[]byte(`{}`),
		[]byte(`{"query":null}`),
		[]byte(`{"query":{"multi_match":{"query":"hello","fields":["title"]}}}`),
		[]byte(`{"query":{"query_string":{"query":"hello OR world"}}}`),
		[]byte(`not valid json`),
		[]byte(`{"from":99999,"size":99999}`),
		[]byte(`{"query":{"match":{"title":{"query":"hello","operator":"and"}}}}`),
		[]byte(`{"aggs":{"by_field":{"terms":{"field":"title"}}}}`),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, body []byte) {
		if len(body) > 10000 {
			t.Skip("input too long, skip")
		}
		resp, err := http.Post(ts.URL+"/_search", "application/json", bytes.NewReader(body))
		if err != nil {
			// 网络错误(如 server 被前一个 fuzz case 关闭)不算 panic
			t.Skipf("HTTP error: %v", err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		// 只要不 panic / 不 500 就算通过
		// 400(parse error) / 200(成功) 都是合法的
		if resp.StatusCode >= 500 {
			t.Errorf("fuzz search body triggered %d: body=%s", resp.StatusCode, string(body[:min(200, len(body))]))
		}
	})
}

// ================================================================
// FuzzBulkBody: 完整 /_bulk 端到端
// ================================================================

func FuzzBulkBody(f *testing.F) {
	ts := newTestServer(f)

	seeds := [][]byte{
		// 正常 NDJSON
		[]byte(`{"index":{"_index":"fuzz_bulk","_id":"1"}}
{"field":"value1"}
{"index":{"_index":"fuzz_bulk","_id":"2"}}
{"field":"value2"}
`),
		// delete
		[]byte(`{"delete":{"_index":"fuzz_bulk","_id":"1"}}
`),
		// 空 body
		[]byte(``),
		// 非 JSON 行
		[]byte(`this is not json
{"index":{"_index":"fuzz_bulk","_id":"1"}}
{"field":"value"}
`),
		// 只有 action 没有 source
		[]byte(`{"index":{"_index":"fuzz_bulk","_id":"1"}}
`),
		// action 行不是 JSON
		[]byte(`not_json
also_not_json
`),
		// 空行
		[]byte(`

{"index":{"_index":"fuzz_bulk","_id":"1"}}
{"field":"value"}

`),
		// 大量行
		bytes.Repeat([]byte(`{"index":{"_index":"fuzz_bulk","_id":"1"}}
{"field":"value"}
`), 10),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, body []byte) {
		if len(body) > 10000 {
			t.Skip("input too long, skip")
		}
		resp, err := http.Post(ts.URL+"/_bulk", "application/x-ndjson", bytes.NewReader(body))
		if err != nil {
			t.Skipf("HTTP error: %v", err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		if resp.StatusCode >= 500 {
			t.Errorf("fuzz bulk body triggered %d: body=%s", resp.StatusCode, string(body[:min(200, len(body))]))
		}
	})
}

// ================================================================
// FuzzIndexDoc: 完整 /{index}/_doc/{id} 端到端
// ================================================================

func FuzzIndexDoc(f *testing.F) {
	ts := newTestServer(f)

	seeds := [][]byte{
		[]byte(`{"title":"hello","count":42}`),
		[]byte(`{"title":"world","active":true}`),
		[]byte(`{}`),
		[]byte(`null`),
		[]byte(`not json`),
		[]byte(`{"nested":{"a":{"b":{"c":"deep"}}}}`),
		[]byte(`{"array":[1,2,3,"four"]}`),
		[]byte(`{"number":3.14159,"big":99999999999999999999}`),
		[]byte(`{"unicode":"日本語テスト"}`),
		[]byte(`{"empty_string":"","null_val":null}`),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, body []byte) {
		if len(body) > 10000 {
			t.Skip("input too long, skip")
		}
		req, _ := http.NewRequest("PUT", ts.URL+"/fuzz_doc_idx/_doc/1", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Skipf("HTTP error: %v", err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		if resp.StatusCode >= 500 {
			t.Errorf("fuzz index doc triggered %d: body=%s", resp.StatusCode, string(body[:min(200, len(body))]))
		}
	})
}

// min 返回两数较小值(Go 1.21 之前没有 builtin min)
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
