// Package search - 倒排持久化 单元测试
package search

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/zixiliuyue/go_es/internal/storage"
)

// newPersistenceTestEngine 创建一个真正用 badger 存储的 engine (临时目录)
func newPersistenceTestEngine(t *testing.T) (*Engine, *storage.Store, func()) {
	t.Helper()
	dir := t.TempDir()
	store, err := storage.Open(filepath.Join(dir, "data"))
	assert.NoError(t, err)
	e := New(store)
	return e, store, func() { _ = store.Close() }
}

// TestPersistDocTokens_AndReload 验证写入 doc 后能从 store 读出 doc-tf
func TestPersistDocTokens_AndReload(t *testing.T) {
	e, store, cleanup := newPersistenceTestEngine(t)
	defer cleanup()

	// 写 3 个 doc
	e.IndexDoc("idx", "1", map[string]interface{}{"title": "the quick brown fox"})
	e.IndexDoc("idx", "2", map[string]interface{}{"title": "the lazy dog"})
	e.IndexDoc("idx", "3", map[string]interface{}{"title": "fox and hound"})

	// 验证 doc-tf 落盘
	var pt PersistedTokens
	found, err := store.Get(storage.DocTFKey("idx", "1"), &pt)
	assert.NoError(t, err)
	assert.True(t, found)
	assert.ElementsMatch(t, []string{"the", "quick", "brown", "fox"}, pt["title"])

	// 验证 version 已递增
	v := e.getPostingsVersion("idx")
	assert.Greater(t, v, int64(0), "version should be > 0 after writes")
}

// TestLoadAll_RestoresInverted 验证重启后能从 store 恢复倒排
func TestLoadAll_RestoresInverted(t *testing.T) {
	e, store, cleanup := newPersistenceTestEngine(t)
	defer cleanup()

	// 模拟 server 写路径: doc source + IndexDoc
	doc1 := map[string]interface{}{"title": "the quick brown fox"}
	doc2 := map[string]interface{}{"title": "the lazy dog"}
	_ = store.Put([]byte("doc/idx/1"), doc1)
	_ = store.Put([]byte("doc/idx/2"), doc2)
	e.IndexDoc("idx", "1", doc1)
	e.IndexDoc("idx", "2", doc2)

	// 验证 inverted 当前有内容
	ids := e.matchDocsTokens("idx", "title", "fox")
	assert.Equal(t, 1, len(ids))

	// 清空 inverted 模拟重启
	e.mu.Lock()
	e.inverted = make(map[string]map[string]map[string]map[string]struct{})
	e.docs = make(map[string]map[string]map[string]interface{})
	e.mu.Unlock()

	err := e.LoadAll()
	assert.NoError(t, err)

	// 验证 inverted 已重建
	ids = e.matchDocsTokens("idx", "title", "fox")
	assert.Equal(t, 1, len(ids), "fox 应命中 1 个 doc")
	_, has := ids["1"]
	assert.True(t, has)
}

// TestDelete_RemovesDocTF 测试删除 doc 时同步删 doc-tf
func TestDelete_RemovesDocTF(t *testing.T) {
	e, store, cleanup := newPersistenceTestEngine(t)
	defer cleanup()

	e.IndexDoc("idx", "1", map[string]interface{}{"title": "hello world"})
	// 验证 doc-tf 存在
	found, _ := store.Exists(storage.DocTFKey("idx", "1"))
	assert.True(t, found)

	// 删
	e.DeleteDoc("idx", "1")
	// 验证 doc-tf 已被删
	found, _ = store.Exists(storage.DocTFKey("idx", "1"))
	assert.False(t, found, "doc-tf 应被删")
}

// TestRebuildInverted 验证 RebuildInverted 走 doc-tf 优先路径
func TestRebuildInverted(t *testing.T) {
	e, store, cleanup := newPersistenceTestEngine(t)
	defer cleanup()

	// 同时写 doc source + index 流程
	for i := 1; i <= 5; i++ {
		id := string(rune('0' + i))
		doc := map[string]interface{}{"title": "the quick brown fox"}
		// 模拟 server 写路径
		_ = store.Put([]byte("doc/idx/"+id), doc)
		e.IndexDoc("idx", id, doc)
	}

	// 清空 inverted
	e.mu.Lock()
	delete(e.inverted, "idx")
	e.mu.Unlock()

	stats, err := e.RebuildInverted("idx")
	assert.NoError(t, err)
	assert.Equal(t, 5, stats.TotalDocs)
	// 所有 doc 都有 doc-tf, 所以 reused 应该是 5
	assert.Equal(t, 5, stats.ReusedTokens)
	assert.Equal(t, 0, stats.Recomputed)

	// 重建后 inverted 应该有内容
	e.mu.RLock()
	_, ok := e.inverted["idx"]
	e.mu.RUnlock()
	assert.True(t, ok, "inverted 应被重建")
}

