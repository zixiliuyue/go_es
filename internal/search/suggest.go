// match_phrase + term/completion suggester
//
// 设计:
//   - match_phrase: 词的精确连续序列匹配
//     - 简化: 把 phrase 切词后, 要求这些 token 全部在 doc 字段值中按相同顺序出现
//     - 不实现 slop 模糊(生产可加, 当前够用)
//   - term suggester: 基于倒排统计, 返回 prefix 匹配 + 编辑距离候选
//     - 用 Levenshtein 距离 ≤ maxEdits 过滤
//   - completion suggester: prefix 匹配某个字段的 token, 倒排直接命中
//     - ES completion 是 FST + in-memory, 我们用倒排 token 集合代替
//
// API:
//   - POST /<index>/_suggest { "mysuggester": { "text": "...", "term": { "field": "title" } } }
//   - POST /<index>/_search { ..., "suggest": { "mysuggester": { "text": "...", "term": {...} } } }
package search

import (
	"fmt"
	"sort"
	"strings"
)

// evalMatchPhrase 处理 match_phrase 查询
// fieldMap: {"field": "phrase string" 或 {"query": "phrase string"}}
// 命中条件: 字段值按原顺序包含 phrase 的所有 token(连续)
func (e *Engine) evalMatchPhrase(index string, fields map[string]interface{}) (map[string]struct{}, error) {
	if len(fields) == 0 {
		return nil, fmt.Errorf("match_phrase requires at least one field")
	}
	out := map[string]struct{}{}
	for field, raw := range fields {
		phrase, err := extractPhraseQuery(raw)
		if err != nil {
			return nil, fmt.Errorf("match_phrase.%s: %w", field, err)
		}
		phraseTokens := tokenize(phrase)
		if len(phraseTokens) == 0 {
			continue
		}
		ids, err := e.phraseMatchInField(index, field, phraseTokens)
		if err != nil {
			return nil, err
		}
		for id := range ids {
			out[id] = struct{}{}
		}
	}
	return out, nil
}

// extractPhraseQuery 从 match_phrase 字段值抽出字符串
func extractPhraseQuery(raw interface{}) (string, error) {
	switch v := raw.(type) {
	case string:
		return v, nil
	case map[string]interface{}:
		if s, ok := v["query"].(string); ok {
			return s, nil
		}
	}
	return "", fmt.Errorf("unsupported match_phrase value type")
}

// phraseMatchInField 找到所有 docID, 它们的 field 字段值包含 phraseTokens 的连续序列
// 简化: token 严格按顺序相邻出现(无 slop, 无 gap)
func (e *Engine) phraseMatchInField(index, field string, phraseTokens []string) (map[string]struct{}, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.docs[index] == nil {
		return map[string]struct{}{}, nil
	}
	out := make(map[string]struct{})
	for id, doc := range e.docs[index] {
		raw, ok := doc[field]
		if !ok {
			continue
		}
		s, ok := raw.(string)
		if !ok {
			continue
		}
		toks := tokenize(s)
		if containsPhrase(toks, phraseTokens) {
			out[id] = struct{}{}
		}
	}
	return out, nil
}

