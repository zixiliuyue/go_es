// 倒排索引 postings 快照持久化 (#7)
//
// 设计目标:
//   - 解决冷启动 LoadAll 逐 doc 重建倒排的 O(N) 瓶颈
//   - 启动时直接读 postings-snapshot/<index>/<field> 填充内存倒排,
//     把 O(N_docs) IO 降为 O(N_fields) IO
//
// 持久化格式 (per field):
//   postings-snapshot/<index>/<field> -> PostingsSnapshot{version, postings, doc_len}
//     postings: {term: [{docID, tf}, ...]} (含 TF, 用于 BM25 重建)
//     doc_len:  {docID: field_length} (text 字段才有, 用于 BM25 归一化)
//   postings-snapshot-version/<index> -> int64 (上次 flush 时的 postings-version)
//
// 写时策略 (保证写路径不放大开销):
//   - 每次写只递增 postings-version/<index> (已有, 不变)
//   - 不立即更新 snapshot (太贵)
//   - 定期 / 显式调用 FlushPostingsSnapshot(index) 全量写 snapshot
//   - 启动时: snapshot 版本 == postings-version 则走快路径, 否则回退 doc-tf
//
// BM25 重建:
//   - snapshot 含 TF + fieldLen, LoadPostingsSnapshot 直接重建 scorer.postings/fieldLen
//   - 不再需要扫 doc-tf 或重新分词, 真正实现 O(M_fields) 冷启动
//
// 限制:
//   - field 名不能包含 '/' (实际场景几乎都满足, ES field 名通常只有字母数字 . _)
package search

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/zixiliuyue/go_es/internal/storage"
)

// PostingsSnapshotEntry 一条 posting 记录 (docID + TF)
type PostingsSnapshotEntry struct {
	DocID string `json:"d"`
	TF    int    `json:"t"`
}

// PostingsSnapshot 单个 field 的 postings 快照
type PostingsSnapshot struct {
	// Version 写入时的 postings-version, 用于启动时判定是否可用
	Version int64 `json:"version"`
	// Postings term -> [{docID, tf}] (已按 docID 排序)
	Postings map[string][]PostingsSnapshotEntry `json:"postings"`
	// DocLen docID -> field length (token count); 仅 text 字段有, 用于 BM25 归一化
	// 非 text 字段 (数字/布尔) 为 nil, 不参与 BM25
	DocLen map[string]int `json:"doc_len,omitempty"`
}

// postingsSnapshotState 快照管理状态
type postingsSnapshotState struct {
	mu           sync.Mutex
	// flushing 正在 flush 的 index 集合, 防止并发 flush
	flushing map[string]struct{}
	// lastFlushVersion 上次 flush 成功后的 postings-version
	lastFlushVersion map[string]int64
}

func newPostingsSnapshotState() *postingsSnapshotState {
	return &postingsSnapshotState{
		flushing:         make(map[string]struct{}),
		lastFlushVersion: make(map[string]int64),
	}
}

