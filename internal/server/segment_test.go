// Package server - Segment 单元测试
package server

import (
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/zixiliuyue/go_es/internal/search"
	"github.com/zixiliuyue/go_es/internal/storage"
)

// mockEngineForSeg mock engine, 支持 SnapshotIndex
type mockEngineForSeg struct {
	mu      sync.Mutex
	inv     map[string]map[string]map[string]map[string]struct{}
	indexed map[string]map[string]map[string]interface{}
}

func newMockEngineForSeg() *mockEngineForSeg {
	return &mockEngineForSeg{
		inv:     make(map[string]map[string]map[string]map[string]struct{}),
		indexed: make(map[string]map[string]map[string]interface{}),
	}
}

func (m *mockEngineForSeg) SnapshotIndex(index string) map[string]map[string]map[string]struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]map[string]map[string]struct{})
	if m.inv[index] == nil {
		return out
	}
	for f, terms := range m.inv[index] {
		fout := make(map[string]map[string]struct{})
		for t, docs := range terms {
			tout := make(map[string]struct{}, len(docs))
			for id := range docs {
				tout[id] = struct{}{}
			}
			fout[t] = tout
		}
		out[f] = fout
	}
	return out
}

// LoadSegmentPostings mock: 把 segment postings 写入 mock 倒排
func (m *mockEngineForSeg) LoadSegmentPostings(index, field string, postings map[string][]string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inv[index] == nil {
		m.inv[index] = make(map[string]map[string]map[string]struct{})
	}
	if m.inv[index][field] == nil {
		m.inv[index][field] = make(map[string]map[string]struct{})
	}
	count := 0
	for term, docIDs := range postings {
		if m.inv[index][field][term] == nil {
			m.inv[index][field][term] = make(map[string]struct{})
		}
		for _, id := range docIDs {
			m.inv[index][field][term][id] = struct{}{}
		}
		count++
	}
	return count
}

func (m *mockEngineForSeg) addDoc(index, field, docID, term string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// inv: index -> field -> term -> set(docID)
	if m.inv[index] == nil {
		m.inv[index] = make(map[string]map[string]map[string]struct{})
	}
	if m.inv[index][field] == nil {
		m.inv[index][field] = make(map[string]map[string]struct{})
	}
	if m.inv[index][field][term] == nil {
		m.inv[index][field][term] = make(map[string]struct{})
	}
	m.inv[index][field][term][docID] = struct{}{}
}

func newSegTestStore(t *testing.T) (*storage.Store, func()) {
	t.Helper()
	dir := t.TempDir()
	store, err := storage.Open(filepath.Join(dir, "data"))
	assert.NoError(t, err)
	return store, func() { _ = store.Close() }
}

// TestSegmentManager_FlushNow 强制 flush
func TestSegmentManager_FlushNow(t *testing.T) {
	store, cleanup := newSegTestStore(t)
	defer cleanup()
	eng := newMockEngineForSeg()
	for i := 1; i <= 5; i++ {
		eng.addDoc("idx", "title", "doc"+string(rune('0'+i)), "fox")
	}
	cfg := SegmentConfig{MaxBufferDocs: 1000, MaxBufferBytes: 1 << 20, AutoFlushIntervalSec: 30}
	sm := NewSegmentManager(cfg, store, eng)
	created, err := sm.FlushNow("idx")
	assert.NoError(t, err)
	assert.Equal(t, 1, created, "1 个 field -> 1 个 segment")
	stats := sm.Stats()
	assert.Equal(t, int64(1), stats.TotalSegments)
	assert.Equal(t, int64(1), stats.TotalFlushes)
	segs := sm.ListSegments("idx")
	assert.Equal(t, 1, len(segs))
	assert.Equal(t, "idx", segs[0].Index)
	assert.Equal(t, 5, segs[0].DocCount)
}

// TestSegmentManager_OnWrite 触发 flush
func TestSegmentManager_OnWrite(t *testing.T) {
	store, cleanup := newSegTestStore(t)
	defer cleanup()
	eng := newMockEngineForSeg()
	eng.addDoc("idx", "title", "1", "fox")
	cfg := SegmentConfig{MaxBufferDocs: 3, MaxBufferBytes: 1 << 20, AutoFlushIntervalSec: 30}
	sm := NewSegmentManager(cfg, store, eng)

	// 3 次写, 第 3 次触发 flush
	should1 := sm.OnWrite("idx", 10)
	should2 := sm.OnWrite("idx", 10)
	should3 := sm.OnWrite("idx", 10)
	assert.False(t, should1)
	assert.False(t, should2)
	assert.True(t, should3, "3 doc 达到阈值应触发 flush")

	// 真正 flush
	_, err := sm.FlushNow("idx")
	assert.NoError(t, err)
	segs := sm.ListSegments("idx")
	assert.Equal(t, 1, len(segs))
}

