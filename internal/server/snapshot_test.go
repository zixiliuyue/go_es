// Package server - 快照/恢复 单元测试
package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/zixiliuyue/go_es/internal/search"
	"github.com/zixiliuyue/go_es/internal/storage"
)

// TestSnapshotPath 计算路径
func TestSnapshotPath(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.Open(dir)
	assert.NoError(t, err)
	defer store.Close()
	s := &Server{store: store, snapDir: filepath.Join(dir, "snapshots")}
	p := s.snapshotPath("repo1", "snap1")
	expected := filepath.Join(dir, "snapshots", "repo1", "snap1.ndjson")
	assert.Equal(t, expected, p)
}

// TestSnapshotCreateAndRestore 完整快照 + 恢复
func TestSnapshotCreateAndRestore(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.Open(dir)
	assert.NoError(t, err)
	defer store.Close()
	eng := search.New(store)

	// 写一些 doc
	for i := 1; i <= 5; i++ {
		err := store.Put([]byte("doc/idx/"+string(rune('0'+i))), map[string]interface{}{"a": i})
		assert.NoError(t, err)
		eng.IndexDoc("idx", string(rune('0'+i)), map[string]interface{}{"a": i})
	}

	s := &Server{store: store, engine: eng, snapDir: filepath.Join(dir, "snapshots")}
	// 建 repo
	err = store.Put(storage.SnapshotRepoKey("repo1"), map[string]interface{}{"type": "fs"})
	assert.NoError(t, err)

	// create snapshot - URL path: /_snapshot/repo1/snap1
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/_snapshot/repo1/snap1", nil)
	s.handleSnapshotCreate(rr, req)
	assert.Equal(t, 200, rr.Code)

	// 验证文件存在
	snapPath := s.snapshotPath("repo1", "snap1")
	_, err = os.Stat(snapPath)
	assert.NoError(t, err, "snapshot file should exist")

	// 在新 store 上 restore
	store2, err := storage.Open(filepath.Join(dir, "data2"))
	assert.NoError(t, err)
	defer store2.Close()
	eng2 := search.New(store2)

	s2 := &Server{store: store2, engine: eng2, snapDir: filepath.Join(dir, "snapshots")}
	// restore - URL path: /_snapshot/repo1/snap1/_restore
	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/_snapshot/repo1/snap1/_restore", nil)
	s2.handleSnapshotRestore(rr2, req2)
	assert.Equal(t, 200, rr2.Code)
	body := rr2.Body.String()
	assert.Contains(t, body, `"restored"`)
}

// TestSnapshotRestore_NotFound 恢复不存在的 snapshot
func TestSnapshotRestore_NotFound(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.Open(dir)
	assert.NoError(t, err)
	defer store.Close()
	eng := search.New(store)
	s := &Server{store: store, engine: eng, snapDir: filepath.Join(dir, "snapshots")}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/_snapshot/r/missing/_restore", nil)
	s.handleSnapshotRestore(rr, req)
	assert.Equal(t, 404, rr.Code)
}

// TestSnapshotCreate_NoRepo
func TestSnapshotCreate_NoRepo(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.Open(dir)
	assert.NoError(t, err)
	defer store.Close()
	eng := search.New(store)
	s := &Server{store: store, engine: eng, snapDir: filepath.Join(dir, "snapshots")}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/_snapshot/missing_repo/snap1", nil)
	s.handleSnapshotCreate(rr, req)
	assert.Equal(t, 404, rr.Code)
}

// TestSnapshotFileFormat 文件内容格式
func TestSnapshotFileFormat(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.Open(dir)
	assert.NoError(t, err)
	defer store.Close()
	eng := search.New(store)

	store.Put([]byte("doc/i/1"), map[string]interface{}{"a": 1})
	eng.IndexDoc("i", "1", map[string]interface{}{"a": 1})
	store.Put(storage.SnapshotRepoKey("r"), map[string]interface{}{})
	s := &Server{store: store, engine: eng, snapDir: filepath.Join(dir, "snapshots")}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/_snapshot/r/s", nil)
	s.handleSnapshotCreate(rr, req)
	assert.Equal(t, 200, rr.Code)

	// 读文件
	content, err := os.ReadFile(s.snapshotPath("r", "s"))
	assert.NoError(t, err)
	// 至少有一行 (doc/i/1)
	lines := splitLines(string(content))
	assert.Greater(t, len(lines), 0)
	// 解析第一行
	var row map[string]interface{}
	err = json.Unmarshal([]byte(lines[0]), &row)
	assert.NoError(t, err)
	assert.Contains(t, row, "key")
	assert.Contains(t, row, "value")
}

