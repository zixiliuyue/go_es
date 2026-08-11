// Package cache 提供 go_es 服务端内存缓存能力
//
// 主要用途:
//   - 搜索结果评分缓存: 对相同 query (index + query body) 做 LRU 缓存,
//     高频重复查询 (如 UI 自动补全、dashboard 轮询) 直接命中缓存, 避免重复执行复杂查询。
//
// 设计原则:
//   - 纯内存实现, 单进程有效, 重启不持久化
//   - 写操作按索引失效 (索引级精确失效 + 全量失效)
//   - 线程安全 (sync.RWMutex 保护)
//   - 可观测: 暴露 hit/miss 计数器供 Prometheus 抓取
package cache

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
)

// Cache 搜索结果 LRU 缓存
//
// 字段:
//   - capacity: 最大缓存条目数, 超限时按 LRU 淘汰
//   - data: 实际缓存条目 (key -> entry)
//   - order: 双向链表维护访问顺序 (头是最新, 尾是最旧)
//   - indexKeys: 索引级失效索引 (index -> set of keys)
//   - hits/misses: 访问计数器
//
// 线程安全: 所有读写操作通过 RWMutex 保护
type Cache struct {
	mu        sync.RWMutex
	capacity  int
	data      map[string]*entry
	// 双向链表 (简化: 用 slice 按访问顺序存储, 每次 Get 移到末尾)
	order    []string
	indexKeys map[string]map[string]struct{} // index -> key set
	hits      uint64
	misses    uint64
}

// entry 缓存条目
type entry struct {
	key       string
	value     []byte        // 序列化后的响应体
	createdAt time.Time
	indexes   []string      // 该条目涉及的索引列表, 用于索引级失效
}

// New 创建一个新的 LRU 缓存
//
// 参数:
//   - capacity: 最大缓存条目数
//
// 返回:
//   - *Cache: 新缓存实例
//
// 使用建议:
//   - 搜索结果缓存建议 capacity >= 1000, 具体值根据内存调整
func New(capacity int) *Cache {
	if capacity <= 0 {
		capacity = 1000 // 默认 1000 条
	}
	return &Cache{
		capacity:  capacity,
		data:      make(map[string]*entry),
		order:     make([]string, 0, capacity),
		indexKeys: make(map[string]map[string]struct{}),
	}
}

// MakeKey 根据索引列表和查询体生成缓存 key
//
// key = SHA1(index1|index2|...|query_body_json)
//
// 参数:
//   - indices: 涉及的索引列表 (排序后拼接, 保证 a,b 与 b,a 结果一致)
//   - queryBody: 原始查询请求体 (JSON 字符串)
//
// 返回:
//   - string: 32 位十六进制 SHA1 哈希
func MakeKey(indices []string, queryBody []byte) string {
	// 排序索引名, 保证幂等
	sorted := make([]string, len(indices))
	copy(sorted, indices)
	sortStrings(sorted)

	h := sha1.New()
	// 写入有序索引列表
	for i, idx := range sorted {
		if i > 0 {
			h.Write([]byte("|"))
		}
		h.Write([]byte(idx))
	}
	h.Write([]byte("#"))
	h.Write(queryBody)
	return hex.EncodeToString(h.Sum(nil))
}

// Get 检查缓存是否命中
//
// 参数:
//   - key: 缓存 key (由 MakeKey 生成)
//
// 返回:
//   - value: 缓存的序列化数据 (命中时), 否则 nil
//   - hit: 是否命中
func (c *Cache) Get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.data[key]
	if !ok {
		c.misses++
		return nil, false
	}
	c.hits++

	// 更新 LRU 顺序: 移到末尾 (最新访问)
	c.removeFromOrder(key)
	c.order = append(c.order, key)

	return e.value, true
}