// TestSegmentManager_BytesThreshold 字节阈值
func TestSegmentManager_BytesThreshold(t *testing.T) {
	store, cleanup := newSegTestStore(t)
	defer cleanup()
	eng := newMockEngineForSeg()
	cfg := SegmentConfig{MaxBufferDocs: 9999, MaxBufferBytes: 100, AutoFlushIntervalSec: 30}
	sm := NewSegmentManager(cfg, store, eng)
	should1 := sm.OnWrite("idx", 50)
	should2 := sm.OnWrite("idx", 60)
	assert.False(t, should1)
	assert.True(t, should2, "累计 110 字节超过 100 阈值")
}

// TestSegmentManager_RealEngine 集成 search.Engine
func TestSegmentManager_RealEngine(t *testing.T) {
	store, cleanup := newSegTestStore(t)
	defer cleanup()
	realEng := search.New(store)
	realEng.IndexDoc("idx", "1", map[string]interface{}{"title": "hello world"})
	realEng.IndexDoc("idx", "2", map[string]interface{}{"title": "fox jumps"})

	cfg := SegmentConfig{MaxBufferDocs: 1000, MaxBufferBytes: 1 << 20, AutoFlushIntervalSec: 30}
	sm := NewSegmentManager(cfg, store, realEng)
	created, err := sm.FlushNow("idx")
	assert.NoError(t, err)
	assert.Equal(t, 1, created)
	segs := sm.ListSegments("idx")
	assert.Equal(t, 1, len(segs))
	assert.Equal(t, 2, segs[0].DocCount)
}

// TestSegmentManager_SearchSegment 验证 segment 数据可查
func TestSegmentManager_SearchSegment(t *testing.T) {
	store, cleanup := newSegTestStore(t)
	defer cleanup()
	eng := newMockEngineForSeg()
	eng.addDoc("idx", "title", "1", "fox")
	eng.addDoc("idx", "title", "2", "fox")
	eng.addDoc("idx", "title", "3", "dog")
	cfg := SegmentConfig{MaxBufferDocs: 1000, MaxBufferBytes: 1 << 20, AutoFlushIntervalSec: 30}
	sm := NewSegmentManager(cfg, store, eng)
	_, err := sm.FlushNow("idx")
	assert.NoError(t, err)

	// 重新加载 segment 数据
	segs := sm.ListSegments("idx")
	// 不用通过 seg_meta, 直接读 segment
	var data SegmentData
	found, err := store.Get(segmentKey("idx", segs[0].SegID), &data)
	assert.NoError(t, err)
	assert.True(t, found)
	ids := data.SearchSegment("fox")
	assert.Equal(t, 2, len(ids))
}

// TestSegmentManager_Empty 倒排为空
func TestSegmentManager_Empty(t *testing.T) {
	store, cleanup := newSegTestStore(t)
	defer cleanup()
	eng := newMockEngineForSeg()
	cfg := SegmentConfig{}
	sm := NewSegmentManager(cfg, store, eng)
	created, err := sm.FlushNow("nonexistent")
	assert.NoError(t, err)
	assert.Equal(t, 0, created, "no docs -> no segment")
}

// TestSegmentManager_Defaults 默认值
func TestSegmentManager_Defaults(t *testing.T) {
	store, cleanup := newSegTestStore(t)
	defer cleanup()
	eng := newMockEngineForSeg()
	sm := NewSegmentManager(SegmentConfig{}, store, eng)
	// 默认 MaxBufferDocs = 10000
	assert.Equal(t, 10000, sm.cfg.MaxBufferDocs)
}

// TestHandleSegmentFlush 端到端
func TestHandleSegmentFlush(t *testing.T) {
	store, cleanup := newSegTestStore(t)
	defer cleanup()
	realEng := search.New(store)
	s := &Server{store: store, engine: realEng, rbac: newRBAC(), seg: NewSegmentManager(SegmentConfig{}, store, realEng)}
	// 建索引
	_ = store.Put([]byte("meta/segidx"), map[string]interface{}{})
	realEng.IndexDoc("segidx", "1", map[string]interface{}{"title": "fox"})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/segidx/_segment/flush", nil)
	s.handleSegmentFlush(rr, req, "segidx")
	assert.Equal(t, 200, rr.Code, "should return 200")
	body := rr.Body.String()
	assert.Contains(t, body, "segments_created")
}

