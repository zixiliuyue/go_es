// Package cache 单元测试
// 覆盖: New/MakeKey/Get/Set/InvalidateIndex/InvalidateAll/Stats/HitRate/Marshal/Unmarshal/evict/并发
package cache

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestNew_DefaultCapacity 验证 New 的默认容量逻辑
func TestNew_DefaultCapacity(t *testing.T) {
	c := New(0)
	assert.Equal(t, 1000, c.capacity, "zero capacity should default to 1000")

	c2 := New(-5)
	assert.Equal(t, 1000, c2.capacity, "negative capacity should default to 1000")

	c3 := New(500)
	assert.Equal(t, 500, c3.capacity, "explicit capacity should be respected")
}

// TestMakeKey_Deterministic 验证 MakeKey 确定性
func TestMakeKey_Deterministic(t *testing.T) {
	// 相同输入产生相同 key
	k1 := MakeKey([]string{"idx1"}, []byte(`{"q":"test"}`))
	k2 := MakeKey([]string{"idx1"}, []byte(`{"q":"test"}`))
	assert.Equal(t, k1, k2)

	// 不同输入产生不同 key
	k3 := MakeKey([]string{"idx1"}, []byte(`{"q":"other"}`))
	assert.NotEqual(t, k1, k3)
}

// TestMakeKey_OrderIndependent 验证索引顺序无关
func TestMakeKey_OrderIndependent(t *testing.T) {
	k1 := MakeKey([]string{"a", "b"}, []byte(`{}`))
	k2 := MakeKey([]string{"b", "a"}, []byte(`{}`))
	assert.Equal(t, k1, k2, "index order should not affect key")
}

// TestMakeKey_EmptyInput 验证空输入
func TestMakeKey_EmptyInput(t *testing.T) {
	k1 := MakeKey(nil, nil)
	assert.NotEmpty(t, k1, "key should still be generated for empty inputs")

	k2 := MakeKey([]string{}, []byte{})
	assert.NotEmpty(t, k2)
	assert.Equal(t, k1, k2, "nil and empty slice should produce same key")
}

// TestCache_SetGet 验证基本 Set/Get
func TestCache_SetGet(t *testing.T) {
	c := New(10)

	c.Set("k1", []byte("value1"), []string{"idx1"})
	val, hit := c.Get("k1")
	assert.True(t, hit)
	assert.Equal(t, []byte("value1"), val)

	// 未命中
	_, hit2 := c.Get("nonexistent")
	assert.False(t, hit2)

	stats := c.Stats()
	assert.Equal(t, uint64(1), stats.Hits)
	assert.Equal(t, uint64(1), stats.Misses)
	assert.Equal(t, 1, stats.Size)
}

// TestCache_Overwrite 验证 Set 覆盖已有 key
func TestCache_Overwrite(t *testing.T) {
	c := New(10)

	c.Set("k1", []byte("v1"), []string{"idx1"})
	c.Set("k1", []byte("v2"), []string{"idx2"})

	val, hit := c.Get("k1")
	assert.True(t, hit)
	assert.Equal(t, []byte("v2"), val)

	stats := c.Stats()
	assert.Equal(t, 1, stats.Size)
}

// TestCache_LRUEviction 验证 LRU 淘汰
func TestCache_LRUEviction(t *testing.T) {
	c := New(2)

	c.Set("k1", []byte("v1"), []string{"idx1"})
	c.Set("k2", []byte("v2"), []string{"idx1"})

	// 访问 k1, 使其变成最近使用
	c.Get("k1")

	// 新增 k3, 应淘汰最旧的 k2
	c.Set("k3", []byte("v3"), []string{"idx2"})

	_, hitK1 := c.Get("k1")
	assert.True(t, hitK1, "k1 should survive (was accessed recently)")

	_, hitK2 := c.Get("k2")
	assert.False(t, hitK2, "k2 should be evicted (oldest, not accessed)")

	_, hitK3 := c.Get("k3")
	assert.True(t, hitK3, "k3 should exist")

	stats := c.Stats()
	assert.Equal(t, 2, stats.Size)
}

