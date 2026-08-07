// Package server - highlight / source 过滤 / track_total_hits
//
// 设计:
//   - highlight: 对 _source 中指定字段, 把匹配 token 用 <em>...</em> 包裹
//   - _source 过滤: true=全显, false=不显, []string=只显指定字段
//   - track_total_hits: true=全量统计, false=截断(默认 10000 上限, 与 ES 一致)
//
// highlight 简化版:
//   - 默认 pre_tag = "<em>", post_tag = "</em>"
//   - 命中判断走 token 集合(tok 出现在字段值中即命中)
//   - 大小写不敏感(小写后比较)
//   - 多 token 拼接(从 query 中提取所有 match 子句的 query 串, 分词后查重)
package server

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/zixiliuyue/go_es/internal/search"
)

// defaultTotalHitsCap track_total_hits 默认上限, 与 ES 一致
const defaultTotalHitsCap = 10000

// defaultHighlightTags 默认高亮标签
var (
	defaultPreTag  = "<em>"
	defaultPostTag = "</em>"
)

// applySourceFilter 按 _source 过滤配置裁剪 doc
// 行为:
//   - nil / true: 原样返回
//   - false: 返回 nil(调用方不写 _source 字段, omitempty 自动省略)
//   - []interface{}: 只保留列出的字段
//   - []string: 同上
//   - string: 单字段
func applySourceFilter(src map[string]interface{}, filter interface{}) map[string]interface{} {
	if filter == nil {
		return src
	}
	switch v := filter.(type) {
	case bool:
		if !v {
			return nil
		}
		return src
	case string:
		return pickFields(src, []string{v})
	case []interface{}:
		fields := make([]string, 0, len(v))
		for _, f := range v {
			if s, ok := f.(string); ok {
				fields = append(fields, s)
			}
		}
		return pickFields(src, fields)
	case []string:
		return pickFields(src, v)
	}
	return src
}

// pickFields 保留 doc 中白名单字段(其他字段不输出)
func pickFields(doc map[string]interface{}, fields []string) map[string]interface{} {
	if doc == nil {
		return nil
	}
	out := make(map[string]interface{}, len(fields))
	for _, f := range fields {
		if v, ok := doc[f]; ok {
			out[f] = v
		}
	}
	return out
}

// applyHighlight 给 _source 字段加高亮
// spec 形如: {"fields": {"title": {}, "content": {}}, "pre_tags": ["<em>"], "post_tags": ["</em>"]}
// 仅对 spec.fields 中声明的字段做高亮, 缺失字段不报错
// 返回 map[field]fragments(fragment 数组, 简化: 1 个 fragment 即整个字段值加 tag)
func applyHighlight(src map[string]interface{}, spec map[string]interface{}, queryTokens []string) map[string][]string {
	if spec == nil || len(queryTokens) == 0 {
		return nil
	}
	fields, _ := spec["fields"].(map[string]interface{})
	if len(fields) == 0 {
		return nil
	}
	preTag := defaultPreTag
	postTag := defaultPostTag
	if arr, ok := spec["pre_tags"].([]interface{}); ok && len(arr) > 0 {
		if s, ok := arr[0].(string); ok && s != "" {
			preTag = s
		}
	}
	if arr, ok := spec["post_tags"].([]interface{}); ok && len(arr) > 0 {
		if s, ok := arr[0].(string); ok && s != "" {
			postTag = s
		}
	}
	// 准备去重后的 query token(小写)
	toks := make(map[string]struct{}, len(queryTokens))
	for _, t := range queryTokens {
		toks[strings.ToLower(t)] = struct{}{}
	}
	out := make(map[string][]string, len(fields))
	for fname := range fields {
		raw, ok := src[fname]
		if !ok || raw == nil {
			continue
		}
		s, ok := raw.(string)
		if !ok {
			// 非字符串字段不参与高亮(数字/布尔/对象)
			continue
		}
		spans := splitHighlightSpans(s)
		fragment := highlightString(s, spans, toks, preTag, postTag)
		if fragment == "" {
			continue
		}
		out[fname] = []string{fragment}
	}
	return out
}

// splitHighlightSpans 切分字符串为 span 序列, 供 highlightString 使用
func splitHighlightSpans(s string) []string {
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9')
	})
	return parts
}

