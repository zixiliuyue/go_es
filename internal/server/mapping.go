// 映射(Mapping)校验模块
// 作用: 在文档写入前,按索引 mapping 做类型校验与动态字段控制
//
// 与 ES 对齐的行为:
//   - dynamic: true  (默认) — 未声明字段自动接受(不校验)
//   - dynamic: false — 未声明字段被忽略(不报错)
//   - dynamic: strict — 未声明字段报错 mapper_parsing_exception
//   - 已声明字段按 type 做类型校验(文本/数值/布尔/日期)
//
// 注意: 本实现是"乐观"校验, 不支持复杂的对象嵌套 mapping(对象类型交给后续迭代)
package server

import (
	"encoding/json"
	"fmt"

	"github.com/zixiliuyue/go_es/internal/storage"
)

// MappingValidator 基于 mapping 配置校验文档
type MappingValidator struct {
	properties map[string]FieldMapping // 字段名 → 字段映射
	dynamic    string                 // true / false / strict, 默认 true
	hasMapping bool                   // 是否存在 mapping(用于区分"无 mapping"与"空 mapping")
}

// FieldMapping 单个字段的映射信息
type FieldMapping struct {
	Name string // 字段名
	Type string // 字段类型(text / keyword / integer / long / float / double / boolean / date / object / nested / ip / geo_point / ...)
}

// NewMappingValidator 从 IndexMeta.Mapping 构造校验器
//
// 参数:
//   - mapping: 从 meta/<index> 读取的 IndexMeta.Mapping 字段(即 "mappings" 子对象)
//     允许 nil(表示没有 mapping 配置, 所有字段都接受)
//
// 返回:
//   - *MappingValidator: 可直接调用 Validate() 校验文档
func NewMappingValidator(mapping map[string]interface{}) *MappingValidator {
	v := &MappingValidator{
		dynamic:    "true",
		hasMapping: false,
	}
	if mapping == nil {
		return v
	}
	v.hasMapping = true

	// 解析 dynamic 字段
	if dy, ok := mapping["dynamic"]; ok {
		if s, ok := dy.(string); ok {
			switch s {
			case "true", "false", "strict":
				v.dynamic = s
			}
		}
	}

	// 解析 properties 字段
	if props, ok := mapping["properties"]; ok {
		if m, ok := props.(map[string]interface{}); ok {
			v.properties = make(map[string]FieldMapping, len(m))
			for name, raw := range m {
				if fm, ok := raw.(map[string]interface{}); ok {
					fd := FieldMapping{Name: name}
					if t, ok := fm["type"]; ok {
						if s, ok := t.(string); ok {
							fd.Type = s
						}
					}
					v.properties[name] = fd
				}
			}
		}
	}
	return v
}

// Validate 校验一个文档对象是否符合 mapping
//
// 参数:
//   - doc: 待写入的 _source 文档
//
// 返回:
//   - nil: 校验通过
//   - error: 校验失败, 错误消息以 "mapper_parsing_exception: " 开头, 便于上层直接透传
func (v *MappingValidator) Validate(doc map[string]interface{}) error {
	// 无 mapping: 所有字段都接受
	if !v.hasMapping {
		return nil
	}

	for field, value := range doc {
		fm, declared := v.properties[field]
		if !declared {
			// 动态字段处理
			switch v.dynamic {
			case "strict":
				return fmt.Errorf("mapper_parsing_exception: field [%s] not defined in mapping and dynamic is [strict]", field)
			case "false":
				// dynamic=false: 忽略未声明字段, 不报错
				continue
			default: // true
				// dynamic=true: 接受
				continue
			}
		}

		// 已声明字段: 校验类型
		if fm.Type != "" {
			if err := validateFieldType(fm, value); err != nil {
				return fmt.Errorf("mapper_parsing_exception: field [%s] %s", field, err.Error())
			}
		}
	}
	return nil
}

// validateFieldType 校验单个字段值的类型是否匹配 mapping 声明
//
// 支持的映射类型:
//   - text / keyword / ip / geo_point / date — 接受 string
//   - integer / long / short / byte / float / double / scaled_float — 接受 int/float(自动类型转换)
//   - boolean — 接受 bool 或 string("true"/"false")
//   - object / nested — 接受 map[string]interface{}
//
// 参数:
//   - fm: 字段映射(含 Name 和 Type)
//   - value: 文档中该字段的值
//
// 返回:
//   - nil: 类型匹配或可接受的类型转换
//   - error: 类型不匹配
func validateFieldType(fm FieldMapping, value interface{}) error {
	switch fm.Type {
	case "text", "keyword", "ip", "geo_point", "date":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("expected type [%s], but got [%T]", fm.Type, value)
		}
	case "integer", "long", "short", "byte", "float", "double", "scaled_float":
		// 接受 int/int64/float64/bool(numeric coercion)/json.Number(UseNumber 解码)
		switch value.(type) {
		case int, int64, float64:
			// 合法数值类型
		case bool:
			// ES 允许 boolean → numeric: true=1, false=0
		case json.Number:
			// decodeJSON 使用 UseNumber(), 数字会以 json.Number 形式出现
			// 尝试转成 float64 验证合法性
			if _, err := value.(json.Number).Float64(); err != nil {
				return fmt.Errorf("expected numeric type [%s], but invalid json.Number: %v", fm.Type, err)
			}
		default:
			return fmt.Errorf("expected numeric type [%s], but got [%T]", fm.Type, value)
		}
	case "boolean":
		if _, ok := value.(bool); ok {
			return nil
		}
		if s, ok := value.(string); ok && (s == "true" || s == "false") {
			return nil
		}
		return fmt.Errorf("expected type [boolean], but got [%T]", value)
	case "object", "nested":
		if _, ok := value.(map[string]interface{}); !ok {
			return fmt.Errorf("expected object type [%s], but got [%T]", fm.Type, value)
		}
	default:
		// 未知类型: 不做校验, 兼容未来扩展
	}
	return nil
}

// Dynamic 返回当前 dynamic 模式
func (v *MappingValidator) Dynamic() string {
	return v.dynamic
}

// HasMapping 返回是否存在 mapping 配置
func (v *MappingValidator) HasMapping() bool {
	return v.hasMapping
}

// FieldCount 返回已声明的字段数量
func (v *MappingValidator) FieldCount() int {
	return len(v.properties)
}

// validateDocMapping 加载索引 meta, 构造 MappingValidator, 校验文档
//
// 调用方: handleDocIndexForName / handleDocIndexAutoIDForName / handleUpdateForName
// 在文档解析完成、pipeline 处理完成后调用, 校验失败直接返回错误响应
//
// 参数:
//   - index: 索引名
//   - doc:  待写入的 _source 文档
//
// 返回:
//   - nil: 校验通过(包括无 mapping 的情况)
//   - error: 校验失败, 包含 HTTP 状态码和错误体
func (s *Server) validateDocMapping(index string, doc map[string]interface{}) error {
	var meta IndexMeta
	found, err := s.store.Get(storage.MetaKey(index), &meta)
	if err != nil || !found {
		return nil // 索引不存在时由后续流程处理, 这里不报错
	}

	validator := NewMappingValidator(meta.Mapping)
	if validator.HasMapping() {
		return validator.Validate(doc)
	}
	return nil
}
