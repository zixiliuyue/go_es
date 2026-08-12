// 倒排索引持久化与重建
//
// 设计:
//   - per-doc 分词结果: doc-tf/<index>/<id> -> {field: [tokens]}
//     写时自动落盘, LoadAll 优先走这条路(免去重新分词)
//   - per-posting 缓存: postings-cache/<index>/<field>/<token> -> {df, docs: [docID]}
//     可选; 若不存在则不命中, 走实时内存版
//   - 倒排版本号: postings-version/<index> -> int64
//     写时递增; 重建时可对照
//   - 提供 RebuildInverted(idx) 强制重建
//   - 启动 LoadAll 优化: 先尝试读 doc-tf, 失败/缺失再回退到 doc source
//
// 持久化时机:
//   - engine.IndexDoc 调用时, 同步落盘 doc-tf
//   - engine.DeleteDoc 调用时, 同步删 doc-tf
//   - 大批量(bulk) 走 batched 落盘(合并为一次事务)
//
// 不做:
//   - 不实现增量 postings 缓存同步(全量 Rebuild)
//   - 不做 checksum (badger 自带)
package search

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/zixiliuyue/go_es/internal/storage"
)

// PersistedTokens 持久化格式(per-doc):
//   { "field_name": ["tok1", "tok2", ...], ... }
type PersistedTokens map[string][]string

// PostingsCacheEntry 倒排缓存条目(per-token):
//   { "df": N, "docs": ["docID1", ...] }
type PostingsCacheEntry struct {
	DF   int      `json:"df"`
	Docs []string `json:"docs"`
}

// indexPersistence Engine 内部持久化状态
type indexPersistence struct {
	mu     sync.Mutex
	// writeBatch 暂存当前 batch 的 (index, id, tokens) 写入, 由 Flush 落盘
	writeBatch map[string]map[string]PersistedTokens
	// versionCache 缓存当前已知 version (避免每次读 store)
	versionCache map[string]int64
	// snapshotState postings-snapshot flush 状态 (#7)
	snapshotState *postingsSnapshotState
}

func newIndexPersistence() *indexPersistence {
	return &indexPersistence{
		writeBatch:    make(map[string]map[string]PersistedTokens),
		versionCache:  make(map[string]int64),
		snapshotState: newPostingsSnapshotState(),
	}
}

// persistDocTokens 把单个 doc 的分词结果落盘
func (e *Engine) persistDocTokens(index, id string, fields map[string][]string) error {
	if e.store == nil {
		return nil
	}
	if fields == nil {
		fields = map[string][]string{}
	}
	pt := PersistedTokens(fields)
	if err := e.store.Put(storage.DocTFKey(index, id), pt); err != nil {
		return err
	}
	return e.bumpPostingsVersion(index)
}

// deleteDocTokens 删 doc-tf
func (e *Engine) deleteDocTokens(index, id string) error {
	if e.store == nil {
		return nil
	}
	if err := e.store.Delete(storage.DocTFKey(index, id)); err != nil {
		return err
	}
	return e.bumpPostingsVersion(index)
}

// bumpPostingsVersion 递增 version (in-memory + on-disk)
func (e *Engine) bumpPostingsVersion(index string) error {
	if e.store == nil {
		return nil
	}
	e.persistence.mu.Lock()
	v := e.persistence.versionCache[index]
	v++
	e.persistence.versionCache[index] = v
	e.persistence.mu.Unlock()
	return e.store.Put(storage.PostingsVersionKey(index), v)
}

// getPostingsVersion 读 version (优先 cache)
func (e *Engine) getPostingsVersion(index string) int64 {
	if e.store == nil {
		return 0
	}
	e.persistence.mu.Lock()
	v, ok := e.persistence.versionCache[index]
	e.persistence.mu.Unlock()
	if ok {
		return v
	}
	raw, found, err := e.store.GetRaw(storage.PostingsVersionKey(index))
	if err != nil || !found {
		return 0
	}
	var ver int64
	_, _ = fmt.Sscanf(string(raw), "%d", &ver)
	e.persistence.mu.Lock()
	e.persistence.versionCache[index] = ver
	e.persistence.mu.Unlock()
	return ver
}

// PersistBatchOp 批量持久化操作
type PersistBatchOp struct {
	Index  string
	ID     string
	Tokens PersistedTokens
	Delete bool
}

// PersistBatch 一次性写多个 doc 的 doc-tf
func (e *Engine) PersistBatch(ops []PersistBatchOp) error {
	if e.store == nil || len(ops) == 0 {
		return nil
	}
	keys := make([][]byte, 0, len(ops))
	vals := make([]interface{}, 0, len(ops))
	dels := make([][]byte, 0, len(ops))
	for _, op := range ops {
		if op.Delete {
			dels = append(dels, storage.DocTFKey(op.Index, op.ID))
		} else {
			if op.Tokens == nil {
				op.Tokens = PersistedTokens{}
			}
			keys = append(keys, storage.DocTFKey(op.Index, op.ID))
			vals = append(vals, op.Tokens)
		}
	}
	for i, k := range keys {
		if err := e.store.Put(k, vals[i]); err != nil {
			return err
		}
	}
	for _, k := range dels {
		if err := e.store.Delete(k); err != nil {
			return err
		}
	}
	return nil
}

