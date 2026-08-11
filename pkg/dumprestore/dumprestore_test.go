// dumprestore 包单元/集成测试
// 使用 httptest 启动真实的 go_es 实例做端到端 round-trip
package dumprestore

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zixiliuyue/go_es/internal/search"
	"github.com/zixiliuyue/go_es/internal/storage"
	"github.com/zixiliuyue/go_es/internal/server"
	"go.uber.org/zap"
)

// newTestServer 启动真实的 go_es 实例(内存存储)
// 返回字符串 URL,失败时在 t 上报错
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	store, err := storage.Open("")
	require.NoError(t, err)
	engine := search.New(store)
	srv := server.New(store, engine, zap.NewNop())
	srv.MarkStartupDone()
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = store.Close()
	})
	return ts
}

// ensureIndex 创建索引(不存在时),断言成功
func ensureIndex(t *testing.T, ts *httptest.Server, idx string) {
	t.Helper()
	_, code, err := doHTTP(t, ts, "PUT", "/"+idx, nil)
	require.NoError(t, err)
	assert.Equal(t, 200, code)
}

// doHTTP 辅助: 发请求, 返回 (响应体, 状态码, 错误)
func doHTTP(t *testing.T, ts *httptest.Server, method, path string, body interface{}) ([]byte, int, error) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, ts.URL+path, rd)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return raw, resp.StatusCode, nil
}

// ---------- 基础工具 ----------

// TestMetaMarker 验证 meta marker 常量稳定
func TestMetaMarker(t *testing.T) {
	assert.Equal(t, "__dump_meta__", metaMarker)
	assert.Equal(t, 1, dumpVersion)
}

// TestParseMeta 验证元数据行解析
func TestParseMeta(t *testing.T) {
	raw := `{"_marker":"__dump_meta__","version":1,"doc_count":10,"index_count":2,"created_at":"2026-08-11T00:00:00Z"}`
	var m map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(raw), &m))
	assert.Equal(t, metaMarker, m["_marker"])

	var em ExportMeta
	require.NoError(t, json.Unmarshal([]byte(raw), &em))
	assert.Equal(t, 1, em.Version)
	assert.Equal(t, 10, em.DocCount)
	assert.Equal(t, 2, em.IndexCount)
}

// ---------- Exporter ----------

// TestNewExporter_Defaults 验证默认值填充
func TestNewExporter_Defaults(t *testing.T) {
	e := NewExporter(ExporterConfig{BaseURL: "http://x"})
	assert.Equal(t, defaultPageSize, e.cfg.PageSize)
	assert.NotNil(t, e.cli)
}

// TestNewExporter_CustomPageSize 自定义 page-size 保留
func TestNewExporter_CustomPageSize(t *testing.T) {
	e := NewExporter(ExporterConfig{BaseURL: "http://x", PageSize: 50})
	assert.Equal(t, 50, e.cfg.PageSize)
}

