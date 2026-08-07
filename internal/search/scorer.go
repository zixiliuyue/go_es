// Package search - BM25 相关性打分
//
// 设计:
//   - 在原 token 级倒排(map[docID]struct{})基础上, 升级为 map[docID]tf(term frequency)
//   - 同时维护 (index,field) -> totalDocs/avgFieldLen 统计
//   - match/multi_match/match_phrase 走 BM25 公式计算 score
//   - term/terms/range/exists/match_all 仍然只输出 score=1.0 或 0(布尔匹配语义)
//   - 支持 constant_score 包一层, 强制走布尔语义
//
// BM25 公式(标准 Lucene/ES 实现):
//   score(D, Q) = Σ tf_idf
//     IDF(t)   = ln(1 + (N - n(t) + 0.5) / (n(t) + 0.5))
//     tf_norm  = tf * (k1 + 1) / (tf + k1 * (1 - b + b * |D|/avgdl))
//   默认: k1 = 1.2, b = 0.75
package search

import (
	"math"
	"strings"
	"sync"
)

// BM25 默认参数(与 Lucene/ES 一致)
const (
	BM25_K1 = 1.2
	BM25_B  = 0.75
)

// Posting 倒排中一条 (token, docID) 的最小元数据
type Posting struct {
	DocID string
	TF    int // term frequency in this doc
}

// PostingList 一个 token 的完整 postings
type PostingList struct {
	Postings []Posting
	DF       int // 多少 doc 包含该 token
}

// FieldStats (index, field) 维度统计, 用于 BM25 归一化
type FieldStats struct {
	TotalDocs   int     // 该字段被索引的 doc 总数
	AvgFieldLen float64 // 平均字段长度(token 数)
}

// Scorer 是 Engine 的一个轻量子结构, 维护 BM25 所需的扩展倒排
// 与 Engine.docs 共享锁, 由 Engine 暴露方法访问
type Scorer struct {
	mu sync.RWMutex
	// postings: index -> field -> token -> []Posting
	// 用 []Posting 而非 map[docID]tf, 是为了排序后顺扫 BM25
	postings map[string]map[string]map[string]*PostingList
	// fieldStats: index -> field -> stats
	fieldStats map[string]map[string]*FieldStats
	// fieldLen: index -> field -> docID -> length(token count)
	fieldLen map[string]map[string]map[string]int
}

func newScorer() *Scorer {
	return &Scorer{
		postings:   make(map[string]map[string]map[string]*PostingList),
		fieldStats: make(map[string]map[string]*FieldStats),
		fieldLen:   make(map[string]map[string]map[string]int),
	}
}

// onIndexDoc 记录一个 (index, field, docID) 的所有 token, 同时更新 posting 与 fieldStats/fieldLen
// tokens: 该字段分词结果(已 lower-case)
func (s *Scorer) onIndexDoc(index, field, docID string, tokens []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.postings[index] == nil {
		s.postings[index] = make(map[string]map[string]*PostingList)
	}
	if s.postings[index][field] == nil {
		s.postings[index][field] = make(map[string]*PostingList)
	}
	if s.fieldLen[index] == nil {
		s.fieldLen[index] = make(map[string]map[string]int)
	}
	if s.fieldLen[index][field] == nil {
		s.fieldLen[index][field] = make(map[string]int)
	}
	// 统计 tf
	tf := make(map[string]int, len(tokens))
	for _, tok := range tokens {
		tf[tok]++
	}
	// 写入 postings
	for tok, c := range tf {
		pl, ok := s.postings[index][field][tok]
		if !ok {
			pl = &PostingList{}
			s.postings[index][field][tok] = pl
		}
		pl.Postings = append(pl.Postings, Posting{DocID: docID, TF: c})
		pl.DF++
	}
	// 写入 fieldLen
	s.fieldLen[index][field][docID] = len(tokens)
}

