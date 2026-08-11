// #11 搜索结果评分缓存集成测试
// 覆盖: 缓存命中、写入失效、MaxSize 限制、并发访问、LRU 淘汰
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zixiliuyue/go_es/internal/search"
	"github.com/zixiliuyue/go_es/internal/storage"
	"go.uber.org/zap"
)

// newTestServerWithCache 启动带缓存的测试服务
func newTestServerWithCache(t *testing.T, cfg SearchCacheConfig) (*httptest.Server, *Server) {
	t.Helper()
	store, err := storage.Open("")
	require.NoError(t, err)
	engine := search.New(store)
	srv := NewWithOptions(store, engine, zap.NewNop(), ServerOptions{
		SearchCache: cfg,
	})
	srv.MarkStartupDone()
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = store.Close()
	})
	return ts, srv
}

// postSearch 发送搜索请求并返回响应 + 响应体
func postSearch(t *testing.T, ts *httptest.Server, index string, query interface{}) (*http.Response, []byte) {
	t.Helper()
	b, err := json.Marshal(query)
	require.NoError(t, err)
	resp, err := http.Post(ts.URL+"/"+index+"/_search", "application/json", bytes.NewReader(b))
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, body
}

// TestSearchCache_HitAndMiss 验证相同 query 二次调用命中缓存
func TestSearchCache_HitAndMiss(t *testing.T) {
	ts, srv := newTestServerWithCache(t, SearchCacheConfig{Enabled: true, Capacity: 100, MaxSize: 1 << 20})

	// 建索引 + 写数据
	do(t, ts, "PUT", "/articles", nil)
	for i := 0; i < 5; i++ {
		do(t, ts, "PUT", "/articles/_doc/d"+strconv.Itoa(i), map[string]interface{}{"title": "hello world", "v": i})
	}

	query := map[string]interface{}{"query": map[string]interface{}{"match_all": map[string]interface{}{}}}

	// 首次搜索
	resp1, body1 := postSearch(t, ts, "articles", query)
	assert.Equal(t, http.StatusOK, resp1.StatusCode)
	assert.Equal(t, "", resp1.Header.Get("X-GoES-Cache"), "first call should not be HIT")
	assert.NotEmpty(t, body1)

	// 二次相同搜索, 应命中缓存
	resp2, body2 := postSearch(t, ts, "articles", query)
	assert.Equal(t, http.StatusOK, resp2.StatusCode)
	assert.Equal(t, "HIT", resp2.Header.Get("X-GoES-Cache"))
	assert.NotEmpty(t, body2)

	// 验证缓存状态
	stats := srv.searchCache.Stats()
	assert.True(t, stats.Hits >= 1, "hits should be >= 1, got %d", stats.Hits)
	assert.True(t, stats.Misses >= 1, "misses should be >= 1, got %d", stats.Misses)
}

// TestSearchCache_InvalidationOnWrite 验证写入后缓存失效
func TestSearchCache_InvalidationOnWrite(t *testing.T) {
	ts, _ := newTestServerWithCache(t, SearchCacheConfig{Enabled: true, Capacity: 100, MaxSize: 1 << 20})

	do(t, ts, "PUT", "/idx", nil)
	for i := 0; i < 3; i++ {
		do(t, ts, "PUT", "/idx/_doc/d"+strconv.Itoa(i), map[string]interface{}{"v": i})
	}

	query := map[string]interface{}{"query": map[string]interface{}{"match_all": map[string]interface{}{}}}

	// 首次搜索 (miss)
	resp1, _ := postSearch(t, ts, "idx", query)
	assert.Equal(t, "", resp1.Header.Get("X-GoES-Cache"))

	// 二次搜索 (hit)
	resp2, _ := postSearch(t, ts, "idx", query)
	assert.Equal(t, "HIT", resp2.Header.Get("X-GoES-Cache"))

	// 写入新文档, 应触发失效
	do(t, ts, "PUT", "/idx/_doc/d3", map[string]interface{}{"v": 3})

	// 再次搜索应 miss (因为缓存已失效)
	resp3, body3 := postSearch(t, ts, "idx", query)
	assert.Equal(t, http.StatusOK, resp3.StatusCode)
	assert.NotEqual(t, "HIT", resp3.Header.Get("X-GoES-Cache"), "cache should be invalidated after write")

	// 验证新文档被搜到
	var res map[string]interface{}
	require.NoError(t, json.Unmarshal(body3, &res))
	hits := res["hits"].(map[string]interface{})["total"].(map[string]interface{})["value"].(float64)
	assert.Equal(t, float64(4), hits, "should see 4 docs after write")
}

