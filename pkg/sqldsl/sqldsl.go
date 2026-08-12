// Package sqldsl 提供 SQL 与 Elasticsearch DSL 之间的双向转换
//
// 支持 SQL 子集:
//   SELECT [field,... | * | COUNT(*)] FROM <index>
//   [WHERE <condition> [AND|OR <condition>...]]
//   [LIMIT n] [OFFSET n]
//   [ORDER BY <field> [ASC|DESC]]
//
// 支持 WHERE 条件:
//   field = 'value'           -> term
//   field = 123               -> term
//   field IN ('a','b')        -> terms
//   field > n / >= / < / <=   -> range
//   field LIKE 'prefix%'      -> match_phrase_prefix (简化为前缀匹配)
//
// 双向转换:
//   SQLToDSL(sql) -> map[string]interface{} (ES _search body)
//   DSLToSQL(body) -> string (SQL)
package sqldsl

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Token
// ---------------------------------------------------------------------------

type tokenType int

const (
	tkEOF tokenType = iota
	tkIdent
	tkNumber
	tkString
	tkKeyword
	tkOperator
	tkPunct
)

type token struct {
	typ   tokenType
	value string
}

// 关键字(小写化匹配)
var keywords = map[string]bool{
	"select": true, "from": true, "where": true, "and": true, "or": true,
	"limit": true, "offset": true, "order": true, "by": true, "asc": true,
	"desc": true, "in": true, "like": true, "count": true, "as": true,
}

// tokenize 把 SQL 字符串切为 token 列表
func tokenize(s string) ([]token, error) {
	var out []token
	i, n := 0, len(s)
	for i < n {
		c := s[i]
		// 跳过空白
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			i++
			continue
		}
		// 字符串字面量(单引号)
		if c == '\'' {
			j := i + 1
			var b strings.Builder
			for j < n {
				if s[j] == '\'' {
					if j+1 < n && s[j+1] == '\'' { // 转义 ''
						b.WriteByte('\'')
						j += 2
						continue
					}
					break
				}
				b.WriteByte(s[j])
				j++
			}
			if j >= n {
				return nil, fmt.Errorf("unterminated string at %d", i)
			}
			out = append(out, token{tkString, b.String()})
			i = j + 1
			continue
		}
		// 双引号包裹的标识符(ES 字段名可能带 -)
		if c == '"' {
			j := i + 1
			for j < n && s[j] != '"' {
				j++
			}
			if j >= n {
				return nil, fmt.Errorf("unterminated quoted identifier at %d", i)
			}
			out = append(out, token{tkIdent, s[i+1 : j]})
			i = j + 1
			continue
		}
		// 数字
		if c >= '0' && c <= '9' {
			j := i
			for j < n && ((s[j] >= '0' && s[j] <= '9') || s[j] == '.') {
				j++
			}
			out = append(out, token{tkNumber, s[i:j]})
			i = j
			continue
		}
		// 标识符 / 关键字
		if isIdentStart(c) {
			j := i
			for j < n && isIdentPart(s[j]) {
				j++
			}
			word := s[i:j]
			lower := strings.ToLower(word)
			if keywords[lower] {
				out = append(out, token{tkKeyword, lower})
			} else {
				out = append(out, token{tkIdent, word})
			}
			i = j
			continue
		}
		// 操作符
		if c == '>' || c == '<' || c == '=' || c == '!' {
			if i+1 < n && s[i+1] == '=' {
				out = append(out, token{tkOperator, s[i : i+2]})
				i += 2
				continue
			}
			out = append(out, token{tkOperator, string(c)})
			i++
			continue
		}
		if c == '*' {
			out = append(out, token{tkOperator, "*"})
			i++
			continue
		}
		// 标点
		if c == ',' || c == '(' || c == ')' || c == ';' || c == '.' {
			out = append(out, token{tkPunct, string(c)})
			i++
			continue
		}
		return nil, fmt.Errorf("unexpected char %q at %d", string(c), i)
	}
	out = append(out, token{tkEOF, ""})
	return out, nil
}

func isIdentStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}
func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9') || c == '-' || c == '@'
}

// ---------------------------------------------------------------------------
// AST
// ---------------------------------------------------------------------------

// SelectStmt 简化的 SELECT 语句
type SelectStmt struct {
	Fields    []FieldRef // 字段列表(* 用空列表+All=true 表示)
	All       bool       // SELECT *
	FromIndex string
	Where     Expr       // 可为 nil
	Limit     int        // 0 = 不限制
	Offset    int
	OrderBy   []OrderTerm
}

// FieldRef 字段引用(支持 COUNT(*) AS alias)
type FieldRef struct {
	Expr  string // "*" 或 字段名 或 "COUNT(*)"
	Alias string
	IsCount bool
}

// OrderTerm 排序项
type OrderTerm struct {
	Field string
	Desc  bool
}

// Expr WHERE 表达式
type Expr interface{ exprNode() }

type BinOp struct {
	Op    string // AND / OR
	Left  Expr
	Right Expr
}

func (BinOp) exprNode() {}

type Compare struct {
	Field string
	Op    string // = != > >= < <=
	Val   interface{}
}

func (Compare) exprNode() {}

type InExpr struct {
	Field string
	Vals  []interface{}
	Neg   bool
}

func (InExpr) exprNode() {}

type LikeExpr struct {
	Field  string
	Prefix string // 已去掉 % 的前缀
}

func (LikeExpr) exprNode() {}

// ---------------------------------------------------------------------------
// Parser
// ---------------------------------------------------------------------------

type parser struct {
	toks []token
	pos  int
}

func (p *parser) peek() token { return p.toks[p.pos] }
func (p *parser) next() token { t := p.toks[p.pos]; p.pos++; return t }
func (p *parser) eof() bool   { return p.peek().typ == tkEOF }

func (p *parser) expectKeyword(kw string) error {
	t := p.peek()
	if t.typ != tkKeyword || t.value != kw {
		return fmt.Errorf("expect keyword %q, got %q", kw, t.value)
	}
	p.next()
	return nil
}

func (p *parser) expectPunct(c string) error {
	t := p.peek()
	if t.typ != tkPunct || t.value != c {
		return fmt.Errorf("expect %q, got %q", c, t.value)
	}
	p.next()
	return nil
}

// parseSelect 解析 SELECT 语句
func (p *parser) parseSelect() (*SelectStmt, error) {
	if err := p.expectKeyword("select"); err != nil {
		return nil, err
	}
	stmt := &SelectStmt{Limit: 0}

	// 字段列表
	fields, all, err := p.parseFieldList()
	if err != nil {
		return nil, err
	}
	stmt.Fields = fields
	stmt.All = all

	// FROM
	if err := p.expectKeyword("from"); err != nil {
		return nil, err
	}
	t := p.next()
	if t.typ != tkIdent {
		return nil, fmt.Errorf("expect index name, got %q", t.value)
	}
	stmt.FromIndex = t.value

	// WHERE
	if p.peek().typ == tkKeyword && p.peek().value == "where" {
		p.next()
		expr, err := p.parseExpr(0)
		if err != nil {
			return nil, err
		}
		stmt.Where = expr
	}

	// ORDER BY
	if p.peek().typ == tkKeyword && p.peek().value == "order" {
		p.next()
		if err := p.expectKeyword("by"); err != nil {
			return nil, err
		}
		for {
			t := p.next()
			if t.typ != tkIdent {
				return nil, fmt.Errorf("expect order field, got %q", t.value)
			}
			ot := OrderTerm{Field: t.value}
			if p.peek().typ == tkKeyword && (p.peek().value == "asc" || p.peek().value == "desc") {
				if p.next().value == "desc" {
					ot.Desc = true
				}
			}
			stmt.OrderBy = append(stmt.OrderBy, ot)
			if !(p.peek().typ == tkPunct && p.peek().value == ",") {
				break
			}
			p.next()
		}
	}

	// LIMIT
	if p.peek().typ == tkKeyword && p.peek().value == "limit" {
		p.next()
		t := p.next()
		if t.typ != tkNumber {
			return nil, fmt.Errorf("expect limit number, got %q", t.value)
		}
		n, err := strconv.Atoi(t.value)
		if err != nil {
			return nil, fmt.Errorf("bad limit: %v", err)
		}
		stmt.Limit = n
	}

	// OFFSET
	if p.peek().typ == tkKeyword && p.peek().value == "offset" {
		p.next()
		t := p.next()
		if t.typ != tkNumber {
			return nil, fmt.Errorf("expect offset number, got %q", t.value)
		}
		n, err := strconv.Atoi(t.value)
		if err != nil {
			return nil, fmt.Errorf("bad offset: %v", err)
		}
		stmt.Offset = n
	}

	// 结束
	if !p.eof() && !(p.peek().typ == tkPunct && p.peek().value == ";") {
		return nil, fmt.Errorf("unexpected trailing token: %q", p.peek().value)
	}
	return stmt, nil
}