// LoadAll 优化版 LoadAll
// 快路径 (#7): 优先读 postings-snapshot/<index>/<field>, 直接填充 inverted
// 慢路径 (回退): 扫 doc-tf/* 逐 doc 重建倒排
//
// 流程:
//  1. 扫 doc/* 拿所有 (index, id, source) — 必须, _source 查询需要
//  2. 对每个 index, 尝试 LoadPostingsSnapshot (快路径)
//     - 成功: inverted 直接填充, 跳过 doc-tf 重建
//     - 失败/版本不匹配: 回退到 doc-tf 慢路径
//  3. 慢路径: 扫 doc-tf/*, 逐 doc rebuildDocInvertedFromTokens
//  4. 重建 scorer.fieldStats (BM25 统计, 不在 snapshot 中)
func (e *Engine) LoadAll() error {
	if e.store == nil {
		return nil
	}
	type docRow struct {
		idx string
		id  string
		src map[string]interface{}
	}
	rows := make([]docRow, 0)
	if err := e.store.Scan([]byte("doc/"), func(k, v []byte) error {
		rest := strings.TrimPrefix(string(k), "doc/")
		sep := strings.IndexByte(rest, '/')
		if sep < 0 {
			return nil
		}
		idx := rest[:sep]
		id := rest[sep+1:]
		var src map[string]interface{}
		if err := jsonUnmarshal(v, &src); err != nil {
			return err
		}
		rows = append(rows, docRow{idx: idx, id: id, src: src})
		return nil
	}); err != nil {
		return err
	}
	// 收集所有 index (从 doc rows)
	indexSet := make(map[string]struct{}, 64)
	for _, r := range rows {
		indexSet[r.idx] = struct{}{}
	}
	// 快路径: 对每个 index 尝试 LoadPostingsSnapshot
	// snapshotOK[index] = true 表示该 index 的倒排已从 snapshot 加载, 不需要走 doc-tf
	snapshotOK := make(map[string]bool, len(indexSet))
	e.mu.Lock()
	// 清空 docs (inverted 由快路径或慢路径填充)
	e.docs = make(map[string]map[string]map[string]interface{})
	// 清空 scorer 全量状态
	e.scorer.mu.Lock()
	e.scorer.postings = make(map[string]map[string]map[string]*PostingList)
	e.scorer.fieldStats = make(map[string]map[string]*FieldStats)
	e.scorer.fieldLen = make(map[string]map[string]map[string]int)
	e.scorer.mu.Unlock()
	for idx := range indexSet {
		loaded, _, ok, err := e.LoadPostingsSnapshot(idx)
		if err != nil {
			e.mu.Unlock()
			return fmt.Errorf("load postings snapshot for %s: %w", idx, err)
		}
		if ok && loaded > 0 {
			snapshotOK[idx] = true
		}
	}
	// 填 docs map
	for _, r := range rows {
		if e.docs[r.idx] == nil {
			e.docs[r.idx] = make(map[string]map[string]interface{})
		}
		e.docs[r.idx][r.id] = r.src
	}
	e.mu.Unlock()
	// 慢路径: 对 snapshot 未命中的 index, 走 doc-tf 重建
	slowIndices := make([]string, 0, len(indexSet))
	for idx := range indexSet {
		if !snapshotOK[idx] {
			slowIndices = append(slowIndices, idx)
		}
	}
	if len(slowIndices) > 0 {
		tfCache := make(map[string]PersistedTokens)
		if err := e.store.Scan([]byte("doc-tf/"), func(k, v []byte) error {
			rest := strings.TrimPrefix(string(k), "doc-tf/")
			sep := strings.IndexByte(rest, '/')
			if sep < 0 {
				return nil
			}
			idx := rest[:sep]
			id := rest[sep+1:]
			// 只加载慢路径 index 的 doc-tf
			if _, need := indexSet[idx]; !need {
				return nil
			}
			if snapshotOK[idx] {
				return nil
			}
			var pt PersistedTokens
			if err := jsonUnmarshal(v, &pt); err != nil {
				return err
			}
			tfCache[idx+"/"+id] = pt
			return nil
		}); err != nil {
			return err
		}
		e.mu.Lock()
		for _, r := range rows {
			if snapshotOK[r.idx] {
				continue
			}
			pt, has := tfCache[r.idx+"/"+r.id]
			e.rebuildDocInvertedFromTokens(r.idx, r.id, r.src, pt, has)
		}
		e.mu.Unlock()
	}
	// 重建 scorer.fieldStats (BM25 统计, 不在 snapshot 中)
	e.mu.Lock()
	e.scorer.rebuildFieldStats()
	e.mu.Unlock()
	return nil
}