// TestSearchCache_InvalidationOnDelete 验证删除后缓存失效
func TestSearchCache_InvalidationOnDelete(t *testing.T) {
	ts, _ := newTestServerWithCache(t, SearchCacheConfig{Enabled: true, Capacity: 100, MaxSize: 1 << 20})

	do(t, ts, "PUT", "/idx", nil)
	do(t, ts, "PUT", "/idx/_doc/d1", map[string]interface{}{"v": 1})

	query := map[string]interface{}{"query": map[string]interface{}{"match_all": map[string]interface{}{}}}

	// 预热
	postSearch(t, ts, "idx", query)

	// 确保命中
	resp, _ := postSearch(t, ts, "idx", query)
	assert.Equal(t, "HIT", resp.Header.Get("X-GoES-Cache"))

	// 删除文档
	do(t, ts, "DELETE", "/idx/_doc/d1", nil)

	// 应 miss
	resp2, _ := postSearch(t, ts, "idx", query)
	assert.NotEqual(t, "HIT", resp2.Header.Get("X-GoES-Cache"))
}

// TestSearchCache_Disabled 验证缓存未启用时不生效
func TestSearchCache_Disabled(t *testing.T) {
	ts, _ := newTestServerWithCache(t, SearchCacheConfig{Enabled: false})

	do(t, ts, "PUT", "/idx", nil)
	do(t, ts, "PUT", "/idx/_doc/d1", map[string]interface{}{"v": 1})

	query := map[string]interface{}{"query": map[string]interface{}{"match_all": map[string]interface{}{}}}

	resp1, _ := postSearch(t, ts, "idx", query)
	assert.NotEqual(t, "HIT", resp1.Header.Get("X-GoES-Cache"))

	// 二次也不应命中
	resp2, _ := postSearch(t, ts, "idx", query)
	assert.NotEqual(t, "HIT", resp2.Header.Get("X-GoES-Cache"))
}

// TestSearchCache_MaxSize 验证超过 MaxSize 的响应不缓存
func TestSearchCache_MaxSize(t *testing.T) {
	// MaxSize 极小, 触发跳过缓存
	ts, _ := newTestServerWithCache(t, SearchCacheConfig{Enabled: true, Capacity: 100, MaxSize: 10})

	do(t, ts, "PUT", "/idx", nil)
	for i := 0; i < 3; i++ {
		do(t, ts, "PUT", "/idx/_doc/d"+strconv.Itoa(i), map[string]interface{}{"content": "lorem ipsum dolor sit amet consectetur adipiscing"})
	}

	query := map[string]interface{}{"query": map[string]interface{}{"match_all": map[string]interface{}{}}}

	resp1, _ := postSearch(t, ts, "idx", query)
	assert.Equal(t, http.StatusOK, resp1.StatusCode)

	// 二次不应命中 (因为响应超过 10 字节没缓存)
	resp2, _ := postSearch(t, ts, "idx", query)
	assert.NotEqual(t, "HIT", resp2.Header.Get("X-GoES-Cache"), "response larger than MaxSize should not be cached")
}

// TestSearchCache_DifferentQueries 验证不同 query 生成不同 cache key
func TestSearchCache_DifferentQueries(t *testing.T) {
	ts, _ := newTestServerWithCache(t, SearchCacheConfig{Enabled: true, Capacity: 100, MaxSize: 1 << 20})

	do(t, ts, "PUT", "/idx", nil)
	do(t, ts, "PUT", "/idx/_doc/d1", map[string]interface{}{"v": 1, "t": "hello"})
	do(t, ts, "PUT", "/idx/_doc/d2", map[string]interface{}{"v": 2, "t": "world"})

	matchAll := map[string]interface{}{"query": map[string]interface{}{"match_all": map[string]interface{}{}}}
	matchHello := map[string]interface{}{"query": map[string]interface{}{"match": map[string]interface{}{"t": "hello"}}}

	// match_all 查询
	resp1, _ := postSearch(t, ts, "idx", matchAll)
	assert.Equal(t, http.StatusOK, resp1.StatusCode)

	// match_all 第二次应命中
	resp2, _ := postSearch(t, ts, "idx", matchAll)
	assert.Equal(t, "HIT", resp2.Header.Get("X-GoES-Cache"))

	// 不同的 match 查询不应命中
	resp3, _ := postSearch(t, ts, "idx", matchHello)
	assert.Equal(t, http.StatusOK, resp3.StatusCode)
	assert.NotEqual(t, "HIT", resp3.Header.Get("X-GoES-Cache"))
}

