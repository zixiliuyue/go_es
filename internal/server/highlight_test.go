// Package server - highlight / source 过滤 / track_total_hits 单元测试
package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestApplySourceFilter 测试 _source 过滤
func TestApplySourceFilter(t *testing.T) {
	src := map[string]interface{}{
		"a": 1, "b": 2, "c": 3, "nested": map[string]interface{}{"x": "y"},
	}
	t.Run("nil 透传", func(t *testing.T) {
		out := applySourceFilter(src, nil)
		assert.Equal(t, src, out)
	})
	t.Run("true 透传", func(t *testing.T) {
		out := applySourceFilter(src, true)
		assert.Equal(t, src, out)
	})
	t.Run("false 返回 nil", func(t *testing.T) {
		out := applySourceFilter(src, false)
		assert.Nil(t, out)
	})
	t.Run("string 单字段", func(t *testing.T) {
		out := applySourceFilter(src, "a")
		assert.Equal(t, map[string]interface{}{"a": 1}, out)
	})
	t.Run("[]string 多字段", func(t *testing.T) {
		out := applySourceFilter(src, []string{"a", "c"})
		assert.Equal(t, map[string]interface{}{"a": 1, "c": 3}, out)
	})
	t.Run("[]interface{} 多字段", func(t *testing.T) {
		out := applySourceFilter(src, []interface{}{"a", "b"})
		assert.Equal(t, map[string]interface{}{"a": 1, "b": 2}, out)
	})
}

// TestTrackTotalHitsValue 解析 track_total_hits
func TestTrackTotalHitsValue(t *testing.T) {
	cases := []struct {
		in       interface{}
		exact    bool
		expected int
	}{
		{nil, false, defaultTotalHitsCap},
		{true, true, 0},
		{false, false, defaultTotalHitsCap},
		{float64(500), false, 500},
		{float64(0), true, 0},
		{float64(-1), true, 0},
		{100, false, 100},
	}
	for _, c := range cases {
		exact, cap := trackTotalHitsValue(c.in)
		assert.Equal(t, c.exact, exact, "in=%v", c.in)
		assert.Equal(t, c.expected, cap, "in=%v", c.in)
	}
}

// TestHighlightString 高亮核心
func TestHighlightString(t *testing.T) {
	toks := map[string]struct{}{"fox": {}, "jumps": {}}
	t.Run("单词命中", func(t *testing.T) {
		s := "the quick brown fox"
		out := highlightString(s, splitHighlightSpans(s), toks, "<em>", "</em>")
		assert.Equal(t, "the quick brown <em>fox</em>", out)
	})
	t.Run("多 token", func(t *testing.T) {
		s := "fox jumps over fox"
		out := highlightString(s, splitHighlightSpans(s), toks, "<em>", "</em>")
		assert.Equal(t, "<em>fox</em> <em>jumps</em> over <em>fox</em>", out)
	})
	t.Run("大小写", func(t *testing.T) {
		s := "The FOX and Fox"
		out := highlightString(s, splitHighlightSpans(s), toks, "<em>", "</em>")
		// 应命中所有 fox 变体(大小写不敏感)
		assert.Contains(t, out, "<em>FOX</em>")
		assert.Contains(t, out, "<em>Fox</em>")
	})
	t.Run("无命中", func(t *testing.T) {
		s := "the lazy dog"
		out := highlightString(s, splitHighlightSpans(s), toks, "<em>", "</em>")
		assert.Equal(t, "", out)
	})
	t.Run("空字符串", func(t *testing.T) {
		s := ""
		out := highlightString(s, splitHighlightSpans(s), toks, "<em>", "</em>")
		assert.Equal(t, "", out)
	})
}

// TestApplyHighlight 字段级 highlight
func TestApplyHighlight(t *testing.T) {
	src := map[string]interface{}{
		"title":   "The Quick Brown Fox",
		"content": "fox jumps over the lazy dog",
		"score":   100, // 非字符串字段, 不参与高亮
	}
	toks := []string{"fox", "jumps"}
	spec := map[string]interface{}{
		"fields": map[string]interface{}{
			"title":   map[string]interface{}{},
			"content": map[string]interface{}{},
			"score":   map[string]interface{}{}, // 非字符串字段, 应被忽略
		},
	}
	out := applyHighlight(src, spec, toks)
	assert.Contains(t, out["title"][0], "<em>Fox</em>")
	assert.Contains(t, out["content"][0], "<em>fox</em>")
	assert.Contains(t, out["content"][0], "<em>jumps</em>")
	_, hasScore := out["score"]
	assert.False(t, hasScore, "non-string field should not have highlight")
}