// LoadIndex 单个索引 LoadAll
// 快路径 (#7): 优先读 postings-snapshot, 失败回退 doc-tf
func (e *Engine) LoadIndex(index string) error {
	if e.store == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.docs, index)
	delete(e.inverted, index)
	// 同时清空 scorer 中该 index 的状态
	e.scorer.mu.Lock()
	delete(e.scorer.fieldStats, index)
	if e.scorer.fieldLen[index] != nil {
		for k := range e.scorer.fieldLen[index] {
			delete(e.scorer.fieldLen[index], k)
		}
		delete(e.scorer.fieldLen, index)
	}
	if e.scorer.postings[index] != nil {
		for field := range e.scorer.postings[index] {
			for tok := range e.scorer.postings[index][field] {
				delete(e.scorer.postings[index][field], tok)
			}
			delete(e.scorer.postings[index], field)
		}
		delete(e.scorer.postings, index)
	}
	e.scorer.mu.Unlock()
	// 读 doc source
	if err := e.store.Scan(storage.DocPrefix(index), func(k, v []byte) error {
		rest := strings.TrimPrefix(string(k), "doc/"+index+"/")
		var src map[string]interface{}
		if err := jsonUnmarshal(v, &src); err != nil {
			return err
		}
		if e.docs[index] == nil {
			e.docs[index] = make(map[string]map[string]interface{})
		}
		e.docs[index][rest] = src
		return nil
	}); err != nil {
		return err
	}
	// 快路径: 尝试 LoadPostingsSnapshot
	loaded, _, ok, err := e.LoadPostingsSnapshot(index)
	if err != nil {
		return fmt.Errorf("load postings snapshot for %s: %w", index, err)
	}
	if ok && loaded > 0 {
		// 快路径命中, 只需重建 scorer 统计
		e.scorer.rebuildFieldStats()
		return nil
	}
	// 慢路径: 扫 doc-tf 重建倒排
	tfCache := make(map[string]PersistedTokens)
	if err := e.store.Scan(storage.DocTFPrefix(index), func(k, v []byte) error {
		rest := strings.TrimPrefix(string(k), "doc-tf/"+index+"/")
		var pt PersistedTokens
		if err := jsonUnmarshal(v, &pt); err != nil {
			return err
		}
		tfCache[rest] = pt
		return nil
	}); err != nil {
		return err
	}
	for id, src := range e.docs[index] {
		pt, has := tfCache[id]
		e.rebuildDocInvertedFromTokens(index, id, src, pt, has)
	}
	e.scorer.rebuildFieldStats()
	return nil
}

// rebuildDocInvertedFromTokens 重建单个 doc 的 inverted + scorer
// hasTokens: true 表示有持久化的分词, false 表示需要实时分词
func (e *Engine) rebuildDocInvertedFromTokens(index, id string, doc map[string]interface{}, pt PersistedTokens, hasTokens bool) {
	if e.inverted[index] == nil {
		e.inverted[index] = make(map[string]map[string]map[string]struct{})
	}
	for field, raw := range doc {
		if e.inverted[index][field] == nil {
			e.inverted[index][field] = make(map[string]map[string]struct{})
		}
		tokens := pickTokensForField(field, raw, pt, hasTokens)
		if tokens == nil {
			continue
		}
		for _, tok := range tokens {
			if e.inverted[index][field][tok] == nil {
				e.inverted[index][field][tok] = make(map[string]struct{})
			}
			e.inverted[index][field][tok][id] = struct{}{}
		}
		e.scorer.onIndexDoc(index, field, id, tokens)
		if v, ok := valueOf(raw); ok {
			e.sortedCache.upsert(index, field, v, id)
		}
	}
}

// pickTokensForField 取某字段的分词结果
// 优先用持久化的 pt; 缺失/不匹配则实时分词
func pickTokensForField(field string, raw interface{}, pt PersistedTokens, hasTokens bool) []string {
	if hasTokens {
		if toks, ok := pt[field]; ok {
			return toks
		}
	}
	if s, ok := raw.(string); ok {
		return tokenize(s)
	}
	return nil
}

// collectTokensForDoc 给一个 doc source 算出 (field, tokens) 列表
func (e *Engine) collectTokensForDoc(doc map[string]interface{}) map[string][]string {
	out := make(map[string][]string, len(doc))
	for field, raw := range doc {
		if s, ok := raw.(string); ok {
			out[field] = tokenize(s)
		}
	}
	return out
}

