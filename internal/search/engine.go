// Package search 提供本仓库自研 Elasticsearch 服务端(见 internal/server) 所使用的
// 极简查询引擎
//
// 设计目标
//   - 覆盖当前 examples/main.go 与单测所需的查询类型:match/term/terms/range/bool
//   - 文本字段用空格的简单分词 + 倒排索引(在内存中维护,启动时从 storage 重建)
//   - keyword/数值字段不做分词,直接 term 精确匹配
//   - 不实现 BM25/TF-IDF 等打分,只返回"是否匹配"以保证 examples 通过
//
// 这是一个**学习用**的内嵌搜索引擎,不是生产级实现
package search

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/zixiliuyue/go_es/internal/storage"
)

// Engine 搜索引擎
type Engine struct {
	mu sync.RWMutex
	// docs: index -> id -> Source(map)
	docs map[string]map[string]map[string]interface{}
	// inverted: index -> field -> token -> set(docID)
	inverted map[string]map[string]map[string]map[string]struct{}
	// sortedCache: 范围查询加速(数值/字符串字段排序倒排)
	sortedCache *sortedIndexCache
	// scorer: BM25 打分所需的扩展倒排(field-level 统计 + per-doc tf)
	scorer *Scorer
	// persistence: 倒排持久化(per-doc 分词落盘 + version 追踪)
	persistence *indexPersistence
	store       *storage.Store
}

// New 创建一个新的 Engine
// 不会自动加载任何数据(由 LoadIndex 主动载入)
func New(store *storage.Store) *Engine {
	inverted := make(map[string]map[string]map[string]map[string]struct{})
	e := &Engine{
		docs:        make(map[string]map[string]map[string]interface{}),
		inverted:    inverted,
		sortedCache: newSortedIndexCache(),
		scorer:      newScorer(),
		persistence: newIndexPersistence(),
		store:       store,
	}
	// 注入 source 查询给聚合包使用(避免 search 包内部循环 import)
	SetSourceLookup(func(index, id string) (map[string]interface{}, bool) {
		return e.GetSource(index, id)
	})
	return e
}

// IndexDoc 索引一个文档(同时维护倒排)
// 内存中的索引在每台服务端进程内,持久化由上层 storage 完成
// 参数:
//
//	index: 索引名
//	id: 文档ID
//	doc: 文档 _source
func (e *Engine) IndexDoc(index, id string, doc map[string]interface{}) {
	e.mu.Lock()
	e.indexDocInMemory(index, id, doc)
	e.mu.Unlock()
	// 异步/同步 落盘 doc-tf (不持锁)
	if e.store != nil {
		tokens := e.collectTokensForDoc(doc)
		_ = e.persistDocTokens(index, id, tokens)
	}
}

// indexDocInMemory 仅操作内存 (锁内), 不做落盘
// 用于 LoadAll 等批量场景
func (e *Engine) indexDocInMemory(index, id string, doc map[string]interface{}) {
	if e.docs[index] == nil {
		e.docs[index] = make(map[string]map[string]interface{})
	}
	e.docs[index][id] = doc

	if e.inverted[index] == nil {
		e.inverted[index] = make(map[string]map[string]map[string]struct{})
	}
	for field, raw := range doc {
		e.indexField(index, id, field, raw)
		// 维护范围查询排序索引
		if v, ok := valueOf(raw); ok {
			e.sortedCache.upsert(index, field, v, id)
		}
	}
}

// indexField 对单个字段做分词+倒排登记
// 字段类型推断: 字符串走分词,其它类型按 string 化后整段写入倒排(term 匹配)
// 同时把 tokens 推给 scorer, 用于 BM25 统计
func (e *Engine) indexField(index, id, field string, raw interface{}) {
	if e.inverted[index] == nil {
		e.inverted[index] = make(map[string]map[string]map[string]struct{})
	}
	if e.inverted[index][field] == nil {
		e.inverted[index][field] = make(map[string]map[string]struct{})
	}
	if str, ok := raw.(string); ok {
		toks := tokenize(str)
		for _, tok := range toks {
			if e.inverted[index][field][tok] == nil {
				e.inverted[index][field][tok] = make(map[string]struct{})
			}
			e.inverted[index][field][tok][id] = struct{}{}
		}
		// 推 scorer(字符串字段, BM25 适用)
		e.scorer.onIndexDoc(index, field, id, toks)
		return
	}
	tok := stringify(raw)
	if e.inverted[index][field][tok] == nil {
		e.inverted[index][field][tok] = make(map[string]struct{})
	}
	e.inverted[index][field][tok][id] = struct{}{}
	// 非字符串字段(数字/布尔): 不参与 BM25 文本打分
}

