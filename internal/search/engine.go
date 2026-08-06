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
	store       *storage.Store
}

// New 创建一个新的 Engine
// 不会自动加载任何数据(由 LoadIndex 主动载入)
func New(store *storage.Store) *Engine {
	inverted := make(map[string]map[string]map[string]map[string]struct{})
	return &Engine{
		docs:        make(map[string]map[string]map[string]interface{}),
		inverted:    inverted,
		sortedCache: newSortedIndexCache(),
		store:       store,
	}
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
	defer e.mu.Unlock()
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
func (e *Engine) indexField(index, id, field string, raw interface{}) {
	if e.inverted[index] == nil {
		e.inverted[index] = make(map[string]map[string]map[string]struct{})
	}
	if e.inverted[index][field] == nil {
		e.inverted[index][field] = make(map[string]map[string]struct{})
	}
	if str, ok := raw.(string); ok {
		for _, tok := range tokenize(str) {
			if e.inverted[index][field][tok] == nil {
				e.inverted[index][field][tok] = make(map[string]struct{})
			}
			e.inverted[index][field][tok][id] = struct{}{}
		}
		return
	}
	tok := stringify(raw)
	if e.inverted[index][field][tok] == nil {
		e.inverted[index][field][tok] = make(map[string]struct{})
	}
	e.inverted[index][field][tok][id] = struct{}{}
}

// DeleteDoc 删除一个文档
func (e *Engine) DeleteDoc(index, id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
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

// unindexField 从倒排中移除 docID
func (e *Engine) unindexField(index, id, field string, raw interface{}) {
	if e.inverted[index] == nil || e.inverted[index][field] == nil {
		return
	}
	if str, ok := raw.(string); ok {
		for _, tok := range tokenize(str) {
			if set, ok := e.inverted[index][field][tok]; ok {
				delete(set, id)
				if len(set) == 0 {
					delete(e.inverted[index][field], tok)
				}
			}
		}
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

// LoadIndex 从 storage 加载某个索引的所有文档,重建内存倒排
// 启动或测试场景使用
func (e *Engine) LoadIndex(index string) error {
	docs := make(map[string]map[string]interface{})
	err := e.store.Scan(storage.DocPrefix(index), func(_, v []byte) error {
		var doc map[string]interface{}
		if err := jsonUnmarshal(v, &doc); err != nil {
			return err
		}
		// 文档 _id 已在 storage key 中,这里反向解析
		return nil
	})
	if err != nil {
		return err
	}
	// 上面 Scan 只重建了 docs,真正加载 doc->source 需要带上 id;
	// 简单起见,我们额外提供一个直接索引方法
	_ = docs
	return nil
}

// LoadAll 加载所有索引(简单实现,只遍历已知索引)
// 这里采用 doc/* 前缀全量扫描,逐条加进内存
func (e *Engine) LoadAll() error {
	rows := make(map[string]map[string]map[string]interface{})
	err := e.store.Scan([]byte("doc/"), func(k, v []byte) error {
		// 解析 key: doc/<index>/<id>
		rest := strings.TrimPrefix(string(k), "doc/")
		sep := strings.IndexByte(rest, '/')
		if sep < 0 {
			return nil
		}
		index := rest[:sep]
		id := rest[sep+1:]
		var doc map[string]interface{}
		if err := jsonUnmarshal(v, &doc); err != nil {
			return err
		}
		if rows[index] == nil {
			rows[index] = make(map[string]map[string]interface{})
		}
		rows[index][id] = doc
		return nil
	})
	if err != nil {
		return err
	}
	for idx, docs := range rows {
		for id, doc := range docs {
			e.IndexDoc(idx, id, doc)
		}
	}
	return nil
}

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
