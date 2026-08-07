// multi_match / query_string / simple_query_string
//
// 设计:
//   - multi_match: 在多个字段上跑 match, 然后用 type 决定如何合并:
//     * "best_fields" (default): 选 score 最高的字段, 适合 search-as-you-type
//     * "most_fields": 把所有字段 score 相加, 适合多个等价字段
//     * "cross_fields": 把所有字段当一个字段(去重 docID 命中即可)
//     * "phrase": match_phrase 等价
//     * "phrase_prefix": 末 token 当 prefix (本实现简化为: 末 token 必须存在, 之前的 token 必须有 match)
//   - query_string: 完整 Lucene 语法 (简化版, 不支持正则/通配/~N/fuzzy 等)
//     * +foo 必含, -foo 必不含, "foo bar" 短语, field:value 字段限定
//     * foo bar (空格分隔) 应都含(隐式 AND)
//     * foo OR bar
//   - simple_query_string: 简版 (没有 + / - 强行, 但 +foo -bar 可用)
//     * 不会抛语法错, 抛弃不识别的字符
package search

import (
	"fmt"
	"strings"
)

// ---------------- multi_match ----------------

// evalMultiMatch 处理 multi_match 查询
// spec: {"query": "...", "fields": ["a", "b", "c"], "type": "best_fields" | "most_fields" | "cross_fields" | "phrase" | "phrase_prefix"}
// 输出: 命中的 docID 集合
func (e *Engine) evalMultiMatch(index string, spec map[string]interface{}) (map[string]struct{}, error) {
	if len(spec) == 0 {
		return nil, fmt.Errorf("multi_match requires spec")
	}
	// query
	q, _ := spec["query"].(string)
	if q == "" {
		return nil, fmt.Errorf("multi_match.query required")
	}
	// fields
	fieldsRaw, ok := spec["fields"].([]interface{})
	if !ok || len(fieldsRaw) == 0 {
		// 也支持 []string
		if fs, ok2 := spec["fields"].([]string); ok2 && len(fs) > 0 {
			fields := fs
			return e.runMultiMatch(index, q, fields, spec)
		}
		return nil, fmt.Errorf("multi_match.fields required")
	}
	fields := make([]string, 0, len(fieldsRaw))
	for _, f := range fieldsRaw {
		if s, ok := f.(string); ok {
			fields = append(fields, s)
		}
	}
	if len(fields) == 0 {
		return nil, fmt.Errorf("multi_match.fields must be non-empty string array")
	}
	return e.runMultiMatch(index, q, fields, spec)
}

// runMultiMatch 按 type 走不同分支
func (e *Engine) runMultiMatch(index, query string, fields []string, spec map[string]interface{}) (map[string]struct{}, error) {
	typ, _ := spec["type"].(string)
	switch typ {
	case "", "best_fields":
		return e.multiMatchBestFields(index, query, fields)
	case "most_fields":
		return e.multiMatchMostFields(index, query, fields)
	case "cross_fields":
		return e.multiMatchCrossFields(index, query, fields)
	case "phrase":
		return e.multiMatchPhrase(index, query, fields)
	case "phrase_prefix":
		return e.multiMatchPhrasePrefix(index, query, fields)
	}
	return nil, fmt.Errorf("multi_match type %q unsupported", typ)
}

// multiMatchBestFields 选 score 最高的字段; 命中 = 任一字段命中
// 简化: 命中 = 至少一个字段上 match 命中(不真算 score, 仅集合合并)
func (e *Engine) multiMatchBestFields(index, query string, fields []string) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	for _, f := range fields {
		set := e.matchDocsTokens(index, f, query)
		for id := range set {
			out[id] = struct{}{}
		}
	}
	return out, nil
}

// multiMatchMostFields 把所有字段 score 相加(命中 = 任一字段命中)
func (e *Engine) multiMatchMostFields(index, query string, fields []string) (map[string]struct{}, error) {
	// 简化: 集合并(不真正累计 score)
	return e.multiMatchBestFields(index, query, fields)
}

// multiMatchCrossFields 跨字段: docID 命中条件 = 任一字段命中
// 与 best_fields 在集合语义上等价
func (e *Engine) multiMatchCrossFields(index, query string, fields []string) (map[string]struct{}, error) {
	return e.multiMatchBestFields(index, query, fields)
}

