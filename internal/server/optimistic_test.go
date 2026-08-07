// Package server - 乐观并发控制 单元测试
package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/zixiliuyue/go_es/internal/search"
	"github.com/zixiliuyue/go_es/internal/storage"
)

// newOptimisticTestServer 构造有 store 的 server
func newOptimisticTestServer(t *testing.T) *Server {
	t.Helper()
	store, err := storage.Open("")
	assert.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	engine := search.New(store)
	s := &Server{store: store, engine: engine, rbac: newRBAC()}
	return s
}

// TestNextMeta_CreateFirst 第一次创建
func TestNextMeta_CreateFirst(t *testing.T) {
	m, err := NextMeta(nil, false, 0, 0, 0, "")
	assert.NoError(t, err)
	assert.Equal(t, int64(1), m.SeqNo, "first create: seq_no=1")
	assert.Equal(t, int64(1), m.Version)
}

// TestNextMeta_UpdateWithoutCondition 连续 update
func TestNextMeta_UpdateWithoutCondition(t *testing.T) {
	cur := &DocMeta{SeqNo: 1, PrimaryTerm: 1, Version: 1, Created: true}
	m, err := NextMeta(cur, true, 0, 0, 0, "")
	assert.NoError(t, err)
	assert.Equal(t, int64(2), m.SeqNo, "update: seq_no=2")
	assert.Equal(t, int64(2), m.Version)
}

// TestNextMeta_ExternalVersion external version type
func TestNextMeta_ExternalVersion(t *testing.T) {
	cur := &DocMeta{SeqNo: 1, PrimaryTerm: 1, Version: 1, Created: true}
	m, err := NextMeta(cur, true, 0, 0, 10, "external")
	assert.NoError(t, err)
	assert.Equal(t, int64(2), m.SeqNo, "seq_no 仍递增")
	assert.Equal(t, int64(10), m.Version, "external version 用 10")
}

// TestNextMeta_ExternalGTEVersion 旧 version 拒绝
func TestNextMeta_ExternalGTEVersion(t *testing.T) {
	cur := &DocMeta{SeqNo: 1, PrimaryTerm: 1, Version: 10, Created: true}
	_, err := NextMeta(cur, true, 0, 0, 5, "external_gte")
	assert.Error(t, err, "external_gte: 旧 version 拒绝")
}

// TestNextMeta_ExternalVersionRequired external 必须传 version
func TestNextMeta_ExternalVersionRequired(t *testing.T) {
	_, err := NextMeta(nil, false, 0, 0, 0, "external")
	assert.Error(t, err)
}

// TestNextMeta_UnsupportedVersionType
func TestNextMeta_UnsupportedVersionType(t *testing.T) {
	_, err := NextMeta(nil, false, 0, 0, 1, "nonsense")
	assert.Error(t, err)
}

// TestApplyWrite_BasicCreate 创建
func TestApplyWrite_BasicCreate(t *testing.T) {
	s := newOptimisticTestServer(t)
	// 创建索引
	_ = s.store.Put([]byte("meta/test"), map[string]interface{}{})

	meta, status, errResp := s.applyWrite(writeOp{
		Index: "test", ID: "1", Doc: map[string]interface{}{"a": 1},
	})
	assert.Nil(t, errResp)
	assert.Equal(t, 201, status, "first create -> 201")
	assert.Equal(t, int64(1), meta.SeqNo)
	assert.Equal(t, int64(1), meta.PrimaryTerm)
}

// TestApplyWrite_Update 第二次写入
func TestApplyWrite_Update(t *testing.T) {
	s := newOptimisticTestServer(t)
	_ = s.store.Put([]byte("meta/test"), map[string]interface{}{})

	_, _, _ = s.applyWrite(writeOp{Index: "test", ID: "1", Doc: map[string]interface{}{"a": 1}})
	meta, status, errResp := s.applyWrite(writeOp{Index: "test", ID: "1", Doc: map[string]interface{}{"a": 2}})
	assert.Nil(t, errResp)
	assert.Equal(t, 200, status, "update -> 200")
	assert.Equal(t, int64(2), meta.SeqNo)
	assert.Equal(t, int64(2), meta.Version)
}

// TestApplyWrite_CreateOpTypeConflict op_type=create 已存在 -> 409
func TestApplyWrite_CreateOpTypeConflict(t *testing.T) {
	s := newOptimisticTestServer(t)
	_ = s.store.Put([]byte("meta/test"), map[string]interface{}{})
	_, _, _ = s.applyWrite(writeOp{Index: "test", ID: "1", Doc: map[string]interface{}{"a": 1}})

	_, status, errResp := s.applyWrite(writeOp{
		Index: "test", ID: "1", Doc: map[string]interface{}{"a": 2}, OpType: "create",
	})
	assert.Equal(t, 409, status)
	assert.NotNil(t, errResp)
}

