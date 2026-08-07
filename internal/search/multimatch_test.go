// Package search - multi_match / query_string / simple_query_string 单元测试
package search

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func newMultiMatchTestEngine(t *testing.T) *Engine {
	t.Helper()
	e := newTestEngine(t)
	e.IndexDoc("idx", "1", map[string]interface{}{"title": "the quick brown fox", "body": "fox runs fast"})
	e.IndexDoc("idx", "2", map[string]interface{}{"title": "the lazy dog", "body": "dog sleeps"})
	e.IndexDoc("idx", "3", map[string]interface{}{"title": "fox and hound", "body": "best friends"})
	e.IndexDoc("idx", "4", map[string]interface{}{"title": "completely unrelated", "body": "no animals here"})
	return e
}

// ---------------- multi_match ----------------

func TestMultiMatch_BestFields(t *testing.T) {
	e := newMultiMatchTestEngine(t)
	ids, err := e.evalMultiMatch("idx", map[string]interface{}{
		"query":  "fox",
		"fields": []interface{}{"title", "body"},
	})
	assert.NoError(t, err)
	// title 或 body 含 fox: 1, 3 (title), 1 (body 已经有)
	assert.ElementsMatch(t, []string{"1", "3"}, setKeys(ids))
}

func TestMultiMatch_Phrase(t *testing.T) {
	e := newMultiMatchTestEngine(t)
	ids, err := e.evalMultiMatch("idx", map[string]interface{}{
		"query":  "quick brown",
		"fields": []interface{}{"title"},
		"type":   "phrase",
	})
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{"1"}, setKeys(ids))
}

func TestMultiMatch_PhrasePrefix(t *testing.T) {
	e := newMultiMatchTestEngine(t)
	// "quick bro" -> 前 1 token "quick" 命中, 末 "bro" prefix 命中 "brown"
	ids, err := e.evalMultiMatch("idx", map[string]interface{}{
		"query":  "quick bro",
		"fields": []interface{}{"title"},
		"type":   "phrase_prefix",
	})
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{"1"}, setKeys(ids))
}

func TestMultiMatch_CrossFields(t *testing.T) {
	e := newMultiMatchTestEngine(t)
	ids, err := e.evalMultiMatch("idx", map[string]interface{}{
		"query":  "fast",
		"fields": []interface{}{"title", "body"},
		"type":   "cross_fields",
	})
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{"1"}, setKeys(ids), "fast only in body of doc 1")
}

func TestMultiMatch_UnsupportedType(t *testing.T) {
	e := newMultiMatchTestEngine(t)
	_, err := e.evalMultiMatch("idx", map[string]interface{}{
		"query":  "fox",
		"fields": []interface{}{"title"},
		"type":   "nonsense",
	})
	assert.Error(t, err)
}

func TestMultiMatch_MissingFields(t *testing.T) {
	e := newMultiMatchTestEngine(t)
	_, err := e.evalMultiMatch("idx", map[string]interface{}{
		"query": "fox",
	})
	assert.Error(t, err)
}

func TestMultiMatch_MissingQuery(t *testing.T) {
	e := newMultiMatchTestEngine(t)
	_, err := e.evalMultiMatch("idx", map[string]interface{}{
		"fields": []interface{}{"title"},
	})
	assert.Error(t, err)
}

// ---------------- query_string ----------------

func TestQueryString_Basic(t *testing.T) {
	e := newMultiMatchTestEngine(t)
	// "fox" 在 title 或 body
	ids, err := e.evalQueryString("idx", map[string]interface{}{
		"query":         "fox",
		"default_field": "title",
	})
	assert.NoError(t, err)
	// doc 1 (title) 和 doc 3 (title) 都有 fox
	assert.ElementsMatch(t, []string{"1", "3"}, setKeys(ids))
}

func TestQueryString_MustMustNot(t *testing.T) {
	e := newMultiMatchTestEngine(t)
	// +fox -dog: 含 fox 且不含 dog
	ids, err := e.evalQueryString("idx", map[string]interface{}{
		"query":         "+fox -dog",
		"default_field": "title",
	})
	assert.NoError(t, err)
	// doc 1 (title 有 fox), doc 3 (title 有 fox)
	// 注: 我们只在 title 字段检查
	assert.ElementsMatch(t, []string{"1", "3"}, setKeys(ids))
}

func TestQueryString_FieldScoped(t *testing.T) {
	e := newMultiMatchTestEngine(t)
	// body:fast 限制到 body
	ids, err := e.evalQueryString("idx", map[string]interface{}{
		"query":         "body:fast",
		"default_field": "title",
	})
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{"1"}, setKeys(ids))
}

func TestQueryString_Phrase(t *testing.T) {
	e := newMultiMatchTestEngine(t)
	ids, err := e.evalQueryString("idx", map[string]interface{}{
		"query":         `"quick brown"`,
		"default_field": "title",
	})
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{"1"}, setKeys(ids))
}

func TestQueryString_Or(t *testing.T) {
	e := newMultiMatchTestEngine(t)
	// fox OR dog
	ids, err := e.evalQueryString("idx", map[string]interface{}{
		"query":         "fox OR dog",
		"default_field": "title",
	})
	assert.NoError(t, err)
	// doc 1 (fox), 2 (dog), 3 (fox)
	assert.ElementsMatch(t, []string{"1", "2", "3"}, setKeys(ids))
}

func TestQueryString_DefaultFieldFallback(t *testing.T) {
	e := newMultiMatchTestEngine(t)
	// 没设 default_field, 视为 "_all", 退化到所有字段(实现为空, 应报错或返回空)
	_, err := e.evalQueryString("idx", map[string]interface{}{
		"query": "fox",
	})
	// _all 字段没索引, 应返空
	assert.NoError(t, err)
}

// ---------------- simple_query_string ----------------

func TestSimpleQueryString_Basic(t *testing.T) {
	e := newMultiMatchTestEngine(t)
	ids, err := e.evalSimpleQueryString("idx", map[string]interface{}{
		"query":         "fox",
		"default_field": "title",
	})
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{"1", "3"}, setKeys(ids))
}

func TestSimpleQueryString_StripsReserved(t *testing.T) {
	e := newMultiMatchTestEngine(t)
	// 包含保留字符应该被剥离, 不报错
	ids, err := e.evalSimpleQueryString("idx", map[string]interface{}{
		"query":         "fox*~ +body:something",
		"default_field": "title",
	})
	assert.NoError(t, err)
	// 实际上只是"fox"被搜索
	_ = ids
}

func TestSimpleQueryString_NoErrorOnBadSyntax(t *testing.T) {
	e := newMultiMatchTestEngine(t)
	// 不像 query_string, simple_query_string 不应因语法错抛错
	_, err := e.evalSimpleQueryString("idx", map[string]interface{}{
		"query":         "(((unmatched",
		"default_field": "title",
	})
	assert.NoError(t, err)
}

// ---------------- 集合工具 ----------------

func TestSetOps(t *testing.T) {
	a := map[string]struct{}{"1": {}, "2": {}, "3": {}}
	b := map[string]struct{}{"2": {}, "3": {}, "4": {}}
	inter := intersectSets(a, b)
	assert.ElementsMatch(t, []string{"2", "3"}, setKeys(inter))
	uni := unionSets(a, b)
	assert.ElementsMatch(t, []string{"1", "2", "3", "4"}, setKeys(uni))
	sub := subtractSets(a, b)
	assert.ElementsMatch(t, []string{"1"}, setKeys(sub))
}

func setKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