// highlightString 找到 s 中所有 query token, 用 preTag/postTag 包裹
// 大小写不敏感; 多 token 命中同一段时, 每个 token 独立包裹(不嵌套)
// spans 是 splitHighlightSpans(s) 预切好的 part 列表
// 返回值: 若无任何 token 命中, 返回 "" (caller 决定不输出)
func highlightString(s string, spans []string, toks map[string]struct{}, preTag, postTag string) string {
	if len(toks) == 0 || len(spans) == 0 {
		return ""
	}
	// 判断每个 span 是否命中
	hits := make([]bool, len(spans))
	anyHit := false
	for i, p := range spans {
		if _, ok := toks[strings.ToLower(p)]; ok {
			hits[i] = true
			anyHit = true
		}
	}
	if !anyHit {
		return ""
	}
	// 拼回: 保留原 s 的非字母数字字符, 用 spans 替换
	var b strings.Builder
	idx := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		isSep := !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9'))
		if isSep {
			b.WriteByte(c)
			continue
		}
		// 字母数字: 找到对应 span
		if idx >= len(spans) {
			b.WriteByte(c)
			continue
		}
		sp := spans[idx]
		if i == 0 || !((s[i-1] >= 'a' && s[i-1] <= 'z') || (s[i-1] >= 'A' && s[i-1] <= 'Z') || (s[i-1] >= '0' && s[i-1] <= '9')) {
			// span 起点: 输出 (可选) tag + token
			if hits[idx] {
				b.WriteString(preTag)
			}
			b.WriteString(sp)
			if hits[idx] {
				b.WriteString(postTag)
			}
		}
		// 走到 span 末尾时 idx++
		if i+1 == len(s) || !((s[i+1] >= 'a' && s[i+1] <= 'z') || (s[i+1] >= 'A' && s[i+1] <= 'Z') || (s[i+1] >= '0' && s[i+1] <= '9')) {
			idx++
		}
	}
	return b.String()
}

// extractQueryTokensFromQuery 从 search.Query 中提取所有 match 子句的 query 串, 分词后输出
// 给 highlight 用作"哪些 token 需要高亮"
// 这里复用 extractTextClauses, 把每条 clause 的 query 分词合并
func extractQueryTokensFromQuery(q *searchQueryView) []string {
	if q == nil {
		return nil
	}
	clauses := q.textClauses
	out := make([]string, 0)
	seen := make(map[string]struct{})
	for _, c := range clauses {
		for _, t := range tokenizeForHighlight(c.Query) {
			lt := strings.ToLower(t)
			if _, ok := seen[lt]; ok {
				continue
			}
			seen[lt] = struct{}{}
			out = append(out, t)
		}
	}
	// 排序保证可测性
	sort.Strings(out)
	return out
}

// tokenizeForHighlight 简单分词: 按非字母数字字符切, 转小写
// 复制 search.tokenize 的语义, 但本文件不依赖 search 包(避免循环)
func tokenizeForHighlight(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	})
}

// searchQueryView 是 search.Query 的"瘦身视图", 暴露给 highlight 模块
// 这样 highlight 不需要 import search 包, 减少依赖
type searchQueryView struct {
	textClauses []fieldQuery
}

// newSearchQueryView 构造函数(导出给同包测试用)
func newSearchQueryView(clauses []fieldQuery) *searchQueryView {
	return &searchQueryView{textClauses: clauses}
}

// viewForHighlight 从 *search.Query 构造 view
// 复用 search.go 中已有的 extractTextClauses 提取文本子句
func viewForHighlight(q *search.Query) *searchQueryView {
	if q == nil {
		return nil
	}
	clauses := extractTextClauses(q)
	if len(clauses) == 0 {
		return nil
	}
	return &searchQueryView{textClauses: clauses}
}

// trackTotalHitsValue 解析 track_total_hits 字段
// 接受 true / false / int(上限) / nil / json.Number(server decodeJSON 启用了 UseNumber)
// 返回 (是否精确统计, 上限); limit=0 表示无限(精确)
func trackTotalHitsValue(v interface{}) (bool, int) {
	if v == nil {
		return false, defaultTotalHitsCap // 默认 ES 行为: 10000 上限
	}
	switch x := v.(type) {
	case bool:
		if x {
			return true, 0
		}
		return false, defaultTotalHitsCap
	case float64:
		if x <= 0 {
			return true, 0
		}
		return false, int(x)
	case int:
		if x <= 0 {
			return true, 0
		}
		return false, x
	case int64:
		if x <= 0 {
			return true, 0
		}
		return false, int(x)
	case string:
		// 字符串 "true"/"false"/"1234"
		switch x {
		case "true":
			return true, 0
		case "false":
			return false, defaultTotalHitsCap
		}
		if n, err := strconv.Atoi(x); err == nil {
			if n <= 0 {
				return true, 0
			}
			return false, n
		}
	}
	// json.Number 或其它数值类型: 走通用反射路径
	if n, ok := jsonNumberToInt(v); ok {
		if n <= 0 {
			return true, 0
		}
		return false, n
	}
	return false, defaultTotalHitsCap
}

// jsonNumberToInt 把 json.Number / int / int64 / float64 / 字符串数字 转 int
func jsonNumberToInt(v interface{}) (int, bool) {
	switch x := v.(type) {
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return int(n), true
		}
		if f, err := x.Float64(); err == nil {
			return int(f), true
		}
	}
	return 0, false
}
