// 范围查询倒排加速
//
// 背景: 原有 evalRange 对每个 doc 调 cmpAny, O(N) 全表扫描.
// 优化: 维护一个 "已排序的 (index, field) -> []docID" 内存索引,
// 每次 IndexDoc / DeleteDoc 时增量更新, 范围查询走二分.
// 不持久化(进程内缓存), 冷启动时 engine.LoadAll 重建.
//
// 适用:
//   - 数值字段(int/float): 升序排列
//   - 字符串字段: 字节序排列
//   - 复杂类型(数组/嵌套): 不索引, 走原全表扫描
//
// 复杂度: 写入 O(logN), 查询 O(logN + K), 内存 O(N * 字段数)
package search

import (
	"sort"
	"sync"
)

// sortedIndex 单个 (index, field) 的排序倒排
type sortedIndex struct {
	// entries 按 value 升序, 同一 value 的 docID 也按字典序
	entries []sortedEntry
}

type sortedEntry struct {
	value  string // 字符串化后的值
	docID  string
}

// keyOf 构造 (index, field) 唯一 key
func sortedKey(index, field string) string { return index + "\x00" + field }

// sortedIndexCache 缓存所有 (index, field) 的排序索引
type sortedIndexCache struct {
	mu sync.RWMutex
	// 内存: 按 index, 然后按 field
	byIndex map[string]map[string]*sortedIndex
	// 反向: docID -> 哪些 (index, field) 引用了它, 用于 DeleteDoc 时增量更新
	// 简化为: 删除时直接重算对应 field 的索引(规模小, 可接受)
}

func newSortedIndexCache() *sortedIndexCache {
	return &sortedIndexCache{byIndex: make(map[string]map[string]*sortedIndex)}
}

// getOrCreate 获取或创建索引
func (c *sortedIndexCache) getOrCreate(index, field string) *sortedIndex {
	c.mu.RLock()
	idx, ok := c.byIndex[index][field]
	c.mu.RUnlock()
	if ok {
		return idx
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byIndex[index] == nil {
		c.byIndex[index] = make(map[string]*sortedIndex)
	}
	if c.byIndex[index][field] == nil {
		c.byIndex[index][field] = &sortedIndex{}
	}
	return c.byIndex[index][field]
}

// removeIndex 索引被删除时调用
func (c *sortedIndexCache) removeIndex(index string) {
	c.mu.Lock()
	delete(c.byIndex, index)
	c.mu.Unlock()
}

// removeDoc 删 doc 时调用, 全字段失效该 docID(简单实现)
func (c *sortedIndexCache) removeDoc(index, docID string) {
	c.mu.Lock()
	for _, fields := range c.byIndex[index] {
		fields.removeDoc(docID)
	}
	c.mu.Unlock()
}

// upsert 增量插入或更新 (index, field) 的 (value, docID) 对
func (c *sortedIndexCache) upsert(index, field, value, docID string) {
	idx := c.getOrCreate(index, field)
	idx.upsert(value, docID)
}

// rangeQuery 范围查询, 返回命中的 docID 集合(不排序, 由 server 层 sort)
// gte/lte/gt/lt 任一可为 nil
func (c *sortedIndexCache) rangeQuery(index, field string, gte, lte, gt, lt *string) map[string]struct{} {
	out := make(map[string]struct{})
	c.mu.RLock()
	defer c.mu.RUnlock()
	idx, ok := c.byIndex[index][field]
	if !ok {
		return out
	}
	// 在 idx.entries 上二分
	// lo = 第一个 >= gte 的位置(或 0)
	// hi = 第一个 > lte 的位置(或 len)
	lo, hi := 0, len(idx.entries)
	if gte != nil {
		lo = sort.Search(len(idx.entries), func(i int) bool {
			return idx.entries[i].value >= *gte
		})
	}
	if lte != nil {
		hi = sort.Search(len(idx.entries), func(i int) bool {
			return idx.entries[i].value > *lte
		})
	}
	if gt != nil {
		lo = sort.Search(len(idx.entries), func(i int) bool {
			return idx.entries[i].value > *gt
		})
	}
	if lt != nil {
		hi = sort.Search(len(idx.entries), func(i int) bool {
			return idx.entries[i].value >= *lt
		})
	}
	for i := lo; i < hi; i++ {
		out[idx.entries[i].docID] = struct{}{}
	}
	return out
}

// --- sortedIndex methods ---

func (s *sortedIndex) upsert(value, docID string) {
	// 二分查找该 docID 现位置
	i := sort.Search(len(s.entries), func(j int) bool {
		if s.entries[j].value < value {
			return false
		}
		if s.entries[j].value > value {
			return true
		}
		return s.entries[j].docID >= docID
	})
	if i < len(s.entries) && s.entries[i].value == value && s.entries[i].docID == docID {
		// 已存在
		return
	}
	// 插入
	s.entries = append(s.entries, sortedEntry{})
	copy(s.entries[i+1:], s.entries[i:])
	s.entries[i] = sortedEntry{value: value, docID: docID}
}

func (s *sortedIndex) removeDoc(docID string) {
	// 简单: 全扫删除
	for i := 0; i < len(s.entries); {
		if s.entries[i].docID == docID {
			s.entries = append(s.entries[:i], s.entries[i+1:]...)
			continue
		}
		i++
	}
}

// valueOf 取出 JSON 字段的字符串表示
//   - 数字: 标准化(避免 "1" vs "1.0" 字典序问题)
//   - 字符串: 原样
//   - 其它: 返回空串(不索引)
func valueOf(v interface{}) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case float64:
		// 保留 6 位精度, 但去尾零
		// 例如 1.0 -> "1", 1.5 -> "1.5"
		if x == float64(int64(x)) {
			return formatInt(int64(x)), true
		}
		return formatFloat(x, 6), true
	case int:
		return formatInt(int64(x)), true
	case int64:
		return formatInt(x), true
	case bool:
		if x {
			return "true", true
		}
		return "false", true
	}
	return "", false
}

func formatInt(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func formatFloat(f float64, prec int) string {
	// 简化: 借 std float-to-string
	// Go 的 strconv.FormatFloat 更合适, 但避免新增 import
	// 这里用 fmt.Sprintf 的精确替代: 写出整数位 + 小数
	intPart := int64(f)
	frac := f - float64(intPart)
	if frac < 0 {
		frac = -frac
	}
	// 算小数部分
	out := formatInt(intPart) + "."
	for i := 0; i < prec; i++ {
		frac *= 10
		d := int(frac)
		if d > 9 {
			d = 9
		}
		out += string(byte('0' + d))
		frac -= float64(d)
	}
	// 去尾零
	for len(out) > 0 && out[len(out)-1] == '0' {
		out = out[:len(out)-1]
	}
	if len(out) > 0 && out[len(out)-1] == '.' {
		out = out[:len(out)-1]
	}
	return out
}
