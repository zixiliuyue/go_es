// Package search benchmark 测试
//
// 为核心搜索路径建立性能基线, 用于检测 PR 的性能回归。
//
// 数据规模:
//   - 小型(N=1,000 条文档): 用于每次提交快速 smoke(≈ 几秒钟跑完)
//   - 中型(N=10,000 条文档): 用于 CI 基线对比(benchstat 推荐 ≥ 10 次采样比较)
//   - 大型(N=100,000 条文档): 用于 release 前跑的最终验证(> 10s 可能, 不默认跑)
//
// 运行方式:
//
//	# 快速 smoke: 跑所有 1k 基准, 1 次采样, 不打印内存分配
//	go test -bench=Benchmark.*_1k -benchmem -count=1 ./internal/search/
//
//	# 基线收集(用于 benchstat 对比): 5 次采样, 输出到文件
//	go test -bench=Benchmark -benchmem -count=5 ./internal/search/ > bench/search.old.txt
//	# ... 做改动 ...
//	go test -bench=Benchmark -benchmem -count=5 ./internal/search/ > bench/search.new.txt
//	# 对比:
//	benchstat bench/search.old.txt bench/search.new.txt
//
// 覆盖路径:
//   - IndexDoc_1k/10k/100k: 写路径 + 倒排构建
//   - Match_match*_1k/10k/100k: match 单字段查询
//   - Match_matchPhrase_1k: match_phrase 短语查询
//   - Match_range_1k: range 查询(走 sorted_index 加速路径)
//   - Match_bool_1k: bool must + filter + should 查询
//   - Match_multiMatch_1k: multi_match best_fields
//   - Match_queryString_1k: query_string Lucene 语法解析
//   - Match_simpleQueryString_1k: simple_query_string
//   - Match_matchAll_1k: match_all(走 allDocsSet 快速路径)
package search

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"encoding/json"

	"github.com/zixiliuyue/go_es/internal/storage"
)

// ------------------------------------------------------------------
// fixture 工厂
// ------------------------------------------------------------------

// buildEngine 返回预置 N 条文档的内存 Engine + 预置 query 列表
// 文档字段: title(text), content(text), count(long), active(boolean)
// 为保证 benchmark 稳定, N 条文档内容固定生成(可复现)
func buildEngine(b *testing.B, N int) (*Engine, []*Query) {
	b.Helper()
	store, err := storage.Open("")
	if err != nil {
		b.Fatalf("storage.Open: %v", err)
	}
	// 注意: benchmark 结束后 store.Close() 不应由 Engine 负责(避免泄漏)
	e := New(store)
	b.Cleanup(func() { _ = store.Close() })

	// 预置文档
	for i := 0; i < N; i++ {
		title := fmt.Sprintf("title_%d hello world search elasticsearch benchmark", i%100)
		content := fmt.Sprintf("content_%d the quick brown fox jumps over the lazy dog document_%d", i%50, i)
		count := json.Number(fmt.Sprintf("%d", (i+1)*3%10000))
		active := (i % 2) == 0
		e.IndexDoc("bench_idx", fmt.Sprintf("doc-%d", i), map[string]interface{}{
			"title":   title,
			"content": content,
			"count":   count,
			"active":  active,
		})
	}

	// 预置常见查询, 避免每个 b.N 迭代重复构造
	qs := []*Query{
		// 0: match_all
		{MatchAll: map[string]interface{}{}},
		// 1: match title = hello
		{Match: map[string]interface{}{"title": "hello"}},
		// 2: match title = world (高基数字典)
		{Match: map[string]interface{}{"title": "world"}},
		// 3: match_phrase title = hello world
		{MatchPhrase: map[string]interface{}{"title": "hello world"}},
		// 4: range count 100 ≤ x ≤ 1000
		{Range: map[string]interface{}{"count": map[string]interface{}{"gte": json.Number("100"), "lte": json.Number("1000")}}},
		// 5: bool must + filter
		{Bool: &BoolQuery{
			Must: []map[string]interface{}{
				{"match": map[string]interface{}{"title": "hello"}},
			},
			Filter: []map[string]interface{}{
				{"term": map[string]interface{}{"active": true}},
			},
		}},
		// 6: multi_match best_fields
		{MultiMatch: map[string]interface{}{
			"query":  "hello world",
			"fields": []interface{}{"title", "content"},
			"type":   "best_fields",
		}},
		// 7: query_string +hello -world
		{QueryString: map[string]interface{}{
			"query":         "+hello -world",
			"default_field": "title",
		}},
		// 8: simple_query_string
		{SimpleQueryString: map[string]interface{}{
			"query":         "hello world",
			"default_field": "title",
		}},
	}
	return e, qs
}

// 用于保证 Benchmark 不被编译器优化掉(把结果写到"黑洞")
var sinkResults []string
var sinkErr error
var sinkOnce sync.Once

// ------------------------------------------------------------------
// 写路径: IndexDoc
// ------------------------------------------------------------------