// TestSearchCache_ConcurrentAccess 验证并发访问缓存安全
func TestSearchCache_ConcurrentAccess(t *testing.T) {
	ts, srv := newTestServerWithCache(t, SearchCacheConfig{Enabled: true, Capacity: 100, MaxSize: 1 << 20})

	do(t, ts, "PUT", "/idx", nil)
	for i := 0; i < 10; i++ {
		do(t, ts, "PUT", "/idx/_doc/d"+strconv.Itoa(i), map[string]interface{}{"v": i})
	}

	query := map[string]interface{}{"query": map[string]interface{}{"match_all": map[string]interface{}{}}}

	// 预热
	postSearch(t, ts, "idx", query)

	// 并发请求
	var wg sync.WaitGroup
	errors := make([]error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			b, _ := json.Marshal(query)
			resp, err := http.Post(ts.URL+"/idx/_search", "application/json", bytes.NewReader(b))
			if err != nil {
				errors[idx] = err
				return
			}
			resp.Body.Close()
		}(i)
	}
	wg.Wait()

	for i, e := range errors {
		assert.NoError(t, e, "goroutine %d failed", i)
	}

	// 缓存仍应正常工作
	stats := srv.searchCache.Stats()
	t.Logf("concurrent access: hits=%d misses=%d size=%d", stats.Hits, stats.Misses, stats.Size)
}

// TestSearchCache_LRUEviction 验证 LRU 淘汰
func TestSearchCache_LRUEviction(t *testing.T) {
	// 容量为 2, 只能缓存 2 条
	ts, srv := newTestServerWithCache(t, SearchCacheConfig{Enabled: true, Capacity: 2, MaxSize: 1 << 20})

	do(t, ts, "PUT", "/idx", nil)
	do(t, ts, "PUT", "/idx/_doc/d1", map[string]interface{}{"v": 1})
	do(t, ts, "PUT", "/idx/_doc/d2", map[string]interface{}{"v": 2})
	do(t, ts, "PUT", "/idx/_doc/d3", map[string]interface{}{"v": 3})

	// 执行 3 次不同查询, 填满缓存
	for i := 0; i < 3; i++ {
		q := map[string]interface{}{"query": map[string]interface{}{"term": map[string]interface{}{"v": i}}}
		postSearch(t, ts, "idx", q)
	}

	stats := srv.searchCache.Stats()
	assert.LessOrEqual(t, stats.Size, 2, "cache size should not exceed capacity")
}

// TestSearchCache_IndexDeletion 验证删除索引时全量失效
func TestSearchCache_IndexDeletion(t *testing.T) {
	ts, srv := newTestServerWithCache(t, SearchCacheConfig{Enabled: true, Capacity: 100, MaxSize: 1 << 20})

	do(t, ts, "PUT", "/idx1", nil)
	do(t, ts, "PUT", "/idx1/_doc/d1", map[string]interface{}{"v": 1})
	do(t, ts, "PUT", "/idx2", nil)
	do(t, ts, "PUT", "/idx2/_doc/d1", map[string]interface{}{"v": 1})

	q := map[string]interface{}{"query": map[string]interface{}{"match_all": map[string]interface{}{}}}

	// 缓存两个索引的搜索
	postSearch(t, ts, "idx1", q)
	postSearch(t, ts, "idx2", q)

	// 验证命中
	resp1, _ := postSearch(t, ts, "idx1", q)
	assert.Equal(t, "HIT", resp1.Header.Get("X-GoES-Cache"))
	resp2, _ := postSearch(t, ts, "idx2", q)
	assert.Equal(t, "HIT", resp2.Header.Get("X-GoES-Cache"))

	// 删除 idx1
	do(t, ts, "DELETE", "/idx1", nil)

	// idx1 应 miss, idx2 仍应 hit
	resp3, _ := postSearch(t, ts, "idx1", q)
	assert.NotEqual(t, "HIT", resp3.Header.Get("X-GoES-Cache"), "deleted index cache should be invalidated")

	// idx2 仍命中 (因为只删了 idx1)
	resp4, _ := postSearch(t, ts, "idx2", q)
	assert.Equal(t, "HIT", resp4.Header.Get("X-GoES-Cache"), "unrelated index cache should remain")

	// 验证缓存状态
	stats := srv.searchCache.Stats()
	t.Logf("after index deletion: size=%d hits=%d misses=%d", stats.Size, stats.Hits, stats.Misses)
}
