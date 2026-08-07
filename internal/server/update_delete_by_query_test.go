// Package server - update_by_query / delete_by_query 单元测试
// 覆盖:
//   - delete_by_query 同步删除
//   - update_by_query 同步修改(set / inc / remove / 嵌套路径)
//   - script 解析错误返回 400
//   - 异步 + 取消
package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/zixiliuyue/go_es/internal/search"
	"github.com/zixiliuyue/go_es/internal/storage"
)

// newUDQTestServer 构造一个最小可用的 Server 实例, 引擎 + store 都准备好了
func newUDQTestServer(t *testing.T) *Server {
	t.Helper()
	store, err := storage.Open("")
	assert.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	engine := search.New(store)
	s := &Server{store: store, engine: engine}
	return s
}

// doUDQ 模拟调用 /<index>/_update_by_query 或 /<index>/_delete_by_query
// 直接调用对应的 handler 方法(需要 index 参数), 包装一个 adapter
func doUDQ(t *testing.T, s *Server, op string, index string, body string) (int, string) {
	t.Helper()
	url := "/" + index + "/_" + op
	r := httptest.NewRequest("POST", url, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	if op == "update_by_query" {
		s.handleUpdateByQuery(w, r, index)
	} else {
		s.handleDeleteByQuery(w, r, index)
	}
	return w.Code, w.Body.String()
}

// TestDeleteByQuery_Sync 测试同步删除
func TestDeleteByQuery_Sync(t *testing.T) {
	s := newUDQTestServer(t)
	s.engine.IndexDoc("idx", "1", map[string]interface{}{"status": "active"})
	s.engine.IndexDoc("idx", "2", map[string]interface{}{"status": "inactive"})
	s.engine.IndexDoc("idx", "3", map[string]interface{}{"status": "active"})

	code, body := doUDQ(t, s, "delete_by_query", "idx",
		`{"query":{"term":{"status":"active"}}}`)
	assert.Equal(t, 200, code)
	assert.Contains(t, body, `"deleted":2`)
	assert.Contains(t, body, `"total":2`)

	// 验证 doc 1 和 3 真的被删
	_, ok := s.engine.GetSource("idx", "1")
	assert.False(t, ok, "doc 1 should be deleted")
	_, ok = s.engine.GetSource("idx", "2")
	assert.True(t, ok, "doc 2 should remain")
	_, ok = s.engine.GetSource("idx", "3")
	assert.False(t, ok, "doc 3 should be deleted")
}

// TestDeleteByQuery_NoQuery 缺 query 默认删除全部
func TestDeleteByQuery_NoQuery(t *testing.T) {
	s := newUDQTestServer(t)
	s.engine.IndexDoc("idx", "1", map[string]interface{}{"a": 1})
	s.engine.IndexDoc("idx", "2", map[string]interface{}{"a": 2})

	code, body := doUDQ(t, s, "delete_by_query", "idx", `{}`)
	assert.Equal(t, 200, code)
	assert.Contains(t, body, `"deleted":2`)
}

// TestUpdateByQuery_Set 测试 set 脚本
func TestUpdateByQuery_Set(t *testing.T) {
	s := newUDQTestServer(t)
	s.engine.IndexDoc("idx", "1", map[string]interface{}{"status": "active", "v": 1})
	s.engine.IndexDoc("idx", "2", map[string]interface{}{"status": "active", "v": 2})

	code, body := doUDQ(t, s, "update_by_query", "idx",
		`{"query":{"term":{"status":"active"}},"script":{"source":"ctx._source.status = 'archived'"}}`)
	assert.Equal(t, 200, code)
	assert.Contains(t, body, `"updated":2`)

	src1, _ := s.engine.GetSource("idx", "1")
	assert.Equal(t, "archived", src1["status"])
	src2, _ := s.engine.GetSource("idx", "2")
	assert.Equal(t, "archived", src2["status"])
}

// TestUpdateByQuery_Inc 测试 += 脚本
func TestUpdateByQuery_Inc(t *testing.T) {
	s := newUDQTestServer(t)
	s.engine.IndexDoc("idx", "1", map[string]interface{}{"views": 10})
	s.engine.IndexDoc("idx", "2", map[string]interface{}{"views": 20})

	code, body := doUDQ(t, s, "update_by_query", "idx",
		`{"query":{"match_all":{}},"script":{"source":"ctx._source.views += 5"}}`)
	assert.Equal(t, 200, code)
	assert.Contains(t, body, `"updated":2`)

	src1, _ := s.engine.GetSource("idx", "1")
	assert.EqualValues(t, 15, src1["views"], "10 + 5 = 15")
	src2, _ := s.engine.GetSource("idx", "2")
	assert.EqualValues(t, 25, src2["views"], "20 + 5 = 25")
}

// TestUpdateByQuery_Remove 测试 remove 脚本
func TestUpdateByQuery_Remove(t *testing.T) {
	s := newUDQTestServer(t)
	s.engine.IndexDoc("idx", "1", map[string]interface{}{"a": 1, "b": 2, "c": 3})

	code, body := doUDQ(t, s, "update_by_query", "idx",
		`{"query":{"match_all":{}},"script":{"source":"ctx._source.remove('b')"}}`)
	assert.Equal(t, 200, code)
	assert.Contains(t, body, `"updated":1`)

	src, _ := s.engine.GetSource("idx", "1")
	_, hasB := src["b"]
	assert.False(t, hasB, "field b should be removed")
	_, hasA := src["a"]
	assert.True(t, hasA, "field a should remain")
}

// TestUpdateByQuery_NestedPath 测试点号路径
func TestUpdateByQuery_NestedPath(t *testing.T) {
	s := newUDQTestServer(t)
	s.engine.IndexDoc("idx", "1", map[string]interface{}{
		"user": map[string]interface{}{"name": "alice", "age": 30},
	})

	code, _ := doUDQ(t, s, "update_by_query", "idx",
		`{"query":{"match_all":{}},"script":{"source":"ctx._source.user.age = 31"}}`)
	assert.Equal(t, 200, code)

	src, _ := s.engine.GetSource("idx", "1")
	user, _ := src["user"].(map[string]interface{})
	assert.EqualValues(t, 31, user["age"])
}

// TestUpdateByQuery_MultiStmts 测试多条语句
func TestUpdateByQuery_MultiStmts(t *testing.T) {
	s := newUDQTestServer(t)
	s.engine.IndexDoc("idx", "1", map[string]interface{}{"a": 1, "b": 2, "c": 3})

	code, _ := doUDQ(t, s, "update_by_query", "idx",
		`{"query":{"match_all":{}},"script":{"source":"ctx._source.a = 10; ctx._source.b += 5; ctx._source.remove('c')"}}`)
	assert.Equal(t, 200, code)

	src, _ := s.engine.GetSource("idx", "1")
	assert.EqualValues(t, 10, src["a"])
	assert.EqualValues(t, 7, src["b"])
	_, hasC := src["c"]
	assert.False(t, hasC)
}

// TestUpdateByQuery_NoScript 缺 script -> 400
func TestUpdateByQuery_NoScript(t *testing.T) {
	s := newUDQTestServer(t)
	code, _ := doUDQ(t, s, "update_by_query", "idx", `{"query":{"match_all":{}}}`)
	assert.Equal(t, 400, code)
}

// TestUpdateByQuery_InvalidScript 非法 script -> 400
func TestUpdateByQuery_InvalidScript(t *testing.T) {
	s := newUDQTestServer(t)
	code, _ := doUDQ(t, s, "update_by_query", "idx",
		`{"query":{"match_all":{}},"script":{"source":"INVALID STATEMENT"}}`)
	assert.Equal(t, 400, code)
}

// TestUpdateByQuery_Async 异步模式
func TestUpdateByQuery_Async(t *testing.T) {
	s := newUDQTestServer(t)
	for i := 1; i <= 5; i++ {
		s.engine.IndexDoc("idx", string(rune('0'+i)), map[string]interface{}{"v": i})
	}

	url := "/idx/_update_by_query?wait_for_completion=false"
	r := httptest.NewRequest("POST", url, strings.NewReader(
		`{"query":{"match_all":{}},"script":{"source":"ctx._source.v = 0"}}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleUpdateByQuery(w, r, "idx")
	assert.Equal(t, 200, w.Code)
	assert.Contains(t, w.Body.String(), `"task":`)
}

// TestDeleteByQuery_Async 异步模式
func TestDeleteByQuery_Async(t *testing.T) {
	s := newUDQTestServer(t)
	for i := 1; i <= 3; i++ {
		s.engine.IndexDoc("idx", string(rune('0'+i)), map[string]interface{}{"a": i})
	}

	url := "/idx/_delete_by_query?wait_for_completion=false"
	r := httptest.NewRequest("POST", url, strings.NewReader(`{"query":{"match_all":{}}}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleDeleteByQuery(w, r, "idx")
	assert.Equal(t, 200, w.Code)
	assert.Contains(t, w.Body.String(), `"task":`)
}

// TestParseScriptValue 单元测试
func TestParseScriptValue(t *testing.T) {
	cases := []struct {
		in   string
		want interface{}
	}{
		{`'hello'`, "hello"},
		{`"world"`, "world"},
		{`true`, true},
		{`false`, false},
		{`null`, nil},
		{`42`, int64(42)},
		{`3.14`, 3.14},
		{`{"a":1}`, map[string]interface{}{"a": float64(1)}},
		{`[1,2,3]`, []interface{}{float64(1), float64(2), float64(3)}},
	}
	for _, c := range cases {
		got, err := parseScriptValue(c.in)
		assert.NoError(t, err, c.in)
		assert.Equal(t, c.want, got, c.in)
	}
}
