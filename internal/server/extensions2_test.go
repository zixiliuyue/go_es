// internal/server 阶段 2+3 扩展能力测试
// 覆盖: 跨索引通配 / Web UI / gzip Vary 头
// 模式匹配通过 HTTP 行为验证(端到端), 不做白盒单测
package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSearch_WildcardPatternAll(t *testing.T) {
	ts := newTestServer(t)
	// 建 3 个索引, 写 1 条
	for _, n := range []string{"idx_a1", "idx_a2", "idx_b1"} {
		_ = doMust(t, ts, "PUT", "/"+n, nil)
		_ = doMust(t, ts, "PUT", "/"+n+"/_doc/x", map[string]interface{}{"k": "v"})
	}
	// 跨 idx_* 搜索 -> 应命中 3 条
	resp, body := do(t, ts, "POST", "/idx_*/_search", map[string]interface{}{
		"query": map[string]interface{}{"match_all": map[string]interface{}{}},
		"size":  20,
	})
	assert.Equal(t, 200, resp.StatusCode)
	var parsed struct {
		Hits struct {
			Total struct {
				Value int `json:"value"`
			} `json:"total"`
		} `json:"hits"`
	}
	assert.NoError(t, jsonUnmarshal(body, &parsed))
	assert.Equal(t, 3, parsed.Hits.Total.Value)
}

func TestSearch_WildcardPatternPrefix(t *testing.T) {
	ts := newTestServer(t)
	for _, n := range []string{"articles_v1", "articles_v2", "products"} {
		_ = doMust(t, ts, "PUT", "/"+n, nil)
		_ = doMust(t, ts, "PUT", "/"+n+"/_doc/1", map[string]interface{}{"k": "v"})
	}
	resp, body := do(t, ts, "POST", "/articles*/_search", map[string]interface{}{
		"query": map[string]interface{}{"match_all": map[string]interface{}{}},
	})
	assert.Equal(t, 200, resp.StatusCode)
	var parsed struct {
		Hits struct {
			Total struct {
				Value int `json:"value"`
			} `json:"total"`
		} `json:"hits"`
	}
	assert.NoError(t, jsonUnmarshal(body, &parsed))
	assert.Equal(t, 2, parsed.Hits.Total.Value)
}

func TestSearch_WildcardPatternSuffix(t *testing.T) {
	ts := newTestServer(t)
	for _, n := range []string{"a_v1", "a_v2", "b_v2"} {
		_ = doMust(t, ts, "PUT", "/"+n, nil)
		_ = doMust(t, ts, "PUT", "/"+n+"/_doc/1", map[string]interface{}{"k": "v"})
	}
	resp, body := do(t, ts, "POST", "/*_v2/_search", map[string]interface{}{
		"query": map[string]interface{}{"match_all": map[string]interface{}{}},
	})
	assert.Equal(t, 200, resp.StatusCode)
	var parsed struct {
		Hits struct {
			Total struct {
				Value int `json:"value"`
			} `json:"total"`
		} `json:"hits"`
	}
	assert.NoError(t, jsonUnmarshal(body, &parsed))
	assert.Equal(t, 2, parsed.Hits.Total.Value)
}

func TestSearch_WildcardPatternExclude(t *testing.T) {
	ts := newTestServer(t)
	for _, n := range []string{"a1", "a2", "b1", "b2"} {
		_ = doMust(t, ts, "PUT", "/"+n, nil)
		_ = doMust(t, ts, "PUT", "/"+n+"/_doc/1", map[string]interface{}{"k": "v"})
	}
	// * 减去 b* -> 应命中 2 (a1, a2)
	resp, body := do(t, ts, "POST", "/*,-b*/_search", map[string]interface{}{
		"query": map[string]interface{}{"match_all": map[string]interface{}{}},
	})
	assert.Equal(t, 200, resp.StatusCode)
	var parsed struct {
		Hits struct {
			Total struct {
				Value int `json:"value"`
			} `json:"total"`
		} `json:"hits"`
	}
	assert.NoError(t, jsonUnmarshal(body, &parsed))
	assert.Equal(t, 2, parsed.Hits.Total.Value)
}

func TestSearch_WildcardPatternExactList(t *testing.T) {
	ts := newTestServer(t)
	for _, n := range []string{"a", "b", "c"} {
		_ = doMust(t, ts, "PUT", "/"+n, nil)
		_ = doMust(t, ts, "PUT", "/"+n+"/_doc/1", map[string]interface{}{"k": "v"})
	}
	resp, body := do(t, ts, "POST", "/a,c/_search", map[string]interface{}{
		"query": map[string]interface{}{"match_all": map[string]interface{}{}},
	})
	assert.Equal(t, 200, resp.StatusCode)
	var parsed struct {
		Hits struct {
			Total struct {
				Value int `json:"value"`
			} `json:"total"`
		} `json:"hits"`
	}
	assert.NoError(t, jsonUnmarshal(body, &parsed))
	assert.Equal(t, 2, parsed.Hits.Total.Value)
}

