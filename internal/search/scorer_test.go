// Package search - BM25 单元测试
// 验证 BM25 公式正确性 + Engine 集成
package search

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

// 准备 4 条文档, 字段 title 长度不同, 共享 token
// 期望: tf 越高 + 字段越短 + token 更稀有 -> score 越高
func newBM25TestEngine(t *testing.T) *Engine {
	t.Helper()
	e := newTestEngine(t)
	// 4 docs
	e.IndexDoc("idx", "1", map[string]interface{}{"title": "the quick brown fox"})     // "the" appears 1x
	e.IndexDoc("idx", "2", map[string]interface{}{"title": "the quick brown fox jumps"}) // "the" appears 1x
	e.IndexDoc("idx", "3", map[string]interface{}{"title": "jumps over the lazy dog"})   // "the" 1x
	e.IndexDoc("idx", "4", map[string]interface{}{"title": "she sells sea shells"})      // 无 "the"
	return e
}

func TestBM25_BasicScore(t *testing.T) {
	e := newBM25TestEngine(t)
	// 查 "the" -- 出现在 1,2,3 三个 doc; df=3, N=4
	// IDF = ln(1 + (4-3+0.5)/(3+0.5)) = ln(1 + 0.4286) = ln(1.4286) ≈ 0.357
	// tf in all 3 docs = 1
	// doc 1: len=4, avg=11/4=2.75, tf_norm=1*2.2/(1+1.2*(0.25+0.75*4/2.75))=2.2/2.56=0.859
	//   score = 0.357 * 0.859 ≈ 0.307
	// doc 2: len=5, tf_norm=1*2.2/(1+1.2*(0.25+0.75*5/2.75))=2.2/2.78=0.791
	//   score = 0.357 * 0.791 ≈ 0.282
	// doc 3: len=5, same as doc 2 ≈ 0.282
	// doc 4: no match -> 0
	// 1 > 2 == 3
	s1 := e.BM25FieldScore("idx", "title", "1", "the")
	s2 := e.BM25FieldScore("idx", "title", "2", "the")
	s3 := e.BM25FieldScore("idx", "title", "3", "the")
	s4 := e.BM25FieldScore("idx", "title", "4", "the")
	assert.Greater(t, s1, s2, "doc 1 (shorter) should score higher than doc 2")
	assert.InDelta(t, s2, s3, 0.001, "doc 2 and doc 3 same length, same score")
	assert.Equal(t, 0.0, s4, "doc 4 has no 'the' -> 0")
}

func TestBM25_HigherTFHigherScore(t *testing.T) {
	e := newTestEngine(t)
	e.IndexDoc("idx", "1", map[string]interface{}{"title": "go go go is great"})
	e.IndexDoc("idx", "2", map[string]interface{}{"title": "go is great"})
	e.IndexDoc("idx", "3", map[string]interface{}{"title": "great"})

	// 查 "go"
	s1 := e.BM25FieldScore("idx", "title", "1", "go")
	s2 := e.BM25FieldScore("idx", "title", "2", "go")
	s3 := e.BM25FieldScore("idx", "title", "3", "go")
	// doc 1 has 3x "go" -> higher
	assert.Greater(t, s1, s2, "higher TF -> higher score")
	assert.Greater(t, s2, s3, "match beats no-match")
	assert.Greater(t, s1, 0.0)
	assert.Equal(t, 0.0, s3, "doc 3 has no 'go'")
}

func TestBM25_RareTermHigherIDF(t *testing.T) {
	e := newTestEngine(t)
	e.IndexDoc("idx", "1", map[string]interface{}{"text": "common word appears everywhere"})
	e.IndexDoc("idx", "2", map[string]interface{}{"text": "another sentence with common word"})
	e.IndexDoc("idx", "3", map[string]interface{}{"text": "rare uncommon"}) // "rare" 仅 doc 3 出现

	// 查 "rare" (仅 1 个 doc 有, df=1)
	rareScore := e.BM25FieldScore("idx", "text", "3", "rare")
	// 查 "common" (2 个 doc 有, df=2)
	commonScore := e.BM25FieldScore("idx", "text", "1", "common")
	// 查 "nonexistent" 任何 doc 都没
	noMatch := e.BM25FieldScore("idx", "text", "1", "nonexistent_term_xyz")
	assert.Greater(t, rareScore, 0.0, "rare term matches")
	assert.Greater(t, commonScore, 0.0, "common term matches")
	assert.Greater(t, rareScore, commonScore, "rare term should have higher IDF")
	assert.Equal(t, 0.0, noMatch, "no match -> 0")
}

