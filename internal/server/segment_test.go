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
