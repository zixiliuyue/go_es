// Package errors 定义了项目中使用的错误类型
// 提供统一的错误处理和错误码定义
package errors

import (
	"fmt"
)

// ErrorCode 错误码类型
type ErrorCode int

const (
	// ErrCodeUnknown 未知错误
	ErrCodeUnknown ErrorCode = iota
	// ErrCodeClientInit 客户端初始化失败
	ErrCodeClientInit
	// ErrCodeIndexExists 索引已存在
	ErrCodeIndexExists
	// ErrCodeIndexNotFound 索引不存在
	ErrCodeIndexNotFound
	// ErrCodeCreateIndex 创建索引失败
	ErrCodeCreateIndex
	// ErrCodeDeleteIndex 删除索引失败
	ErrCodeDeleteIndex
	// ErrCodeIndexExistsCheck 检查索引存在性失败
	ErrCodeIndexExistsCheck
	// ErrCodeDocumentNotFound 文档不存在
	ErrCodeDocumentNotFound
	// ErrCodeDocumentCreate 创建文档失败
	ErrCodeDocumentCreate
	// ErrCodeDocumentGet 获取文档失败
	ErrCodeDocumentGet
	// ErrCodeDocumentUpdate 更新文档失败
	ErrCodeDocumentUpdate
	// ErrCodeDocumentDelete 删除文档失败
	ErrCodeDocumentDelete
	// ErrCodeBulkOperation 批量操作失败
	ErrCodeBulkOperation
	// ErrCodeSearch 搜索失败
	ErrCodeSearch
	// ErrCodeAggregate 聚合失败
	ErrCodeAggregate
	// ErrCodeMarshalJSON JSON序列化失败
	ErrCodeMarshalJSON
	// ErrCodeUnmarshalJSON JSON反序列化失败
	ErrCodeUnmarshalJSON
)

// ESError 自定义错误类型
type ESError struct {
	Code    ErrorCode
	Message string
	Cause   error
}

// Error 实现error接口
func (e *ESError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("[%d] %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("[%d] %s", e.Code, e.Message)
}

// Unwrap 获取原始错误
func (e *ESError) Unwrap() error {
	return e.Cause
}

// New 创建新的错误
func New(code ErrorCode, message string) *ESError {
	return &ESError{
		Code:    code,
		Message: message,
	}
}

// Wrap 包装已有错误
func Wrap(code ErrorCode, message string, cause error) *ESError {
	return &ESError{
		Code:    code,
		Message: message,
		Cause:   cause,
	}
}

// Is 判断错误码是否匹配
func Is(err error, code ErrorCode) bool {
	if err == nil {
		return false
	}

	esErr, ok := err.(*ESError)
	if !ok {
		return false
	}

	return esErr.Code == code
}

// 预定义错误实例
var (
	ErrIndexNotFound     = New(ErrCodeIndexNotFound, "index not found")
	ErrIndexExists       = New(ErrCodeIndexExists, "index already exists")
	ErrDocumentNotFound  = New(ErrCodeDocumentNotFound, "document not found")
	ErrClientInitFailed  = New(ErrCodeClientInit, "client initialization failed")
	ErrMarshalJSONFailed = New(ErrCodeMarshalJSON, "json marshal failed")
)
