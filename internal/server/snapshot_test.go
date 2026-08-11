// Package server - 快照/恢复 单元测试
package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// TestSnapshot_1000Docs 1000 条文档的真实快照 + 恢复压力测试
func TestSnapshot_1000Docs(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.Open(dir)
	assert.NoError(t, err)
	defer store.Close()
	eng := search.New(store)

	const numDocs = 1000

	// 生成 1000 条 mock 文档 (模拟真实业务数据)
	products := []string{"laptop", "phone", "tablet", "monitor", "keyboard", "mouse", "headphone", "camera", "printer", "speaker"}
	categories := []string{"electronics", "office", "gaming", "mobile", "accessories"}

	t.Logf("开始写入 %d 条 mock 文档...", numDocs)
	startWrite := time.Now()

	for i := 1; i <= numDocs; i++ {
		product := products[i%len(products)]
		category := categories[i%len(categories)]
		doc := map[string]interface{}{
			"name":        fmt.Sprintf("%s_%d", product, i),
			"description": fmt.Sprintf("High quality %s for %s users, model-%d, latest edition with premium features and extended warranty coverage up to 3 years", product, category, i),
			"price":       float64(i*10 + 99),
			"stock":       i * 7,
			"rating":      float64(3 + i%3) + 0.5,
			"category":    category,
			"tags":        []string{product, category, fmt.Sprintf("tag_%d", i%20)},
			"active":      i%3 != 0,
			"created_at":  fmt.Sprintf("2026-%02d-%02dT10:30:00Z", 1+i%12, 1+i%28),
			"metadata": map[string]interface{}{
				"weight":    float64(i%50) + 0.1,
				"color":     fmt.Sprintf("color_%d", i%10),
				"seller_id": fmt.Sprintf("seller_%03d", i%50),
			},
		}
		docID := fmt.Sprintf("doc_%04d", i)
		err := store.Put([]byte("doc/products/"+docID), doc)
		assert.NoError(t, err)
		eng.IndexDoc("products", docID, doc)
	}
	t.Logf("写入完成, 耗时 %v", time.Since(startWrite))

	// 建 repo
	err = store.Put(storage.SnapshotRepoKey("big_repo"), map[string]interface{}{"type": "fs"})
	assert.NoError(t, err)

	s := &Server{store: store, engine: eng, snapDir: filepath.Join(dir, "snapshots")}

	// 创建快照
	t.Log("开始创建快照...")
	startSnap := time.Now()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/_snapshot/big_repo/big_snap", nil)
	s.handleSnapshotCreate(rr, req)
	assert.Equal(t, 200, rr.Code)
	snapDuration := time.Since(startSnap)
	t.Logf("快照创建完成, 耗时 %v", snapDuration)

	// 验证快照响应
	var createResp map[string]interface{}
	err = json.Unmarshal(rr.Body.Bytes(), &createResp)
	assert.NoError(t, err)
	assert.Equal(t, true, createResp["accepted"])
	docCount := int(createResp["doc_count"].(float64))
	assert.Equal(t, numDocs, docCount)
	t.Logf("快照包含 %d 条文档", docCount)

	// 验证文件大小
	snapPath := s.snapshotPath("big_repo", "big_snap")
	snapInfo, err := os.Stat(snapPath)
	assert.NoError(t, err)
	snapSizeKB := float64(snapInfo.Size()) / 1024.0
	t.Logf("快照文件大小: %.1f KB", snapSizeKB)

	// 读取并验证文件行数(包含 meta 行)
	snapContent, err := os.ReadFile(snapPath)
	assert.NoError(t, err)
	lines := splitLines(string(snapContent))
	t.Logf("快照文件共 %d 行 (含 meta 行)", len(lines))
	assert.Equal(t, numDocs+1, len(lines), "应有 1000 条数据行 + 1 条 meta 行")

	// 验证最后一行是 meta 行
	var metaRow map[string]interface{}
	err = json.Unmarshal([]byte(lines[len(lines)-1]), &metaRow)
	assert.NoError(t, err)
	assert.Equal(t, "__snapshot_meta__", metaRow["key"])
	metaVal := metaRow["value"].(map[string]interface{})
	assert.Equal(t, float64(numDocs), metaVal["doc_count"])
	t.Logf("内嵌 meta 行验证通过: doc_count=%v", metaVal["doc_count"])

	// 删除原 store 中的所有产品数据
	t.Log("删除原数据...")
	for i := 1; i <= numDocs; i++ {
		docID := fmt.Sprintf("doc_%04d", i)
		_ = store.Delete([]byte("doc/products/"+docID))
	}
	// 清空倒排
	eng.LoadAll()
	// 验证已删除
	remaining := eng.AllIDs("products")
	assert.Equal(t, 0, len(remaining), "删除后应为 0 条")

	// 在新 store 上恢复
	store2, err := storage.Open(filepath.Join(dir, "restored"))
	assert.NoError(t, err)
	defer store2.Close()
	eng2 := search.New(store2)

	s2 := &Server{store: store2, engine: eng2, snapDir: filepath.Join(dir, "snapshots")}

	t.Log("开始恢复快照...")
	startRestore := time.Now()
	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/_snapshot/big_repo/big_snap/_restore", nil)
	s2.handleSnapshotRestore(rr2, req2)
	restoreDuration := time.Since(startRestore)
	t.Logf("恢复完成, 耗时 %v", restoreDuration)
	assert.Equal(t, 200, rr2.Code)

	// 验证恢复响应
	var restoreResp map[string]interface{}
	err = json.Unmarshal(rr2.Body.Bytes(), &restoreResp)
	assert.NoError(t, err)
	assert.Equal(t, true, restoreResp["accepted"])
	assert.Equal(t, float64(numDocs), restoreResp["restored_docs"])
	assert.Equal(t, float64(numDocs), restoreResp["expected_docs"])
	t.Logf("恢复响应: restored=%v, expected=%v", restoreResp["restored_docs"], restoreResp["expected_docs"])

	// 验证恢复后数据完整性
	restoredIDs := eng2.AllIDs("products")
	assert.Equal(t, numDocs, len(restoredIDs), "恢复后应有 1000 条")
	t.Logf("恢复后索引中有 %d 条文档", len(restoredIDs))

	// 抽样验证数据正确性
	sampleIDs := []string{"doc_0001", "doc_0100", "doc_0500", "doc_0999", "doc_1000"}
	for _, id := range sampleIDs {
		src, ok := eng2.GetSource("products", id)
		assert.True(t, ok, "doc %s should exist after restore", id)
		assert.NotNil(t, src["name"], "doc %s should have name", id)
		assert.NotNil(t, src["price"], "doc %s should have price", id)
		assert.NotNil(t, src["description"], "doc %s should have description", id)
	}
	t.Log("抽样验证通过: doc_0001, doc_0100, doc_0500, doc_0999, doc_1000")

	// 验证搜索功能
	searchBody := `{"query":{"match":{"name":"laptop_1"}},"size":5}`
	rr3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("POST", "/products/_search",
		strings.NewReader(searchBody))
	req3.Header.Set("Content-Type", "application/json")
	s2.handleSearchForNamePattern(rr3, req3, "products")
	assert.Equal(t, 200, rr3.Code)
	var searchResp map[string]interface{}
	err = json.Unmarshal(rr3.Body.Bytes(), &searchResp)
	assert.NoError(t, err)
	hits := searchResp["hits"].(map[string]interface{})
	total := hits["total"].(map[string]interface{})
	totalValue := int(total["value"].(float64))
	assert.Greater(t, totalValue, 0, "搜索 'laptop_1' 应至少命中 1 条")
	t.Logf("搜索 'laptop_1' 命中 %d 条", totalValue)

	// 验证分类聚合
	aggBody := `{"query":{"match_all":{}},"aggs":{"by_category":{"terms":{"field":"category"}}},"size":0}`
	rr4 := httptest.NewRecorder()
	req4 := httptest.NewRequest("POST", "/products/_search",
		strings.NewReader(aggBody))
	req4.Header.Set("Content-Type", "application/json")
	s2.handleSearchForNamePattern(rr4, req4, "products")
	assert.Equal(t, 200, rr4.Code)
	var aggResp map[string]interface{}
	err = json.Unmarshal(rr4.Body.Bytes(), &aggResp)
	assert.NoError(t, err)
	agg := aggResp["aggregations"].(map[string]interface{})
	byCat := agg["by_category"].(map[string]interface{})
	buckets := byCat["buckets"].([]interface{})
	assert.Greater(t, len(buckets), 0, "聚合桶应不为空")
	t.Logf("分类聚合: %d 个桶", len(buckets))

	// 性能总结
	t.Log("========== 1000 文档快照测试完成 ==========")
	t.Logf("写入耗时:     %v", time.Duration(0)) // 已记录
	t.Logf("快照耗时:     %v", snapDuration)
	t.Logf("恢复耗时:     %v", restoreDuration)
	t.Logf("快照文件大小: %.1f KB", snapSizeKB)
	t.Logf("恢复完整性:   %d/%d 文档", len(restoredIDs), numDocs)
}