// containsPhrase 检查 toks 是否按原顺序包含 phrase
func containsPhrase(toks, phrase []string) bool {
	if len(phrase) == 0 || len(toks) < len(phrase) {
		return false
	}
	for i := 0; i+len(phrase) <= len(toks); i++ {
		match := true
		for j, p := range phrase {
			if toks[i+j] != p {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// ---------------- Term / Completion Suggester ----------------

// SuggestRequest 一次 suggest 调用
type SuggestRequest struct {
	Name           string                 `json:"-"` // 由调用方指定
	Text           string                 `json:"text"`
	Term           *TermSuggesterConfig   `json:"term,omitempty"`
	Completion     *CompletionSuggesterConfig `json:"completion,omitempty"`
	Prefix         *PrefixSuggesterConfig `json:"prefix,omitempty"`
	MaxEdits       int                    `json:"max_edits,omitempty"`        // term 用
	PrefixLen      int                    `json:"prefix_len,omitempty"`      // term 用
	MinWordLen     int                    `json:"min_word_len,omitempty"`    // term 用
	MaxInspections int                    `json:"max_inspections,omitempty"` // term 用
	ShardSize      int                    `json:"shard_size,omitempty"`      // term 用
	Size           int                    `json:"size,omitempty"`            // term/completion 用
}

// TermSuggesterConfig term suggester 配置
type TermSuggesterConfig struct {
	Field            string `json:"field"`
	Analyzer         string `json:"analyzer,omitempty"`
	Sort             string `json:"sort,omitempty"` // "score" | "frequency"
	SuggestMode      string `json:"suggest_mode,omitempty"` // "missing" | "popular" | "always"
}

// CompletionSuggesterConfig completion suggester 配置
type CompletionSuggesterConfig struct {
	Field  string `json:"field"`
	Size   int    `json:"size,omitempty"`
	SkipDuplicates bool `json:"skip_duplicates,omitempty"`
}

// PrefixSuggesterConfig 简化版 prefix suggester(就是以 text 前缀匹配 field 的 token)
type PrefixSuggesterConfig struct {
	Field string `json:"field"`
	Size  int    `json:"size,omitempty"`
}

// SuggestResult 一组 suggest 结果(name 维度)
type SuggestResult struct {
	Text    string         `json:"text"`
	Offset  int            `json:"offset"`
	Length  int            `json:"length"`
	Options []SuggestOption `json:"options"`
}

// SuggestOption 单个候选
type SuggestOption struct {
	Text    string  `json:"text"`
	Score   float64 `json:"score"`
	Freq    int     `json:"freq,omitempty"`     // term only
	Payload string  `json:"payload,omitempty"`  // completion only
	Source  string  `json:"source,omitempty"`   // completion only (doc id)
}

// TermSuggest 实现 term suggester
// 流程: 把 text 切词, 对每个 token 在 field 倒排中查相邻/相似 token, 按 freq 排序
func (e *Engine) TermSuggest(index, field, text string, cfg TermSuggesterConfig, maxEdits, prefixLen, minWordLen, size int) []SuggestOption {
	if size <= 0 {
		size = 5
	}
	if maxEdits <= 0 {
		maxEdits = 2
	}
	if prefixLen < 1 {
		prefixLen = 1
	}
	if minWordLen < 3 {
		minWordLen = 3
	}
	toks := tokenize(text)
	// 收集所有候选: (token, source, freq, score)
	type cand struct {
		token   string
		freq    int
		score   float64
	}
	seen := make(map[string]*cand)
	for _, tok := range toks {
		if len(tok) < minWordLen {
			continue
		}
		// 找所有同 prefix 的 token
		prefix := tok
		if len(prefix) > prefixLen {
			prefix = prefix[:prefixLen]
		}
		e.mu.RLock()
		allTokens := e.suggestTokensWithPrefix(index, field, prefix)
		e.mu.RUnlock()
		for _, t := range allTokens {
			dist := levenshtein(tok, t, maxEdits+1)
			if dist < 0 || dist > maxEdits {
				continue
			}
			df := e.tokenDF(index, field, t)
			if df == 0 {
				continue
			}
			score := float64(df)
			if dist > 0 {
				// 距离越大分越低
				score = score / float64(dist+1)
			}
			if c, ok := seen[t]; ok {
				if c.score < score {
					c.score = score
				}
				c.freq += df
			} else {
				seen[t] = &cand{token: t, freq: df, score: score}
			}
		}
	}
	out := make([]SuggestOption, 0, len(seen))
	for _, c := range seen {
		out = append(out, SuggestOption{Text: c.token, Freq: c.freq, Score: c.score})
	}
	sort.Slice(out, func(i, j int) bool {
		if cfg.Sort == "frequency" {
			return out[i].Freq > out[j].Freq
		}
		// 默认: 按 score
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Text < out[j].Text
	})
	if len(out) > size {
		out = out[:size]
	}
	return out
}

// CompletionSuggest 简化版 completion suggester
// 行为: 找 field 倒排中所有以 text 为前缀的 token, 返回 (text, doc) 对
func (e *Engine) CompletionSuggest(index, field, text string, cfg CompletionSuggesterConfig) []SuggestOption {
	if cfg.Size <= 0 {
		cfg.Size = 10
	}
	if text == "" {
		return nil
	}
	prefix := strings.ToLower(text)
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.inverted[index] == nil || e.inverted[index][field] == nil {
		return nil
	}
	tokens := e.suggestTokensWithPrefix(index, field, prefix)
	sort.Strings(tokens)
	if cfg.SkipDuplicates {
		// tokens 已去重(来自 map keys)
	}
	out := make([]SuggestOption, 0, len(tokens))
	seen := make(map[string]bool)
	for _, t := range tokens {
		if cfg.SkipDuplicates && seen[t] {
			continue
		}
		seen[t] = true
		// 收集 doc id 作为 source(只取第一个)
		var firstDoc string
		if pl, ok := e.inverted[index][field][t]; ok {
			for id := range pl {
				firstDoc = id
				break
			}
		}
		out = append(out, SuggestOption{
			Text:   t,
			Score:  1.0,
			Source: firstDoc,
		})
		if len(out) >= cfg.Size {
			break
		}
	}
	return out
}

// PrefixSuggest 极简版 prefix suggester
// 行为: 找 field 倒排中所有以 text 为前缀的 token, 按字典序返回前 size 个
func (e *Engine) PrefixSuggest(index, field, text string, size int) []SuggestOption {
	if size <= 0 {
		size = 10
	}
	if text == "" {
		return nil
	}
	prefix := strings.ToLower(text)
	tokens := e.suggestTokensWithPrefix(index, field, prefix)
	sort.Strings(tokens)
	if len(tokens) > size {
		tokens = tokens[:size]
	}
	out := make([]SuggestOption, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, SuggestOption{Text: t, Score: 1.0})
	}
	return out
}

// suggestTokensWithPrefix 在 (index, field) 倒排中找所有以 prefix 开头的 token
// 必须持锁调用(读锁即可)
func (e *Engine) suggestTokensWithPrefix(index, field, prefix string) []string {
	out := make([]string, 0)
	if e.inverted[index] == nil || e.inverted[index][field] == nil {
		return out
	}
	for tok := range e.inverted[index][field] {
		if strings.HasPrefix(tok, prefix) {
			out = append(out, tok)
		}
	}
	return out
}

// tokenDF 查某 token 在某 (index, field) 的文档频率
func (e *Engine) tokenDF(index, field, token string) int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.inverted[index] == nil || e.inverted[index][field] == nil {
		return 0
	}
	pl, ok := e.inverted[index][field][token]
	if !ok {
		return 0
	}
	return len(pl)
}

// ---------------- Levenshtein 距离 (限定 maxDistance) ----------------

// levenshtein 计算 a 到 b 的 Levenshtein 编辑距离
// 若距离 > maxDistance 直接返回 -1(剪枝, 用于大字符串加速)
// 算法: 经典 DP, O(m*n) 时间, O(min(m,n)) 空间
func levenshtein(a, b string, maxDistance int) int {
	ar := []rune(a)
	br := []rune(b)
	la := len(ar)
	lb := len(br)
	if absInt(la-lb) > maxDistance {
		return -1
	}
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	// 让短串作为列
	if la > lb {
		ar, br = br, ar
		la, lb = lb, la
	}
	// prev row
	prev := make([]int, la+1)
	curr := make([]int, la+1)
	for i := 0; i <= la; i++ {
		prev[i] = i
	}
	for j := 1; j <= lb; j++ {
		curr[0] = j
		// 剪枝: 跟踪本行最小值
		rowMin := curr[0]
		for i := 1; i <= la; i++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			curr[i] = min3(curr[i-1]+1, prev[i]+1, prev[i-1]+cost)
			if curr[i] < rowMin {
				rowMin = curr[i]
			}
		}
		if rowMin > maxDistance {
			return -1
		}
		prev, curr = curr, prev
	}
	d := prev[la]
	if d > maxDistance {
		return -1
	}
	return d
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