// Set 写入缓存
//
// 参数:
//   - key: 缓存 key
//   - value: 要缓存的序列化数据
//   - indexes: 该条目涉及的索引列表 (用于索引级失效)
func (c *Cache) Set(key string, value []byte, indexes []string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 检查容量
	if len(c.data) >= c.capacity && c.data[key] == nil {
		// 淘汰最旧的条目
		c.evict()
	}

	e := &entry{
		key:       key,
		value:     value,
		createdAt: time.Now(),
		indexes:   indexes,
	}
	c.data[key] = e

	// 维护 LRU 顺序
	c.removeFromOrder(key)
	c.order = append(c.order, key)

	// 建立索引级失效索引
	for _, idx := range indexes {
		if c.indexKeys[idx] == nil {
			c.indexKeys[idx] = make(map[string]struct{})
		}
		c.indexKeys[idx][key] = struct{}{}
	}
}

// InvalidateIndex 按索引名失效缓存
//
// 用例: 某个索引发生写操作后, 所有涉及该索引的缓存条目都必须失效
//
// 参数:
//   - index: 要失效的索引名
func (c *Cache) InvalidateIndex(index string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	keys, ok := c.indexKeys[index]
	if !ok {
		return
	}
	for k := range keys {
		delete(c.data, k)
		c.removeFromOrder(k)
	}
	delete(c.indexKeys, index)
}

// InvalidateAll 失效全部缓存
//
// 用例: 全局 flush、配置变更等
func (c *Cache) InvalidateAll() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.data = make(map[string]*entry)
	c.order = c.order[:0]
	c.indexKeys = make(map[string]map[string]struct{})
}

// Stats 返回缓存统计信息 (只读)
//
// 返回:
//   - Hits: 命中次数
//   - Misses: 未命中次数
//   - Size: 当前缓存条目数
//   - Capacity: 最大容量
type Stats struct {
	Hits     uint64
	Misses   uint64
	Size     int
	Capacity int
}

// Stats 获取当前缓存统计
func (c *Cache) Stats() Stats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return Stats{
		Hits:     c.hits,
		Misses:   c.misses,
		Size:     len(c.data),
		Capacity: c.capacity,
	}
}

// HitRate 计算缓存命中率 (0.0 ~ 1.0)
func (c *Cache) HitRate() float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	total := c.hits + c.misses
	if total == 0 {
		return 0.0
	}
	return float64(c.hits) / float64(total)
}

// MarshalResponse 将 map[string]interface{} 序列化为 JSON 字节
//
// 参数:
//   - resp: 要序列化的响应 map
//
// 返回:
//   - []byte: JSON 序列化结果
//   - error: 序列化错误
func MarshalResponse(resp map[string]interface{}) ([]byte, error) {
	return json.Marshal(resp)
}

// UnmarshalResponse 将 JSON 字节反序列化为 map[string]interface{}
//
// 参数:
//   - data: JSON 字节
//
// 返回:
//   - map[string]interface{}: 反序列化结果
//   - error: 反序列化错误
func UnmarshalResponse(data []byte) (map[string]interface{}, error) {
	var resp map[string]interface{}
	err := json.Unmarshal(data, &resp)
	return resp, err
}

// removeFromOrder 从 LRU 顺序列表中移除指定 key (调用方需持有锁)
func (c *Cache) removeFromOrder(key string) {
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
}

// evict 淘汰最旧的缓存条目 (调用方需持有锁)
func (c *Cache) evict() {
	if len(c.order) == 0 {
		return
	}
	// 最旧的在头部
	oldest := c.order[0]
	c.order = c.order[1:]

	// 从索引级失效索引中移除
	if e, ok := c.data[oldest]; ok {
		for _, idx := range e.indexes {
			if keys, exists := c.indexKeys[idx]; exists {
				delete(keys, oldest)
				if len(keys) == 0 {
					delete(c.indexKeys, idx)
				}
			}
		}
		delete(c.data, oldest)
	}
}

// sortStrings 对字符串切片排序 (简单排序, 避免引入 sort 包的 import 冲突)
func sortStrings(s []string) {
	for i := 0; i < len(s); i++ {
		for j := i + 1; j < len(s); j++ {
			if s[i] > s[j] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
}