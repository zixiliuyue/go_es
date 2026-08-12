// Package search - postings-snapshot 单元测试 (#7)
package search

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/zixiliuyue/go_es/internal/storage"
)

func newSnapshotTestStore(t *testing.T) (*storage.Store, func()) {
	t.Helper()
	dir := t.TempDir()
	store, err := storage.Open(filepath.Join(dir, "data"))
	assert.NoError(t, err)
	return store, func() { _ = store.Close() }
}

// indexDocFull 模拟 server 层: 同时写 doc/ (source) + doc-tf/ (tokens) + 内存倒排
// engine.IndexDoc 只写内存 + doc-tf, doc/ 由 server 层 (optimistic.go) 写
func indexDocFull(eng *Engine, store *storage.Store, index, id string, doc map[string]interface{}) {
	// 写 doc/ (server 层职责)
	_ = store.Put(storage.DocKey(index, id), doc)
	// 写内存倒排 + doc-tf (engine 职责)
	eng.IndexDoc(index, id, doc)
}

// TestPostingsSnapshot_FlushAndLoad 基本 flush + load round-trip
func TestPostingsSnapshot_FlushAndLoad(t *testing.T) {
	store, cleanup := newSnapshotTestStore(t)
	defer cleanup()
	eng := New(store)
	// 写 5 个 doc (同时写 doc/ + doc-tf/)
	indexDocFull(eng, store, "idx", "1", map[string]interface{}{"title": "hello world", "cat": "A"})
	indexDocFull(eng, store, "idx", "2", map[string]interface{}{"title": "hello fox", "cat": "B"})
	indexDocFull(eng, store, "idx", "3", map[string]interface{}{"title": "world peace", "cat": "A"})
	indexDocFull(eng, store, "idx", "4", map[string]interface{}{"title": "fox dog", "cat": "C"})
	indexDocFull(eng, store, "idx", "5", map[string]interface{}{"title": "hello dog", "cat": "A"})

	// flush snapshot
	fields, _, err := eng.FlushPostingsSnapshot("idx")
	assert.NoError(t, err)
	assert.Equal(t, 2, fields, "title + cat = 2 fields")

	// 验证 snapshot 信息
	info, err := eng.GetPostingsSnapshotInfo("idx")
	assert.NoError(t, err)
	assert.True(t, info.HasSnapshot)
	assert.True(t, info.VersionMatch)
	assert.Equal(t, 2, info.FieldCount)

	// 新建 engine, LoadAll 应走快路径
	eng2 := New(store)
	err = eng2.LoadAll()
	assert.NoError(t, err)

	// 验证倒排一致
	ids := eng2.matchDocsTokens("idx", "title", "hello")
	assert.Equal(t, 3, len(ids), "hello 应命中 3 个 doc")
	ids = eng2.matchDocsTokens("idx", "title", "world")
	assert.Equal(t, 2, len(ids), "world 应命中 2 个 doc")
	// cat 字段值 ("A"/"B"/"C") 在 indexField 中经 tokenize 小写化为 "a"/"b"/"c"
	// matchDocsTokens 会做同样的小写化, matchDocs 不会, 故此处用 matchDocsTokens
	ids = eng2.matchDocsTokens("idx", "cat", "A")
	assert.Equal(t, 3, len(ids), "cat=A 应命中 3 个 doc")
	ids = eng2.matchDocsTokens("idx", "cat", "C")
	assert.Equal(t, 1, len(ids), "cat=C 应命中 1 个 doc")
}

// TestPostingsSnapshot_VersionMismatch 写后 snapshot 版本不匹配, 走慢路径
func TestPostingsSnapshot_VersionMismatch(t *testing.T) {
	store, cleanup := newSnapshotTestStore(t)
	defer cleanup()
	eng := New(store)
	indexDocFull(eng, store, "idx", "1", map[string]interface{}{"title": "hello world"})
	indexDocFull(eng, store, "idx", "2", map[string]interface{}{"title": "hello fox"})

	// flush snapshot
	_, _, err := eng.FlushPostingsSnapshot("idx")
	assert.NoError(t, err)

	// 再写一个 doc, version 递增
	indexDocFull(eng, store, "idx", "3", map[string]interface{}{"title": "hello dog"})

	// 版本不匹配
	info, err := eng.GetPostingsSnapshotInfo("idx")
	assert.NoError(t, err)
	assert.True(t, info.HasSnapshot)
	assert.False(t, info.VersionMatch, "写后 version 变化, snapshot 不匹配")

	// LoadAll 应回退慢路径, 仍能查到所有 3 个 doc
	eng2 := New(store)
	err = eng2.LoadAll()
	assert.NoError(t, err)
	ids := eng2.matchDocsTokens("idx", "title", "hello")
	assert.Equal(t, 3, len(ids), "慢路径应查到全部 3 个 doc")
}

// TestPostingsSnapshot_Invalidate 失效 snapshot
func TestPostingsSnapshot_Invalidate(t *testing.T) {
	store, cleanup := newSnapshotTestStore(t)
	defer cleanup()
	eng := New(store)
	indexDocFull(eng, store, "idx", "1", map[string]interface{}{"title": "hello"})
	_, _, err := eng.FlushPostingsSnapshot("idx")
	assert.NoError(t, err)

	info, _ := eng.GetPostingsSnapshotInfo("idx")
	assert.True(t, info.HasSnapshot)

	err = eng.InvalidatePostingsSnapshot("idx")
	assert.NoError(t, err)

	info, _ = eng.GetPostingsSnapshotInfo("idx")
	assert.False(t, info.HasSnapshot, "失效后无 snapshot")
}

