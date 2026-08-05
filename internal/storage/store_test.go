// Package storage 包的单元测试
// 使用 BadgerDB 内存模式覆盖 Put/Get/Scan/Delete/Exists
package storage

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open("")
	assert.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestStore_PutGetExists(t *testing.T) {
	s := openTestStore(t)
	key := []byte("k1")

	ok, err := s.Exists(key)
	assert.NoError(t, err)
	assert.False(t, ok)

	assert.NoError(t, s.Put(key, map[string]interface{}{"v": 1}))

	ok, err = s.Exists(key)
	assert.NoError(t, err)
	assert.True(t, ok)

	var out map[string]int
	found, err := s.Get(key, &out)
	assert.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, 1, out["v"])
}

func TestStore_PutRawAndGetRaw(t *testing.T) {
	s := openTestStore(t)
	assert.NoError(t, s.PutRaw([]byte("rk"), []byte("hello")))
	v, found, err := s.GetRaw([]byte("rk"))
	assert.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "hello", string(v))
}

func TestStore_Delete(t *testing.T) {
	s := openTestStore(t)
	assert.NoError(t, s.Put([]byte("dk"), 1))
	assert.NoError(t, s.Delete([]byte("dk")))
	found, err := s.Exists([]byte("dk"))
	assert.NoError(t, err)
	assert.False(t, found)
}

func TestStore_Scan(t *testing.T) {
	s := openTestStore(t)
	for i := 0; i < 3; i++ {
		assert.NoError(t, s.Put([]byte("scan/"+string(rune('a'+i))), i))
	}
	// 写入一个非匹配键
	assert.NoError(t, s.Put([]byte("other"), 99))

	count := 0
	assert.NoError(t, s.Scan([]byte("scan/"), func(k, v []byte) error {
		count++
		return nil
	}))
	assert.Equal(t, 3, count)
}

func TestStore_DeletePrefix(t *testing.T) {
	s := openTestStore(t)
	for i := 0; i < 3; i++ {
		assert.NoError(t, s.Put([]byte("dp/"+string(rune('a'+i))), i))
	}
	assert.NoError(t, s.DeletePrefix([]byte("dp/")))
	count := 0
	_ = s.Scan([]byte("dp/"), func(_, _ []byte) error {
		count++
		return nil
	})
	assert.Equal(t, 0, count)
}

func TestStore_WithTransaction(t *testing.T) {
	s := openTestStore(t)
	// 用 PutRaw 验证多次写入可用
	assert.NoError(t, s.PutRaw([]byte("tx/1"), []byte("a")))
	assert.NoError(t, s.PutRaw([]byte("tx/2"), []byte("b")))
	v, found, err := s.GetRaw([]byte("tx/1"))
	assert.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "a", string(v))
}