// TestCache_InvalidateIndex 验证按索引失效
func TestCache_InvalidateIndex(t *testing.T) {
	c := New(10)

	c.Set("k1", []byte("v1"), []string{"idx1"})
	c.Set("k2", []byte("v2"), []string{"idx1", "idx2"})
	c.Set("k3", []byte("v3"), []string{"idx2"})

	// 失效 idx1, k1 和 k2 应该被移除, k3 保留
	c.InvalidateIndex("idx1")

	_, hit := c.Get("k1")
	assert.False(t, hit, "k1 should be invalidated")

	_, hit2 := c.Get("k2")
	assert.False(t, hit2, "k2 should be invalidated (linked to idx1)")

	_, hit3 := c.Get("k3")
	assert.True(t, hit3, "k3 should survive (only linked to idx2)")

	stats := c.Stats()
	assert.Equal(t, 1, stats.Size)
}

// TestCache_InvalidateIndex_Empty 验证失效不存在的索引不报错
func TestCache_InvalidateIndex_Empty(t *testing.T) {
	c := New(10)
	assert.NotPanics(t, func() {
		c.InvalidateIndex("nonexistent")
	})
}

// TestCache_InvalidateAll 验证全量失效
func TestCache_InvalidateAll(t *testing.T) {
	c := New(10)

	c.Set("k1", []byte("v1"), []string{"idx1"})
	c.Set("k2", []byte("v2"), []string{"idx2"})
	c.Set("k3", []byte("v3"), []string{"idx3"})

	c.InvalidateAll()

	stats := c.Stats()
	assert.Equal(t, 0, stats.Size)

	_, hit := c.Get("k1")
	assert.False(t, hit)
}

// TestCache_HitRate 验证命中率计算
func TestCache_HitRate(t *testing.T) {
	c := New(10)

	// 空缓存 0%
	assert.Equal(t, 0.0, c.HitRate())

	c.Set("k1", []byte("v1"), []string{"idx1"})
	c.Get("k1") // hit
	c.Get("k2") // miss
	c.Get("k1") // hit

	assert.InDelta(t, 2.0/3.0, c.HitRate(), 0.001)
}

// TestMarshalUnmarshal 验证序列化/反序列化
func TestMarshalUnmarshal(t *testing.T) {
	resp := map[string]interface{}{
		"took": 5,
		"hits": map[string]interface{}{
			"total": map[string]interface{}{"value": float64(10)},
		},
	}

	data, err := MarshalResponse(resp)
	assert.NoError(t, err)
	assert.NotEmpty(t, data)

	decoded, err := UnmarshalResponse(data)
	assert.NoError(t, err)
	assert.Equal(t, float64(5), decoded["took"])
}

// TestUnmarshal_InvalidJSON 验证反序列化非法 JSON
func TestUnmarshal_InvalidJSON(t *testing.T) {
	_, err := UnmarshalResponse([]byte("not valid json"))
	assert.Error(t, err)
}

// TestCache_Concurrent 验证并发安全
func TestCache_Concurrent(t *testing.T) {
	c := New(100)

	var wg sync.WaitGroup
	n := 50

	// 并发写入
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			key := "k" + string(rune('0'+idx%10))
			c.Set(key, []byte("v"), []string{"idx"})
		}(i)
	}
	wg.Wait()

	// 并发读写
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(idx int) {
			defer wg.Done()
			key := "k" + string(rune('0'+idx%10))
			c.Get(key)
		}(i)
		go func(idx int) {
			defer wg.Done()
			c.InvalidateIndex("idx")
		}(i)
	}
	wg.Wait()

	// 不应 panic
	stats := c.Stats()
	t.Logf("concurrent test final stats: size=%d hits=%d misses=%d", stats.Size, stats.Hits, stats.Misses)
}
