// 倒排索引分段(Segment)
//
// 设计:
//   - 内存 Buffer: 新写入直接走内存倒排(同 #7)
//   - Segment: 不可变倒排快照, 落盘到 segment/<index>/<seg_id>
//   - 触发条件: Buffer 写数达到 threshold 时, 自动 flush
//   - 搜索: 合并 buffer + 已 flush 的 segment 结果
//
// 存储 key:
//   - segment/<index>/<seg_id>      -> SegmentData{field, postings: {term: [docID]}}
//   - seg_meta/<index>/<seg_id>     -> {created_at, doc_count, size_bytes}
//   - seg_active/<index>            -> [seg_id, ...] (按时间升序)
//   - seg_counter/<index>           -> int64 (下一个 seg id)
//
// 不做:
//   - 不做 segment merge (留待 #10 后续)
//   - 不做 doc values / column stride (ES segment 概念广, 此处仅核心倒排)
//   - 不做 per-segment sorted index (依赖 #4 的 sortedCache)
package server

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zixiliuyue/go_es/internal/storage"
)

// SegmentConfig segment flush 触发配置
type SegmentConfig struct {
	// MaxBufferDocs 内存 buffer 达到该 doc 数触发 flush
	MaxBufferDocs int
	// MaxBufferBytes 内存 buffer 达到该字节数触发 flush
	MaxBufferBytes int64
	// AutoFlushInterval 即使未达阈值, 这么久也 flush (s)
	AutoFlushIntervalSec int
}

// SegmentData 单个 segment 的倒排数据
// 序列化格式: {"field":..., "postings":{"term":[docID,...]}, "doc_ids":[...], "bloom":...}
type SegmentData struct {
	Field    string              `json:"field"`
	Postings map[string][]string `json:"postings"`
	DocIDs   []string            `json:"doc_ids"` // segment 内的所有 doc id
	// Bloom 术语布隆过滤器 (base64 bitset), 用于快速跳过不含某 term 的 segment
	// 空 = 未启用; 非空 = SearchTerm 先查 bloom, miss 则跳过该 segment
	Bloom string `json:"bloom,omitempty"`
}

// SegmentMeta segment 元信息
type SegmentMeta struct {
	SegID     int64     `json:"seg_id"`
	Index     string    `json:"index"`
	CreatedAt time.Time `json:"created_at"`
	DocCount  int       `json:"doc_count"`
	SizeBytes int64     `json:"size_bytes"`
}

// SegmentManager 各索引的 segment 管理
type SegmentManager struct {
	mu     sync.Mutex
	cfg    SegmentConfig
	store  *storage.Store
	engine searchEngineIface

	// 各索引的 active segment 列表 (从 store 加载)
	activeSegs map[string][]int64
	// 各索引的 segment counter
	segCounter map[string]int64
	// 各索引的 buffer 写计数
	bufferCount map[string]int64
	// 各索引的 buffer 字节数(估算)
	bufferBytes map[string]int64
	// 各索引的 last flush 时间
	lastFlush map[string]time.Time

	// 统计
	stats SegmentStats
}

// SegmentStats segment 统计
type SegmentStats struct {
	TotalSegments int64 `json:"total_segments"`
	TotalFlushes  int64 `json:"total_flushes"`
	TotalBytes    int64 `json:"total_bytes"`
}

// searchEngineIface 抽象 engine 所需方法, 便于 mock
type searchEngineIface interface {
	// SnapshotIndex 拿 index 倒排的浅拷贝
	//   - 返回 field -> term -> set(docID)
	SnapshotIndex(index string) map[string]map[string]map[string]struct{}
	// LoadSegmentPostings 把一个 segment 的 postings 加载到 engine 内存倒排
	//   - 返回加载的 term 数
	LoadSegmentPostings(index, field string, postings map[string][]string) int
}

// NewSegmentManager 构造
func NewSegmentManager(cfg SegmentConfig, store *storage.Store, engine searchEngineIface) *SegmentManager {
	if cfg.MaxBufferDocs <= 0 {
		cfg.MaxBufferDocs = 10000
	}
	if cfg.MaxBufferBytes <= 0 {
		cfg.MaxBufferBytes = 64 << 20 // 64 MB
	}
	if cfg.AutoFlushIntervalSec <= 0 {
		cfg.AutoFlushIntervalSec = 30
	}
	return &SegmentManager{
		cfg:         cfg,
		store:       store,
		engine:      engine,
		activeSegs:  make(map[string][]int64),
		segCounter:  make(map[string]int64),
		bufferCount: make(map[string]int64),
		bufferBytes: make(map[string]int64),
		lastFlush:   make(map[string]time.Time),
	}
}

