// Package server HTTP 端到端 benchmark 测试
//
// 覆盖:
//   - HTTP 搜索: match_all / match / range / multi_match / bool
//   - HTTP 写入: 单文档 PUT / _bulk 批量(100/500/1000)
//   - HTTP 综合: create index + 1k docs + 搜索
//
// 运行:
//
//	go test -bench=BenchmarkHTTP -benchmem -count=1 -benchtime=500ms ./internal/server/
package server

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ------------------------------------------------------------------
// 工具: 预置 N 条文档, 测试 server
// ------------------------------------------------------------------

// benchServer 封装测试服务器、已建立的 HTTP client(复用以减少连接开销) 与 URL
type benchServer struct {
	ts   *httptest.Server
	http *http.Client
}

// newBenchServer 创建一个 benchServer + 预置 N 条文档
func newBenchServer(b *testing.B, preloadDocs int) *benchServer {
	b.Helper()
	ts := newTestServer(b)
	hc := &http.Client{Transport: http.DefaultTransport} // 单例复用

	if preloadDocs > 0 {
		// 预置文档走 bulk
		var buf bytes.Buffer
		for i := 0; i < preloadDocs; i++ {
			title := fmt.Sprintf("title_%d hello world benchmark quick document", i%100)
			count := (i + 1) * 3 % 10000
			active := (i % 2) == 0
			fmt.Fprintf(&buf, `{"index":{"_index":"bench_idx","_id":"doc-%d"}}`+"\n", i)
			fmt.Fprintf(&buf, `{"title":%q,"count":%d,"active":%v}`+"\n", title, count, active)
		}
		resp, err := hc.Post(ts.URL+"/_bulk", "application/x-ndjson", &buf)
		if err != nil {
			b.Fatalf("preload bulk: %v", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b.Fatalf("preload bulk status=%d", resp.StatusCode)
		}
	}

	return &benchServer{ts: ts, http: hc}
}

// mustDoHTTP 执行 HTTP 请求, 吞掉 body, status != 200 就 Fatal
func (bs *benchServer) mustDoHTTP(b *testing.B, method, path string, body []byte, ct string) {
	b.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, _ := http.NewRequest(method, bs.ts.URL+path, rd)
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	resp, err := bs.http.Do(req)
	if err != nil {
		b.Fatalf("http %s %s: %v", method, path, err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		b.Fatalf("http %s %s: status=%d want 2xx/4xx", method, path, resp.StatusCode)
	}
}

// ------------------------------------------------------------------
// HTTP 搜索基准
// ------------------------------------------------------------------

// BenchmarkHTTPSearch_matchAll_1k: 1k 文档, GET /_search match_all
func BenchmarkHTTPSearch_matchAll_1k(b *testing.B) {
	bs := newBenchServer(b, 1000)
	body := []byte(`{"query":{"match_all":{}}}`)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		bs.mustDoHTTP(b, "POST", "/_search", body, "application/json")
	}
}

// BenchmarkHTTPSearch_matchAll_10k: 10k 文档, match_all
func BenchmarkHTTPSearch_matchAll_10k(b *testing.B) {
	bs := newBenchServer(b, 10000)
	body := []byte(`{"query":{"match_all":{}}}`)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		bs.mustDoHTTP(b, "POST", "/_search", body, "application/json")
	}
}

// BenchmarkHTTPSearch_match_hello_1k: match title=hello(selective)
func BenchmarkHTTPSearch_match_hello_1k(b *testing.B) {
	bs := newBenchServer(b, 1000)
	body := []byte(`{"query":{"match":{"title":"hello"}}}`)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		bs.mustDoHTTP(b, "POST", "/_search", body, "application/json")
	}
}

// BenchmarkHTTPSearch_match_hello_10k: 10k 文档, match
func BenchmarkHTTPSearch_match_hello_10k(b *testing.B) {
	bs := newBenchServer(b, 10000)
	body := []byte(`{"query":{"match":{"title":"hello"}}}`)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		bs.mustDoHTTP(b, "POST", "/_search", body, "application/json")
	}
}

// BenchmarkHTTPSearch_range_1k: range count 100..1000
func BenchmarkHTTPSearch_range_1k(b *testing.B) {
	bs := newBenchServer(b, 1000)
	body := []byte(`{"query":{"range":{"count":{"gte":100,"lte":1000}}}}`)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		bs.mustDoHTTP(b, "POST", "/_search", body, "application/json")
	}
}

// BenchmarkHTTPSearch_bool_1k: bool must + term
func BenchmarkHTTPSearch_bool_1k(b *testing.B) {
	bs := newBenchServer(b, 1000)
	body := []byte(`{"query":{"bool":{"must":[{"match":{"title":"hello"}}],"filter":[{"term":{"active":true}}]}}}`)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		bs.mustDoHTTP(b, "POST", "/_search", body, "application/json")
	}
}

// BenchmarkHTTPSearch_multiMatch_1k: multi_match best_fields
func BenchmarkHTTPSearch_multiMatch_1k(b *testing.B) {
	bs := newBenchServer(b, 1000)
	body := []byte(`{"query":{"multi_match":{"query":"hello world","fields":["title"],"type":"best_fields"}}}`)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		bs.mustDoHTTP(b, "POST", "/_search", body, "application/json")
	}
}