// TestPostingsSnapshot_EmptyIndex 空索引 flush
func TestPostingsSnapshot_EmptyIndex(t *testing.T) {
	store, cleanup := newSnapshotTestStore(t)
	defer cleanup()
	eng := New(store)
	fields, _, err := eng.FlushPostingsSnapshot("idx")
	assert.NoError(t, err)
	assert.Equal(t, 0, fields, "空索引无 field")
}

// TestPostingsSnapshot_DeleteIndex 删索引后 snapshot 失效
func TestPostingsSnapshot_DeleteIndex(t *testing.T) {
	store, cleanup := newSnapshotTestStore(t)
	defer cleanup()
	eng := New(store)
	indexDocFull(eng, store, "idx", "1", map[string]interface{}{"title": "hello"})
	_, _, _ = eng.FlushPostingsSnapshot("idx")

	eng.DeleteIndex("idx")

	info, _ := eng.GetPostingsSnapshotInfo("idx")
	assert.False(t, info.HasSnapshot, "DeleteIndex 后 snapshot 应失效")
}

// TestPostingsSnapshot_RebuildInverted 重建后 snapshot 失效
func TestPostingsSnapshot_RebuildInverted(t *testing.T) {
	store, cleanup := newSnapshotTestStore(t)
	defer cleanup()
	eng := New(store)
	indexDocFull(eng, store, "idx", "1", map[string]interface{}{"title": "hello"})
	_, _, _ = eng.FlushPostingsSnapshot("idx")

	_, err := eng.RebuildInverted("idx")
	assert.NoError(t, err)

	info, _ := eng.GetPostingsSnapshotInfo("idx")
	assert.False(t, info.HasSnapshot, "RebuildInverted 后 snapshot 应失效")
}

// TestPostingsSnapshot_MultipleIndices 多索引 flush + load
func TestPostingsSnapshot_MultipleIndices(t *testing.T) {
	store, cleanup := newSnapshotTestStore(t)
	defer cleanup()
	eng := New(store)
	indexDocFull(eng, store, "idx1", "1", map[string]interface{}{"title": "hello world"})
	indexDocFull(eng, store, "idx1", "2", map[string]interface{}{"title": "hello fox"})
	indexDocFull(eng, store, "idx2", "1", map[string]interface{}{"name": "alice bob"})
	indexDocFull(eng, store, "idx2", "2", map[string]interface{}{"name": "alice carol"})

	_, _, err := eng.FlushPostingsSnapshot("idx1")
	assert.NoError(t, err)
	_, _, err = eng.FlushPostingsSnapshot("idx2")
	assert.NoError(t, err)

	eng2 := New(store)
	err = eng2.LoadAll()
	assert.NoError(t, err)

	ids := eng2.matchDocsTokens("idx1", "title", "hello")
	assert.Equal(t, 2, len(ids))
	ids = eng2.matchDocsTokens("idx2", "name", "alice")
	assert.Equal(t, 2, len(ids))
}

// TestPostingsSnapshot_ConcurrentFlush 并发 flush 同一 index 应被拒绝
func TestPostingsSnapshot_ConcurrentFlush(t *testing.T) {
	store, cleanup := newSnapshotTestStore(t)
	defer cleanup()
	eng := New(store)
	indexDocFull(eng, store, "idx", "1", map[string]interface{}{"title": "hello"})

	// 标记正在 flush
	eng.persistence.snapshotState.mu.Lock()
	eng.persistence.snapshotState.flushing["idx"] = struct{}{}
	eng.persistence.snapshotState.mu.Unlock()

	_, _, err := eng.FlushPostingsSnapshot("idx")
	assert.Error(t, err, "并发 flush 应报错")
	assert.Contains(t, err.Error(), "already in progress")
}

// TestPostingsSnapshot_BM25StatsRebuild snapshot 加载后 BM25 统计应重建
func TestPostingsSnapshot_BM25StatsRebuild(t *testing.T) {
	store, cleanup := newSnapshotTestStore(t)
	defer cleanup()
	eng := New(store)
	indexDocFull(eng, store, "idx", "1", map[string]interface{}{"title": "hello world hello"})
	indexDocFull(eng, store, "idx", "2", map[string]interface{}{"title": "hello fox"})

	_, _, _ = eng.FlushPostingsSnapshot("idx")

	eng2 := New(store)
	err := eng2.LoadAll()
	assert.NoError(t, err)

	// BM25 打分应可用
	score := eng2.BM25FieldScore("idx", "title", "1", "hello")
	assert.Greater(t, score, 0.0, "BM25 打分应 > 0")
}

// TestPostingsSnapshot_PartialSnapshot 部分索引有 snapshot, 部分无, 混合加载
func TestPostingsSnapshot_PartialSnapshot(t *testing.T) {
	store, cleanup := newSnapshotTestStore(t)
	defer cleanup()
	eng := New(store)
	indexDocFull(eng, store, "idx1", "1", map[string]interface{}{"title": "hello world"})
	indexDocFull(eng, store, "idx1", "2", map[string]interface{}{"title": "hello fox"})
	indexDocFull(eng, store, "idx2", "1", map[string]interface{}{"name": "alice"})

	// 只给 idx1 flush snapshot, idx2 没有
	_, _, _ = eng.FlushPostingsSnapshot("idx1")

	eng2 := New(store)
	err := eng2.LoadAll()
	assert.NoError(t, err)

	// idx1 走快路径
	ids := eng2.matchDocsTokens("idx1", "title", "hello")
	assert.Equal(t, 2, len(ids))
	// idx2 走慢路径
	ids = eng2.matchDocsTokens("idx2", "name", "alice")
	assert.Equal(t, 1, len(ids))
}