// TestSnapshot_1000Docs_MultiIndex 多索引快照
func TestSnapshot_1000Docs_MultiIndex(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.Open(dir)
	assert.NoError(t, err)
	defer store.Close()
	eng := search.New(store)

	const numDocs = 1000
	indices := []string{"products", "orders", "reviews"}
	// 分配: 334 + 333 + 333 = 1000
	docsPerIndex := []int{334, 333, 333}

	t.Logf("开始写入 %d 条文档, 分布于 %d 个索引...", numDocs, len(indices))
	start := time.Now()

	globalID := 1
	for idxIdx, idx := range indices {
		for j := 0; j < docsPerIndex[idxIdx]; j++ {
			doc := map[string]interface{}{
				"title":    fmt.Sprintf("%s_doc_%d", idx, globalID),
				"content":  fmt.Sprintf("This is a %s document number %d with some additional text content for testing purposes", idx, globalID),
				"value":    float64(globalID * 10),
				"tags":     []string{idx, fmt.Sprintf("tag%d", globalID%10)},
				"active":   globalID%2 == 0,
				"priority": globalID % 5,
			}
			docID := fmt.Sprintf("%s_%04d", idx, j+1)
			err := store.Put([]byte("doc/"+idx+"/"+docID), doc)
			assert.NoError(t, err)
			eng.IndexDoc(idx, docID, doc)
			globalID++
		}
	}
	t.Logf("写入完成, 耗时 %v", time.Since(start))

	// 建 repo
	err = store.Put(storage.SnapshotRepoKey("multi_repo"), map[string]interface{}{"type": "fs"})
	assert.NoError(t, err)

	s := &Server{store: store, engine: eng, snapDir: filepath.Join(dir, "snapshots")}

	// 创建快照
	t.Log("创建多索引快照...")
	startSnap := time.Now()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/_snapshot/multi_repo/multi_snap", nil)
	s.handleSnapshotCreate(rr, req)
	assert.Equal(t, 200, rr.Code)
	t.Logf("快照创建完成, 耗时 %v", time.Since(startSnap))

	// 验证快照包含所有索引的文档
	var snapResp map[string]interface{}
	err = json.Unmarshal(rr.Body.Bytes(), &snapResp)
	assert.NoError(t, err)
	assert.Equal(t, float64(numDocs), snapResp["doc_count"])

	// 验证文件大小
	snapPath := s.snapshotPath("multi_repo", "multi_snap")
	snapInfo, _ := os.Stat(snapPath)
	t.Logf("多索引快照文件大小: %.1f KB", float64(snapInfo.Size())/1024.0)

	// 从新 store 恢复
	store2, err := storage.Open(filepath.Join(dir, "restored2"))
	assert.NoError(t, err)
	defer store2.Close()
	eng2 := search.New(store2)

	s2 := &Server{store: store2, engine: eng2, snapDir: filepath.Join(dir, "snapshots")}

	t.Log("恢复多索引快照...")
	startRestore := time.Now()
	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/_snapshot/multi_repo/multi_snap/_restore", nil)
	s2.handleSnapshotRestore(rr2, req2)
	t.Logf("恢复完成, 耗时 %v", time.Since(startRestore))
	assert.Equal(t, 200, rr2.Code)

	// 验证恢复响应
	var restoreResp map[string]interface{}
	err = json.Unmarshal(rr2.Body.Bytes(), &restoreResp)
	assert.NoError(t, err)
	assert.Equal(t, float64(numDocs), restoreResp["restored_docs"])
	assert.Equal(t, float64(numDocs), restoreResp["expected_docs"])

	// 验证每个索引的文档数
	totalRestored := 0
	for idxIdx, idx := range indices {
		count := len(eng2.AllIDs(idx))
		expected := docsPerIndex[idxIdx]
		assert.Equal(t, expected, count, "索引 %s 应恢复 %d 条, 实际 %d", idx, expected, count)
		totalRestored += count
		t.Logf("索引 %s: %d 条恢复成功", idx, count)
	}
	assert.Equal(t, numDocs, totalRestored)

	// 跨索引搜索验证
	searchBody := `{"query":{"match":{"title":"orders"}},"size":10}`
	rr3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("POST", "/orders/_search",
		strings.NewReader(searchBody))
	req3.Header.Set("Content-Type", "application/json")
	s2.handleSearchForNamePattern(rr3, req3, "orders")
	assert.Equal(t, 200, rr3.Code)

	t.Log("========== 1000 文档多索引测试完成 ==========")
}