// TestApplyWrite_IfSeqNo_Match 条件符合 -> 成功
func TestApplyWrite_IfSeqNo_Match(t *testing.T) {
	s := newOptimisticTestServer(t)
	_ = s.store.Put([]byte("meta/test"), map[string]interface{}{})
	_, _, _ = s.applyWrite(writeOp{Index: "test", ID: "1", Doc: map[string]interface{}{"a": 1}})

	// 当前 seq_no=1, if_seq_no=1 应通过
	meta, status, errResp := s.applyWrite(writeOp{
		Index: "test", ID: "1", Doc: map[string]interface{}{"a": 2},
		IfSeqNo: 1,
	})
	assert.Nil(t, errResp)
	assert.Equal(t, 200, status)
	assert.Equal(t, int64(2), meta.SeqNo)
}

// TestApplyWrite_IfSeqNo_Stale 条件过期 -> 409
func TestApplyWrite_IfSeqNo_Stale(t *testing.T) {
	s := newOptimisticTestServer(t)
	_ = s.store.Put([]byte("meta/test"), map[string]interface{}{})
	_, _, _ = s.applyWrite(writeOp{Index: "test", ID: "1", Doc: map[string]interface{}{"a": 1}})
	_, _, _ = s.applyWrite(writeOp{Index: "test", ID: "1", Doc: map[string]interface{}{"a": 2}})

	// 当前 seq_no=2, if_seq_no=1 应失败
	_, status, errResp := s.applyWrite(writeOp{
		Index: "test", ID: "1", Doc: map[string]interface{}{"a": 3},
		IfSeqNo: 1,
	})
	assert.Equal(t, 409, status)
	assert.NotNil(t, errResp)
}

// TestApplyWrite_IfPrimaryTerm_Mismatch
func TestApplyWrite_IfPrimaryTerm_Mismatch(t *testing.T) {
	s := newOptimisticTestServer(t)
	_ = s.store.Put([]byte("meta/test"), map[string]interface{}{})
	_, _, _ = s.applyWrite(writeOp{Index: "test", ID: "1", Doc: map[string]interface{}{"a": 1}})

	_, status, errResp := s.applyWrite(writeOp{
		Index: "test", ID: "1", Doc: map[string]interface{}{"a": 2},
		IfPrimaryTerm: 99,
	})
	assert.Equal(t, 409, status)
	assert.NotNil(t, errResp)
}

// TestApplyWrite_ExternalVersion 用 external version
func TestApplyWrite_ExternalVersion(t *testing.T) {
	s := newOptimisticTestServer(t)
	_ = s.store.Put([]byte("meta/test"), map[string]interface{}{})

	meta, status, errResp := s.applyWrite(writeOp{
		Index: "test", ID: "1", Doc: map[string]interface{}{"a": 1},
		Version: 5, VersionType: "external",
	})
	assert.Nil(t, errResp)
	assert.Equal(t, 201, status)
	assert.Equal(t, int64(5), meta.Version)
}

// TestReadDocMeta_NotExist 不存在的 doc
func TestReadDocMeta_NotExist(t *testing.T) {
	s := newOptimisticTestServer(t)
	_, ok := s.readDocMeta("nonexistent", "x")
	assert.False(t, ok)
}

// TestHandleDocIndexWithSeqNo 端到端: PUT 带 if_seq_no
func TestHandleDocIndexWithSeqNo(t *testing.T) {
	s := newOptimisticTestServer(t)
	_ = s.store.Put([]byte("meta/test"), map[string]interface{}{})

	// 第一次创建
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/test/_doc/1?op_type=create", strings.NewReader(`{"a":1}`))
	s.handleDocIndexForName(rr, req, "test", "1")
	assert.Equal(t, 201, rr.Code, "create -> 201")

	// 第二次同 if_seq_no=1 -> 200 (matched)
	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("PUT", "/test/_doc/1?if_seq_no=1", strings.NewReader(`{"a":2}`))
	s.handleDocIndexForName(rr2, req2, "test", "1")
	assert.Equal(t, 200, rr2.Code, "if_seq_no matched -> 200")

	// 第三次 if_seq_no=1 (now stale) -> 409
	rr3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("PUT", "/test/_doc/1?if_seq_no=1", strings.NewReader(`{"a":3}`))
	s.handleDocIndexForName(rr3, req3, "test", "1")
	assert.Equal(t, 409, rr3.Code, "stale if_seq_no -> 409")
}

// TestHandleDocGet_ReturnsSeqNo
func TestHandleDocGet_ReturnsSeqNo(t *testing.T) {
	s := newOptimisticTestServer(t)
	_ = s.store.Put([]byte("meta/test"), map[string]interface{}{})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/test/_doc/1", strings.NewReader(`{"a":1}`))
	s.handleDocIndexForName(rr, req, "test", "1")
	assert.Equal(t, 201, rr.Code)

	// GET
	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/test/_doc/1", nil)
	s.handleDocGetForName(rr2, req2, "test", "1")
	assert.Equal(t, 200, rr2.Code)
	body := rr2.Body.String()
	assert.Contains(t, body, `"_seq_no":1`)
	assert.Contains(t, body, `"_primary_term":1`)
	assert.Contains(t, body, `"_version":1`)
}
