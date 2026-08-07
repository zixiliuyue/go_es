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
// 序列化格式: {"field":..., "postings":{"term":[docID,...]}, "doc_ids":[...]}
type SegmentData struct {
	Field    string              `json:"field"`
	Postings map[string][]string `json:"postings"`
	DocIDs   []string            `json:"doc_ids"` // segment 内的所有 doc id
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
	// 拼装: field -> {term: [docID]}
	type fieldSeg struct {
		field    string
		postings map[string][]string
		docIDs   []string
	}
	var fields []fieldSeg
	docIDSet := make(map[string]struct{})
	for field, terms := range snap {
		fs := fieldSeg{field: field, postings: make(map[string][]string, len(terms))}
		for term, docSet := range terms {
			ids := make([]string, 0, len(docSet))
			for id := range docSet {
				ids = append(ids, id)
				docIDSet[id] = struct{}{}
			}
			sort.Strings(ids)
			fs.postings[term] = ids
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
