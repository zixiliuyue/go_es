// Package search - suggest / match_phrase 单元测试
package search

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
)

func newSuggestTestEngine(t *testing.T) *Engine {
	t.Helper()
	e := newTestEngine(t)
	// 5 docs, 字段 title 涵盖 fox/jumps/lazy/quick/brown/apple
	e.IndexDoc("idx", "1", map[string]interface{}{"title": "the quick brown fox"})
	e.IndexDoc("idx", "2", map[string]interface{}{"title": "the quick brown fox jumps"})
	e.IndexDoc("idx", "3", map[string]interface{}{"title": "jumps over the lazy dog"})
	e.IndexDoc("idx", "4", map[string]interface{}{"title": "the apple pie"})
	e.IndexDoc("idx", "5", map[string]interface{}{"title": "she sells sea shells"})
	return e
}

// ---------------- match_phrase ----------------

func TestEvalMatchPhrase_BasicPhrase(t *testing.T) {
	e := newSuggestTestEngine(t)
	ids, err := e.Match("idx", &Query{MatchPhrase: map[string]interface{}{
		"title": "quick brown",
	}})
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{"1", "2"}, ids)
}

func TestEvalMatchPhrase_WordOrderRequired(t *testing.T) {
	e := newSuggestTestEngine(t)
	// "brown quick" 不在原文顺序中
	ids, err := e.Match("idx", &Query{MatchPhrase: map[string]interface{}{
		"title": "brown quick",
	}})
	assert.NoError(t, err)
	assert.Empty(t, ids, "phrase 要求 token 顺序相邻")
}

func TestEvalMatchPhrase_SingleToken(t *testing.T) {
	e := newSuggestTestEngine(t)
	ids, err := e.Match("idx", &Query{MatchPhrase: map[string]interface{}{
		"title": "fox",
	}})
	assert.NoError(t, err)
	// 1 和 2 包含 "fox"
	assert.ElementsMatch(t, []string{"1", "2"}, ids)
}

func TestEvalMatchPhrase_LongPhrase(t *testing.T) {
	e := newSuggestTestEngine(t)
	// "the quick brown fox" 完整短语, 只有 doc 2
	ids, err := e.Match("idx", &Query{MatchPhrase: map[string]interface{}{
		"title": "the quick brown fox",
	}})
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{"1", "2"}, ids)
}

func TestEvalMatchPhrase_QuerySyntax(t *testing.T) {
	e := newSuggestTestEngine(t)
	// 也支持 {"query": "..."} 包装
	ids, err := e.Match("idx", &Query{MatchPhrase: map[string]interface{}{
		"title": map[string]interface{}{"query": "apple pie"},
	}})
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{"4"}, ids)
}

// ---------------- term suggester ----------------

func TestTermSuggest_Basic(t *testing.T) {
	e := newSuggestTestEngine(t)
	// 找 typo "quik" -> 期望看到 "quick"
	opts := e.TermSuggest("idx", "title", "quik",
		TermSuggesterConfig{Field: "title", Sort: "score"},
		2, 1, 3, 5)
	assert.NotEmpty(t, opts)
	texts := make([]string, len(opts))
	for i, o := range opts {
		texts[i] = o.Text
	}
	// 至少应包含 quick
	found := false
	for _, t := range texts {
		if t == "quick" {
			found = true
			break
		}
	}
	assert.True(t, found, "term suggester 应找到 'quick', got %v", texts)
}

func TestTermSuggest_ExactMatch(t *testing.T) {
	e := newSuggestTestEngine(t)
	// 精确词, 应该返回自己(score 最高)
	opts := e.TermSuggest("idx", "title", "brown",
		TermSuggesterConfig{Field: "title"},
		2, 1, 3, 5)
	assert.NotEmpty(t, opts)
	assert.Equal(t, "brown", opts[0].Text, "exact match should rank first")
}

func TestTermSuggest_NoResult(t *testing.T) {
	e := newSuggestTestEngine(t)
	// 完全无相似, max_edits 太小
	opts := e.TermSuggest("idx", "title", "z",
		TermSuggesterConfig{Field: "title"},
		0, 1, 3, 5)
	// "z" 长度 1 < minWordLen 3, 应不返回任何
	assert.Empty(t, opts)
}