// segmentKey segment 数据的 key
func segmentKey(index string, segID int64) []byte {
	return []byte(fmt.Sprintf("segment/%s/%020d", index, segID))
}

// segMetaKey segment 元信息 key
func segMetaKey(index string, segID int64) []byte {
	return []byte(fmt.Sprintf("seg_meta/%s/%020d", index, segID))
}

// segActiveKey active segment 列表 key
func segActiveKey(index string) []byte {
	return []byte("seg_active/" + index)
}

// segCounterKey segment counter key
func segCounterKey(index string) []byte {
	return []byte("seg_counter/" + index)
}

// loadActiveSegs 从 store 加载某 index 的 active segments
func (m *SegmentManager) loadActiveSegs(index string) []int64 {
	var ids []int64
	found, err := m.store.Get(segActiveKey(index), &ids)
	if err != nil || !found {
		return nil
	}
	return ids
}

// saveActiveSegs 持久化 active list
func (m *SegmentManager) saveActiveSegs(index string, ids []int64) error {
	return m.store.Put(segActiveKey(index), ids)
}

// nextSegID 分配下一个 seg id (原子)
func (m *SegmentManager) nextSegID(index string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.segCounter[index]
	if !ok {
		// 从 store 加载
		raw, found, _ := m.store.GetRaw(segCounterKey(index))
		if found {
			fmt.Sscanf(string(raw), "%d", &v)
		}
	}
	v++
	m.segCounter[index] = v
	_ = m.store.Put(segCounterKey(index), v)
	return v
}

// OnWrite 写后回调, 增加 buffer 计数
// 返回 true 表示应触发 flush
func (m *SegmentManager) OnWrite(index string, docSize int) bool {
	m.mu.Lock()
	m.bufferCount[index]++
	m.bufferBytes[index] += int64(docSize)
	shouldFlush := m.bufferCount[index] >= int64(m.cfg.MaxBufferDocs) ||
		m.bufferBytes[index] >= m.cfg.MaxBufferBytes
	m.mu.Unlock()
	return shouldFlush
}

// FlushNow 强制 flush 某 index 的 buffer 到 segment
// 返回新建的 segment 数量
func (m *SegmentManager) FlushNow(index string) (int, error) {
	m.mu.Lock()
	if m.engine == nil {
		m.mu.Unlock()
		return 0, fmt.Errorf("no engine")
	}
	// 拿倒排快照
	snap := m.engine.SnapshotIndex(index)
	// 按 field 拆分(简化: 整个 index 倒排作为 1 segment)
	if len(snap) == 0 {
		m.mu.Unlock()
		return 0, nil
	}
	// 拼装: field -> {term: [docID], docIDs, bloom}
	type fieldSeg struct {
		field    string
		postings map[string][]string
		docIDs   []string
		bloom    *bloomFilter
	}
	var fields []fieldSeg
	for field, terms := range snap {
		fs := fieldSeg{
			field:    field,
			postings: make(map[string][]string, len(terms)),
			bloom:    newBloomFilter(len(terms)),
		}
		docIDSet := make(map[string]struct{})
		for term, docSet := range terms {
			ids := make([]string, 0, len(docSet))
			for id := range docSet {
				ids = append(ids, id)
				docIDSet[id] = struct{}{}
			}
			sort.Strings(ids)
			fs.postings[term] = ids
			fs.bloom.Add(term)
		}
		fs.docIDs = make([]string, 0, len(docIDSet))
		for id := range docIDSet {
			fs.docIDs = append(fs.docIDs, id)
		}
		sort.Strings(fs.docIDs)
		fields = append(fields, fs)
	}
	// 重置 buffer
	m.bufferCount[index] = 0
	m.bufferBytes[index] = 0
	m.lastFlush[index] = time.Now()
	m.mu.Unlock()

	// 给每个 field 写一个 segment
	created := 0
	for _, f := range fields {
		segID := m.nextSegID(index)
		data := SegmentData{
			Field:    f.field,
			Postings: f.postings,
			DocIDs:   f.docIDs,
			Bloom:    f.bloom.MarshalBinary(),
		}
		raw, _ := json.Marshal(data)
		if err := m.store.PutRaw(segmentKey(index, segID), raw); err != nil {
			return created, err
		}
		// meta
		meta := SegmentMeta{
			SegID:     segID,
			Index:     index,
			CreatedAt: time.Now(),
			DocCount:  len(f.docIDs),
			SizeBytes: int64(len(raw)),
		}
		if err := m.store.Put(segMetaKey(index, segID), meta); err != nil {
			return created, err
		}
		// 追加到 active list
		m.mu.Lock()
		list := m.activeSegs[index]
		if list == nil {
			list = m.loadActiveSegs(index)
			m.activeSegs[index] = list
		}
		list = append(list, segID)
		m.activeSegs[index] = list
		_ = m.saveActiveSegs(index, list)
		m.mu.Unlock()
		created++
		atomic.AddInt64(&m.stats.TotalSegments, 1)
		atomic.AddInt64(&m.stats.TotalFlushes, 1)
		atomic.AddInt64(&m.stats.TotalBytes, meta.SizeBytes)
	}
	return created, nil
}