// InvertedStats 倒排重建统计
type InvertedStats struct {
	Index        string `json:"index"`
	TotalDocs    int    `json:"total_docs"`
	ReusedTokens int    `json:"reused_tokens"`
	Recomputed   int    `json:"recomputed"`
	DurationMs   int64  `json:"duration_ms"`
	Version      int64  `json:"version"`
}

// RebuildInverted 强制重建某索引的 inverted + scorer
// 走 doc + doc-tf 路径, 与 LoadAll 慢路径一致
// 重建后失效 postings-snapshot, 确保下次 LoadAll 走慢路径 (用户可显式 FlushPostingsSnapshot 重建快路径)
func (e *Engine) RebuildInverted(index string) (InvertedStats, error) {
	if e.store == nil {
		return InvertedStats{Index: index}, fmt.Errorf("no store")
	}
	stats := InvertedStats{Index: index}
	start := timeNow()
	e.mu.Lock()
	defer e.mu.Unlock()
	// 清空该 index 的 inverted
	delete(e.inverted, index)
	// 同时清空 scorer 中该 index 的 fieldStats / fieldLen / postings
	e.scorer.mu.Lock()
	delete(e.scorer.fieldStats, index)
	if e.scorer.fieldLen[index] != nil {
		// 清空该 index 的所有 doc fieldLen
		for k := range e.scorer.fieldLen[index] {
			delete(e.scorer.fieldLen[index], k)
		}
		delete(e.scorer.fieldLen, index)
	}
	if e.scorer.postings[index] != nil {
		// 清空该 index 的所有 postings
		for field := range e.scorer.postings[index] {
			for tok := range e.scorer.postings[index][field] {
				delete(e.scorer.postings[index][field], tok)
			}
			delete(e.scorer.postings[index], field)
		}
		delete(e.scorer.postings, index)
	}
	e.scorer.mu.Unlock()
	// 失效 postings-snapshot (强制下次 LoadAll 走慢路径)
	_ = e.InvalidatePostingsSnapshot(index)
	// 扫 doc
	type docRow struct {
		id  string
		src map[string]interface{}
	}
	rows := make([]docRow, 0)
	if err := e.store.Scan(storage.DocPrefix(index), func(k, v []byte) error {
		rest := strings.TrimPrefix(string(k), "doc/"+index+"/")
		var src map[string]interface{}
		if err := jsonUnmarshal(v, &src); err != nil {
			return err
		}
		rows = append(rows, docRow{id: rest, src: src})
		return nil
	}); err != nil {
		return stats, err
	}
	tfCache := make(map[string]PersistedTokens)
	if err := e.store.Scan(storage.DocTFPrefix(index), func(k, v []byte) error {
		rest := strings.TrimPrefix(string(k), "doc-tf/"+index+"/")
		var pt PersistedTokens
		if err := jsonUnmarshal(v, &pt); err != nil {
			return err
		}
		tfCache[rest] = pt
		return nil
	}); err != nil {
		return stats, err
	}
	stats.TotalDocs = len(rows)
	for _, r := range rows {
		pt, has := tfCache[r.id]
		if has && len(pt) > 0 {
			stats.ReusedTokens++
		} else {
			stats.Recomputed++
		}
		e.rebuildDocInvertedFromTokens(index, r.id, r.src, pt, has)
	}
	e.scorer.rebuildFieldStats()
	stats.DurationMs = timeNow() - start
	stats.Version = e.getPostingsVersion(index)
	return stats, nil
}

// InvertedInfo 当前索引的倒排信息(只读, 用于诊断)
type InvertedInfo struct {
	Index             string `json:"index"`
	DocCount          int    `json:"doc_count"`
	FieldCount        int    `json:"field_count"`
	TokenCount        int    `json:"token_count"`
	PostingsVersion   int64  `json:"postings_version"`
	HasDocTFPersisted bool   `json:"has_doc_tf_persisted"`
}

// GetInvertedInfo 取得索引的倒排信息
func (e *Engine) GetInvertedInfo(index string) (InvertedInfo, error) {
	if e.store == nil {
		return InvertedInfo{Index: index}, fmt.Errorf("no store")
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	info := InvertedInfo{
		Index:           index,
		DocCount:        len(e.docs[index]),
		PostingsVersion: e.getPostingsVersion(index),
	}
	if e.inverted[index] != nil {
		info.FieldCount = len(e.inverted[index])
		tokens := 0
		for _, f := range e.inverted[index] {
			tokens += len(f)
		}
		info.TokenCount = tokens
	}
	// 检查 doc-tf 是否持久化
	found := false
	_ = e.store.Scan(storage.DocTFPrefix(index), func(_, _ []byte) error {
		found = true
		return nil
	})
	info.HasDocTFPersisted = found
	return info, nil
}

// sort 包内 utility, 避免引入 sort 在 top-level 冲突
var _ = sort.Strings