// multiMatchPhrase 多字段 match_phrase 等价
func (e *Engine) multiMatchPhrase(index, query string, fields []string) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	for _, f := range fields {
		set, err := e.phraseMatchInField(index, f, tokenize(query))
		if err != nil {
			return nil, err
		}
		for id := range set {
			out[id] = struct{}{}
		}
	}
	return out, nil
}

// multiMatchPhrasePrefix 末 token 视为 prefix
// 简化: 切词, 前 n-1 个 token 严格存在, 末 token 取 prefix 找所有候选
func (e *Engine) multiMatchPhrasePrefix(index, query string, fields []string) (map[string]struct{}, error) {
	toks := tokenize(query)
	if len(toks) == 0 {
		return nil, nil
	}
	out := map[string]struct{}{}
	for _, f := range fields {
		// 取该字段所有 doc, 检查: 前 n-1 token 出现, 末 token 至少存在一个以它开头的 token
		e.mu.RLock()
		docs := make(map[string]map[string]interface{}, len(e.docs[index]))
		for id, doc := range e.docs[index] {
			docs[id] = doc
		}
		e.mu.RUnlock()
		for id, doc := range docs {
			raw, ok := doc[f]
			if !ok {
				continue
			}
			s, ok := raw.(string)
			if !ok {
				continue
			}
			toksDoc := tokenize(s)
			if len(toksDoc) < len(toks) {
				continue
			}
			// 前 n-1 token 必须在 toksDoc 中出现
			okAll := true
			for _, t := range toks[:len(toks)-1] {
				found := false
				for _, td := range toksDoc {
					if td == t {
						found = true
						break
					}
				}
				if !found {
					okAll = false
					break
				}
			}
			if !okAll {
				continue
			}
			// 末 token 用 prefix 匹配
			last := toks[len(toks)-1]
			foundLast := false
			for _, td := range toksDoc {
				if strings.HasPrefix(td, last) {
					foundLast = true
					break
				}
			}
			if foundLast {
				out[id] = struct{}{}
			}
		}
	}
	return out, nil
}

// ---------------- query_string ----------------

// evalQueryString 处理 query_string
// spec: {"query": "+foo -bar OR \"exact phrase\" field:baz", "default_field": "title"}
// 简化语法: +must, -must_not, "phrase", field:value, foo bar (隐式 AND), OR
func (e *Engine) evalQueryString(index string, spec map[string]interface{}) (map[string]struct{}, error) {
	if len(spec) == 0 {
		return nil, fmt.Errorf("query_string requires spec")
	}
	q, _ := spec["query"].(string)
	if q == "" {
		return nil, fmt.Errorf("query_string.query required")
	}
	defaultField, _ := spec["default_field"].(string)
	if defaultField == "" {
		defaultField = "_all"
	}
	// 也支持 ["field1", "field2"] 形式
	if fieldsRaw, ok := spec["fields"].([]interface{}); ok && len(fieldsRaw) > 0 {
		fields := make([]string, 0, len(fieldsRaw))
		for _, f := range fieldsRaw {
			if s, ok := f.(string); ok {
				fields = append(fields, s)
			}
		}
		if len(fields) > 0 {
			defaultField = strings.Join(fields, ",")
		}
	}
	clauses, err := parseQueryString(q, defaultField)
	if err != nil {
		return nil, err
	}
	return e.evalQueryStringClauses(index, clauses)
}

// queryStringClause 解析后的 query_string 子句
type queryStringClause struct {
	field   string // 限定字段(空=默认)
	must    bool   // true=必须, false=必须不含
	phrase  bool   // 是否短语
	text    string // 内容
	isOr    bool   // 是否 OR 关系(否则为 AND)
}