// TestExporter_RoundTrip 完整导出/导入闭环
func TestExporter_RoundTrip(t *testing.T) {
	ts := newTestServer(t)
	ctx := testCtx(t, 10*time.Second)

	// 先建索引
	ensureIndex(t, ts, "idx_a")
	ensureIndex(t, ts, "idx_b")

	// 写 5 条文档到 2 个索引
	for i := 0; i < 3; i++ {
		_, code, err := doHTTP(t, ts, "PUT", "/idx_a/_doc/a"+string(rune('0'+i)), map[string]interface{}{"v": i, "t": "a"})
		require.NoError(t, err)
		assertDocCreated(t, code)
	}
	for i := 0; i < 2; i++ {
		_, code, err := doHTTP(t, ts, "PUT", "/idx_b/_doc/b"+string(rune('0'+i)), map[string]interface{}{"v": i, "t": "b"})
		require.NoError(t, err)
		assertDocCreated(t, code)
	}

	// 导出
	out := filepath.Join(t.TempDir(), "dump.ndjson")
	n, err := NewExporter(ExporterConfig{BaseURL: ts.URL}).Run(ctx, out)
	require.NoError(t, err)
	assert.Equal(t, 5, n)

	// 验证文件行数:5 文档 + 1 元数据
	data, err := os.ReadFile(out)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	assert.Equal(t, 6, len(lines))

	// 最后一行应该是 meta
	var em ExportMeta
	require.NoError(t, json.Unmarshal([]byte(lines[len(lines)-1]), &em))
	assert.Equal(t, metaMarker, em.Marker)
	assert.Equal(t, 5, em.DocCount)

	// 清理源索引
	_, code, err := doHTTP(t, ts, "DELETE", "/idx_a", nil)
	require.NoError(t, err)
	assert.Equal(t, 200, code)
	_, code, err = doHTTP(t, ts, "DELETE", "/idx_b", nil)
	require.NoError(t, err)
	assert.Equal(t, 200, code)

	// 重新导入
	restored, errs, meta, err := NewImporter(ImporterConfig{BaseURL: ts.URL, BatchSize: 2}).Run(ctx, out)
	require.NoError(t, err)
	assert.Equal(t, 5, restored)
	assert.Equal(t, 0, errs)
	require.NotNil(t, meta)
	assert.Equal(t, 5, meta.DocCount)

	// 验证数据恢复正确
	body, code, err := doHTTP(t, ts, "POST", "/idx_a/_search", map[string]interface{}{"query": map[string]interface{}{"match_all": map[string]interface{}{}}})
	require.NoError(t, err)
	assert.Equal(t, 200, code)
	var sr struct {
		Hits struct {
			Total struct {
				Value int `json:"value"`
			} `json:"total"`
		} `json:"hits"`
	}
	require.NoError(t, json.Unmarshal(body, &sr))
	assert.Equal(t, 3, sr.Hits.Total.Value)

	body, code, err = doHTTP(t, ts, "POST", "/idx_b/_search", map[string]interface{}{"query": map[string]interface{}{"match_all": map[string]interface{}{}}})
	require.NoError(t, err)
	assert.Equal(t, 200, code)
	require.NoError(t, json.Unmarshal(body, &sr))
	assert.Equal(t, 2, sr.Hits.Total.Value)
}

// TestExporter_IndexFilter 按指定索引导出
func TestExporter_IndexFilter(t *testing.T) {
	ts := newTestServer(t)
	ctx := testCtx(t, 10*time.Second)

	ensureIndex(t, ts, "idx_a")
	ensureIndex(t, ts, "idx_b")

	var code int
	_, code, _ = doHTTP(t, ts, "PUT", "/idx_a/_doc/a1", map[string]interface{}{"v": 1})
	assertDocCreated(t, code)
	_, code, _ = doHTTP(t, ts, "PUT", "/idx_b/_doc/b1", map[string]interface{}{"v": 2})
	assertDocCreated(t, code)

	out := filepath.Join(t.TempDir(), "partial.ndjson")
	n, err := NewExporter(ExporterConfig{BaseURL: ts.URL, Indices: []string{"idx_a"}}).Run(ctx, out)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	data, _ := os.ReadFile(out)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	assert.Len(t, lines, 2) // 1 doc + 1 meta
	var d ExportedDoc
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &d))
	assert.Equal(t, "idx_a", d.Index)
}

// TestExporter_EmptyIndex 空索引时导出返回 0 + 非 nil error (无数据可导出)
func TestExporter_EmptyIndex(t *testing.T) {
	ts := newTestServer(t)
	ctx := testCtx(t, 5*time.Second)

	out := filepath.Join(t.TempDir(), "empty.ndjson")
	_, err := NewExporter(ExporterConfig{BaseURL: ts.URL}).Run(ctx, out)
	assert.Error(t, err) // 无索引时报 "no indices to export"
}

// TestExporter_ProgressCallback 进度回调
func TestExporter_ProgressCallback(t *testing.T) {
	ts := newTestServer(t)
	ctx := testCtx(t, 10*time.Second)

	ensureIndex(t, ts, "idx_p")

	for i := 0; i < 5; i++ {
		_, code, _ := doHTTP(t, ts, "PUT", "/idx_p/_doc/d"+string(rune('0'+i)), map[string]interface{}{"v": i})
		assertDocCreated(t, code)
	}

	var counts []int
	out := filepath.Join(t.TempDir(), "progress.ndjson")
	n, err := NewExporter(ExporterConfig{
		BaseURL:  ts.URL,
		PageSize: 2,
		Progress: func(i int) { counts = append(counts, i) },
	}).Run(ctx, out)
	require.NoError(t, err)
	assert.Equal(t, 5, n)
	assert.NotEmpty(t, counts)
	assert.Equal(t, 5, counts[len(counts)-1])
}