// TestSnapshot_1000Docs_DeleteAndRecreate 快照删除 + 重新创建
func TestSnapshot_1000Docs_DeleteAndRecreate(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.Open(dir)
	assert.NoError(t, err)
	defer store.Close()
	eng := search.New(store)

	// 写入 1000 条
	for i := 1; i <= 1000; i++ {
		doc := map[string]interface{}{"id": i, "name": fmt.Sprintf("item_%d", i)}
		docID := fmt.Sprintf("item_%04d", i)
		store.Put([]byte("doc/inventory/"+docID), doc)
		eng.IndexDoc("inventory", docID, doc)
	}

	store.Put(storage.SnapshotRepoKey("inv_repo"), map[string]interface{}{})
	s := &Server{store: store, engine: eng, snapDir: filepath.Join(dir, "snapshots")}

	// 创建快照 1
	rr1 := httptest.NewRecorder()
	req1 := httptest.NewRequest("PUT", "/_snapshot/inv_repo/inv_snap_v1", nil)
	s.handleSnapshotCreate(rr1, req1)
	assert.Equal(t, 200, rr1.Code)

	snapPath := s.snapshotPath("inv_repo", "inv_snap_v1")
	_, err = os.Stat(snapPath)
	assert.NoError(t, err)

	// 删除快照 1
	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("DELETE", "/_snapshot/inv_repo/inv_snap_v1", nil)
	s.handleSnapshotDelete(rr2, req2)
	assert.Equal(t, 200, rr2.Code)

	_, err = os.Stat(snapPath)
	assert.True(t, os.IsNotExist(err), "删除后文件不应存在")

	// 验证已删除
	rr3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("GET", "/_snapshot/inv_repo/inv_snap_v1", nil)
	s.handleSnapshotGet(rr3, req3)
	assert.Equal(t, 404, rr3.Code)

	// 恢复已删除的快照 -> 404
	rr4 := httptest.NewRecorder()
	req4 := httptest.NewRequest("POST", "/_snapshot/inv_repo/inv_snap_v1/_restore", nil)
	s.handleSnapshotRestore(rr4, req4)
	assert.Equal(t, 404, rr4.Code)

	// 写入更多数据 (共 2000 条)
	for i := 1001; i <= 2000; i++ {
		doc := map[string]interface{}{"id": i, "name": fmt.Sprintf("item_%d", i)}
		docID := fmt.Sprintf("item_%04d", i)
		store.Put([]byte("doc/inventory/"+docID), doc)
		eng.IndexDoc("inventory", docID, doc)
	}

	// 创建快照 2 (2000 条)
	rr5 := httptest.NewRecorder()
	req5 := httptest.NewRequest("PUT", "/_snapshot/inv_repo/inv_snap_v2", nil)
	s.handleSnapshotCreate(rr5, req5)
	assert.Equal(t, 200, rr5.Code)

	var resp map[string]interface{}
	json.Unmarshal(rr5.Body.Bytes(), &resp)
	assert.Equal(t, float64(2000), resp["doc_count"])

	// 在新 store 恢复快照 2
	store2, _ := storage.Open(filepath.Join(dir, "restored_v2"))
	defer store2.Close()
	eng2 := search.New(store2)

	s2 := &Server{store: store2, engine: eng2, snapDir: filepath.Join(dir, "snapshots")}
	rr6 := httptest.NewRecorder()
	req6 := httptest.NewRequest("POST", "/_snapshot/inv_repo/inv_snap_v2/_restore", nil)
	s2.handleSnapshotRestore(rr6, req6)
	assert.Equal(t, 200, rr6.Code)

	restored := eng2.AllIDs("inventory")
	assert.Equal(t, 2000, len(restored), "v2 快照应恢复 2000 条")
	t.Logf("快照 2 恢复: %d 条文档", len(restored))
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