func TestBM25_MissingField(t *testing.T) {
	e := newTestEngine(t)
	e.IndexDoc("idx", "1", map[string]interface{}{"title": "hello"})
	// 字段不存在
	score := e.BM25FieldScore("idx", "nonexistent", "1", "hello")
	assert.Equal(t, 0.0, score)
}

func TestBM25_EmptyQuery(t *testing.T) {
	e := newTestEngine(t)
	e.IndexDoc("idx", "1", map[string]interface{}{"title": "hello world"})
	score := e.BM25FieldScore("idx", "title", "1", "")
	assert.Equal(t, 0.0, score)
}

func TestBM25_DeleteDoc(t *testing.T) {
	e := newTestEngine(t)
	e.IndexDoc("idx", "1", map[string]interface{}{"title": "hello world hello"})
	e.IndexDoc("idx", "2", map[string]interface{}{"title": "hello"})

	// 删除前
	s1 := e.BM25FieldScore("idx", "title", "1", "hello")
	s2 := e.BM25FieldScore("idx", "title", "2", "hello")
	assert.Greater(t, s1, 0.0)
	assert.Greater(t, s2, 0.0)

	// 删除 doc 1
	e.DeleteDoc("idx", "1")
	// doc 1 不应再被算分
	s1After := e.BM25FieldScore("idx", "title", "1", "hello")
	assert.Equal(t, 0.0, s1After)
	// doc 2 score 不应变化(语义: "hello" 仍命中, df 减少, IDF 上升 -> score 微升)
	s2After := e.BM25FieldScore("idx", "title", "2", "hello")
	assert.Greater(t, s2After, 0.0)
	// df=1 vs df=2 -> 重新算的 score 不同
	_ = math.Abs
	assert.NotEqual(t, s2, s2After, "deleting one doc changes IDF")
}

func TestBM25_MultiToken(t *testing.T) {
	e := newTestEngine(t)
	e.IndexDoc("idx", "1", map[string]interface{}{"title": "hello world foo"})
	e.IndexDoc("idx", "2", map[string]interface{}{"title": "hello world bar"})
	e.IndexDoc("idx", "3", map[string]interface{}{"title": "hello world"})

	// 查 "hello world" - 两个 token
	s1 := e.BM25FieldScore("idx", "title", "1", "hello world")
	s2 := e.BM25FieldScore("idx", "title", "2", "hello world")
	s3 := e.BM25FieldScore("idx", "title", "3", "hello world")
	// doc 1,2,3 都命中; score 应 > 0
	assert.Greater(t, s1, 0.0)
	assert.Greater(t, s2, 0.0)
	assert.Greater(t, s3, 0.0)
}

func TestBM25_FormulaSanity(t *testing.T) {
	// 单字段单 doc 单 token: 验证公式
	// N=1, df=1, tf=2, |D|=3, avgdl=3
	// IDF = ln(1 + (1-1+0.5)/(1+0.5)) = ln(1 + 0.333) = ln(1.333) = 0.2877
	// tf_norm = 2*2.2/(2 + 1.2*(0.25 + 0.75*3/3)) = 4.4/(2 + 1.2) = 4.4/3.2 = 1.375
	// score = 0.2877 * 1.375 = 0.3956
	e := newTestEngine(t)
	e.IndexDoc("idx", "1", map[string]interface{}{"f": "a b a"}) // 2 token, 2 个 a
	s := e.BM25FieldScore("idx", "f", "1", "a")
	expected := 0.2877 * 1.375
	assert.InDelta(t, expected, s, 0.01, "formula sanity check")
}
