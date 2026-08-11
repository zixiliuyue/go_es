package server

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zixiliuyue/go_es/internal/search"
	"github.com/zixiliuyue/go_es/internal/storage"
	"go.uber.org/zap"
)

// TestMappingValidator_NoMapping 无 mapping 配置时, 所有字段都接受
func TestMappingValidator_NoMapping(t *testing.T) {
	v := NewMappingValidator(nil)
	assert.False(t, v.HasMapping())
	assert.Equal(t, "true", v.Dynamic())
	assert.Equal(t, 0, v.FieldCount())

	err := v.Validate(map[string]interface{}{"any_field": "hello"})
	assert.NoError(t, err)

	err = v.Validate(map[string]interface{}{"a": 1, "b": true, "c": 3.14})
	assert.NoError(t, err)
}

// TestMappingValidator_EmptyMapping 空 mapping 对象(有 mapping 字段但没有 properties), 所有字段都接受
func TestMappingValidator_EmptyMapping(t *testing.T) {
	v := NewMappingValidator(map[string]interface{}{})
	assert.True(t, v.HasMapping())
	assert.Equal(t, "true", v.Dynamic())
	assert.Equal(t, 0, v.FieldCount())

	err := v.Validate(map[string]interface{}{"any_field": "hello"})
	assert.NoError(t, err)
}