// DeleteDoc 删除一个文档
func (e *Engine) DeleteDoc(index, id string) {
	e.mu.Lock()
	e.deleteDocInMemory(index, id)
	e.mu.Unlock()
	// 落盘: 删 doc-tf
	if e.store != nil {
		_ = e.deleteDocTokens(index, id)
	}
}

// deleteDocInMemory 仅内存删 (锁内)
func (e *Engine) deleteDocInMemory(index, id string) {
	if docs, ok := e.docs[index]; ok {
		if doc, ok := docs[id]; ok {
			// 从倒排里逐个 token 移除
			for field, raw := range doc {
				e.unindexField(index, id, field, raw)
			}
			delete(docs, id)
			// 同步 sortedCache
			e.sortedCache.removeDoc(index, id)
		}
	}
}

// unindexField 从倒排中移除 docID, 同时通知 scorer 撤销 BM25 计数
func (e *Engine) unindexField(index, id, field string, raw interface{}) {
	if e.inverted[index] == nil || e.inverted[index][field] == nil {
		return
	}
	if str, ok := raw.(string); ok {
		toks := tokenize(str)
		for _, tok := range toks {
			if set, ok := e.inverted[index][field][tok]; ok {
				delete(set, id)
				if len(set) == 0 {
					delete(e.inverted[index][field], tok)
				}
			}
		}
		// 通知 scorer 撤销
		e.scorer.onDeleteDoc(index, field, id, toks)
		return
	}
	tok := stringify(raw)
	if set, ok := e.inverted[index][field][tok]; ok {
		delete(set, id)
		if len(set) == 0 {
			delete(e.inverted[index][field], tok)
		}
	}
}

// LoadAll 见 persistence.go (loadAllOptimized)

// matchDocs 在某字段上查找匹配的 docID 集合
func (e *Engine) matchDocs(index, field, value string) map[string]struct{} {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make(map[string]struct{})
	if e.inverted[index] == nil || e.inverted[index][field] == nil {
		return out
	}
	if set, ok := e.inverted[index][field][value]; ok {
		for id := range set {
			out[id] = struct{}{}
		}
	}
	return out
}

// matchDocsTokens 在某字段上查找包含任一 token 的 docID 集合(类似 match 查询)
func (e *Engine) matchDocsTokens(index, field, value string) map[string]struct{} {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make(map[string]struct{})
	if e.inverted[index] == nil || e.inverted[index][field] == nil {
		return out
	}
	for _, tok := range tokenize(value) {
		if set, ok := e.inverted[index][field][tok]; ok {
			for id := range set {
				out[id] = struct{}{}
			}
		}
	}
	return out
}

// termValue 直接按字段原值匹配(不做分词)
func (e *Engine) termValue(index, field string, value interface{}) map[string]struct{} {
	tok := stringify(value)
	return e.matchDocs(index, field, tok)
}

// GetSource 返回某文档的 _source
func (e *Engine) GetSource(index, id string) (map[string]interface{}, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if docs, ok := e.docs[index]; ok {
		if doc, ok := docs[id]; ok {
			return doc, true
		}
	}
	return nil, false
}

// AllIDs 返回某索引下所有文档 ID(用于 _reindex、delete_by_query 等)
func (e *Engine) AllIDs(index string) []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	ids := make([]string, 0)
	if docs, ok := e.docs[index]; ok {
		for id := range docs {
			ids = append(ids, id)
		}
	}
	return ids
}

// BM25FieldScore 计算 docID 在某字段上对 query 字符串的 BM25 得分
// 缺失字段或非字符串字段返回 0
// 若字段统计未建好(totalDocs=0), 自动重建一次
func (e *Engine) BM25FieldScore(index, field, docID, query string) float64 {
	stats := e.ensureFieldStats(index, field)
	if stats == nil || stats.TotalDocs == 0 {
		return 0
	}
	toks := tokenize(query)
	return BM25Score(stats.TotalDocs, stats.AvgFieldLen, toks, field, e.scorer, index, docID)
}

// readDocMeta 读 doc 的版本元数据 (server 层定义, 由 json 反序列化)
// timeNow 暴露给 server 层做时戳
func timeNow() int64 { return stdTimeNow() }