// TestSnapshotsDir_Default 默认目录
func TestSnapshotsDir_Default(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.Open(dir)
	assert.NoError(t, err)
	defer store.Close()
	s := &Server{store: store}
	// 没设 snapDir, 用 store.Dir() + snapshots
	p := s.snapshotsDir()
	expected := filepath.Join(dir, "snapshots")
	assert.Equal(t, expected, p)
}

// TestSnapshotRestore_Reindexable 验证恢复后数据存在
func TestSnapshotRestore_Reindexable(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.Open(dir)
	assert.NoError(t, err)
	defer store.Close()
	eng := search.New(store)

	// 写入文档并建立倒排
	store.Put([]byte("doc/books/1"), map[string]interface{}{"title": "Go in Action", "year": 2015})
	eng.IndexDoc("books", "1", map[string]interface{}{"title": "Go in Action", "year": 2015})
	store.Put(storage.SnapshotRepoKey("r"), map[string]interface{}{})

	s := &Server{store: store, engine: eng, snapDir: filepath.Join(dir, "snapshots")}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/_snapshot/r/snap1", nil)
	s.handleSnapshotCreate(rr, req)
	assert.Equal(t, 200, rr.Code)

	// 在新 store 上恢复
	store2, err := storage.Open(filepath.Join(dir, "restored"))
	assert.NoError(t, err)
	defer store2.Close()
	eng2 := search.New(store2)

	s2 := &Server{store: store2, engine: eng2, snapDir: filepath.Join(dir, "snapshots")}
	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/_snapshot/r/snap1/_restore", nil)
	s2.handleSnapshotRestore(rr2, req2)
	assert.Equal(t, 200, rr2.Code)

	// 验证恢复后数据存在
	var val map[string]interface{}
	found, err := store2.Get([]byte("doc/books/1"), &val)
	assert.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "Go in Action", val["title"])
}

// TestSnapshotDelete 删除快照
func TestSnapshotDelete(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.Open(dir)
	assert.NoError(t, err)
	defer store.Close()
	eng := search.New(store)

	store.Put(storage.SnapshotRepoKey("r"), map[string]interface{}{})
	store.Put([]byte("doc/i/1"), map[string]interface{}{"a": 1})
	eng.IndexDoc("i", "1", map[string]interface{}{"a": 1})

	s := &Server{store: store, engine: eng, snapDir: filepath.Join(dir, "snapshots")}

	// 先创建 snapshot
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/_snapshot/r/snap1", nil)
	s.handleSnapshotCreate(rr, req)
	assert.Equal(t, 200, rr.Code)

	// 验证文件存在
	snapPath := s.snapshotPath("r", "snap1")
	_, err = os.Stat(snapPath)
	assert.NoError(t, err, "snapshot file should exist before delete")

	// 再删除
	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("DELETE", "/_snapshot/r/snap1", nil)
	s.handleSnapshotDelete(rr2, req2)
	assert.Equal(t, 200, rr2.Code)

	// 验证文件已删除
	_, err = os.Stat(snapPath)
	assert.True(t, os.IsNotExist(err), "snapshot file should be deleted")

	// 验证元数据已删除
	rr3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("GET", "/_snapshot/r/snap1", nil)
	s.handleSnapshotGet(rr3, req3)
	assert.Equal(t, 404, rr3.Code)
}