// TestRebuildInverted_Fallback 验证 doc-tf 缺失时 fallback 重新分词
func TestRebuildInverted_Fallback(t *testing.T) {
	e, store, cleanup := newPersistenceTestEngine(t)
	defer cleanup()

	doc := map[string]interface{}{"title": "the quick brown fox"}
	_ = store.Put([]byte("doc/idx/1"), doc)
	e.IndexDoc("idx", "1", doc)

	// 手动删 doc-tf, 模拟缺失
	_ = store.Delete(storage.DocTFKey("idx", "1"))

	stats, err := e.RebuildInverted("idx")
	assert.NoError(t, err)
	assert.Equal(t, 1, stats.TotalDocs)
	assert.Equal(t, 0, stats.ReusedTokens)
	assert.Equal(t, 1, stats.Recomputed, "doc-tf 缺失时走实时分词")

	// 重建后 inverted 仍有内容(从 doc source 实时分词)
	ids := e.matchDocsTokens("idx", "title", "fox")
	assert.Equal(t, 1, len(ids))
}

// TestGetInvertedInfo 验证 info 端点
func TestGetInvertedInfo(t *testing.T) {
	e, _, cleanup := newPersistenceTestEngine(t)
	defer cleanup()

	e.IndexDoc("idx", "1", map[string]interface{}{"title": "hello", "body": "world"})
	e.IndexDoc("idx", "2", map[string]interface{}{"title": "hello"})

	info, err := e.GetInvertedInfo("idx")
	assert.NoError(t, err)
	assert.Equal(t, "idx", info.Index)
	assert.Equal(t, 2, info.DocCount)
	assert.Greater(t, info.FieldCount, 0)
	assert.Greater(t, info.TokenCount, 0)
	assert.Greater(t, info.PostingsVersion, int64(0))
	assert.True(t, info.HasDocTFPersisted, "doc-tf 应被持久化")
}

// TestPostingsVersionIncrement 验证 version 随写递增
func TestPostingsVersionIncrement(t *testing.T) {
	e, _, cleanup := newPersistenceTestEngine(t)
	defer cleanup()

	v0 := e.getPostingsVersion("idx")
	assert.Equal(t, int64(0), v0)

	e.IndexDoc("idx", "1", map[string]interface{}{"a": "x"})
	v1 := e.getPostingsVersion("idx")
	assert.Greater(t, v1, v0)

	e.IndexDoc("idx", "2", map[string]interface{}{"a": "y"})
	v2 := e.getPostingsVersion("idx")
	assert.Greater(t, v2, v1)

	e.DeleteDoc("idx", "1")
	v3 := e.getPostingsVersion("idx")
	assert.Greater(t, v3, v2, "delete 也应递增 version")
}

// TestLoadIndex_Single 验证 LoadIndex (单索引)
func TestLoadIndex_Single(t *testing.T) {
	e, store, cleanup := newPersistenceTestEngine(t)
	defer cleanup()

	doc1 := map[string]interface{}{"title": "hello"}
	doc2 := map[string]interface{}{"title": "world"}
	_ = store.Put([]byte("doc/idx1/1"), doc1)
	_ = store.Put([]byte("doc/idx2/1"), doc2)
	e.IndexDoc("idx1", "1", doc1)
	e.IndexDoc("idx2", "1", doc2)

	// 清空 idx1
	e.mu.Lock()
	delete(e.inverted, "idx1")
	delete(e.docs, "idx1")
	e.mu.Unlock()

	err := e.LoadIndex("idx1")
	assert.NoError(t, err)

	// idx1 应该有 inverted, idx2 不变
	e.mu.RLock()
	_, has1 := e.inverted["idx1"]
	_, has2 := e.inverted["idx2"]
	e.mu.RUnlock()
	assert.True(t, has1)
	assert.True(t, has2, "LoadIndex 不应影响 idx2")
}