// BenchmarkIndexDoc_1k: 写 1,000 条文档, 同时重建倒排
func BenchmarkIndexDoc_1k(b *testing.B) { benchmarkIndexDoc(b, 1000) }

// BenchmarkIndexDoc_10k: 写 10,000 条文档
func BenchmarkIndexDoc_10k(b *testing.B) { benchmarkIndexDoc(b, 10000) }

// BenchmarkIndexDoc_100k: 写 100,000 条文档(release 前跑)
func BenchmarkIndexDoc_100k(b *testing.B) { benchmarkIndexDoc(b, 100000) }

func benchmarkIndexDoc(b *testing.B, N int) {
	b.StopTimer()
	for n := 0; n < b.N; n++ {
		// 每个迭代都新建 engine + store, 避免写入累积
		store, err := storage.Open("")
		if err != nil {
			b.Fatalf("open store: %v", err)
		}
		e := New(store)
		b.StartTimer()
		for i := 0; i < N; i++ {
			e.IndexDoc("bench_idx", fmt.Sprintf("doc-%d", i), map[string]interface{}{
				"title":   fmt.Sprintf("hello world document_%d", i),
				"content": fmt.Sprintf("quick brown fox document_%d", i),
				"count":   json.Number(fmt.Sprintf("%d", i)),
				"active":  (i % 2) == 0,
			})
		}
		b.StopTimer()
		_ = store.Close()
	}
}

// ------------------------------------------------------------------
// 读路径: Engine.Match 各类查询 × 数据集规模
// ------------------------------------------------------------------

// --- match_all (走 allDocsSet 快速路径) ---
func BenchmarkMatch_matchAll_1k(b *testing.B)   { benchmarkMatch(b, 1000, 0) }
func BenchmarkMatch_matchAll_10k(b *testing.B)  { benchmarkMatch(b, 10000, 0) }
func BenchmarkMatch_matchAll_100k(b *testing.B) { benchmarkMatch(b, 100000, 0) }

// --- match: 低基数字典("hello" 1%) ---
func BenchmarkMatch_match_lowCardinality_1k(b *testing.B)   { benchmarkMatch(b, 1000, 1) }
func BenchmarkMatch_match_lowCardinality_10k(b *testing.B)  { benchmarkMatch(b, 10000, 1) }
func BenchmarkMatch_match_lowCardinality_100k(b *testing.B) { benchmarkMatch(b, 100000, 1) }

// --- match: 高基数字典("world" 1%) ---
func BenchmarkMatch_match_highCardinality_1k(b *testing.B)   { benchmarkMatch(b, 1000, 2) }
func BenchmarkMatch_match_highCardinality_10k(b *testing.B)  { benchmarkMatch(b, 10000, 2) }
func BenchmarkMatch_match_highCardinality_100k(b *testing.B) { benchmarkMatch(b, 100000, 2) }

// --- match_phrase ---
func BenchmarkMatch_matchPhrase_1k(b *testing.B)   { benchmarkMatch(b, 1000, 3) }
func BenchmarkMatch_matchPhrase_10k(b *testing.B)  { benchmarkMatch(b, 10000, 3) }
func BenchmarkMatch_matchPhrase_100k(b *testing.B) { benchmarkMatch(b, 100000, 3) }

// --- range 查询 ---
func BenchmarkMatch_range_1k(b *testing.B)   { benchmarkMatch(b, 1000, 4) }
func BenchmarkMatch_range_10k(b *testing.B)  { benchmarkMatch(b, 10000, 4) }
func BenchmarkMatch_range_100k(b *testing.B) { benchmarkMatch(b, 100000, 4) }

// --- bool must + filter ---
func BenchmarkMatch_bool_1k(b *testing.B)   { benchmarkMatch(b, 1000, 5) }
func BenchmarkMatch_bool_10k(b *testing.B)  { benchmarkMatch(b, 10000, 5) }
func BenchmarkMatch_bool_100k(b *testing.B) { benchmarkMatch(b, 100000, 5) }

// --- multi_match ---
func BenchmarkMatch_multiMatch_1k(b *testing.B)   { benchmarkMatch(b, 1000, 6) }
func BenchmarkMatch_multiMatch_10k(b *testing.B)  { benchmarkMatch(b, 10000, 6) }
func BenchmarkMatch_multiMatch_100k(b *testing.B) { benchmarkMatch(b, 100000, 6) }

// --- query_string ---
func BenchmarkMatch_queryString_1k(b *testing.B)   { benchmarkMatch(b, 1000, 7) }
func BenchmarkMatch_queryString_10k(b *testing.B)  { benchmarkMatch(b, 10000, 7) }
func BenchmarkMatch_queryString_100k(b *testing.B) { benchmarkMatch(b, 100000, 7) }