// ---------- Importer ----------

// TestNewImporter_Defaults 默认值填充
func TestNewImporter_Defaults(t *testing.T) {
	im := NewImporter(ImporterConfig{BaseURL: "http://x"})
	assert.Equal(t, defaultBatchSize, im.cfg.BatchSize)
	assert.NotNil(t, im.cli)
}

// TestImporter_TargetIndex 强制覆盖索引名
func TestImporter_TargetIndex(t *testing.T) {
	ts := newTestServer(t)
	ctx := testCtx(t, 10*time.Second)

	ensureIndex(t, ts, "src_idx")

	_, code, _ := doHTTP(t, ts, "PUT", "/src_idx/_doc/d1", map[string]interface{}{"v": 1})
	assertDocCreated(t, code)

	out := filepath.Join(t.TempDir(), "target.ndjson")
	n, err := NewExporter(ExporterConfig{BaseURL: ts.URL, Indices: []string{"src_idx"}}).Run(ctx, out)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	// 清理源索引, 验证 restore 到新索引
	_, code, _ = doHTTP(t, ts, "DELETE", "/src_idx", nil)
	assert.Equal(t, 200, code)

	restored, errs, meta, err := NewImporter(ImporterConfig{BaseURL: ts.URL, TargetIndex: "dst_idx"}).Run(ctx, out)
	require.NoError(t, err)
	assert.Equal(t, 1, restored)
	assert.Equal(t, 0, errs)
	require.NotNil(t, meta)

	body, code, _ := doHTTP(t, ts, "POST", "/dst_idx/_search", map[string]interface{}{"query": map[string]interface{}{"match_all": map[string]interface{}{}}})
	assert.Equal(t, 200, code)
	var sr2 struct {
		Hits struct {
			Total struct {
				Value int `json:"value"`
			} `json:"total"`
			Hits []struct {
				ID string `json:"_id"`
			} `json:"hits"`
		} `json:"hits"`
	}
	require.NoError(t, json.Unmarshal(body, &sr2))
	assert.Equal(t, 1, sr2.Hits.Total.Value)
	assert.Equal(t, "d1", sr2.Hits.Hits[0].ID)
}

// TestImporter_EmptyFile 空文件返回 0
func TestImporter_EmptyFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "empty.ndjson")
	require.NoError(t, os.WriteFile(p, []byte(""), 0600))
	ts := newTestServer(t)
	ctx := testCtx(t, 5*time.Second)
	restored, errs, meta, err := NewImporter(ImporterConfig{BaseURL: ts.URL}).Run(ctx, p)
	require.NoError(t, err)
	assert.Equal(t, 0, restored)
	assert.Equal(t, 0, errs)
	assert.Nil(t, meta)
}

// TestImporter_MetaOnly 只有 meta 行的文件被忽略
func TestImporter_MetaOnly(t *testing.T) {
	p := filepath.Join(t.TempDir(), "meta_only.ndjson")
	metaLine, _ := json.Marshal(ExportMeta{Marker: metaMarker, Version: 1, DocCount: 5})
	require.NoError(t, os.WriteFile(p, append(metaLine, '\n'), 0600))
	ts := newTestServer(t)
	ctx := testCtx(t, 5*time.Second)
	restored, errs, meta, err := NewImporter(ImporterConfig{BaseURL: ts.URL}).Run(ctx, p)
	require.NoError(t, err)
	assert.Equal(t, 0, restored)
	assert.Equal(t, 0, errs)
	require.NotNil(t, meta)
	assert.Equal(t, 5, meta.DocCount)
}

// TestImporter_MissingIndexOrID 缺字段记录计入错误数
func TestImporter_MissingIndexOrID(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.ndjson")
	var b strings.Builder
	b.WriteString(`{"_source":{"v":1}}` + "\n")
	b.WriteString(`{"_index":"x","_source":{"v":2}}` + "\n")
	require.NoError(t, os.WriteFile(p, []byte(b.String()), 0600))
	ts := newTestServer(t)
	ctx := testCtx(t, 5*time.Second)
	restored, errs, _, err := NewImporter(ImporterConfig{BaseURL: ts.URL, BatchSize: 1}).Run(ctx, p)
	require.NoError(t, err)
	assert.Equal(t, 0, restored)
	assert.Equal(t, 2, errs)
}