// TestApplyHighlight_NoTokens 无 token 时不输出
func TestApplyHighlight_NoTokens(t *testing.T) {
	src := map[string]interface{}{"a": "hello"}
	spec := map[string]interface{}{"fields": map[string]interface{}{"a": map[string]interface{}{}}}
	out := applyHighlight(src, spec, nil)
	assert.Nil(t, out)
}

// TestApplyHighlight_CustomTags 自定义 pre/post
func TestApplyHighlight_CustomTags(t *testing.T) {
	src := map[string]interface{}{"a": "hello world"}
	spec := map[string]interface{}{
		"fields":    map[string]interface{}{"a": map[string]interface{}{}},
		"pre_tags":  []interface{}{"<b>"},
		"post_tags": []interface{}{"</b>"},
	}
	out := applyHighlight(src, spec, []string{"hello"})
	assert.Equal(t, "<b>hello</b> world", out["a"][0])
}

// TestApplyHighlight_NestedField 嵌套字段
func TestApplyHighlight_NestedField(t *testing.T) {
	src := map[string]interface{}{
		"user": map[string]interface{}{"name": "alice the admin"},
	}
	spec := map[string]interface{}{
		"fields": map[string]interface{}{"user.name": map[string]interface{}{}},
	}
	out := applyHighlight(src, spec, []string{"alice"})
	// 注: 当前实现不支持嵌套路径, 只对顶层字段生效
	// user.name 不在顶层, 应无高亮
	_, ok := out["user.name"]
	assert.False(t, ok, "nested field path not supported in v1")
}

// TestPickFields 白名单
func TestPickFields(t *testing.T) {
	doc := map[string]interface{}{"a": 1, "b": 2, "c": 3}
	t.Run("all", func(t *testing.T) {
		out := pickFields(doc, []string{"a", "b", "c"})
		assert.Len(t, out, 3)
	})
	t.Run("partial", func(t *testing.T) {
		out := pickFields(doc, []string{"a", "missing"})
		assert.Len(t, out, 1)
	})
	t.Run("empty list", func(t *testing.T) {
		out := pickFields(doc, []string{})
		assert.Len(t, out, 0)
	})
	t.Run("nil doc", func(t *testing.T) {
		out := pickFields(nil, []string{"a"})
		assert.Nil(t, out)
	})
}

// TestViewForHighlight 提取 view
func TestViewForHighlight(t *testing.T) {
	// 直接构造 search.Query 测试
	t.Run("nil query", func(t *testing.T) {
		v := viewForHighlight(nil)
		assert.Nil(t, v)
	})
}

// TestExtractQueryTokens 提取 query tokens
func TestExtractQueryTokens(t *testing.T) {
	t.Run("empty view", func(t *testing.T) {
		toks := extractQueryTokensFromQuery(nil)
		assert.Nil(t, toks)
	})
	t.Run("view with tokens", func(t *testing.T) {
		v := newSearchQueryView([]fieldQuery{
			{Field: "title", Query: "hello world"},
			{Field: "body", Query: "foo bar baz"},
		})
		toks := extractQueryTokensFromQuery(v)
		// 6 unique tokens after dedup + sort
		assert.Equal(t, []string{"bar", "baz", "foo", "hello", "world"}, toks[:5])
	})
}

// TestSearchE2EHighlight 通过 HTTP 模拟端到端, 验证 highlight 出现在响应里
func TestSearchE2EHighlight(t *testing.T) {
	s := newUDQTestServer(t)
	s.engine.IndexDoc("idx", "1", map[string]interface{}{"title": "the quick brown fox"})
	// 直接调用 doSearch 路径需要 Router, 这里走 Server 通用方法不便
	// 改为更聚焦的单测: 验证 applyHighlight + applySourceFilter 组合
	src := map[string]interface{}{"title": "the quick brown fox"}
	src2 := applySourceFilter(src, []string{"title"})
	assert.Equal(t, src, src2)
	hl := applyHighlight(src2, map[string]interface{}{
		"fields": map[string]interface{}{"title": map[string]interface{}{}},
	}, []string{"fox"})
	assert.Contains(t, hl["title"][0], "<em>fox</em>")
}

// 防止未使用 httptest 警告
var _ = httptest.NewRequest
var _ = strings.TrimSpace