// parseQueryString 简化的 Lucene 解析器
// 支持: +must -must_not "phrase" field:value
// 连接: 空格(隐式 AND), OR
// 字段限定: field:value (field 可为 a 或 a,b 多字段)
func parseQueryString(q, defaultField string) ([]queryStringClause, error) {
	out := make([]queryStringClause, 0)
	i := 0
	for i < len(q) {
		// 跳过空白
		for i < len(q) && (q[i] == ' ' || q[i] == '\t' || q[i] == '\n') {
			i++
		}
		if i >= len(q) {
			break
		}
		// must / must_not
		must := true
		if q[i] == '+' {
			must = true
			i++
		} else if q[i] == '-' {
			must = false
			i++
		}
		// 跳过空白
		for i < len(q) && (q[i] == ' ' || q[i] == '\t' || q[i] == '\n') {
			i++
		}
		if i >= len(q) {
			break
		}
		// 短语 "..."
		if q[i] == '"' {
			j := i + 1
			for j < len(q) && q[j] != '"' {
				j++
			}
			if j >= len(q) {
				return nil, fmt.Errorf("query_string: unterminated phrase at %d", i)
			}
			clause := queryStringClause{
				field:  defaultField,
				must:   must,
				phrase: true,
				text:   q[i+1 : j],
			}
			out = append(out, clause)
			// 如果后面是 OR, 显式插入一个 OR 子句
			if peekOrAfter(q, j+1) {
				out = append(out, queryStringClause{text: "OR"})
			}
			i = j + 1
			continue
		}
		// OR
		if i+2 <= len(q) && q[i:i+2] == "OR" && (i+2 == len(q) || q[i+2] == ' ') {
			// OR 单独存在(不应作为子句)
			i += 2
			continue
		}
		// 普通 token: 找空格
		j := i
		for j < len(q) && q[j] != ' ' && q[j] != '\t' && q[j] != '\n' {
			j++
		}
		tok := q[i:j]
		// 字段限定: field:value
		field := defaultField
		text := tok
		if k := strings.Index(tok, ":"); k > 0 {
			candidate := tok[:k]
			// 仅当 candidate 是合法标识符(无空格)时才算
			if !strings.ContainsAny(candidate, " \t") {
				field = candidate
				text = tok[k+1:]
			}
		}
		clause := queryStringClause{
			field: field,
			must:  must,
			text:  text,
		}
		out = append(out, clause)
		// 如果后面是 OR, 显式插入一个 OR 子句
		if peekOrAfter(q, j) {
			out = append(out, queryStringClause{text: "OR"})
		}
		i = j
	}
	return out, nil
}

// peekOrAfter 看 j 之后是否是 OR (忽略空白)
func peekOrAfter(q string, j int) bool {
	for j < len(q) && (q[j] == ' ' || q[j] == '\t') {
		j++
	}
	return j+2 <= len(q) && q[j:j+2] == "OR" && (j+2 == len(q) || q[j+2] == ' ' || q[j+2] == '\t')
}

// evalQueryStringClauses 在字段上评估子句
// 逻辑: must 子句的命中求交(AND), must_not 子句的命中求差集
// OR 关系: 同优先级的连续 must 子句之间支持 OR(本次简化为: OR 连接的两个 must 取并集, 再与其它 must 求交)
func (e *Engine) evalQueryStringClauses(index string, clauses []queryStringClause) (map[string]struct{}, error) {
	// 第一遍: 把所有子句的命中算出来
	allDocs := e.allDocsSet(index)
	out := allDocs
	for i := 0; i < len(clauses); i++ {
		c := clauses[i]
		// 跳过纯 "OR" 标记(本实现中 peekOrAfter 只是标记, 不需要单独处理)
		if c.text == "OR" {
			continue
		}
		set, err := e.evalQSClause(index, c)
		if err != nil {
			return nil, err
		}
		// 必须不含
		if !c.must {
			out = subtractSets(out, set)
			continue
		}
		// 必须含: 与当前结果求交
		// 但如果下一个子句是 OR, 那么连续 OR 内的 must 取并集, 再与外层求交
		if i+1 < len(clauses) && clauses[i+1].text == "OR" {
			// 收集 OR 链: A OR B OR C
			// 当前 clauses[i] 是 A, 然后 OR pairs: (OR,B), (OR,C)
			union := set
			j := i + 1
			for j < len(clauses) && clauses[j].text == "OR" {
				if j+1 >= len(clauses) {
					break
				}
				s2, err := e.evalQSClause(index, clauses[j+1])
				if err != nil {
					return nil, err
				}
				union = unionSets(union, s2)
				j += 2
			}
			out = intersectSets(out, union)
			// 跳过 OR 链: 把 i 推到 j-1(下一次 for 循环会 i++)
			i = j - 1
			continue
		}
		out = intersectSets(out, set)
	}
	return out, nil
}