// parseFieldList 解析 SELECT 后的字段列表
func (p *parser) parseFieldList() ([]FieldRef, bool, error) {
	// SELECT *
	if p.peek().typ == tkOperator && p.peek().value == "*" {
		p.next()
		return nil, true, nil
	}
	var fields []FieldRef
	for {
		t := p.peek()
		// COUNT(*)
		if t.typ == tkKeyword && t.value == "count" {
			p.next()
			if err := p.expectPunct("("); err != nil {
				return nil, false, err
			}
			if !(p.peek().typ == tkOperator && p.peek().value == "*") {
				return nil, false, fmt.Errorf("expect * in COUNT()")
			}
			p.next()
			if err := p.expectPunct(")"); err != nil {
				return nil, false, err
			}
			fr := FieldRef{Expr: "COUNT(*)", IsCount: true}
			// alias
			if p.peek().typ == tkKeyword && p.peek().value == "as" {
				p.next()
				a := p.next()
				if a.typ != tkIdent {
					return nil, false, fmt.Errorf("expect alias, got %q", a.value)
				}
				fr.Alias = a.value
			}
			fields = append(fields, fr)
		} else if t.typ == tkIdent {
			p.next()
			fields = append(fields, FieldRef{Expr: t.value})
		} else {
			return nil, false, fmt.Errorf("expect field, got %q", t.value)
		}
		if !(p.peek().typ == tkPunct && p.peek().value == ",") {
			break
		}
		p.next()
	}
	return fields, false, nil
}

// parseExpr 递归下降解析 OR / AND / 比较
// 优先级: OR < AND < 比较
func (p *parser) parseExpr(minPrio int) (Expr, error) {
	// 左项
	left, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.typ != tkKeyword {
			break
		}
		var op string
		var prio int
		switch t.value {
		case "and":
			op, prio = "AND", 1
		case "or":
			op, prio = "OR", 0
		default:
			goto done
		}
		if prio < minPrio {
			break
		}
		p.next()
		right, err := p.parseExpr(prio + 1)
		if err != nil {
			return nil, err
		}
		left = BinOp{Op: op, Left: left, Right: right}
	}
done:
	return left, nil
}

