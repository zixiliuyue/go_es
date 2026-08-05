// Package search 包的单元测试
// 覆盖倒排索引与 query 引擎的核心路径
package search

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/zixiliuyue/go_es/internal/storage"
)

func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	store, err := storage.Open("")
	assert.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return New(store)
}

func TestEngine_IndexAndMatch(t *testing.T) {
	e := newTestEngine(t)
	e.IndexDoc("idx", "1", map[string]interface{}{"title": "hello world", "tag": "go"})
	e.IndexDoc("idx", "2", map[string]interface{}{"title": "goodbye world", "tag": "py"})
	e.IndexDoc("idx", "3", map[string]interface{}{"title": "hello go", "tag": "go"})

	// match:hello
	ids, err := e.Match("idx", &Query{Match: map[string]interface{}{"title": "hello"}})
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{"1", "3"}, ids)

	// term:tag=go
	ids, err = e.Match("idx", &Query{Term: map[string]interface{}{"tag": "go"}})
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{"1", "3"}, ids)

	// terms:tag in (go,py)
	ids, err = e.Match("idx", &Query{Terms: map[string]interface{}{"tag": []interface{}{"go", "py"}}})
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{"1", "2", "3"}, ids)
}

func TestEngine_RangeQuery(t *testing.T) {
	e := newTestEngine(t)
	e.IndexDoc("idx", "1", map[string]interface{}{"views": 10})
	e.IndexDoc("idx", "2", map[string]interface{}{"views": 20})
	e.IndexDoc("idx", "3", map[string]interface{}{"views": 30})
	e.IndexDoc("idx", "4", map[string]interface{}{"views": 40})

	ids, err := e.Match("idx", &Query{Range: map[string]interface{}{
		"views": map[string]interface{}{"gte": 20, "lte": 30},
	}})
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{"2", "3"}, ids)
}

func TestEngine_BoolQuery(t *testing.T) {
	e := newTestEngine(t)
	e.IndexDoc("idx", "1", map[string]interface{}{"title": "hello go", "tag": "go"})
	e.IndexDoc("idx", "2", map[string]interface{}{"title": "hello py", "tag": "py"})
	e.IndexDoc("idx", "3", map[string]interface{}{"title": "hi go", "tag": "go"})

	// must match title=hello, must_not term tag=py
	boolQ := &Query{
		Bool: &BoolQuery{
			Must: []map[string]interface{}{{"match": map[string]interface{}{"title": "hello"}}},
			MustNot: []map[string]interface{}{{"term": map[string]interface{}{"tag": "py"}}},
		},
	}
	ids, err := e.Match("idx", boolQ)
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{"1"}, ids)
}

func TestEngine_MatchAll(t *testing.T) {
	e := newTestEngine(t)
	e.IndexDoc("idx", "1", map[string]interface{}{"a": 1})
	e.IndexDoc("idx", "2", map[string]interface{}{"a": 2})

	ids, err := e.Match("idx", &Query{MatchAll: map[string]interface{}{}})
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{"1", "2"}, ids)
}

func TestEngine_Delete(t *testing.T) {
	e := newTestEngine(t)
	e.IndexDoc("idx", "1", map[string]interface{}{"a": "hello"})
	e.DeleteDoc("idx", "1")
	_, ok := e.GetSource("idx", "1")
	assert.False(t, ok)
}

// _ = 抑制 unused import 警告
var (
	_ = fmt.Sprint
	_ = sync.Mutex{}
	_ = context.TODO
)