// ---------- 便捷函数 ----------

// TestDumpToFile 验证便捷函数
func TestDumpToFile(t *testing.T) {
	ts := newTestServer(t)
	ctx := testCtx(t, 10*time.Second)
	ensureIndex(t, ts, "idx_c")
	_, code, _ := doHTTP(t, ts, "PUT", "/idx_c/_doc/c1", map[string]interface{}{"v": 1})
	assertDocCreated(t, code)
	_, code, _ = doHTTP(t, ts, "PUT", "/idx_c/_doc/c2", map[string]interface{}{"v": 2})
	assertDocCreated(t, code)

	out := filepath.Join(t.TempDir(), "c.ndjson")
	n, err := DumpToFile(ctx, ts.URL, out, []string{"idx_c"})
	require.NoError(t, err)
	assert.Equal(t, 2, n)
}

// TestRestoreFromFile 验证便捷函数
func TestRestoreFromFile(t *testing.T) {
	ts := newTestServer(t)
	ctx := testCtx(t, 10*time.Second)
	ensureIndex(t, ts, "idx_d")
	_, code, _ := doHTTP(t, ts, "PUT", "/idx_d/_doc/d1", map[string]interface{}{"v": 1})
	assertDocCreated(t, code)

	out := filepath.Join(t.TempDir(), "d.ndjson")
	n, err := DumpToFile(ctx, ts.URL, out, []string{"idx_d"})
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	_, code, _ = doHTTP(t, ts, "DELETE", "/idx_d", nil)
	assert.Equal(t, 200, code)

	restored, errs, meta, err := RestoreFromFile(ctx, ts.URL, out, "")
	require.NoError(t, err)
	assert.Equal(t, 1, restored)
	assert.Equal(t, 0, errs)
	assert.NotNil(t, meta)
}

// ---------- 进阶测试 ----------

// TestExporter_Pagination 验证大 page-size 场景翻页正确
func TestExporter_Pagination(t *testing.T) {
	ts := newTestServer(t)
	ctx := testCtx(t, 10*time.Second)
	ensureIndex(t, ts, "idx_pg")
	for i := 0; i < 15; i++ {
		_, code, _ := doHTTP(t, ts, "PUT", "/idx_pg/_doc/d"+string(rune('0'+i)), map[string]interface{}{"v": i})
		assertDocCreated(t, code)
	}
	out := filepath.Join(t.TempDir(), "pg.ndjson")
	n, err := NewExporter(ExporterConfig{BaseURL: ts.URL, PageSize: 4}).Run(ctx, out)
	require.NoError(t, err)
	assert.Equal(t, 15, n)
}

// TestExporter_DuplicateIndicesList 重复索引名去重
func TestExporter_DuplicateIndicesList(t *testing.T) {
	ts := newTestServer(t)
	ctx := testCtx(t, 5*time.Second)
	// 用一个 mock 覆盖实际 resolve 流程的一部分: 直接给 cfg.Indices 走分支
	ensureIndex(t, ts, "idx_x")
	_, code, _ := doHTTP(t, ts, "PUT", "/idx_x/_doc/x1", map[string]interface{}{"v": 1})
	assertDocCreated(t, code)

	out := filepath.Join(t.TempDir(), "dup.ndjson")
	n, err := NewExporter(ExporterConfig{BaseURL: ts.URL, Indices: []string{"idx_x", "idx_x", "idx_x"}}).Run(ctx, out)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

// TestExporter_InvalidURL 非法 baseURL 立即报错
func TestExporter_InvalidURL(t *testing.T) {
	ctx := testCtx(t, 5*time.Second)
	out := filepath.Join(t.TempDir(), "inv.ndjson")
	// 启一个空 baseURL, 确保 http 调用时必然报错
	_, err := NewExporter(ExporterConfig{BaseURL: "http://localhost:1"}).Run(ctx, out)
	assert.Error(t, err)
}

// TestImporter_InvalidFile 无法解析的文件返回错误
func TestImporter_InvalidFile(t *testing.T) {
	// 不存在的文件
	ts := newTestServer(t)
	ctx := testCtx(t, 5*time.Second)
	_, _, _, err := NewImporter(ImporterConfig{BaseURL: ts.URL}).Run(ctx, "/tmp/nonexistent-xyz.ndjson")
	assert.Error(t, err)
}

// TestImporter_BadJSON 格式错误的 NDJSON 返回解析错误
func TestImporter_BadJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "badjson.ndjson")
	require.NoError(t, os.WriteFile(p, []byte(`not a json line`+"\n"), 0600))
	ts := newTestServer(t)
	ctx := testCtx(t, 5*time.Second)
	_, _, _, err := NewImporter(ImporterConfig{BaseURL: ts.URL}).Run(ctx, p)
	assert.Error(t, err)
}