// ------------------------------------------------------------------
// Bloom Filter (#10.2)
// ------------------------------------------------------------------

func TestBloomFilter_Basic(t *testing.T) {
	bf := newBloomFilter(100)
	terms := []string{"hello", "world", "fox", "dog", "cat"}
	for _, term := range terms {
		bf.Add(term)
	}
	for _, term := range terms {
		assert.True(t, bf.MayContain(term), "已添加的 term 应 MayContain=true: %s", term)
	}
	// 未添加的 term 大概率 MayContain=false
	misses := []string{"elephant", "zebra", "giraffe", "hippo", "rhino"}
	falsePositive := 0
	for _, term := range misses {
		if bf.MayContain(term) {
			falsePositive++
		}
	}
	// 5 个未添加 term, 误判率应 < 50% (实际远低于此)
	assert.True(t, falsePositive < 3, "误判数 %d/5 应 < 3", falsePositive)
}

func TestBloomFilter_Serialize(t *testing.T) {
	bf := newBloomFilter(50)
	bf.Add("alpha")
	bf.Add("beta")
	bf.Add("gamma")
	s := bf.MarshalBinary()
	assert.NotEmpty(t, s)
	// 反序列化
	bf2 := bloomFromBase64(s)
	assert.NotNil(t, bf2)
	assert.True(t, bf2.MayContain("alpha"))
	assert.True(t, bf2.MayContain("beta"))
	assert.True(t, bf2.MayContain("gamma"))
}

func TestBloomFilter_Empty(t *testing.T) {
	bf := newBloomFilter(0)
	assert.NotNil(t, bf)
	// 空 bloom 对任何 term 都应 MayContain=false (大概率)
	// 注: 极小 bloom 可能误判, 但 m=1024 时对单个 term 误判率极低
	assert.False(t, bf.MayContain("nonexistent_term_12345"))
}

// ------------------------------------------------------------------
// SearchTerm (#10.1)
// ------------------------------------------------------------------

// TestSegmentManager_SearchTerm 跨 segment 合并查 term
func TestSegmentManager_SearchTerm(t *testing.T) {
	store, cleanup := newSegTestStore(t)
	defer cleanup()
	eng := newMockEngineForSeg()
	// doc1-3 含 "fox", doc4-5 含 "dog"
	eng.addDoc("idx", "title", "1", "fox")
	eng.addDoc("idx", "title", "2", "fox")
	eng.addDoc("idx", "title", "3", "fox")
	eng.addDoc("idx", "title", "4", "dog")
	eng.addDoc("idx", "title", "5", "dog")
	cfg := SegmentConfig{MaxBufferDocs: 1000, MaxBufferBytes: 1 << 20, AutoFlushIntervalSec: 30}
	sm := NewSegmentManager(cfg, store, eng)
	_, err := sm.FlushNow("idx")
	assert.NoError(t, err)

	// 查 "fox" 应命中 3 个
	ids := sm.SearchTerm("idx", "title", "fox")
	assert.Equal(t, 3, len(ids), "fox 应命中 3 个 doc")
	// 查 "dog" 应命中 2 个
	ids = sm.SearchTerm("idx", "title", "dog")
	assert.Equal(t, 2, len(ids), "dog 应命中 2 个 doc")
	// 查不存在的 term 应返回空
	ids = sm.SearchTerm("idx", "title", "nonexistent")
	assert.Equal(t, 0, len(ids), "不存在的 term 应返回空")
}

// TestSegmentManager_SearchTerm_BloomSkip 验证 bloom filter 跳过
func TestSegmentManager_SearchTerm_BloomSkip(t *testing.T) {
	store, cleanup := newSegTestStore(t)
	defer cleanup()
	eng := newMockEngineForSeg()
	eng.addDoc("idx", "title", "1", "alpha")
	cfg := SegmentConfig{}
	sm := NewSegmentManager(cfg, store, eng)
	_, err := sm.FlushNow("idx")
	assert.NoError(t, err)

	// 查存在的 term
	ids := sm.SearchTerm("idx", "title", "alpha")
	assert.Equal(t, 1, len(ids))
	// 查不存在的 term (bloom miss -> 快速跳过, 不读 Postings)
	ids = sm.SearchTerm("idx", "title", "zzz_not_in_segment")
	assert.Equal(t, 0, len(ids))
}