func TestTermSuggest_Size(t *testing.T) {
	e := newSuggestTestEngine(t)
	opts := e.TermSuggest("idx", "title", "the",
		TermSuggesterConfig{Field: "title"},
		0, 1, 3, 2)
	assert.LessOrEqual(t, len(opts), 2, "size 限制应生效")
}

// ---------------- completion suggester ----------------

func TestCompletionSuggest_Prefix(t *testing.T) {
	e := newSuggestTestEngine(t)
	// "qu" -> quick
	opts := e.CompletionSuggest("idx", "title", "qu", CompletionSuggesterConfig{Field: "title", Size: 5})
	assert.NotEmpty(t, opts)
	texts := make([]string, 0)
	for _, o := range opts {
		texts = append(texts, o.Text)
	}
	sort.Strings(texts)
	assert.Contains(t, texts, "quick")
}

func TestCompletionSuggest_NoPrefix(t *testing.T) {
	e := newSuggestTestEngine(t)
	opts := e.CompletionSuggest("idx", "title", "qu", CompletionSuggesterConfig{Field: "title"})
	assert.NotEmpty(t, opts)
	for _, o := range opts {
		assert.True(t, len(o.Text) >= 2, "all options should have prefix 'qu' or longer")
		assert.True(t, o.Text[:2] == "qu", "prefix match, got "+o.Text)
	}
}

func TestCompletionSuggest_EmptyText(t *testing.T) {
	e := newSuggestTestEngine(t)
	opts := e.CompletionSuggest("idx", "title", "", CompletionSuggesterConfig{Field: "title"})
	assert.Nil(t, opts)
}

func TestCompletionSuggest_SkipDuplicates(t *testing.T) {
	e := newSuggestTestEngine(t)
	// "the" 在 4 个 doc 中出现, 去重后只 1 个
	opts := e.CompletionSuggest("idx", "title", "the", CompletionSuggesterConfig{Field: "title", Size: 10, SkipDuplicates: true})
	for _, o := range opts {
		if o.Text == "the" {
			// OK
			return
		}
	}
	t.Errorf("should find 'the'")
}

// ---------------- prefix suggester ----------------

func TestPrefixSuggest(t *testing.T) {
	e := newSuggestTestEngine(t)
	opts := e.PrefixSuggest("idx", "title", "qu", 5)
	assert.NotEmpty(t, opts)
	for _, o := range opts {
		assert.True(t, len(o.Text) >= 2 && o.Text[:2] == "qu")
	}
}

func TestPrefixSuggest_EmptyText(t *testing.T) {
	e := newSuggestTestEngine(t)
	opts := e.PrefixSuggest("idx", "title", "", 5)
	assert.Nil(t, opts)
}

// ---------------- Levenshtein ----------------

func TestLevenshtein_Basic(t *testing.T) {
	assert.Equal(t, 0, levenshtein("abc", "abc", 10))
	assert.Equal(t, 1, levenshtein("abc", "abd", 10))
	assert.Equal(t, 1, levenshtein("abc", "abcd", 10))
	assert.Equal(t, 3, levenshtein("abc", "xyz", 10))
}

func TestLevenshtein_Empty(t *testing.T) {
	assert.Equal(t, 3, levenshtein("", "abc", 10))
	assert.Equal(t, 3, levenshtein("abc", "", 10))
	assert.Equal(t, 0, levenshtein("", "", 10))
}

func TestLevenshtein_MaxDistanceEarlyExit(t *testing.T) {
	// 距离 > maxDistance, 应返回 -1
	assert.Equal(t, -1, levenshtein("abc", "xyz", 1))
	assert.Equal(t, -1, levenshtein("hello", "world", 2))
}

func TestContainsPhrase(t *testing.T) {
	toks := []string{"the", "quick", "brown", "fox"}
	assert.True(t, containsPhrase(toks, []string{"quick", "brown"}))
	assert.True(t, containsPhrase(toks, []string{"fox"}))
	assert.False(t, containsPhrase(toks, []string{"brown", "quick"}), "顺序敏感")
	assert.False(t, containsPhrase(toks, []string{"slow"}))
	assert.True(t, containsPhrase(toks, []string{"the", "quick", "brown", "fox"}))
}