// TestImporter_ContextCancelled ctx 已取消时提前返回
func TestImporter_ContextCancelled(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cancel.ndjson")
	var b strings.Builder
	for i := 0; i < 100; i++ {
		b.WriteString(`{"_index":"x","_id":"d` + string(rune('0'+i)) + `","_source":{"v":` + string(rune('0'+i)) + `}}` + "\n")
	}
	require.NoError(t, os.WriteFile(p, []byte(b.String()), 0600))
	ts := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消
	_, _, _, err := NewImporter(ImporterConfig{BaseURL: ts.URL, BatchSize: 1}).Run(ctx, p)
	assert.Error(t, err)
}

// TestImporter_HTTPError 错误 URL 触发 httpError
func TestImporter_HTTPError(t *testing.T) {
	// 启一个 404 的服务
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found"))
	}))
	defer fake.Close()
	p := filepath.Join(t.TempDir(), "http.ndjson")
	require.NoError(t, os.WriteFile(p, []byte(`{"_index":"x","_id":"d1","_source":{"v":1}}`+"\n"), 0600))
	ctx := testCtx(t, 5*time.Second)
	_, _, _, err := NewImporter(ImporterConfig{BaseURL: fake.URL}).Run(ctx, p)
	assert.Error(t, err)
	var he *httpError
	assert.ErrorAs(t, err, &he)
	assert.Equal(t, http.StatusNotFound, he.Status)
}

// TestExportMeta_ZeroValues 验证零值 ExportMeta 行为
func TestExportMeta_ZeroValues(t *testing.T) {
	var m ExportMeta
	assert.Equal(t, "", m.Marker)
	assert.Equal(t, 0, m.Version)
	assert.True(t, m.CreatedAt.IsZero())
}

// TestHTTPError_Message 验证错误消息
func TestHTTPError_Message(t *testing.T) {
	e := &httpError{Status: 400, Body: "bad"}
	assert.Contains(t, e.Error(), "HTTP 400")
	assert.Contains(t, e.Error(), "bad")
}

// TestImporter_ProgressCallback 导入进度回调
func TestImporter_ProgressCallback(t *testing.T) {
	ts := newTestServer(t)
	ctx := testCtx(t, 10*time.Second)
	ensureIndex(t, ts, "idx_prog")

	// 先 dump 一些数据
	for i := 0; i < 3; i++ {
		_, code, _ := doHTTP(t, ts, "PUT", "/idx_prog/_doc/d"+string(rune('0'+i)), map[string]interface{}{"v": i})
		assertDocCreated(t, code)
	}
	out := filepath.Join(t.TempDir(), "prog.ndjson")
	_, err := NewExporter(ExporterConfig{BaseURL: ts.URL, Indices: []string{"idx_prog"}}).Run(ctx, out)
	require.NoError(t, err)

	// 删掉源索引
	_, code, _ := doHTTP(t, ts, "DELETE", "/idx_prog", nil)
	assertOK(t, code)

	var progresses [][2]int
	_, _, _, err = NewImporter(ImporterConfig{
		BaseURL:   ts.URL,
		BatchSize: 2,
		Progress: func(r, e int) { progresses = append(progresses, [2]int{r, e}) },
	}).Run(ctx, out)
	require.NoError(t, err)
	assert.NotEmpty(t, progresses)
}

// ---------- helpers ----------

// testCtx 创建带超时的 context, 并在 t 结束时取消
func testCtx(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// assertDocCreated 断言写文档返回 201 (首次创建)
func assertDocCreated(t *testing.T, code int) {
	t.Helper()
	assert.Equal(t, 201, code)
}

// assertOK 断言请求返回 200 (索引/查询)
func assertOK(t *testing.T, code int) {
	t.Helper()
	assert.Equal(t, 200, code)
}
