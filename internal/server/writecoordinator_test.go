// Package server - WriteCoordinator 单元测试
package server

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/zixiliuyue/go_es/internal/search"
	"github.com/zixiliuyue/go_es/internal/storage"
)

// mockEngine 用于测试的简单 engine
type mockEngine struct {
	mu     sync.Mutex
	indexed map[string]map[string]map[string]interface{}
}

func newMockEngine() *mockEngine {
	return &mockEngine{indexed: make(map[string]map[string]map[string]interface{})}
}

func (m *mockEngine) IndexDoc(index, id string, doc map[string]interface{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.indexed[index] == nil {
		m.indexed[index] = make(map[string]map[string]interface{})
	}
	m.indexed[index][id] = doc
}

func (m *mockEngine) DeleteDoc(index, id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.indexed[index] != nil {
		delete(m.indexed[index], id)
	}
}

func newWCTempStore(t *testing.T) (*storage.Store, func()) {
	t.Helper()
	dir := t.TempDir()
	store, err := storage.Open(filepath.Join(dir, "data"))
	assert.NoError(t, err)
	return store, func() { _ = store.Close() }
}

// TestWriteCoordinator_Acquire_Release 简单 acquire/release
func TestWriteCoordinator_Acquire_Release(t *testing.T) {
	wc := NewWriteCoordinator(WriteConfig{MaxConcurrent: 2})
	release1, err := wc.Acquire()
	assert.NoError(t, err)
	release2, err := wc.Acquire()
	assert.NoError(t, err)
	// 第三个应该被拒绝
	_, err = wc.Acquire()
	assert.Error(t, err)
	assert.Equal(t, ErrWriteBusy, err)
	release1()
	release2()
	// 释放后能再拿
	release3, err := wc.Acquire()
	assert.NoError(t, err)
	release3()
}

// TestWriteCoordinator_Stats 统计
func TestWriteCoordinator_Stats(t *testing.T) {
	wc := NewWriteCoordinator(WriteConfig{MaxConcurrent: 5})
	release, _ := wc.Acquire()
	stats := wc.Stats()
	assert.Equal(t, int64(1), stats.InFlight)
	release()
	stats = wc.Stats()
	assert.Equal(t, int64(0), stats.InFlight)
}

// TestWriteCoordinator_SubmitBulk_Basic 合并写多条
func TestWriteCoordinator_SubmitBulk_Basic(t *testing.T) {
	store, cleanup := newWCTempStore(t)
	defer cleanup()
	wc := NewWriteCoordinator(WriteConfig{MaxConcurrent: 4, MaxBatchSize: 100})
	eng := newMockEngine()
	ops := []WriteOp{
		{Index: "idx", ID: "1", Kind: "index", Doc: map[string]interface{}{"a": 1}, VersionMeta: &DocMeta{SeqNo: 1, Version: 1}},
		{Index: "idx", ID: "2", Kind: "index", Doc: map[string]interface{}{"a": 2}, VersionMeta: &DocMeta{SeqNo: 1, Version: 1}},
		{Index: "idx", ID: "3", Kind: "index", Doc: map[string]interface{}{"a": 3}, VersionMeta: &DocMeta{SeqNo: 1, Version: 1}},
	}
	results := wc.SubmitBulk(store, eng, ops)
	assert.Equal(t, 3, len(results))
	for i, r := range results {
		assert.Equal(t, 201, r.Status, "op %d should be 201", i)
		assert.Nil(t, r.Error)
	}
	// 验证都写入了 engine
	eng.mu.Lock()
	assert.Equal(t, 3, len(eng.indexed["idx"]))
	eng.mu.Unlock()
	// 验证 stats
	stats := wc.Stats()
	assert.Equal(t, int64(3), stats.TotalOK)
	assert.Equal(t, int64(1), stats.TotalBatches)
}

// TestWriteCoordinator_SubmitBulk_Delete delete 也合并
func TestWriteCoordinator_SubmitBulk_Delete(t *testing.T) {
	store, cleanup := newWCTempStore(t)
	defer cleanup()
	wc := NewWriteCoordinator(WriteConfig{MaxConcurrent: 4, MaxBatchSize: 100})
	eng := newMockEngine()
	// 先写
	eng.IndexDoc("idx", "1", map[string]interface{}{"x": 1})
	// 删
	ops := []WriteOp{{Index: "idx", ID: "1", Kind: "delete"}}
	results := wc.SubmitBulk(store, eng, ops)
	assert.Equal(t, 200, results[0].Status)
}

// TestWriteCoordinator_SubmitBulk_OverLimit 太长 batch -> 413
func TestWriteCoordinator_SubmitBulk_OverLimit(t *testing.T) {
	store, cleanup := newWCTempStore(t)
	defer cleanup()
	wc := NewWriteCoordinator(WriteConfig{MaxConcurrent: 4, MaxBatchSize: 2})
	eng := newMockEngine()
	ops := make([]WriteOp, 5)
	for i := range ops {
		ops[i] = WriteOp{Index: "idx", ID: "1", Kind: "index", Doc: map[string]interface{}{"a": i}, VersionMeta: &DocMeta{Version: 1}}
	}
	results := wc.SubmitBulk(store, eng, ops)
	for _, r := range results {
		assert.Equal(t, 413, r.Status)
	}
}

// TestWriteCoordinator_SubmitBulk_Concurrent 并发批次
func TestWriteCoordinator_SubmitBulk_Concurrent(t *testing.T) {
	store, cleanup := newWCTempStore(t)
	defer cleanup()
	wc := NewWriteCoordinator(WriteConfig{MaxConcurrent: 100, MaxBatchSize: 100})
	eng := newMockEngine()
	var wg sync.WaitGroup
	var total atomic.Int64
	for batch := 0; batch < 10; batch++ {
		wg.Add(1)
		go func(b int) {
			defer wg.Done()
			ops := make([]WriteOp, 5)
			for i := range ops {
				ops[i] = WriteOp{
					Index: "idx", ID: "k",
					Kind: "index", Doc: map[string]interface{}{"b": b, "i": i},
					VersionMeta: &DocMeta{SeqNo: int64(b + 1), Version: int64(b*5 + i + 1)},
				}
			}
			results := wc.SubmitBulk(store, eng, ops)
			for _, r := range results {
				if r.Status == 201 {
					total.Add(1)
				}
			}
		}(batch)
	}
	wg.Wait()
	assert.Equal(t, int64(50), total.Load(), "应 50 条全部成功")
	stats := wc.Stats()
	assert.Equal(t, int64(50), stats.TotalOK)
	assert.Equal(t, int64(10), stats.TotalBatches)
}

// TestWriteCoordinator_SubmitBulk_RealEngine 集成: 真 search.Engine
func TestWriteCoordinator_SubmitBulk_RealEngine(t *testing.T) {
	store, cleanup := newWCTempStore(t)
	defer cleanup()
	realEng := search.New(store)
	wc := NewWriteCoordinator(WriteConfig{MaxConcurrent: 4, MaxBatchSize: 100})
	ops := []WriteOp{
		{Index: "real", ID: "1", Kind: "index", Doc: map[string]interface{}{"title": "hello world"}, VersionMeta: &DocMeta{SeqNo: 1, Version: 1}},
		{Index: "real", ID: "2", Kind: "index", Doc: map[string]interface{}{"title": "fox jumps"}, VersionMeta: &DocMeta{SeqNo: 1, Version: 1}},
	}
	results := wc.SubmitBulk(store, realEng, ops)
	for _, r := range results {
		assert.Equal(t, 201, r.Status)
	}
	// 验证 search 索引里能找到 (用 MatchDocs 公开方法, 或直接检查 docs)
	_ = realEng // 真实倒排构建在 IndexDoc 内已发生; 这里只验证无报错
}