// orChainLen 返回 OR 链中除当前 i 之外的子句数(包含所有 OR + 跟随项)
// 例: clauses=[A, OR, B, OR, C]  startOrIdx=1 -> 返回 4 (OR,B,OR,C)
// 例: clauses=[A, OR, B]         startOrIdx=1 -> 返回 2 (OR,B)
func orChainLen(clauses []queryStringClause, startOrIdx int) int {
	cnt := 0
	for i := startOrIdx; i < len(clauses); i += 2 {
		// clauses[i] 应该是 OR 标记
		if i >= len(clauses) || clauses[i].text != "OR" {
			break
		}
		// OR 配对一个右侧子句
		if i+1 >= len(clauses) {
			break
		}
		cnt += 2
	}
	return cnt
}

// evalQSClause 在某字段上执行一个子句
func (e *Engine) evalQSClause(index string, c queryStringClause) (map[string]struct{}, error) {
	// 字段支持多字段(逗号分隔)
	fields := strings.Split(c.field, ",")
	out := map[string]struct{}{}
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		var set map[string]struct{}
		if c.phrase {
			set, _ = e.phraseMatchInField(index, f, tokenize(c.text))
		} else {
			set = e.matchDocsTokens(index, f, c.text)
		}
		for id := range set {
			out[id] = struct{}{}
		}
	}
	return out, nil
}

// ---------------- simple_query_string ----------------

// evalSimpleQueryString 处理 simple_query_string
// spec: {"query": "foo bar -baz", "fields": ["title"], "default_operator": "or"|"and"}
// 与 query_string 类似, 但不抛语法错: 抛弃不识别的字符
// 简化: +foo -baz 应都含 / 不含, "phrase" 支持, OR 支持
func (e *Engine) evalSimpleQueryString(index string, spec map[string]interface{}) (map[string]struct{}, error) {
	if len(spec) == 0 {
		return nil, fmt.Errorf("simple_query_string requires spec")
	}
	q, _ := spec["query"].(string)
	if q == "" {
		return nil, fmt.Errorf("simple_query_string.query required")
	}
	defaultField, _ := spec["default_field"].(string)
	if defaultField == "" {
		defaultField = "_all"
	}
	if fieldsRaw, ok := spec["fields"].([]interface{}); ok && len(fieldsRaw) > 0 {
		fields := make([]string, 0, len(fieldsRaw))
		for _, f := range fieldsRaw {
			if s, ok := f.(string); ok {
				fields = append(fields, s)
			}
		}
		if len(fields) > 0 {
			defaultField = strings.Join(fields, ",")
		}
	}
	clauses := parseSimpleQueryString(q, defaultField)
	return e.evalQueryStringClauses(index, clauses)
}

// parseSimpleQueryString 简版 (无语法错)
// 与 query_string 主要区别:
//   - 抛弃 | < > ( ) { } [ ] ^ " ~ * ? : \ /
//   - AND/OR 是显式大写关键字
//   - 必须项用 +foo, 排除用 -foo (与 query_string 一致)
func parseSimpleQueryString(q, defaultField string) []queryStringClause {
	// 抛弃特殊字符
	stripped := stripSQSReserved(q)
	clauses, _ := parseQueryString(stripped, defaultField)
	return clauses
}

func stripSQSReserved(q string) string {
	// 简单替换: 把不应出现的字符替换为空格
	runes := []rune(q)
	for i, c := range runes {
		switch c {
		case '|', '<', '>', '(', ')', '{', '}', '[', ']', '^', '~', '*', '?', '\\', '/':
			runes[i] = ' '
		}
	}
	return string(runes)
}

// ---------------- 集合工具 ----------------

func (e *Engine) allDocsSet(index string) map[string]struct{} {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make(map[string]struct{}, len(e.docs[index]))
	if e.docs[index] == nil {
		return out
	}
	for id := range e.docs[index] {
		out[id] = struct{}{}
	}
	return out
}

func intersectSets(a, b map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(b))
	if len(a) > len(b) {
		a, b = b, a
	}
	for k := range a {
		if _, ok := b[k]; ok {
			out[k] = struct{}{}
		}
	}
	return out
}

func unionSets(a, b map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		out[k] = struct{}{}
	}
	for k := range b {
		out[k] = struct{}{}
	}
	return out
}

func subtractSets(a, b map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(a))
	for k := range a {
		if _, ok := b[k]; ok {
			continue
		}
		out[k] = struct{}{}
	}
	return out
}