// BenchmarkHTTPSearch_clusterHealth: 轻量 GET /_cluster/health
// 作为"最小开销"对比基线
func BenchmarkHTTPSearch_clusterHealth(b *testing.B) {
	bs := newBenchServer(b, 0)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		bs.mustDoHTTP(b, "GET", "/_cluster/health", nil, "")
	}
}

// ------------------------------------------------------------------
// HTTP 写入基准
// ------------------------------------------------------------------

// BenchmarkHTTPIndexDoc_single: 单文档 PUT /{index}/_doc/{id}
// 每次迭代写入一个新文档, 用于测量单 doc HTTP 写路径开销
func BenchmarkHTTPIndexDoc_single(b *testing.B) {
	bs := newBenchServer(b, 0)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		id := fmt.Sprintf("bench-%d", n)
		body := fmt.Sprintf(`{"title":"hello_%d","count":%d,"active":true}`, n, n)
		path := fmt.Sprintf("/bench_single_idx/_doc/%s", id)
		bs.mustDoHTTP(b, "PUT", path, []byte(body), "application/json")
	}
}

// benchmarkHTTPBulk_N: 每迭代发送一个含 N 条文档的 _bulk 请求
func benchmarkHTTPBulk_N(b *testing.B, N int) {
	bs := newBenchServer(b, 0)
	b.ReportAllocs()

	// 构造一个包含 N 条 index 操作的 NDJSON body (b.StopTimer 构造)
	b.StopTimer()
	var batchBuf bytes.Buffer
	for i := 0; i < N; i++ {
		docID := fmt.Sprintf("bulk-%d", i)
		title := fmt.Sprintf("bulk_title_%d", i)
		fmt.Fprintf(&batchBuf, `{"index":{"_index":"bench_bulk_%d","_id":%q}}`+"\n", N, docID)
		fmt.Fprintf(&batchBuf, `{"title":%q,"count":%d,"active":true}`+"\n", title, i)
	}
	batchBody := batchBuf.Bytes()
	b.StartTimer()

	// 每次迭代发一个 N 条的批量
	for n := 0; n < b.N; n++ {
		bs.mustDoHTTP(b, "POST", "/_bulk", batchBody, "application/x-ndjson")
	}
}

// BenchmarkHTTPBulk_100: 每次批量 100 条
func BenchmarkHTTPBulk_100(b *testing.B)  { benchmarkHTTPBulk_N(b, 100) }

// BenchmarkHTTPBulk_500: 每次批量 500 条
func BenchmarkHTTPBulk_500(b *testing.B)  { benchmarkHTTPBulk_N(b, 500) }

// BenchmarkHTTPBulk_1000: 每次批量 1,000 条
func BenchmarkHTTPBulk_1000(b *testing.B) { benchmarkHTTPBulk_N(b, 1000) }

// BenchmarkHTTPBulk_5000: 每次批量 5,000 条
func BenchmarkHTTPBulk_5000(b *testing.B) { benchmarkHTTPBulk_N(b, 5000) }

// ------------------------------------------------------------------
// HTTP 综合基准: 真实场景
// ------------------------------------------------------------------

// BenchmarkHTTPEndToEnd_WriteAndSearch_1k: 每次迭代
//   1) bulk 写 1k 文档 → 2) match_all 搜索 → 3) range 搜索
// 用于模拟"写+读"混合场景
func BenchmarkHTTPEndToEnd_WriteAndSearch_1k(b *testing.B) {
	b.ReportAllocs()

	for n := 0; n < b.N; n++ {
		b.StopTimer()
		bs := newBenchServer(b, 0)
		// 构造 1k 批量
		var buf bytes.Buffer
		for i := 0; i < 1000; i++ {
			fmt.Fprintf(&buf, `{"index":{"_index":"e2e_idx","_id":"doc-%d"}}`+"\n", i)
			fmt.Fprintf(&buf, `{"title":"hello_%d","count":%d,"active":true}`+"\n", i, i%1000)
		}
		bulkBody := buf.Bytes()
		b.StartTimer()

		bs.mustDoHTTP(b, "POST", "/_bulk", bulkBody, "application/x-ndjson")
		bs.mustDoHTTP(b, "POST", "/e2e_idx/_search", []byte(`{"query":{"match_all":{}}}`), "application/json")
		bs.mustDoHTTP(b, "POST", "/e2e_idx/_search", []byte(`{"query":{"range":{"count":{"gte":100,"lte":900}}}}`), "application/json")
	}
}

// BenchmarkHTTPCreateIndex: 每次迭代创建并设置 1 个新索引
func BenchmarkHTTPCreateIndex(b *testing.B) {
	bs := newBenchServer(b, 0)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		path := fmt.Sprintf("/bench_create_idx_%d", n)
		bs.mustDoHTTP(b, "PUT", path, []byte(`{}`), "application/json")
	}
}

// 用于避免 unused 报错 (strings 包未来可能扩展使用)
var _ = strings.Contains