// TestSegmentManager_SearchTerm_MultiSegment 多 segment 合并
func TestSegmentManager_SearchTerm_MultiSegment(t *testing.T) {
	store, cleanup := newSegTestStore(t)
	defer cleanup()
	eng := newMockEngineForSeg()
	// 第一次 flush: doc1-2 含 "fox"
	eng.addDoc("idx", "title", "1", "fox")
	eng.addDoc("idx", "title", "2", "fox")
	sm := NewSegmentManager(SegmentConfig{}, store, eng)
	_, _ = sm.FlushNow("idx")
	// 第二次 flush: doc3-4 含 "fox"
	eng.addDoc("idx", "title", "3", "fox")
	eng.addDoc("idx", "title", "4", "fox")
	_, _ = sm.FlushNow("idx")

	// 查 "fox" 应命中 4 个 (跨 2 个 segment)
	ids := sm.SearchTerm("idx", "title", "fox")
	assert.Equal(t, 4, len(ids), "跨 segment 合并应命中 4 个 doc")
	// 结果应已排序
	assert.Equal(t, []string{"1", "2", "3", "4"}, ids)
}

// TestSegmentManager_SearchAllDocIDs match_all 诊断
func TestSegmentManager_SearchAllDocIDs(t *testing.T) {
	store, cleanup := newSegTestStore(t)
	defer cleanup()
	eng := newMockEngineForSeg()
	eng.addDoc("idx", "title", "1", "fox")
	eng.addDoc("idx", "title", "2", "dog")
	eng.addDoc("idx", "title", "3", "cat")
	sm := NewSegmentManager(SegmentConfig{}, store, eng)
	_, _ = sm.FlushNow("idx")
	ids := sm.SearchAllDocIDs("idx")
	assert.Equal(t, 3, len(ids))
	assert.Equal(t, []string{"1", "2", "3"}, ids)
}

// ------------------------------------------------------------------
// LoadSegmentsIntoEngine (#10.1 冷启动)
// ------------------------------------------------------------------

// TestSegmentManager_LoadSegmentsIntoEngine 从 segment 恢复倒排
func TestSegmentManager_LoadSegmentsIntoEngine(t *testing.T) {
	store, cleanup := newSegTestStore(t)
	defer cleanup()
	realEng := search.New(store)
	realEng.IndexDoc("idx", "1", map[string]interface{}{"title": "hello world"})
	realEng.IndexDoc("idx", "2", map[string]interface{}{"title": "fox jumps"})
	sm := NewSegmentManager(SegmentConfig{}, store, realEng)
	_, _ = sm.FlushNow("idx")

	// 新建一个空 engine, 模拟冷启动
	eng2 := search.New(store)
	sm2 := NewSegmentManager(SegmentConfig{}, store, eng2)
	segs, terms, err := sm2.LoadSegmentsIntoEngine("idx")
	assert.NoError(t, err)
	assert.Equal(t, 1, segs, "1 个 field segment")
	assert.True(t, terms > 0, "应加载了若干 term")
	// 验证倒排已恢复: 用 SnapshotIndex 查
	snap := eng2.SnapshotIndex("idx")
	assert.Contains(t, snap, "title")
	// "hello" 应命中 doc "1"
	assert.Contains(t, snap["title"]["hello"], "1")
	// "fox" 应命中 doc "2"
	assert.Contains(t, snap["title"]["fox"], "2")
}

// ------------------------------------------------------------------
// MergeSegments (#10.3)
// ------------------------------------------------------------------