// TestSnapshotRestore_Searchable 恢复后文档可搜索
func TestSnapshotRestore_Searchable(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.Open(dir)
	assert.NoError(t, err)
	defer store.Close()
	eng := search.New(store)

	// 写入带字符串字段的文档
	store.Put([]byte("doc/books/1"), map[string]interface{}{"title": "Go in Action"})
	eng.IndexDoc("books", "1", map[string]interface{}{"title": "Go in Action"})
	store.Put([]byte("doc/books/2"), map[string]interface{}{"title": "Learning Go"})
	eng.IndexDoc("books", "2", map[string]interface{}{"title": "Learning Go"})
	store.Put(storage.SnapshotRepoKey("r"), map[string]interface{}{})

	s := &Server{store: store, engine: eng, snapDir: filepath.Join(dir, "snapshots")}

	// 创建快照
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/_snapshot/r/snap1", nil)
	s.handleSnapshotCreate(rr, req)
	assert.Equal(t, 200, rr.Code)

	// 在新 store 上恢复
	store2, err := storage.Open(filepath.Join(dir, "restored"))
	assert.NoError(t, err)
	defer store2.Close()
	eng2 := search.New(store2)

	s2 := &Server{store: store2, engine: eng2, snapDir: filepath.Join(dir, "snapshots")}
	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/_snapshot/r/snap1/_restore", nil)
	s2.handleSnapshotRestore(rr2, req2)
	assert.Equal(t, 200, rr2.Code)

	// 验证响应包含 restored_docs
	body := rr2.Body.String()
	assert.Contains(t, body, `"restored_docs"`)
	assert.Contains(t, body, `"expected_docs"`)

	// 验证恢复后文档 ID 都存在
	ids := eng2.AllIDs("books")
	assert.Len(t, ids, 2, "should have 2 docs after restore")

	// 验证可获取 source
	src1, ok := eng2.GetSource("books", "1")
	assert.True(t, ok, "doc 1 should exist")
	assert.Equal(t, "Go in Action", src1["title"])

	// 通过 HTTP handler 验证搜索
	searchBody := `{"query":{"match":{"title":"Go"}}}`
	rr3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("POST", "/books/_search", 
		strings.NewReader(searchBody))
	req3.Header.Set("Content-Type", "application/json")
	s2.handleSearchForNamePattern(rr3, req3, "books")
	assert.Equal(t, 200, rr3.Code)
	var searchResp map[string]interface{}
	err = json.Unmarshal(rr3.Body.Bytes(), &searchResp)
	assert.NoError(t, err)
	hits, ok := searchResp["hits"].(map[string]interface{})
	assert.True(t, ok)
	total, ok := hits["total"].(map[string]interface{})
	assert.True(t, ok)
	value, ok := total["value"].(float64)
	assert.True(t, ok)
	assert.Equal(t, float64(2), value, "search for 'Go' should hit 2 docs after restore")
}

// TestSnapshotRestore_RespondsWithDocCounts 恢复响应包含文档计数
func TestSnapshotRestore_RespondsWithDocCounts(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.Open(dir)
	assert.NoError(t, err)
	defer store.Close()
	eng := search.New(store)

	// 写入 3 个文档
	for i := 1; i <= 3; i++ {
		store.Put([]byte("doc/idx/"+string(rune('0'+i))), map[string]interface{}{"v": i})
		eng.IndexDoc("idx", string(rune('0'+i)), map[string]interface{}{"v": i})
	}
	store.Put(storage.SnapshotRepoKey("r"), map[string]interface{}{})

	s := &Server{store: store, engine: eng, snapDir: filepath.Join(dir, "snapshots")}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/_snapshot/r/snap1", nil)
	s.handleSnapshotCreate(rr, req)
	assert.Equal(t, 200, rr.Code)

	// 验证快照响应包含 doc_count
	createBody := rr.Body.String()
	assert.Contains(t, createBody, `"doc_count"`)

	// 恢复并验证响应
	store2, err := storage.Open(filepath.Join(dir, "data2"))
	assert.NoError(t, err)
	defer store2.Close()
	eng2 := search.New(store2)

	s2 := &Server{store: store2, engine: eng2, snapDir: filepath.Join(dir, "snapshots")}
	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/_snapshot/r/snap1/_restore", nil)
	s2.handleSnapshotRestore(rr2, req2)
	assert.Equal(t, 200, rr2.Code)

	// 验证恢复响应
	var resp map[string]interface{}
	err = json.Unmarshal(rr2.Body.Bytes(), &resp)
	assert.NoError(t, err)
	assert.Equal(t, true, resp["accepted"])
	assert.Equal(t, float64(3), resp["restored_docs"])
	assert.Equal(t, float64(3), resp["expected_docs"])
}

// splitLines 简单分行
func splitLines(s string) []string {
	var out []string
	start := 0
	for i, c := range s {
		if c == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
