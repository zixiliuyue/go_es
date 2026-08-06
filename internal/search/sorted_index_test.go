// sorted index 单元测试
package search

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSortedIndex_NumberRange(t *testing.T) {
	cache := newSortedIndexCache()
	for i := 1; i <= 10; i++ {
		cache.upsert("idx", "score", strconv.Itoa(i), "id_"+strconv.Itoa(i))
	}
	// 5 <= score <= 7 -> 5, 6, 7
	gte, lte := "5", "7"
	ids := cache.rangeQuery("idx", "score", &gte, &lte, nil, nil)
	assert.ElementsMatch(t, []string{"id_5", "id_6", "id_7"}, keysOf(ids))
}

func TestSortedIndex_OpenRange(t *testing.T) {
	cache := newSortedIndexCache()
	for i := 1; i <= 5; i++ {
		cache.upsert("idx", "n", strconv.Itoa(i), "id_"+strconv.Itoa(i))
	}
	// score > 3 -> 4, 5
	gt := "3"
	ids := cache.rangeQuery("idx", "n", nil, nil, &gt, nil)
	assert.ElementsMatch(t, []string{"id_4", "id_5"}, keysOf(ids))
}

func TestSortedIndex_DeleteDoc(t *testing.T) {
	cache := newSortedIndexCache()
	for i := 1; i <= 5; i++ {
		cache.upsert("idx", "n", strconv.Itoa(i), "id_"+strconv.Itoa(i))
	}
	cache.removeDoc("idx", "id_3")
	gte, lte := "1", "5"
	ids := cache.rangeQuery("idx", "n", &gte, &lte, nil, nil)
	assert.ElementsMatch(t, []string{"id_1", "id_2", "id_4", "id_5"}, keysOf(ids))
}

func TestSortedIndex_StringOrder(t *testing.T) {
	cache := newSortedIndexCache()
	// 字符串字典序
	for _, w := range []string{"banana", "apple", "cherry"} {
		cache.upsert("idx", "word", w, "id_"+w)
	}
	// "b" <= w < "d"  -> banana, cherry
	gte, lt := "b", "d"
	ids := cache.rangeQuery("idx", "word", &gte, nil, nil, &lt)
	assert.ElementsMatch(t, []string{"id_banana", "id_cherry"}, keysOf(ids))
}

func TestValueOf_NormalizeNumbers(t *testing.T) {
	// 1 vs 1.0 应视为相等 -> 字符串相同
	v1, _ := valueOf(float64(1))
	v2, _ := valueOf(int64(1))
	assert.Equal(t, v1, v2)
}

func keysOf(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