// ListSegments 列某 index 的所有 active segments
func (m *SegmentManager) ListSegments(index string) []SegmentMeta {
	m.mu.Lock()
	ids := m.activeSegs[index]
	if ids == nil {
		ids = m.loadActiveSegs(index)
		m.activeSegs[index] = ids
	}
	m.mu.Unlock()
	out := make([]SegmentMeta, 0, len(ids))
	for _, id := range ids {
		var meta SegmentMeta
		found, err := m.store.Get(segMetaKey(index, id), &meta)
		if err == nil && found {
			out = append(out, meta)
		}
	}
	return out
}

// SearchSegment 在某 segment 数据中查 term 的 docID 集合
func (s *SegmentData) SearchSegment(term string) []string {
	return s.Postings[term]
}

// Stats 统计
func (m *SegmentManager) Stats() SegmentStats {
	return SegmentStats{
		TotalSegments: atomic.LoadInt64(&m.stats.TotalSegments),
		TotalFlushes:  atomic.LoadInt64(&m.stats.TotalFlushes),
		TotalBytes:    atomic.LoadInt64(&m.stats.TotalBytes),
	}
}

// 防止 import 漂移
var _ = json.Marshal

// ------------------------------------------------------------------
// Bloom Filter (#10.2)
// ------------------------------------------------------------------

// bloomFilter 术语布隆过滤器
// 用于: SearchTerm 时快速跳过不含某 term 的 segment
//
// 设计:
//   - m = max(1024, n*10)  (bit 数, n=term 数)
//   - k = 4                 (hash 函数数, 用 FNV-1a + 不同 seed)
//   - 误判率 ≈ (1 - e^(-kn/m))^k, n=1000 时 ≈ 0.4%
type bloomFilter struct {
	bits []uint64 // bit 数组, 每个 uint64 存 64 bit
	m    int      // 总 bit 数
}

// newBloomFilter 根据预期 term 数构造
func newBloomFilter(n int) *bloomFilter {
	m := n * 10
	if m < 1024 {
		m = 1024
	}
	words := (m + 63) / 64
	return &bloomFilter{
		bits: make([]uint64, words),
		m:    m,
	}
}

// hash 生成 k 个不同位置的 hash (double hashing)
func (b *bloomFilter) hash(term string, k int) int {
	h1 := fnv1a(term, 0)
	h2 := fnv1a(term, 1)
	if h2 == 0 {
		h2 = 0x9e3779b9
	}
	return int((h1 + uint64(k)*h2) % uint64(b.m))
}

// Add 加入一个 term
func (b *bloomFilter) Add(term string) {
	for k := 0; k < 4; k++ {
		pos := b.hash(term, k)
		b.bits[pos/64] |= 1 << (pos % 64)
	}
}

// MayContain 检查 term 是否可能存在 (false = 一定不存在, true = 可能存在)
func (b *bloomFilter) MayContain(term string) bool {
	for k := 0; k < 4; k++ {
		pos := b.hash(term, k)
		if b.bits[pos/64]&(1<<(pos%64)) == 0 {
			return false
		}
	}
	return true
}