// TestMappingValidator_DynamicTrue dynamic=true(默认), 未声明字段接受, 已声明字段类型校验
func TestMappingValidator_DynamicTrue(t *testing.T) {
	mapping := map[string]interface{}{
		"properties": map[string]interface{}{
			"title": map[string]interface{}{"type": "text"},
			"count": map[string]interface{}{"type": "integer"},
		},
	}
	v := NewMappingValidator(mapping)
	assert.True(t, v.HasMapping())
	assert.Equal(t, "true", v.Dynamic())
	assert.Equal(t, 2, v.FieldCount())

	// 已声明字段, 类型正确 → 通过
	err := v.Validate(map[string]interface{}{
		"title": "hello world",
		"count": float64(42),
	})
	assert.NoError(t, err)

	// 未声明字段, dynamic=true → 接受
	err = v.Validate(map[string]interface{}{
		"title":    "hello",
		"new_field": "should be accepted",
	})
	assert.NoError(t, err)

	// 已声明字段类型不匹配 → 报错
	err = v.Validate(map[string]interface{}{
		"title": float64(123),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "mapper_parsing_exception")
	assert.Contains(t, err.Error(), "title")
	assert.Contains(t, err.Error(), "text")
}

// TestMappingValidator_DynamicFalse dynamic=false, 未声明字段被忽略不报错
func TestMappingValidator_DynamicFalse(t *testing.T) {
	mapping := map[string]interface{}{
		"dynamic": "false",
		"properties": map[string]interface{}{
			"title": map[string]interface{}{"type": "text"},
		},
	}
	v := NewMappingValidator(mapping)
	assert.Equal(t, "false", v.Dynamic())

	// 未声明字段 + dynamic=false → 忽略不报错
	err := v.Validate(map[string]interface{}{
		"title":        "hello",
		"unknown_field": "should be ignored",
	})
	assert.NoError(t, err)

	// 已声明字段类型不匹配 → 仍然报错
	err = v.Validate(map[string]interface{}{
		"title": float64(123),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "mapper_parsing_exception")
}

// TestMappingValidator_DynamicStrict dynamic=strict, 未声明字段报错
func TestMappingValidator_DynamicStrict(t *testing.T) {
	mapping := map[string]interface{}{
		"dynamic": "strict",
		"properties": map[string]interface{}{
			"title": map[string]interface{}{"type": "text"},
		},
	}
	v := NewMappingValidator(mapping)
	assert.Equal(t, "strict", v.Dynamic())

	// 已声明字段 → 通过
	err := v.Validate(map[string]interface{}{
		"title": "hello",
	})
	assert.NoError(t, err)

	// 未声明字段 → 报错
	err = v.Validate(map[string]interface{}{
		"title":     "hello",
		"bad_field": "not allowed",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "mapper_parsing_exception")
	assert.Contains(t, err.Error(), "bad_field")
	assert.Contains(t, err.Error(), "strict")
}

// TestMappingValidator_TypeValidation 各种字段类型的校验
func TestMappingValidator_TypeValidation(t *testing.T) {
	tests := []struct {
		name    string
		fields  map[string]interface{} // properties 配置
		doc     map[string]interface{} // 文档内容
		wantErr bool
		errMsg  string // 错误消息片段
	}{
		{
			name: "text 接受字符串",
			fields: map[string]interface{}{
				"title": map[string]interface{}{"type": "text"},
			},
			doc:     map[string]interface{}{"title": "hello"},
			wantErr: false,
		},
		{
			name: "text 拒绝数值",
			fields: map[string]interface{}{
				"title": map[string]interface{}{"type": "text"},
			},
			doc:     map[string]interface{}{"title": float64(123)},
			wantErr: true,
			errMsg:  "text",
		},
		{
			name: "keyword 接受字符串",
			fields: map[string]interface{}{
				"status": map[string]interface{}{"type": "keyword"},
			},
			doc:     map[string]interface{}{"status": "active"},
			wantErr: false,
		},
		{
			name: "keyword 拒绝布尔",
			fields: map[string]interface{}{
				"status": map[string]interface{}{"type": "keyword"},
			},
			doc:     map[string]interface{}{"status": true},
			wantErr: true,
			errMsg:  "keyword",
		},
		{
			name: "integer 接受数值",
			fields: map[string]interface{}{
				"count": map[string]interface{}{"type": "integer"},
			},
			doc:     map[string]interface{}{"count": float64(42)},
			wantErr: false,
		},
		{
			name: "integer 接受整数",
			fields: map[string]interface{}{
				"count": map[string]interface{}{"type": "integer"},
			},
			doc:     map[string]interface{}{"count": float64(42)},
			wantErr: false,
		},
		{
			name: "integer 接受布尔(数值转换)",
			fields: map[string]interface{}{
				"flag": map[string]interface{}{"type": "integer"},
			},
			doc:     map[string]interface{}{"flag": true},
			wantErr: false,
		},
		{
			name: "integer 拒绝字符串",
			fields: map[string]interface{}{
				"count": map[string]interface{}{"type": "integer"},
			},
			doc:     map[string]interface{}{"count": "not a number"},
			wantErr: true,
			errMsg:  "integer",
		},
		{
			name: "boolean 接受布尔",
			fields: map[string]interface{}{
				"active": map[string]interface{}{"type": "boolean"},
			},
			doc:     map[string]interface{}{"active": true},
			wantErr: false,
		},
		{
			name: "boolean 接受字符串 true",
			fields: map[string]interface{}{
				"active": map[string]interface{}{"type": "boolean"},
			},
			doc:     map[string]interface{}{"active": "true"},
			wantErr: false,
		},
		{
			name: "boolean 接受字符串 false",
			fields: map[string]interface{}{
				"active": map[string]interface{}{"type": "boolean"},
			},
			doc:     map[string]interface{}{"active": "false"},
			wantErr: false,
		},
		{
			name: "boolean 拒绝字符串 yes",
			fields: map[string]interface{}{
				"active": map[string]interface{}{"type": "boolean"},
			},
			doc:     map[string]interface{}{"active": "yes"},
			wantErr: true,
			errMsg:  "boolean",
		},
		{
			name: "object 接受对象",
			fields: map[string]interface{}{
				"address": map[string]interface{}{"type": "object"},
			},
			doc: map[string]interface{}{"address": map[string]interface{}{
				"city": "beijing",
			}},
			wantErr: false,
		},
		{
			name: "object 拒绝字符串",
			fields: map[string]interface{}{
				"address": map[string]interface{}{"type": "object"},
			},
			doc:     map[string]interface{}{"address": "not an object"},
			wantErr: true,
			errMsg:  "object",
		},
		{
			name: "date 接受字符串",
			fields: map[string]interface{}{
				"created": map[string]interface{}{"type": "date"},
			},
			doc:     map[string]interface{}{"created": "2025-01-01"},
			wantErr: false,
		},
		{
			name: "date 拒绝数值",
			fields: map[string]interface{}{
				"created": map[string]interface{}{"type": "date"},
			},
			doc:     map[string]interface{}{"created": float64(1234567890)},
			wantErr: true,
			errMsg:  "date",
		},
		{
			name: "unknown type 跳过校验",
			fields: map[string]interface{}{
				"weird": map[string]interface{}{"type": "some_unknown_type"},
			},
			doc:     map[string]interface{}{"weird": map[string]interface{}{"anything": "goes"}},
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mapping := map[string]interface{}{
				"properties": tc.fields,
			}
			v := NewMappingValidator(mapping)
			err := v.Validate(tc.doc)
			if tc.wantErr {
				assert.Error(t, err)
				if tc.errMsg != "" {
					assert.Contains(t, err.Error(), tc.errMsg)
				}
				assert.Contains(t, err.Error(), "mapper_parsing_exception")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestMappingValidator_MultipleFields 多字段混合校验(部分对,部分错)
func TestMappingValidator_MultipleFields(t *testing.T) {
	mapping := map[string]interface{}{
		"dynamic": "strict",
		"properties": map[string]interface{}{
			"title": map[string]interface{}{"type": "text"},
			"count": map[string]interface{}{"type": "integer"},
			"active": map[string]interface{}{"type": "boolean"},
		},
	}
	v := NewMappingValidator(mapping)

	// 全部正确
	err := v.Validate(map[string]interface{}{
		"title":  "hello",
		"count":  float64(42),
		"active": true,
	})
	assert.NoError(t, err)

	// 一个字段类型错误
	err = v.Validate(map[string]interface{}{
		"title":  float64(123),
		"count":  float64(42),
		"active": true,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "title")

	// 未声明字段 + strict
	err = v.Validate(map[string]interface{}{
		"title":  "hello",
		"count":  float64(42),
		"active": true,
		"extra":  "not allowed",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "extra")
}

// TestMappingValidator_EmptyDoc 空文档校验
func TestMappingValidator_EmptyDoc(t *testing.T) {
	v := NewMappingValidator(map[string]interface{}{
		"dynamic": "strict",
		"properties": map[string]interface{}{
			"title": map[string]interface{}{"type": "text"},
		},
	})
	err := v.Validate(map[string]interface{}{})
	assert.NoError(t, err)
}

// TestMappingValidator_InvalidDynamicValue dynamic 字段为非法值时回退到默认 true
func TestMappingValidator_InvalidDynamicValue(t *testing.T) {
	mapping := map[string]interface{}{
		"dynamic": "invalid_value",
		"properties": map[string]interface{}{
			"title": map[string]interface{}{"type": "text"},
		},
	}
	v := NewMappingValidator(mapping)
	assert.Equal(t, "true", v.Dynamic()) // 回退到默认值

	// 未声明字段应被接受(dynamic=true)
	err := v.Validate(map[string]interface{}{
		"title":     "hello",
		"new_field": "should be accepted",
	})
	assert.NoError(t, err)
}

// TestMappingValidator_NoTypeField property 声明存在但没有 type 字段 → 跳过类型校验
func TestMappingValidator_NoTypeField(t *testing.T) {
	mapping := map[string]interface{}{
		"properties": map[string]interface{}{
			"custom": map[string]interface{}{}, // 空对象, 无 type
		},
	}
	v := NewMappingValidator(mapping)
	assert.Equal(t, 1, v.FieldCount())

	// 无 type 的字段跳过校验
	err := v.Validate(map[string]interface{}{
		"custom": "anything",
	})
	assert.NoError(t, err)

	err = v.Validate(map[string]interface{}{
		"custom": 123,
	})
	assert.NoError(t, err)
}

// TestMappingValidator_InvalidPropertyFormat properties 下的 value 不是 map → 跳过
func TestMappingValidator_InvalidPropertyFormat(t *testing.T) {
	mapping := map[string]interface{}{
		"properties": map[string]interface{}{
			"bad_field": "not a map", // 格式错误
			"good":      map[string]interface{}{"type": "text"},
		},
	}
	v := NewMappingValidator(mapping)
	assert.Equal(t, 1, v.FieldCount()) // 只有 good 被解析

	// good 字段应被校验
	err := v.Validate(map[string]interface{}{
		"good": float64(123),
	})
	assert.Error(t, err)
}

// TestMappingValidator_Integration 集成测试: 通过 validateDocMapping 在实际 server 中校验
func TestMappingValidator_Integration(t *testing.T) {
	store, err := storage.Open("")
	require.NoError(t, err)
	defer store.Close()
	engine := search.New(store)
	srv := New(store, engine, zap.NewNop())

	// 1. 创建带 mapping 的索引
	meta := IndexMeta{
		Name: "test_index",
		Mapping: map[string]interface{}{
			"dynamic": "strict",
			"properties": map[string]interface{}{
				"title": map[string]interface{}{"type": "text"},
				"count": map[string]interface{}{"type": "integer"},
			},
		},
	}
	require.NoError(t, store.Put(storage.MetaKey("test_index"), meta))

	// 2. 校验正确文档 → 通过
	err = srv.validateDocMapping("test_index", map[string]interface{}{
		"title": "hello",
		"count": float64(10),
	})
	assert.NoError(t, err)

	// 3. 校验类型错误 → 报错
	err = srv.validateDocMapping("test_index", map[string]interface{}{
		"title": float64(123),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "mapper_parsing_exception")

	// 4. 校验未声明字段 + strict → 报错
	err = srv.validateDocMapping("test_index", map[string]interface{}{
		"title": "hello",
		"extra": "not allowed",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "extra")

	// 5. 不存在的索引 → 不报错(交给后续流程处理)
	err = srv.validateDocMapping("nonexistent", map[string]interface{}{
		"field": "value",
	})
	assert.NoError(t, err)
}

// TestMappingValidator_Integration_HTTP 集成测试: 通过 HTTP 写入, mapping 校验返回正确的 HTTP 状态码
func TestMappingValidator_Integration_HTTP(t *testing.T) {
	ts := newTestServer(t)

	// 1. 创建带 mapping 的索引
	body := map[string]interface{}{
		"mappings": map[string]interface{}{
			"dynamic": "strict",
			"properties": map[string]interface{}{
				"title": map[string]interface{}{"type": "text"},
				"count": map[string]interface{}{"type": "integer"},
			},
		},
	}
	resp, raw := do(t, ts, "PUT", "/strict_idx", body)
	assert.Equal(t, 200, resp.StatusCode)
	assert.NotEmpty(t, raw)

	// 2. 正确类型写入 → 201
	resp, _ = do(t, ts, "PUT", "/strict_idx/_doc/1", map[string]interface{}{"title": "hello", "count": 42})
	assert.Equal(t, 201, resp.StatusCode)

	// 3. 类型错误 → 400 + mapper_parsing_exception
	resp, raw = do(t, ts, "PUT", "/strict_idx/_doc/2", map[string]interface{}{"title": 123})
	assert.Equal(t, 400, resp.StatusCode)
	assert.Contains(t, string(raw), "mapper_parsing_exception")

	// 4. 未声明字段 + strict → 400
	resp, raw = do(t, ts, "PUT", "/strict_idx/_doc/3", map[string]interface{}{"title": "hi", "unknown": "field"})
	assert.Equal(t, 400, resp.StatusCode)
	assert.Contains(t, string(raw), "mapper_parsing_exception")

	// 5. 不存在的索引 → 404
	resp, _ = do(t, ts, "PUT", "/no_idx/_doc/1", map[string]interface{}{"title": "hi"})
	assert.Equal(t, 404, resp.StatusCode)
}

// TestMappingValidator_DynamicFalse_HTTP 集成测试: dynamic=false 模式
func TestMappingValidator_DynamicFalse_HTTP(t *testing.T) {
	ts := newTestServer(t)

	body := map[string]interface{}{
		"mappings": map[string]interface{}{
			"dynamic": "false",
			"properties": map[string]interface{}{
				"title": map[string]interface{}{"type": "text"},
			},
		},
	}
	resp, _ := do(t, ts, "PUT", "/dyn_false", body)
	assert.Equal(t, 200, resp.StatusCode)

	// 未声明字段被忽略 → 201
	resp, _ = do(t, ts, "PUT", "/dyn_false/_doc/1", map[string]interface{}{"title": "hi", "extra": "ignored"})
	assert.Equal(t, 201, resp.StatusCode)

	// 类型错误仍报错 → 400
	resp, raw := do(t, ts, "PUT", "/dyn_false/_doc/2", map[string]interface{}{"title": 123})
	assert.Equal(t, 400, resp.StatusCode)
	assert.Contains(t, string(raw), "mapper_parsing_exception")
}

// TestMappingValidator_Update_HTTP 集成测试: update 路径也走 mapping 校验
func TestMappingValidator_Update_HTTP(t *testing.T) {
	ts := newTestServer(t)

	body := map[string]interface{}{
		"mappings": map[string]interface{}{
			"dynamic": "strict",
			"properties": map[string]interface{}{
				"title": map[string]interface{}{"type": "text"},
			},
		},
	}
	resp, _ := do(t, ts, "PUT", "/upd_idx", body)
	assert.Equal(t, 200, resp.StatusCode)

	// 写入初始文档
	resp, _ = do(t, ts, "PUT", "/upd_idx/_doc/1", map[string]interface{}{"title": "hello"})
	assert.Equal(t, 201, resp.StatusCode)

	// update 类型不匹配 → 400
	resp, raw := do(t, ts, "POST", "/upd_idx/_update/1", map[string]interface{}{"doc": map[string]interface{}{"title": 123}})
	assert.Equal(t, 400, resp.StatusCode)
	assert.Contains(t, string(raw), "mapper_parsing_exception")

	// update 未声明字段 + strict → 400
	resp, raw = do(t, ts, "POST", "/upd_idx/_update/1", map[string]interface{}{"doc": map[string]interface{}{"extra": "bad"}})
	assert.Equal(t, 400, resp.StatusCode)
	assert.Contains(t, string(raw), "mapper_parsing_exception")

	// update 正确 → 200
	resp, _ = do(t, ts, "POST", "/upd_idx/_update/1", map[string]interface{}{"doc": map[string]interface{}{"title": "world"}})
	assert.Equal(t, 200, resp.StatusCode)
}

// TestMappingValidator_AutoID_HTTP 集成测试: auto-ID 路径也走 mapping 校验
func TestMappingValidator_AutoID_HTTP(t *testing.T) {
	ts := newTestServer(t)

	body := map[string]interface{}{
		"mappings": map[string]interface{}{
			"dynamic": "strict",
			"properties": map[string]interface{}{
				"title": map[string]interface{}{"type": "text"},
			},
		},
	}
	resp, _ := do(t, ts, "PUT", "/auto_idx", body)
	assert.Equal(t, 200, resp.StatusCode)

	// 类型错误 → 400
	resp, raw := do(t, ts, "POST", "/auto_idx/_doc", map[string]interface{}{"title": 123})
	assert.Equal(t, 400, resp.StatusCode)
	assert.Contains(t, string(raw), "mapper_parsing_exception")

	// 正确 → 201
	resp, _ = do(t, ts, "POST", "/auto_idx/_doc", map[string]interface{}{"title": "hello"})
	assert.Equal(t, 201, resp.StatusCode)
}

// TestMappingValidator_NoMapping_HTTP 集成测试: 无 mapping 时所有写入都通过
func TestMappingValidator_NoMapping_HTTP(t *testing.T) {
	ts := newTestServer(t)

	resp, _ := do(t, ts, "PUT", "/free_idx", map[string]interface{}{})
	assert.Equal(t, 200, resp.StatusCode)

	// 任何类型都能写入
	resp, _ = do(t, ts, "PUT", "/free_idx/_doc/1", map[string]interface{}{"a": "text", "b": 123, "c": true})
	assert.Equal(t, 201, resp.StatusCode)
}

// TestMappingValidator_DynamicField_HTTP 集成测试: dynamic=true 时未声明字段接受
func TestMappingValidator_DynamicField_HTTP(t *testing.T) {
	ts := newTestServer(t)

	body := map[string]interface{}{
		"mappings": map[string]interface{}{
			"properties": map[string]interface{}{
				"known": map[string]interface{}{"type": "text"},
			},
		},
	}
	resp, _ := do(t, ts, "PUT", "/dyn_true", body)
	assert.Equal(t, 200, resp.StatusCode)

	// 未声明字段接受
	resp, _ = do(t, ts, "PUT", "/dyn_true/_doc/1", map[string]interface{}{"known": "hi", "unknown": "accepted"})
	assert.Equal(t, 201, resp.StatusCode)

	// 已声明字段类型错误仍报错
	resp, raw := do(t, ts, "PUT", "/dyn_true/_doc/2", map[string]interface{}{"known": 123})
	assert.Equal(t, 400, resp.StatusCode)
	assert.Contains(t, string(raw), "mapper_parsing_exception")
}