// FlushPostingsSnapshot 全量扫描 index 的内存倒排 + scorer, 按 field 写 snapshot
// 调用时机:
//   - 显式 API: POST /<index>/_postings/flush
//   - segment flush 之后 (复用 SnapshotIndex)
//   - 优雅关闭前 (cmd/server/main.go Shutdown)
//
// 返回: 写入的 field 数量 + 耗时
//
// 说明:
//   - 在同一把 e.mu.RLock + scorer.mu.RLock 下读 inverted + scorer, 保证一致性
//   - snapshot 含 TF + fieldLen, LoadPostingsSnapshot 可直接重建 scorer
func (e *Engine) FlushPostingsSnapshot(index string) (int, int64, error) {
	if e.store == nil {
		return 0, 0, fmt.Errorf("no store")
	}
	// 防止同一 index 并发 flush
	e.persistence.snapshotState.mu.Lock()
	if _, ok := e.persistence.snapshotState.flushing[index]; ok {
		e.persistence.snapshotState.mu.Unlock()
		return 0, 0, fmt.Errorf("snapshot flush already in progress for index %s", index)
	}
	e.persistence.snapshotState.flushing[index] = struct{}{}
	e.persistence.snapshotState.mu.Unlock()
	defer func() {
		e.persistence.snapshotState.mu.Lock()
		delete(e.persistence.snapshotState.flushing, index)
		e.persistence.snapshotState.mu.Unlock()
	}()

	start := timeNow()
	// 在同一把锁下读 inverted + scorer, 保证一致
	type fieldSnapshot struct {
		postings map[string][]PostingsSnapshotEntry
		docLen   map[string]int
	}
	e.mu.RLock()
	invFields := e.inverted[index]
	if len(invFields) == 0 {
		e.mu.RUnlock()
		// 没有倒排, 删除旧 snapshot
		_ = e.store.DeletePrefix(storage.PostingsSnapshotPrefix(index))
		_ = e.store.Delete(storage.PostingsSnapshotVersionKey(index))
		return 0, timeNow() - start, nil
	}
	e.scorer.mu.RLock()
	// 读 scorer 的 postings + fieldLen (仅 text 字段有)
	var scorerPostings map[string]map[string]*PostingList
	var scorerFieldLen map[string]map[string]int
	if sp, ok := e.scorer.postings[index]; ok {
		scorerPostings = sp
	}
	if fl, ok := e.scorer.fieldLen[index]; ok {
		scorerFieldLen = fl
	}
	fieldsData := make(map[string]fieldSnapshot, len(invFields))
	for field, terms := range invFields {
		fd := fieldSnapshot{
			postings: make(map[string][]PostingsSnapshotEntry, len(terms)),
		}
		// 该 field 的 scorer postings (text 字段)
		var sp map[string]*PostingList
		var sfl map[string]int
		if scorerPostings != nil {
			sp = scorerPostings[field]
		}
		if scorerFieldLen != nil {
			sfl = scorerFieldLen[field]
		}
		for term, docSet := range terms {
			entries := make([]PostingsSnapshotEntry, 0, len(docSet))
			for id := range docSet {
				tf := 1
				if sp != nil {
					if pl, ok := sp[term]; ok {
						for _, p := range pl.Postings {
							if p.DocID == id {
								tf = p.TF
								break
							}
						}
					}
				}
				entries = append(entries, PostingsSnapshotEntry{DocID: id, TF: tf})
			}
			sort.Slice(entries, func(i, j int) bool { return entries[i].DocID < entries[j].DocID })
			fd.postings[term] = entries
		}
		// DocLen (仅 text 字段)
		if sfl != nil {
			fd.docLen = make(map[string]int, len(sfl))
			for id, l := range sfl {
				fd.docLen[id] = l
			}
		}
		fieldsData[field] = fd
	}
	e.scorer.mu.RUnlock()
	e.mu.RUnlock()

	// 当前 version (在锁外读, version 单调递增不影响一致性判定)
	curVer := e.getPostingsVersion(index)
	// 按 field 写 snapshot
	fields := 0
	for field, fd := range fieldsData {
		ps := PostingsSnapshot{
			Version:  curVer,
			Postings: fd.postings,
			DocLen:   fd.docLen,
		}
		if err := e.store.Put(storage.PostingsSnapshotKey(index, field), ps); err != nil {
			return fields, timeNow() - start, err
		}
		fields++
	}
	// 写 version
	if err := e.store.Put(storage.PostingsSnapshotVersionKey(index), curVer); err != nil {
		return fields, timeNow() - start, err
	}
	e.persistence.snapshotState.mu.Lock()
	e.persistence.snapshotState.lastFlushVersion[index] = curVer
	e.persistence.snapshotState.mu.Unlock()
	return fields, timeNow() - start, nil
}