// --- simple_query_string ---
func BenchmarkMatch_simpleQueryString_1k(b *testing.B)   { benchmarkMatch(b, 1000, 8) }
func BenchmarkMatch_simpleQueryString_10k(b *testing.B)  { benchmarkMatch(b, 10000, 8) }
func BenchmarkMatch_simpleQueryString_100k(b *testing.B) { benchmarkMatch(b, 100000, 8) }

// benchmarkMatch 是所有 Engine.Match 基准的通用实现
//
// 说明:
//   - 预置 Engine + 查询在 b.StopTimer() 外完成, 不计入基准时间
//   - 每个 b.N 迭代都调用一次 e.Match("bench_idx", q)
//   - 结果写入 sinkResults 防止编译器 DCE 优化掉调用
func benchmarkMatch(b *testing.B, N int, queryIdx int) {
	b.StopTimer()
	e, qs := buildEngine(b, N)
	if queryIdx >= len(qs) {
		b.Fatalf("queryIdx=%d out of range", queryIdx)
	}
	q := qs[queryIdx]
	b.ReportAllocs() // 每次迭代都报告堆分配
	b.StartTimer()

	for n := 0; n < b.N; n++ {
		r, err := e.Match("bench_idx", q)
		// 写入全局 sink 防止被优化掉
		sinkResults = r
		sinkErr = err
	}
}

// ------------------------------------------------------------------
// 冷启动: LoadAll 快路径(snapshot) vs 慢路径(doc-tf) (#7)
// ------------------------------------------------------------------

// prepareColdStartStore 准备一个磁盘 store, 写入 N 条 doc + flush snapshot
// 返回 store 路径 (benchmark 期间复用同一份磁盘数据)
func prepareColdStartStore(b *testing.B, N int) string {
	b.Helper()
	dir := b.TempDir()
	store, err := storage.Open(filepath.Join(dir, "data"))
	if err != nil {
		b.Fatalf("open store: %v", err)
	}
	e := New(store)
	for i := 0; i < N; i++ {
		title := fmt.Sprintf("title_%d hello world search elasticsearch benchmark", i%100)
		content := fmt.Sprintf("content_%d the quick brown fox jumps over the lazy dog document_%d", i%50, i)
		count := json.Number(fmt.Sprintf("%d", (i+1)*3%10000))
		active := (i % 2) == 0
		// 写 doc/ + doc-tf/ + 内存倒排
		_ = store.Put(storage.DocKey("bench_idx", fmt.Sprintf("doc-%d", i)), map[string]interface{}{
			"title":   title,
			"content": content,
			"count":   count,
			"active":  active,
		})
		e.IndexDoc("bench_idx", fmt.Sprintf("doc-%d", i), map[string]interface{}{
			"title":   title,
			"content": content,
			"count":   count,
			"active":  active,
		})
	}
	// flush postings snapshot
	_, _, err = e.FlushPostingsSnapshot("bench_idx")
	if err != nil {
		b.Fatalf("flush snapshot: %v", err)
	}
	_ = store.Close()
	return filepath.Join(dir, "data")
}

// BenchmarkColdStart_Snapshot_1k LoadAll 走 snapshot 快路径
func BenchmarkColdStart_Snapshot_1k(b *testing.B)  { benchmarkColdStartSnapshot(b, 1000) }
func BenchmarkColdStart_Snapshot_10k(b *testing.B) { benchmarkColdStartSnapshot(b, 10000) }

func benchmarkColdStartSnapshot(b *testing.B, N int) {
	b.StopTimer()
	dataDir := prepareColdStartStore(b, N)
	b.ResetTimer()
	b.ReportAllocs()
	for n := 0; n < b.N; n++ {
		store, err := storage.Open(dataDir)
		if err != nil {
			b.Fatalf("open store: %v", err)
		}
		e := New(store)
		b.StartTimer()
		sinkErr = e.LoadAll()
		b.StopTimer()
		_ = store.Close()
	}
}

// BenchmarkColdStart_DocTF_1k LoadAll 走 doc-tf 慢路径 (snapshot 失效后)
func BenchmarkColdStart_DocTF_1k(b *testing.B)  { benchmarkColdStartDocTF(b, 1000) }
func BenchmarkColdStart_DocTF_10k(b *testing.B) { benchmarkColdStartDocTF(b, 10000) }

func benchmarkColdStartDocTF(b *testing.B, N int) {
	b.StopTimer()
	dataDir := prepareColdStartStore(b, N)
	// 先失效 snapshot, 强制走 doc-tf 慢路径
	store, err := storage.Open(dataDir)
	if err != nil {
		b.Fatalf("open store: %v", err)
	}
	e := New(store)
	_ = e.InvalidatePostingsSnapshot("bench_idx")
	_ = store.Close()
	b.ResetTimer()
	b.ReportAllocs()
	for n := 0; n < b.N; n++ {
		store, err := storage.Open(dataDir)
		if err != nil {
			b.Fatalf("open store: %v", err)
		}
		e := New(store)
		b.StartTimer()
		sinkErr = e.LoadAll()
		b.StopTimer()
		_ = store.Close()
	}
}
