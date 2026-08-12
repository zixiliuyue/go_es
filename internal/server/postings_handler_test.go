// Package server - postings-snapshot 端点测试 (#7)
package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestPostingsFlush_Endpoint POST /{index}/_postings/flush 基本流程
func TestPostingsFlush_Endpoint(t *testing.T) {
	ts := newTestServer(t)
	// 建索引 + 写 doc
	do(t, ts, http.MethodPut, "/idx", nil)
	do(t, ts, http.MethodPut, "/idx/_doc/1", map[string]interface{}{"title": "hello world"})
	do(t, ts, http.MethodPut, "/idx/_doc/2", map[string]interface{}{"title": "hello fox"})

	// flush snapshot
	resp, body := do(t, ts, http.MethodPost, "/idx/_postings/flush", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	var flushResp map[string]interface{}
	assert.NoError(t, json.Unmarshal(body, &flushResp))
	assert.Equal(t, "idx", flushResp["index"])
	assert.EqualValues(t, 1, flushResp["fields_flushed"], "title = 1 field")

	// 查 snapshot info
	resp, body = do(t, ts, http.MethodGet, "/idx/_postings/snapshot", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	var info map[string]interface{}
	assert.NoError(t, json.Unmarshal(body, &info))
	assert.Equal(t, "idx", info["index"])
	assert.Equal(t, true, info["has_snapshot"])
	assert.Equal(t, true, info["version_match"])
	assert.EqualValues(t, 1, info["field_count"])
}

// TestPostingsFlush_EmptyIndex 空索引 flush 不报错
func TestPostingsFlush_EmptyIndex(t *testing.T) {
	ts := newTestServer(t)
	do(t, ts, http.MethodPut, "/empty", nil)

	resp, body := do(t, ts, http.MethodPost, "/empty/_postings/flush", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	var flushResp map[string]interface{}
	assert.NoError(t, json.Unmarshal(body, &flushResp))
	assert.EqualValues(t, 0, flushResp["fields_flushed"])
}

// TestPostingsSnapshot_NoSnapshot 索引无 snapshot 时 GET 返回 has_snapshot=false
func TestPostingsSnapshot_NoSnapshot(t *testing.T) {
	ts := newTestServer(t)
	do(t, ts, http.MethodPut, "/idx", nil)
	do(t, ts, http.MethodPut, "/idx/_doc/1", map[string]interface{}{"title": "hello"})

	resp, body := do(t, ts, http.MethodGet, "/idx/_postings/snapshot", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	var info map[string]interface{}
	assert.NoError(t, json.Unmarshal(body, &info))
	assert.Equal(t, false, info["has_snapshot"])
}

// TestPostingsFlush_VersionMismatch 写后 snapshot 版本不匹配
func TestPostingsFlush_VersionMismatch(t *testing.T) {
	ts := newTestServer(t)
	do(t, ts, http.MethodPut, "/idx", nil)
	do(t, ts, http.MethodPut, "/idx/_doc/1", map[string]interface{}{"title": "hello"})

	// flush
	resp, _ := do(t, ts, http.MethodPost, "/idx/_postings/flush", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// 再写一个 doc, version 递增
	do(t, ts, http.MethodPut, "/idx/_doc/2", map[string]interface{}{"title": "world"})

	// snapshot 应不匹配
	resp, body := do(t, ts, http.MethodGet, "/idx/_postings/snapshot", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	var info map[string]interface{}
	assert.NoError(t, json.Unmarshal(body, &info))
	assert.Equal(t, true, info["has_snapshot"])
	assert.Equal(t, false, info["version_match"])
}

// TestPostingsFlush_BM25StillWorks flush 后搜索仍能 BM25 打分
func TestPostingsFlush_BM25StillWorks(t *testing.T) {
	ts := newTestServer(t)
	do(t, ts, http.MethodPut, "/idx", nil)
	do(t, ts, http.MethodPut, "/idx/_doc/1", map[string]interface{}{"title": "hello world hello"})
	do(t, ts, http.MethodPut, "/idx/_doc/2", map[string]interface{}{"title": "hello fox"})

	// flush 不影响在线查询 (snapshot 只是额外写盘)
	resp, _ := do(t, ts, http.MethodPost, "/idx/_postings/flush", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// 搜索仍正常
	resp, body := do(t, ts, http.MethodPost, "/idx/_search", map[string]interface{}{
		"query": map[string]interface{}{
			"match": map[string]interface{}{"title": "hello"},
		},
	})
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	var searchResp map[string]interface{}
	assert.NoError(t, json.Unmarshal(body, &searchResp))
	hits := searchResp["hits"].(map[string]interface{})["hits"].([]interface{})
	assert.Equal(t, 2, len(hits), "hello 应命中 2 个 doc")
}