// TestSegmentManager_MergeSegments 合并小 segment
func TestSegmentManager_MergeSegments(t *testing.T) {
	store, cleanup := newSegTestStore(t)
	defer cleanup()
	eng := newMockEngineForSeg()
	sm := NewSegmentManager(SegmentConfig{}, store, eng)
	// flush 3 次, 产生 3 个 segment
	eng.addDoc("idx", "title", "1", "fox")
	_, _ = sm.FlushNow("idx")
	eng.addDoc("idx", "title", "2", "fox")
	_, _ = sm.FlushNow("idx")
	eng.addDoc("idx", "title", "3", "fox")
	_, _ = sm.FlushNow("idx")
	before := sm.ListSegments("idx")
	assert.Equal(t, 3, len(before), "合并前 3 个 segment")

	// 合并到 maxSegments=1
	merged, created, err := sm.MergeSegments("idx", 1)
	assert.NoError(t, err)
	assert.Equal(t, 3, merged, "合并掉 3 个")
	assert.Equal(t, 1, created, "新建 1 个")
	after := sm.ListSegments("idx")
	assert.Equal(t, 1, len(after), "合并后 1 个 segment")

	// 合并后查 "fox" 仍命中 3 个 doc
	ids := sm.SearchTerm("idx", "title", "fox")
	assert.Equal(t, 3, len(ids), "合并后 fox 应命中 3 个 doc")
	// 合并后 segment 的 docIDs 应含全部 3 个
	allIDs := sm.SearchAllDocIDs("idx")
	assert.Equal(t, 3, len(allIDs))
}

// TestSegmentManager_MergeSegments_Noop 不够多不合并
func TestSegmentManager_MergeSegments_Noop(t *testing.T) {
	store, cleanup := newSegTestStore(t)
	defer cleanup()
	eng := newMockEngineForSeg()
	sm := NewSegmentManager(SegmentConfig{}, store, eng)
	eng.addDoc("idx", "title", "1", "fox")
	_, _ = sm.FlushNow("idx")
	// 只有 1 个 segment, maxSegments=2 -> 不合并
	merged, created, err := sm.MergeSegments("idx", 2)
	assert.NoError(t, err)
	assert.Equal(t, 0, merged)
	assert.Equal(t, 0, created)
}

// TestSegmentManager_MergeSegments_BloomPreserved 合并后 bloom 仍在
func TestSegmentManager_MergeSegments_BloomPreserved(t *testing.T) {
	store, cleanup := newSegTestStore(t)
	defer cleanup()
	eng := newMockEngineForSeg()
	sm := NewSegmentManager(SegmentConfig{}, store, eng)
	eng.addDoc("idx", "title", "1", "alpha")
	_, _ = sm.FlushNow("idx")
	eng.addDoc("idx", "title", "2", "beta")
	_, _ = sm.FlushNow("idx")
	_, _, _ = sm.MergeSegments("idx", 1)
	// 合并后查存在的 term 仍命中
	ids := sm.SearchTerm("idx", "title", "alpha")
	assert.Equal(t, 1, len(ids))
	ids = sm.SearchTerm("idx", "title", "beta")
	assert.Equal(t, 1, len(ids))
}

// ------------------------------------------------------------------
// HTTP 端点 (#10)
// ------------------------------------------------------------------

// TestHandleSegmentMerge HTTP 端到端
func TestHandleSegmentMerge(t *testing.T) {
	store, cleanup := newSegTestStore(t)
	defer cleanup()
	realEng := search.New(store)
	s := &Server{store: store, engine: realEng, rbac: newRBAC(), seg: NewSegmentManager(SegmentConfig{}, store, realEng)}
	// 写 doc + flush 3 次
	realEng.IndexDoc("segidx", "1", map[string]interface{}{"title": "fox"})
	_, _ = s.seg.FlushNow("segidx")
	realEng.IndexDoc("segidx", "2", map[string]interface{}{"title": "fox"})
	_, _ = s.seg.FlushNow("segidx")
	realEng.IndexDoc("segidx", "3", map[string]interface{}{"title": "fox"})
	_, _ = s.seg.FlushNow("segidx")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/segidx/_segment/merge?max_segments=1", nil)
	s.handleSegmentMerge(rr, req, "segidx")
	assert.Equal(t, 200, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, "segments_merged")
	assert.Contains(t, body, "segments_created")
}

// TestHandleSegmentMerge_DefaultMaxSegs 默认 max_segments=2
func TestHandleSegmentMerge_DefaultMaxSegs(t *testing.T) {
	store, cleanup := newSegTestStore(t)
	defer cleanup()
	realEng := search.New(store)
	s := &Server{store: store, engine: realEng, rbac: newRBAC(), seg: NewSegmentManager(SegmentConfig{}, store, realEng)}
	// flush 3 次
	for i := 1; i <= 3; i++ {
		realEng.IndexDoc("segidx", string(rune('0'+i)), map[string]interface{}{"title": "fox"})
		_, _ = s.seg.FlushNow("segidx")
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/segidx/_segment/merge", nil)
	s.handleSegmentMerge(rr, req, "segidx")
	assert.Equal(t, 200, rr.Code)
}