// onDeleteDoc 撤销一个 docID 的所有 token 计数
func (s *Scorer) onDeleteDoc(index, field, docID string, tokens []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.postings[index] == nil || s.postings[index][field] == nil {
		return
	}
	tf := make(map[string]int, len(tokens))
	for _, tok := range tokens {
		tf[tok]++
	}
	for tok := range tf {
		pl, ok := s.postings[index][field][tok]
		if !ok {
			continue
		}
		// 找到并删除该 docID 的 posting
		for i, p := range pl.Postings {
			if p.DocID == docID {
				pl.Postings = append(pl.Postings[:i], pl.Postings[i+1:]...)
				pl.DF--
				break
			}
		}
		if pl.DF <= 0 {
			delete(s.postings[index][field], tok)
		}
	}
	if s.fieldLen[index] != nil && s.fieldLen[index][field] != nil {
		delete(s.fieldLen[index][field], docID)
	}
}

// rebuildFieldStats 重新计算 (index, field) 的 totalDocs/avgFieldLen
// 在 onIndexDoc 增量维护之外, 提供一个 O(N) 重建入口(LoadAll 时调用)
func (s *Scorer) rebuildFieldStats() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fieldStats == nil {
		s.fieldStats = make(map[string]map[string]*FieldStats)
	}
	for index, fields := range s.fieldLen {
		if s.fieldStats[index] == nil {
			s.fieldStats[index] = make(map[string]*FieldStats)
		}
		for field, docLens := range fields {
			st := &FieldStats{TotalDocs: len(docLens)}
			if st.TotalDocs == 0 {
				st.AvgFieldLen = 0
			} else {
				sum := 0
				for _, l := range docLens {
					sum += l
				}
				st.AvgFieldLen = float64(sum) / float64(st.TotalDocs)
			}
			s.fieldStats[index][field] = st
		}
	}
}

// lookupPostings 取一个 token 的 posting list(只读)
func (s *Scorer) lookupPostings(index, field, token string) *PostingList {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.postings[index] == nil || s.postings[index][field] == nil {
		return nil
	}
	return s.postings[index][field][token]
}

// fieldDocLen 查某 doc 在某字段的 token 数
func (s *Scorer) fieldDocLen(index, field, docID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.fieldLen[index] == nil || s.fieldLen[index][field] == nil {
		return 0
	}
	return s.fieldLen[index][field][docID]
}

// tokenizeForBM25 与 engine.tokenize 等价的本地副本,避免循环 import
// (虽然都在同包,这里显式声明以方便将来分文件)
func tokenizeForBM25(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	})
}

// BM25Score 计算一个 doc 在某字段上对 query tokens 的 BM25 得分
// totalDocs / avgFieldLen: 字段级统计
// k1 / b: 调节参数
// 返回值: 累计 BM25 得分(无 token 命中则返回 0)
func BM25Score(totalDocs int, avgFieldLen float64, queryTokens []string, field string, scorer *Scorer, index, docID string) float64 {
	if totalDocs == 0 || avgFieldLen == 0 || len(queryTokens) == 0 {
		return 0
	}
	docLen := scorer.fieldDocLen(index, field, docID)
	if docLen == 0 {
		return 0
	}
	// 统计每个 query token 在该 doc 中的 tf
	docTF := make(map[string]int, len(queryTokens))
	for _, tok := range queryTokens {
		pl := scorer.lookupPostings(index, field, tok)
		if pl == nil {
			continue
		}
		for _, p := range pl.Postings {
			if p.DocID == docID {
				docTF[tok] = p.TF
				break
			}
		}
	}
	if len(docTF) == 0 {
		return 0
	}
	var total float64
	for tok, tf := range docTF {
		pl := scorer.lookupPostings(index, field, tok)
		if pl == nil || pl.DF == 0 {
			continue
		}
		// IDF: ln(1 + (N - n + 0.5) / (n + 0.5))
		df := float64(pl.DF)
		n := float64(totalDocs)
		idf := math.Log(1 + (n - df + 0.5) / (df + 0.5))
		// tf normalization
		tfNorm := float64(tf) * (BM25_K1 + 1) /
			(float64(tf) + BM25_K1*(1-BM25_B+BM25_B*float64(docLen)/avgFieldLen))
		total += idf * tfNorm
	}
	return total
}