// MarshalBinary 序列化为 base64 字符串 (用于 SegmentData.Bloom)
func (b *bloomFilter) MarshalBinary() string {
	if b == nil || len(b.bits) == 0 {
		return ""
	}
	raw := make([]byte, len(b.bits)*8)
	for i, w := range b.bits {
		binary.LittleEndian.PutUint64(raw[i*8:], w)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// UnmarshalBinary 从 base64 字符串反序列化
func bloomFromBase64(s string) *bloomFilter {
	if s == "" {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(raw) < 8 {
		return nil
	}
	words := len(raw) / 8
	bits := make([]uint64, words)
	for i := 0; i < words; i++ {
		bits[i] = binary.LittleEndian.Uint64(raw[i*8:])
	}
	return &bloomFilter{
		bits: bits,
		m:    words * 64,
	}
}

// fnv1a FNV-1a 64-bit hash with seed
func fnv1a(s string, seed int) uint64 {
	h := uint64(14695981039346656037) ^ uint64(seed)*1099511628211
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

// ------------------------------------------------------------------
// 跨 segment 搜索 (#10.1)
// ------------------------------------------------------------------

// SearchTerm 在所有 active segment 中查 (field, term) 的 docID 集合
// 利用 bloom filter 快速跳过不含 term 的 segment
//
// 返回: 合并后的 docID 列表 (已排序)
func (m *SegmentManager) SearchTerm(index, field, term string) []string {
	m.mu.Lock()
	ids := m.activeSegs[index]
	if ids == nil {
		ids = m.loadActiveSegs(index)
		m.activeSegs[index] = ids
	}
	m.mu.Unlock()
	if len(ids) == 0 {
		return nil
	}
	set := make(map[string]struct{})
	for _, segID := range ids {
		var data SegmentData
		found, err := m.store.Get(segmentKey(index, segID), &data)
		if err != nil || !found {
			continue
		}
		if data.Field != field {
			continue
		}
		// bloom filter 快速跳过
		if data.Bloom != "" {
			bf := bloomFromBase64(data.Bloom)
			if bf != nil && !bf.MayContain(term) {
				continue
			}
		}
		for _, id := range data.Postings[term] {
			set[id] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// SearchAllDocIDs 返回 index 所有 active segment 的 docID 并集 (已排序)
// 用于 match_all 场景的诊断
func (m *SegmentManager) SearchAllDocIDs(index string) []string {
	m.mu.Lock()
	ids := m.activeSegs[index]
	if ids == nil {
		ids = m.loadActiveSegs(index)
		m.activeSegs[index] = ids
	}
	m.mu.Unlock()
	if len(ids) == 0 {
		return nil
	}
	set := make(map[string]struct{})
	for _, segID := range ids {
		var data SegmentData
		found, err := m.store.Get(segmentKey(index, segID), &data)
		if err != nil || !found {
			continue
		}
		for _, id := range data.DocIDs {
			set[id] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// LoadSegmentsIntoEngine 把 index 的所有 active segment 加载到 engine 内存倒排
// 用于冷启动: 从 segment 恢复倒排, 避免逐 doc 重新分词
//
// 返回: 加载的 segment 数 + term 数
func (m *SegmentManager) LoadSegmentsIntoEngine(index string) (segs, terms int, err error) {
	m.mu.Lock()
	ids := m.activeSegs[index]
	if ids == nil {
		ids = m.loadActiveSegs(index)
		m.activeSegs[index] = ids
	}
	m.mu.Unlock()
	if len(ids) == 0 {
		return 0, 0, nil
	}
	for _, segID := range ids {
		var data SegmentData
		found, gerr := m.store.Get(segmentKey(index, segID), &data)
		if gerr != nil {
			return segs, terms, gerr
		}
		if !found {
			continue
		}
		n := m.engine.LoadSegmentPostings(index, data.Field, data.Postings)
		segs++
		terms += n
	}
	return segs, terms, nil
}

// ------------------------------------------------------------------
// Segment Merge (#10.3)
// ------------------------------------------------------------------

// MergeSegments 合并 index 的小 segment 为更大的 segment
// 策略: 按 size_bytes 升序, 把最小的 N 个 segment 合并成一个
//
// 参数:
//   - index: 索引名
//   - maxSegments: 剩余 segment 数上限 (<=1 表示不合并)
//
// 返回: 合并掉的 segment 数 + 新建 segment 数
//
// 说明:
//   - 不阻塞查询: 合并期间旧 segment 仍在 active 列表中, 合完后原子替换
//   - 合并后旧 segment 数据 + meta 从 store 删除
func (m *SegmentManager) MergeSegments(index string, maxSegments int) (merged, created int, err error) {
	if maxSegments < 1 {
		return 0, 0, nil
	}
	m.mu.Lock()
	ids := m.activeSegs[index]
	if ids == nil {
		ids = m.loadActiveSegs(index)
		m.activeSegs[index] = ids
	}
	m.mu.Unlock()
	if len(ids) <= maxSegments {
		return 0, 0, nil
	}
	// 读所有 segment meta, 按 size 升序
	type segInfo struct {
		segID int64
		meta  SegmentMeta
	}
	infos := make([]segInfo, 0, len(ids))
	for _, id := range ids {
		var meta SegmentMeta
		found, _ := m.store.Get(segMetaKey(index, id), &meta)
		if found {
			infos = append(infos, segInfo{segID: id, meta: meta})
		}
	}
	if len(infos) <= maxSegments {
		return 0, 0, nil
	}
	// 按 size 升序, 合并最小的 (len - maxSegments + 1) 个
	sort.Slice(infos, func(i, j int) bool { return infos[i].meta.SizeBytes < infos[j].meta.SizeBytes })
	numToMerge := len(infos) - maxSegments + 1
	toMerge := infos[:numToMerge]
	// 保留的 segment
	keepIDs := make([]int64, 0, maxSegments)
	for _, info := range infos[numToMerge:] {
		keepIDs = append(keepIDs, info.segID)
	}
	// 按 field 分组合并 (同 field 的 segment 合并成一个)
	fieldGroups := make(map[string][]segInfo)
	for _, info := range toMerge {
		var data SegmentData
		found, _ := m.store.Get(segmentKey(index, info.segID), &data)
		if !found {
			continue
		}
		fieldGroups[data.Field] = append(fieldGroups[data.Field], info)
	}
	// 合并每个 field 的 segment
	newSegIDs := make([]int64, 0)
	for field, group := range fieldGroups {
		mergedPostings := make(map[string]map[string]struct{})
		docIDSet := make(map[string]struct{})
		for _, info := range group {
			var data SegmentData
			found, _ := m.store.Get(segmentKey(index, info.segID), &data)
			if !found {
				continue
			}
			for term, docIDs := range data.Postings {
				if mergedPostings[term] == nil {
					mergedPostings[term] = make(map[string]struct{})
				}
				for _, id := range docIDs {
					mergedPostings[term][id] = struct{}{}
					docIDSet[id] = struct{}{}
				}
			}
		}
		// 写新 segment
		segID := m.nextSegID(index)
		postings := make(map[string][]string, len(mergedPostings))
		bf := newBloomFilter(len(mergedPostings))
		for term, docSet := range mergedPostings {
			ids := make([]string, 0, len(docSet))
			for id := range docSet {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			postings[term] = ids
			bf.Add(term)
		}
		docIDs := make([]string, 0, len(docIDSet))
		for id := range docIDSet {
			docIDs = append(docIDs, id)
		}
		sort.Strings(docIDs)
		newData := SegmentData{
			Field:    field,
			Postings: postings,
			DocIDs:   docIDs,
			Bloom:    bf.MarshalBinary(),
		}
		raw, _ := json.Marshal(newData)
		if err := m.store.PutRaw(segmentKey(index, segID), raw); err != nil {
			return merged, created, err
		}
		newMeta := SegmentMeta{
			SegID:     segID,
			Index:     index,
			CreatedAt: time.Now(),
			DocCount:  len(docIDs),
			SizeBytes: int64(len(raw)),
		}
		if err := m.store.Put(segMetaKey(index, segID), newMeta); err != nil {
			return merged, created, err
		}
		newSegIDs = append(newSegIDs, segID)
		atomic.AddInt64(&m.stats.TotalSegments, 1)
		atomic.AddInt64(&m.stats.TotalFlushes, 1)
		atomic.AddInt64(&m.stats.TotalBytes, newMeta.SizeBytes)
		created++
	}
	merged = numToMerge
	// 原子替换 active list: 保留的 + 新建的
	finalIDs := append(keepIDs, newSegIDs...)
	sort.Slice(finalIDs, func(i, j int) bool { return finalIDs[i] < finalIDs[j] })
	m.mu.Lock()
	m.activeSegs[index] = finalIDs
	_ = m.saveActiveSegs(index, finalIDs)
	m.mu.Unlock()
	// 删除旧 segment 数据 + meta
	for _, info := range toMerge {
		_ = m.store.Delete(segmentKey(index, info.segID))
		_ = m.store.Delete(segMetaKey(index, info.segID))
		atomic.AddInt64(&m.stats.TotalSegments, -1)
		atomic.AddInt64(&m.stats.TotalBytes, -info.meta.SizeBytes)
	}
	return merged, created, nil
}