// SnapshotIndex 拿 index 倒排的浅拷贝
//   - 用途: segment flush 时给 segment layer 一份不可变快照
//   - 不影响正常读路径(只是多拷一份 map 头)
// 返回 index -> field -> term -> set(docID)
func (e *Engine) SnapshotIndex(index string) map[string]map[string]map[string]struct{} {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make(map[string]map[string]map[string]struct{})
	if e.inverted[index] == nil {
		return out
	}
	for field, terms := range e.inverted[index] {
		fout := make(map[string]map[string]struct{}, len(terms))
		for term, docSet := range terms {
			tout := make(map[string]struct{}, len(docSet))
			for id := range docSet {
				tout[id] = struct{}{}
			}
			fout[term] = tout
		}
		out[field] = fout
	}
	return out
}

// LoadSegmentPostings 把一个 segment 的倒排数据加载到引擎内存中
// 用于 #10 冷启动: 从 segment 恢复倒排, 避免逐 doc 重新分词
//
// 参数:
//   - index: 索引名
//   - field: 字段名
//   - postings: term -> [docID] (来自 SegmentData.Postings)
//
// 返回: 加载的 term 数量
//
// 说明:
//   - 调用方不需要持锁, 本函数自行加 e.mu.Lock
//   - 仅填充 inverted (布尔查询用), 不填充 scorer (BM25 打分需 TF, segment 不含)
//   - 若需要 BM25 打分, 应走 #7 postings-snapshot 快路径 (含 TF + DocLen)
func (e *Engine) LoadSegmentPostings(index, field string, postings map[string][]string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.inverted[index] == nil {
		e.inverted[index] = make(map[string]map[string]map[string]struct{})
	}
	if e.inverted[index][field] == nil {
		e.inverted[index][field] = make(map[string]map[string]struct{})
	}
	count := 0
	for term, docIDs := range postings {
		if e.inverted[index][field][term] == nil {
			e.inverted[index][field][term] = make(map[string]struct{}, len(docIDs))
		}
		for _, id := range docIDs {
			e.inverted[index][field][term][id] = struct{}{}
		}
		count++
	}
	return count
}

// CreateIndex 在引擎内存中为指定索引预留存储空间
//
// 说明:
//   - 本引擎为懒加载模式, 首次 IndexDoc 会自动创建内部 map;
//     显式调用只是为了在 rollover 等场景下提前初始化, 避免 nil map 读写
//   - 调用必须在持有 mu 锁外进行, 本函数自行加锁
//
// 参数:
//   - index: 要初始化的索引名(若已存在则为 no-op)
func (e *Engine) CreateIndex(index string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.docs[index] == nil {
		e.docs[index] = make(map[string]map[string]interface{})
	}
	if e.inverted[index] == nil {
		e.inverted[index] = make(map[string]map[string]map[string]struct{})
	}
}

// DeleteIndex 从引擎内存中彻底删除指定索引的所有文档与倒排
//
// 说明:
//   - 仅清理内存态(docs/inverted/sortedCache/scorer),
//     持久层数据由上层(server/storage)负责删除
//   - 同时失效 postings-snapshot (#7)
//   - 若索引不存在, 本函数为 no-op
//
// 参数:
//   - index: 要删除的索引名
func (e *Engine) DeleteIndex(index string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	// 通知 sortedCache 清理
	e.sortedCache.removeIndex(index)
	// 通知 scorer 清理 BM25 统计
	e.scorer.onDeleteIndex(index)
	// 移除 docs 与倒排
	delete(e.docs, index)
	delete(e.inverted, index)
	// 失效 postings-snapshot (不持 e.mu, 内部用 snapshotState.mu)
	_ = e.InvalidatePostingsSnapshot(index)
}

// ensureFieldStats 取得字段统计; 若缺失,触发一次重建后返回
func (e *Engine) ensureFieldStats(index, field string) *FieldStats {
	e.scorer.mu.RLock()
	if st, ok := e.scorer.fieldStats[index]; ok {
		if s, ok := st[field]; ok {
			e.scorer.mu.RUnlock()
			return s
		}
	}
	e.scorer.mu.RUnlock()
	// 重建
	e.scorer.rebuildFieldStats()
	e.scorer.mu.RLock()
	defer e.scorer.mu.RUnlock()
	if e.scorer.fieldStats[index] == nil {
		return nil
	}
	return e.scorer.fieldStats[index][field]
}

// 工具函数

// jsonUnmarshal 是 encoding/json 的小封装,便于在文件顶部不直接 import encoding/json
func jsonUnmarshal(data []byte, v interface{}) error {
	return stdJSONUnmarshal(data, v)
}

// tokenize 简单按空白分词并小写
func tokenize(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		// 切分所有非字母数字字符
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	})
	return fields
}

// stringify 将任意值转为可用于 term 查询的字符串
func stringify(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 32)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		return strings.ToLower(fmt.Sprintf("%v", v))
	}
}