func TestSearch_WildcardPattern_NoMatch(t *testing.T) {
	ts := newTestServer(t)
	_ = doMust(t, ts, "PUT", "/foo", nil)
	resp, body := do(t, ts, "POST", "/nonexistent*/_search", map[string]interface{}{
		"query": map[string]interface{}{"match_all": map[string]interface{}{}},
	})
	assert.Equal(t, 200, resp.StatusCode)
	var parsed struct {
		Hits struct {
			Total struct {
				Value int `json:"value"`
			} `json:"total"`
		} `json:"hits"`
	}
	assert.NoError(t, jsonUnmarshal(body, &parsed))
	assert.Equal(t, 0, parsed.Hits.Total.Value)
}

func TestUI_PageLoads(t *testing.T) {
	ts := newTestServer(t)
	resp, body := do(t, ts, "GET", "/_ui", nil)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, string(body), "<!doctype html>")
	assert.Contains(t, string(body), "go_es · 控制台")

	resp, body = do(t, ts, "GET", "/_ui/index.html", nil)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, string(body), "<!doctype html>")
}

func TestUI_NoAuthRequired(t *testing.T) {
	ts := newTestServerWith(t, ServerOptions{
		Auth: AuthConfig{
			Enabled: true,
			Basic:   map[string]string{"u": "p"},
		},
	})
	// 不带 auth 头访问 /_ui
	req, _ := http.NewRequest("GET", ts.URL+"/_ui", nil)
	r, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	defer func() { _ = r.Body.Close() }()
	assert.Equal(t, 200, r.StatusCode)
}

func TestGzip_VaryHeaderSet(t *testing.T) {
	ts := newTestServer(t)
	req, _ := http.NewRequest("GET", ts.URL+"/_health/liveness", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	r, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	defer func() { _ = r.Body.Close() }()
	assert.Equal(t, 200, r.StatusCode)
	vary := r.Header.Get("Vary")
	// 我们中间件只加 Vary: Accept-Encoding, 不做实际压缩
	assert.Contains(t, vary, "Accept-Encoding")
}

func TestGzip_MetricsSkipped(t *testing.T) {
	ts := newTestServer(t)
	req, _ := http.NewRequest("GET", ts.URL+"/metrics", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	r, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	defer func() { _ = r.Body.Close() }()
	assert.Equal(t, 200, r.StatusCode)
}

func TestGzip_RealCompression(t *testing.T) {
	ts := newTestServer(t)
	// 建索引 + 写大文档, 触发压缩 (>512B)
	_ = doMust(t, ts, "PUT", "/big", nil)
	big := make(map[string]interface{})
	big["payload"] = strings.Repeat("abcdefgh", 500) // 4KB
	_ = doMust(t, ts, "PUT", "/big/_doc/1", big)

	// 不带 Accept-Encoding -> 明文
	req, _ := http.NewRequest("GET", ts.URL+"/big/_doc/1", nil)
	r1, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	rawBody, _ := io.ReadAll(r1.Body)
	_ = r1.Body.Close()
	assert.Equal(t, 200, r1.StatusCode)
	assert.Empty(t, r1.Header.Get("Content-Encoding"), "no Accept-Encoding should not compress")
	assert.Contains(t, string(rawBody), "abcdefgh")

	// 带 Accept-Encoding: gzip -> 应真正压缩
	req, _ = http.NewRequest("GET", ts.URL+"/big/_doc/1", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	r2, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	defer func() { _ = r2.Body.Close() }()
	assert.Equal(t, 200, r2.StatusCode)
	assert.Equal(t, "gzip", r2.Header.Get("Content-Encoding"))
	assert.Contains(t, r2.Header.Get("Vary"), "Accept-Encoding")
	// 压缩后字节数应小于原文
	gzBody, _ := io.ReadAll(r2.Body)
	assert.Less(t, len(gzBody), len(rawBody), "gzip body should be smaller than raw")
	// 真正解压后内容仍正确
	gz, err := gzip.NewReader(bytes.NewReader(gzBody))
	assert.NoError(t, err)
	decoded, _ := io.ReadAll(gz)
	_ = gz.Close()
	assert.Contains(t, string(decoded), "abcdefgh")
}

func TestGzip_4xxNotCompressed(t *testing.T) {
	ts := newTestServer(t)
	req, _ := http.NewRequest("GET", ts.URL+"/nonexistent_index", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	r, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	defer func() { _ = r.Body.Close() }()
	// 4xx 不应压缩
	assert.NotEqual(t, "gzip", r.Header.Get("Content-Encoding"))
}