// LoadPostingsSnapshot 从 store 加载 index 的 postings snapshot 到内存倒排 + scorer
// 调用时机: LoadAll 优先走此快路径
// 返回:
//   - loaded: 加载的 field 数
//   - version: snapshot 的 postings-version
//   - ok: 是否成功 (snapshot 存在且 version 匹配)
//   - err: IO/反序列化错误
//
// 说明:
//   - 填充 e.inverted (term -> set(docID)) 用于布尔查询
//   - 填充 e.scorer.postings (term -> []Posting{DocID, TF}) 用于 BM25 打分
//   - 填充 e.scorer.fieldLen (docID -> length) 用于 BM25 归一化
//   - 调用方需持有 e.mu.Lock() (本函数直接写 inverted + scorer)
//   - fieldStats 由 LoadAll 结尾的 rebuildFieldStats() 重建
func (e *Engine) LoadPostingsSnapshot(index string) (loaded int, version int64, ok bool, err error) {
	if e.store == nil {
		return 0, 0, false, nil
	}
	// 读 snapshot version
	var snapVer int64
	found, err := e.store.Get(storage.PostingsSnapshotVersionKey(index), &snapVer)
	if err != nil {
		return 0, 0, false, err
	}
	if !found {
		return 0, 0, false, nil
	}
	// 与当前 postings-version 比较
	curVer := e.getPostingsVersion(index)
	if snapVer != curVer {
		return 0, snapVer, false, nil
	}
	// 扫 snapshot 填充 inverted + 收集 scorer 数据 (IO 期间不持 scorer 锁)
	type fieldScorerData struct {
		postings map[string]*PostingList
		docLen   map[string]int
	}
	scorerData := make(map[string]fieldScorerData)
	err = e.store.Scan(storage.PostingsSnapshotPrefix(index), func(k, v []byte) error {
		// key = postings-snapshot/<index>/<field>
		rest := strings.TrimPrefix(string(k), string(storage.PostingsSnapshotPrefix(index)))
		if rest == "" {
			return nil
		}
		field := rest
		var ps PostingsSnapshot
		if err := jsonUnmarshal(v, &ps); err != nil {
			return err
		}
		if e.inverted[index] == nil {
			e.inverted[index] = make(map[string]map[string]map[string]struct{})
		}
		if e.inverted[index][field] == nil {
			e.inverted[index][field] = make(map[string]map[string]struct{})
		}
		// 收集 scorer 数据
		sd := fieldScorerData{
			postings: make(map[string]*PostingList, len(ps.Postings)),
		}
		if len(ps.DocLen) > 0 {
			sd.docLen = make(map[string]int, len(ps.DocLen))
		}
		for term, entries := range ps.Postings {
			// 填 inverted
			if e.inverted[index][field][term] == nil {
				e.inverted[index][field][term] = make(map[string]struct{}, len(entries))
			}
			// 填 scorer postings
			pl := &PostingList{
				Postings: make([]Posting, 0, len(entries)),
				DF:       len(entries),
			}
			for _, entry := range entries {
				e.inverted[index][field][term][entry.DocID] = struct{}{}
				pl.Postings = append(pl.Postings, Posting{DocID: entry.DocID, TF: entry.TF})
			}
			sd.postings[term] = pl
		}
		// 填 scorer fieldLen
		for id, l := range ps.DocLen {
			sd.docLen[id] = l
		}
		scorerData[field] = sd
		loaded++
		return nil
	})
	if err != nil {
		return loaded, snapVer, false, err
	}
	if loaded == 0 {
		return 0, snapVer, false, nil
	}
	// 写 scorer (调用方持 e.mu.Lock, 锁序 e.mu -> scorer.mu 安全)
	e.scorer.mu.Lock()
	if e.scorer.postings[index] == nil {
		e.scorer.postings[index] = make(map[string]map[string]*PostingList)
	}
	if e.scorer.fieldLen[index] == nil {
		e.scorer.fieldLen[index] = make(map[string]map[string]int)
	}
	for field, sd := range scorerData {
		e.scorer.postings[index][field] = sd.postings
		if sd.docLen != nil {
			e.scorer.fieldLen[index][field] = sd.docLen
		}
	}
	e.scorer.mu.Unlock()
	return loaded, snapVer, true, nil
}

// HasPostingsSnapshot 检查 index 是否有可用的 postings snapshot (版本匹配)
// 用于诊断 / 启动日志
func (e *Engine) HasPostingsSnapshot(index string) bool {
	if e.store == nil {
		return false
	}
	var snapVer int64
	found, err := e.store.Get(storage.PostingsSnapshotVersionKey(index), &snapVer)
	if err != nil || !found {
		return false
	}
	return snapVer == e.getPostingsVersion(index)
}

// InvalidatePostingsSnapshot 删除 index 的 postings snapshot (版本不匹配时清理)
// 用于: 索引删除 / 显式重建
func (e *Engine) InvalidatePostingsSnapshot(index string) error {
	if e.store == nil {
		return nil
	}
	e.persistence.snapshotState.mu.Lock()
	delete(e.persistence.snapshotState.lastFlushVersion, index)
	e.persistence.snapshotState.mu.Unlock()
	if err := e.store.DeletePrefix(storage.PostingsSnapshotPrefix(index)); err != nil {
		return err
	}
	return e.store.Delete(storage.PostingsSnapshotVersionKey(index))
}

// PostingsSnapshotInfo 快照诊断信息
type PostingsSnapshotInfo struct {
	Index           string `json:"index"`
	HasSnapshot     bool   `json:"has_snapshot"`
	SnapshotVersion int64  `json:"snapshot_version"`
	CurrentVersion  int64  `json:"current_version"`
	VersionMatch    bool   `json:"version_match"`
	FieldCount      int    `json:"field_count"`
}

// GetPostingsSnapshotInfo 取 index 的 snapshot 诊断信息
func (e *Engine) GetPostingsSnapshotInfo(index string) (PostingsSnapshotInfo, error) {
	info := PostingsSnapshotInfo{Index: index}
	if e.store == nil {
		return info, fmt.Errorf("no store")
	}
	var snapVer int64
	found, err := e.store.Get(storage.PostingsSnapshotVersionKey(index), &snapVer)
	if err != nil {
		return info, err
	}
	info.HasSnapshot = found
	info.SnapshotVersion = snapVer
	info.CurrentVersion = e.getPostingsVersion(index)
	info.VersionMatch = found && snapVer == info.CurrentVersion
	// 数 field 数
	fieldCount := 0
	_ = e.store.Scan(storage.PostingsSnapshotPrefix(index), func(_, _ []byte) error {
		fieldCount++
		return nil
	})
	info.FieldCount = fieldCount
	return info, nil
}
