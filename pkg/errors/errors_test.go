// Package errors 包的单元测试
// 测试错误处理功能
package errors

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNew(t *testing.T) {
	err := New(ErrCodeUnknown, "test error")
	assert.NotNil(t, err)
	assert.Equal(t, ErrCodeUnknown, err.Code)
	assert.Equal(t, "test error", err.Message)
	assert.Nil(t, err.Cause)
	assert.Contains(t, err.Error(), "[0] test error")
}

func TestWrap(t *testing.T) {
	cause := errors.New("original error")
	err := Wrap(ErrCodeUnknown, "wrap error", cause)
	assert.NotNil(t, err)
	assert.Equal(t, ErrCodeUnknown, err.Code)
	assert.Equal(t, "wrap error", err.Message)
	assert.Equal(t, cause, err.Cause)
	assert.Contains(t, err.Error(), "original error")
}

func TestESError_Unwrap(t *testing.T) {
	cause := errors.New("original error")
	err := Wrap(ErrCodeUnknown, "wrap error", cause)
	unwrapped := err.Unwrap()
	assert.Equal(t, cause, unwrapped)
}

func TestIs(t *testing.T) {
	err := New(ErrCodeIndexNotFound, "index not found")
	assert.True(t, Is(err, ErrCodeIndexNotFound))
	assert.False(t, Is(err, ErrCodeDocumentNotFound))

	// 非ESError类型应该返回false
	stdErr := errors.New("standard error")
	assert.False(t, Is(stdErr, ErrCodeUnknown))
}

func TestPredefinedErrors(t *testing.T) {
	assert.NotNil(t, ErrIndexNotFound)
	assert.NotNil(t, ErrIndexExists)
	assert.NotNil(t, ErrDocumentNotFound)
	assert.NotNil(t, ErrClientInitFailed)
	assert.NotNil(t, ErrMarshalJSONFailed)

	assert.Equal(t, ErrCodeIndexNotFound, ErrIndexNotFound.Code)
	assert.Equal(t, ErrCodeIndexExists, ErrIndexExists.Code)
}

func TestErrorMessages(t *testing.T) {
	testCases := []struct {
		err  *ESError
		code ErrorCode
		msg  string
	}{
		{ErrIndexNotFound, ErrCodeIndexNotFound, "index not found"},
		{ErrIndexExists, ErrCodeIndexExists, "index already exists"},
		{ErrDocumentNotFound, ErrCodeDocumentNotFound, "document not found"},
		{ErrClientInitFailed, ErrCodeClientInit, "client initialization failed"},
	}

	for _, tc := range testCases {
		assert.Equal(t, tc.code, tc.err.Code)
		assert.Equal(t, tc.msg, tc.err.Message)
	}
}

func TestError_ErrorWithoutCause(t *testing.T) {
	err := New(ErrCodeUnknown, "test error")
	msg := err.Error()
	assert.Equal(t, "[0] test error", msg)
}