// parsePrimary 解析一个比较 / IN / LIKE / (expr)
func (p *parser) parsePrimary() (Expr, error) {
	// 括号
	if p.peek().typ == tkPunct && p.peek().value == "(" {
		p.next()
		e, err := p.parseExpr(0)
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
		return e, nil
	}
	// 字段名
	t := p.next()
	if t.typ != tkIdent {
		return nil, fmt.Errorf("expect field name, got %q", t.value)
	}
	field := t.value
	// IN
	if p.peek().typ == tkKeyword && p.peek().value == "in" {
		p.next()
		if err := p.expectPunct("("); err != nil {
			return nil, err
		}
		var vals []interface{}
		for {
			v, err := p.parseLiteral()
			if err != nil {
				return nil, err
			}
			vals = append(vals, v)
			if !(p.peek().typ == tkPunct && p.peek().value == ",") {
				break
			}
			p.next()
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
		return InExpr{Field: field, Vals: vals}, nil
	}
	// LIKE
	if p.peek().typ == tkKeyword && p.peek().value == "like" {
		p.next()
		v := p.next()
		if v.typ != tkString {
			return nil, fmt.Errorf("LIKE expects string, got %q", v.value)
		}
		// 只支持 prefix%
		prefix := strings.TrimSuffix(v.value, "%")
		if !strings.HasSuffix(v.value, "%") || strings.Contains(prefix, "%") || strings.Contains(prefix, "_") {
			return nil, fmt.Errorf("LIKE only supports prefix%% pattern, got %q", v.value)
		}
		return LikeExpr{Field: field, Prefix: prefix}, nil
	}
	// 比较操作符
	if p.peek().typ != tkOperator {
		return nil, fmt.Errorf("expect operator after %q, got %q", field, p.peek().value)
	}
	op := p.next().value
	v, err := p.parseLiteral()
	if err != nil {
		return nil, err
	}
	return Compare{Field: field, Op: op, Val: v}, nil
}

// parseLiteral 解析字面值(字符串 / 数字)
func (p *parser) parseLiteral() (interface{}, error) {
	t := p.next()
	switch t.typ {
	case tkString:
		return t.value, nil
	case tkNumber:
		if strings.Contains(t.value, ".") {
			return strconv.ParseFloat(t.value, 64)
		}
		return strconv.Atoi(t.value)
	}
	return nil, fmt.Errorf("expect literal, got %q", t.value)
}

// ---------------------------------------------------------------------------
// SQLToDSL: SQL AST -> ES _search body
// ---------------------------------------------------------------------------

// SQLToDSL 把 SQL 字符串转为 ES _search body
func SQLToDSL(sql string) (map[string]interface{}, error) {
	toks, err := tokenize(sql)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	stmt, err := p.parseSelect()
	if err != nil {
		return nil, err
	}
	return stmtToDSL(stmt)
}

// stmtToDSL 把 SelectStmt 转为 _search body
func stmtToDSL(s *SelectStmt) (map[string]interface{}, error) {
	body := map[string]interface{}{}
	// query
	if s.Where != nil {
		body["query"] = exprToQuery(s.Where)
	} else {
		body["query"] = map[string]interface{}{"match_all": map[string]interface{}{}}
	}
	// from / size
	if s.Offset > 0 {
		body["from"] = s.Offset
	}
	if s.Limit > 0 {
		body["size"] = s.Limit
	}
	// _source
	if !s.All && len(s.Fields) > 0 {
		var srcFields []string
		var aggAdded bool
		for _, f := range s.Fields {
			if f.IsCount {
				// COUNT(*) -> 用 cardinality 或 hits.total 替代
				// 这里不产生 _source, 改为 size:0 + 用 hits.total
				body["size"] = 0
				if !aggAdded {
					body["aggs"] = map[string]interface{}{
						"count": map[string]interface{}{
							"value_count": map[string]interface{}{"_count": ""},
						},
					}
					aggAdded = true
				}
			} else {
				srcFields = append(srcFields, f.Expr)
			}
		}
		if len(srcFields) > 0 {
			body["_source"] = srcFields
		}
	}
	// sort
	if len(s.OrderBy) > 0 {
		sortArr := make([]interface{}, 0, len(s.OrderBy))
		for _, ot := range s.OrderBy {
			dir := "asc"
			if ot.Desc {
				dir = "desc"
			}
			sortArr = append(sortArr, map[string]interface{}{ot.Field: dir})
		}
		body["sort"] = sortArr
	}
	return body, nil
}

// exprToQuery 把 WHERE 表达式转为 ES query 子句
func exprToQuery(e Expr) map[string]interface{} {
	switch x := e.(type) {
	case BinOp:
		clauses := []interface{}{
			exprToQuery(x.Left),
			exprToQuery(x.Right),
		}
		if x.Op == "AND" {
			// AND -> bool.filter(must)
			return map[string]interface{}{
				"bool": map[string]interface{}{"filter": clauses},
			}
		}
		// OR -> bool.should
		return map[string]interface{}{
			"bool": map[string]interface{}{"should": clauses, "minimum_should_match": 1},
		}
	case Compare:
		return compareToQuery(x)
	case InExpr:
		return map[string]interface{}{
			"terms": map[string]interface{}{x.Field: x.Vals},
		}
	case LikeExpr:
		// prefix 模糊匹配 -> match_phrase_prefix (简化)
		return map[string]interface{}{
			"match_phrase_prefix": map[string]interface{}{x.Field: x.Prefix},
		}
	}
	return map[string]interface{}{"match_all": map[string]interface{}{}}
}

// compareToQuery 把比较转为 ES query
func compareToQuery(c Compare) map[string]interface{} {
	// = -> term; 其他 -> range
	if c.Op == "=" {
		return map[string]interface{}{
			"term": map[string]interface{}{c.Field: c.Val},
		}
	}
	if c.Op == "!=" {
		return map[string]interface{}{
			"bool": map[string]interface{}{
				"must_not": []interface{}{
					map[string]interface{}{"term": map[string]interface{}{c.Field: c.Val}},
				},
			},
		}
	}
	// range
	rng := map[string]interface{}{}
	switch c.Op {
	case ">":
		rng["gt"] = c.Val
	case ">=":
		rng["gte"] = c.Val
	case "<":
		rng["lt"] = c.Val
	case "<=":
		rng["lte"] = c.Val
	}
	return map[string]interface{}{
		"range": map[string]interface{}{c.Field: rng},
	}
}

// ---------------------------------------------------------------------------
// DSLToSQL: ES _search body -> SQL (反向转换)
// ---------------------------------------------------------------------------

// DSLToSQL 把 ES _search body 转为 SQL 字符串
func DSLToSQL(body map[string]interface{}, index string) string {
	var b strings.Builder
	b.WriteString("SELECT * FROM ")
	b.WriteString(quoteIdent(index))

	// query -> WHERE
	if q, ok := body["query"].(map[string]interface{}); ok {
		where := queryToWhere(q)
		if where != "" {
			b.WriteString(" WHERE ")
			b.WriteString(where)
		}
	}

	// sort -> ORDER BY
	if sort, ok := body["sort"].([]interface{}); ok && len(sort) > 0 {
		b.WriteString(" ORDER BY ")
		for i, s := range sort {
			if i > 0 {
				b.WriteString(", ")
			}
			if m, ok := s.(map[string]interface{}); ok {
				for f, d := range m {
					b.WriteString(quoteIdent(f))
					if ds, _ := d.(string); ds == "desc" {
						b.WriteString(" DESC")
					} else {
						b.WriteString(" ASC")
					}
					break
				}
			}
		}
	}

	// from -> OFFSET
	if from, ok := body["from"]; ok {
		b.WriteString(" OFFSET ")
		b.WriteString(fmt.Sprintf("%v", from))
	}

	// size -> LIMIT
	if size, ok := body["size"]; ok {
		b.WriteString(" LIMIT ")
		b.WriteString(fmt.Sprintf("%v", size))
	}

	return b.String()
}

// queryToWhere 把 ES query 子句转回 WHERE 表达式
func queryToWhere(q map[string]interface{}) string {
	// match_all -> 无 WHERE
	if _, ok := q["match_all"]; ok {
		return ""
	}
	if m, ok := q["match"].(map[string]interface{}); ok {
		for f, v := range m {
			// match 子句可能是 {field: "value"} 或 {field: {query: "value"}}
			val := extractMatchValue(v)
			return fmt.Sprintf("%s = %s", quoteIdent(f), quoteLiteral(val))
		}
	}
	if m, ok := q["term"].(map[string]interface{}); ok {
		for f, v := range m {
			return fmt.Sprintf("%s = %s", quoteIdent(f), quoteLiteral(v))
		}
	}
	if m, ok := q["terms"].(map[string]interface{}); ok {
		for f, v := range m {
			if vals, ok := v.([]interface{}); ok {
				return fmt.Sprintf("%s IN (%s)", quoteIdent(f), joinLiterals(vals))
			}
		}
	}
	if m, ok := q["range"].(map[string]interface{}); ok {
		for f, v := range m {
			if rng, ok := v.(map[string]interface{}); ok {
				return rangeToSQL(f, rng)
			}
		}
	}
	if m, ok := q["bool"].(map[string]interface{}); ok {
		return boolToSQL(m)
	}
	return ""
}

// extractMatchValue 从 match 子句取值
// match 的两种形式:
//   {"field": "value"}
//   {"field": {"query": "value"}}
func extractMatchValue(v interface{}) interface{} {
	if m, ok := v.(map[string]interface{}); ok {
		if q, ok := m["query"]; ok {
			return q
		}
	}
	return v
}

// rangeToSQL 把 range 子句转为 SQL
// 多个条件用 AND 连接
func rangeToSQL(field string, rng map[string]interface{}) string {
	var parts []string
	for op, v := range rng {
		var sqlOp string
		switch op {
		case "gt":
			sqlOp = ">"
		case "gte":
			sqlOp = ">="
		case "lt":
			sqlOp = "<"
		case "lte":
			sqlOp = "<="
		default:
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %s %s", quoteIdent(field), sqlOp, quoteLiteral(v)))
	}
	return strings.Join(parts, " AND ")
}

// boolToSQL 把 bool query 转为 SQL
func boolToSQL(b map[string]interface{}) string {
	var parts []string
	if must, ok := b["filter"].([]interface{}); ok {
		for _, c := range must {
			if m, ok := c.(map[string]interface{}); ok {
				if s := queryToWhere(m); s != "" {
					parts = append(parts, s)
				}
			}
		}
	}
	if must, ok := b["must"].([]interface{}); ok {
		for _, c := range must {
			if m, ok := c.(map[string]interface{}); ok {
				if s := queryToWhere(m); s != "" {
					parts = append(parts, s)
				}
			}
		}
	}
	if should, ok := b["should"].([]interface{}); ok && len(should) > 0 {
		var shouldParts []string
		for _, c := range should {
			if m, ok := c.(map[string]interface{}); ok {
				if s := queryToWhere(m); s != "" {
					shouldParts = append(shouldParts, "("+s+")")
				}
			}
		}
		if len(shouldParts) > 0 {
			parts = append(parts, strings.Join(shouldParts, " OR "))
		}
	}
	if mustNot, ok := b["must_not"].([]interface{}); ok {
		for _, c := range mustNot {
			if m, ok := c.(map[string]interface{}); ok {
				if s := queryToWhere(m); s != "" {
					parts = append(parts, "NOT ("+s+")")
				}
			}
		}
	}
	return strings.Join(parts, " AND ")
}

func quoteIdent(s string) string {
	// 含特殊字符才加引号
	if strings.ContainsAny(s, " -@") {
		return `"` + s + `"`
	}
	return s
}

func quoteLiteral(v interface{}) string {
	switch x := v.(type) {
	case string:
		return "'" + strings.ReplaceAll(x, "'", "''") + "'"
	case int, int64, int32, float64, float32, bool:
		return fmt.Sprintf("%v", v)
	case json.Number:
		return x.String()
	default:
		return fmt.Sprintf("%v", v)
	}
}

func joinLiterals(vals []interface{}) string {
	parts := make([]string, 0, len(vals))
	for _, v := range vals {
		parts = append(parts, quoteLiteral(v))
	}
	return strings.Join(parts, ", ")
}

// ---------------------------------------------------------------------------
// ParseError 便利类型
// ---------------------------------------------------------------------------

// ParseError 表示 SQL 解析错误
type ParseError struct {
	Msg string
	Pos int
}

func (e *ParseError) Error() string { return e.Msg }
