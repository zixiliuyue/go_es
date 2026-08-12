// Package sqldsl — 单元测试
package sqldsl

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSQLToDSL_BasicSelect 基础 SELECT
func TestSQLToDSL_BasicSelect(t *testing.T) {
	body, err := SQLToDSL("SELECT * FROM articles")
	if err != nil {
		t.Fatal(err)
	}
	if body["query"] == nil {
		t.Fatal("expect query")
	}
	q, _ := body["query"].(map[string]interface{})
	if _, ok := q["match_all"]; !ok {
		t.Fatalf("expect match_all, got %v", q)
	}
}

// TestSQLToDSL_WhereEqual 等值 WHERE
func TestSQLToDSL_WhereEqual(t *testing.T) {
	body, err := SQLToDSL("SELECT * FROM articles WHERE cat = 'news'")
	if err != nil {
		t.Fatal(err)
	}
	q := body["query"].(map[string]interface{})
	term, ok := q["term"].(map[string]interface{})
	if !ok {
		t.Fatalf("expect term, got %v", q)
	}
	if term["cat"] != "news" {
		t.Fatalf("expect cat=news, got %v", term["cat"])
	}
}

// TestSQLToDSL_WhereRange 范围 WHERE
func TestSQLToDSL_WhereRange(t *testing.T) {
	cases := []struct {
		sql string
		op  string
	}{
		{"SELECT * FROM t WHERE price > 10", "gt"},
		{"SELECT * FROM t WHERE price >= 10", "gte"},
		{"SELECT * FROM t WHERE price < 10", "lt"},
		{"SELECT * FROM t WHERE price <= 10", "lte"},
	}
	for _, c := range cases {
		body, err := SQLToDSL(c.sql)
		if err != nil {
			t.Fatalf("%s: %v", c.sql, err)
		}
		q := body["query"].(map[string]interface{})
		rng, ok := q["range"].(map[string]interface{})
		if !ok {
			t.Fatalf("%s: expect range, got %v", c.sql, q)
		}
		priceRng := rng["price"].(map[string]interface{})
		if _, ok := priceRng[c.op]; !ok {
			t.Fatalf("%s: expect %s, got %v", c.sql, c.op, priceRng)
		}
	}
}

// TestSQLToDSL_WhereAnd OR/AND
func TestSQLToDSL_WhereAnd(t *testing.T) {
	body, err := SQLToDSL("SELECT * FROM t WHERE a = 1 AND b = 2")
	if err != nil {
		t.Fatal(err)
	}
	q := body["query"].(map[string]interface{})
	bq, ok := q["bool"].(map[string]interface{})
	if !ok {
		t.Fatalf("expect bool, got %v", q)
	}
	filter, ok := bq["filter"].([]interface{})
	if !ok || len(filter) != 2 {
		t.Fatalf("expect 2 filter clauses, got %v", bq)
	}
}

// TestSQLToDSL_WhereOr OR
func TestSQLToDSL_WhereOr(t *testing.T) {
	body, err := SQLToDSL("SELECT * FROM t WHERE a = 1 OR b = 2")
	if err != nil {
		t.Fatal(err)
	}
	q := body["query"].(map[string]interface{})
	bq, ok := q["bool"].(map[string]interface{})
	if !ok {
		t.Fatalf("expect bool, got %v", q)
	}
	should, ok := bq["should"].([]interface{})
	if !ok || len(should) != 2 {
		t.Fatalf("expect 2 should clauses, got %v", bq)
	}
}

// TestSQLToDSL_In IN 操作
func TestSQLToDSL_In(t *testing.T) {
	body, err := SQLToDSL("SELECT * FROM t WHERE cat IN ('a', 'b', 'c')")
	if err != nil {
		t.Fatal(err)
	}
	q := body["query"].(map[string]interface{})
	terms, ok := q["terms"].(map[string]interface{})
	if !ok {
		t.Fatalf("expect terms, got %v", q)
	}
	vals, _ := terms["cat"].([]interface{})
	if len(vals) != 3 {
		t.Fatalf("expect 3 values, got %v", vals)
	}
}

// TestSQLToDSL_Like LIKE 前缀
func TestSQLToDSL_Like(t *testing.T) {
	body, err := SQLToDSL("SELECT * FROM t WHERE name LIKE 'foo%'")
	if err != nil {
		t.Fatal(err)
	}
	q := body["query"].(map[string]interface{})
	mpp, ok := q["match_phrase_prefix"].(map[string]interface{})
	if !ok {
		t.Fatalf("expect match_phrase_prefix, got %v", q)
	}
	if mpp["name"] != "foo" {
		t.Fatalf("expect name=foo, got %v", mpp["name"])
	}
}

// TestSQLToDSL_NotEqual 不等于
func TestSQLToDSL_NotEqual(t *testing.T) {
	body, err := SQLToDSL("SELECT * FROM t WHERE x != 5")
	if err != nil {
		t.Fatal(err)
	}
	q := body["query"].(map[string]interface{})
	bq, ok := q["bool"].(map[string]interface{})
	if !ok {
		t.Fatalf("expect bool must_not, got %v", q)
	}
	mn, ok := bq["must_not"].([]interface{})
	if !ok || len(mn) != 1 {
		t.Fatalf("expect 1 must_not, got %v", bq)
	}
}

// TestSQLToDSL_Limit LIMIT / OFFSET
func TestSQLToDSL_Limit(t *testing.T) {
	body, err := SQLToDSL("SELECT * FROM t LIMIT 10 OFFSET 5")
	if err != nil {
		t.Fatal(err)
	}
	if body["size"] != 10 {
		t.Fatalf("expect size=10, got %v", body["size"])
	}
	if body["from"] != 5 {
		t.Fatalf("expect from=5, got %v", body["from"])
	}
}

// TestSQLToDSL_OrderBy ORDER BY
func TestSQLToDSL_OrderBy(t *testing.T) {
	body, err := SQLToDSL("SELECT * FROM t ORDER BY price DESC, name ASC")
	if err != nil {
		t.Fatal(err)
	}
	sort, ok := body["sort"].([]interface{})
	if !ok || len(sort) != 2 {
		t.Fatalf("expect 2 sort clauses, got %v", body["sort"])
	}
	first, _ := sort[0].(map[string]interface{})
	if first["price"] != "desc" {
		t.Fatalf("expect price desc, got %v", first)
	}
}

// TestSQLToDSL_FieldList 字段列表
func TestSQLToDSL_FieldList(t *testing.T) {
	body, err := SQLToDSL("SELECT title, cat FROM t")
	if err != nil {
		t.Fatal(err)
	}
	src, ok := body["_source"].([]string)
	if !ok || len(src) != 2 {
		t.Fatalf("expect 2 _source fields, got %v", body["_source"])
	}
}

// TestSQLToDSL_Count COUNT(*)
func TestSQLToDSL_Count(t *testing.T) {
	body, err := SQLToDSL("SELECT COUNT(*) FROM t")
	if err != nil {
		t.Fatal(err)
	}
	if body["size"] != 0 {
		t.Fatalf("expect size=0 for COUNT, got %v", body["size"])
	}
	if body["aggs"] == nil {
		t.Fatal("expect aggs for COUNT")
	}
}

// TestSQLToDSL_ParseError 解析错误
func TestSQLToDSL_ParseError(t *testing.T) {
	bad := []string{
		"SELECT FROM t",            // 缺字段
		"SELECT * FROM",            // 缺表名
		"SELECT * FROM t WHERE",    // 缺表达式
		"SELECT * FROM t WHERE a =", // 缺值
		"SELECT * FROM t LIMIT",    // 缺数字
		"NOT A SQL",
	}
	for _, sql := range bad {
		if _, err := SQLToDSL(sql); err == nil {
			t.Fatalf("expect error for %q", sql)
		}
	}
}

// TestSQLToDSL_QuotedIdent 双引号包裹字段
func TestSQLToDSL_QuotedIdent(t *testing.T) {
	body, err := SQLToDSL(`SELECT * FROM "my-index" WHERE "user-name" = 'alice'`)
	if err != nil {
		t.Fatal(err)
	}
	if body["query"] == nil {
		t.Fatal("expect query")
	}
}

// TestSQLToDSL_ParenGrouping 括号分组
func TestSQLToDSL_ParenGrouping(t *testing.T) {
	body, err := SQLToDSL("SELECT * FROM t WHERE (a = 1 OR b = 2) AND c = 3")
	if err != nil {
		t.Fatal(err)
	}
	q := body["query"].(map[string]interface{})
	bq, ok := q["bool"].(map[string]interface{})
	if !ok {
		t.Fatalf("expect bool (AND), got %v", q)
	}
	filter, _ := bq["filter"].([]interface{})
	if len(filter) != 2 {
		t.Fatalf("expect 2 filter clauses, got %v", filter)
	}
}

// ---------------------------------------------------------------------------
// DSLToSQL 反向转换
// ---------------------------------------------------------------------------

// TestDSLToSQL_MatchAll match_all -> 无 WHERE
func TestDSLToSQL_MatchAll(t *testing.T) {
	body := map[string]interface{}{
		"query": map[string]interface{}{"match_all": map[string]interface{}{}},
	}
	sql := DSLToSQL(body, "articles")
	if sql != "SELECT * FROM articles" {
		t.Fatalf("got %q", sql)
	}
}

// TestDSLToSQL_Term term -> WHERE field = 'value'
func TestDSLToSQL_Term(t *testing.T) {
	body := map[string]interface{}{
		"query": map[string]interface{}{
			"term": map[string]interface{}{"cat": "news"},
		},
	}
	sql := DSLToSQL(body, "articles")
	want := "SELECT * FROM articles WHERE cat = 'news'"
	if sql != want {
		t.Fatalf("got %q want %q", sql, want)
	}
}

// TestDSLToSQL_Range range -> WHERE field > n
func TestDSLToSQL_Range(t *testing.T) {
	body := map[string]interface{}{
		"query": map[string]interface{}{
			"range": map[string]interface{}{
				"price": map[string]interface{}{"gte": 10, "lt": 100},
			},
		},
	}
	sql := DSLToSQL(body, "items")
	// 多条件用 AND 连接
	if sql == "" {
		t.Fatal("empty sql")
	}
	if !contains(sql, ">=") || !contains(sql, "<") {
		t.Fatalf("missing range ops: %q", sql)
	}
	if !contains(sql, "AND") {
		t.Fatalf("missing AND: %q", sql)
	}
}

// TestDSLToSQL_BoolAnd bool.filter -> AND
func TestDSLToSQL_BoolAnd(t *testing.T) {
	body := map[string]interface{}{
		"query": map[string]interface{}{
			"bool": map[string]interface{}{
				"filter": []interface{}{
					map[string]interface{}{"term": map[string]interface{}{"a": 1}},
					map[string]interface{}{"term": map[string]interface{}{"b": 2}},
				},
			},
		},
	}
	sql := DSLToSQL(body, "t")
	if !contains(sql, "a = 1") || !contains(sql, "b = 2") {
		t.Fatalf("expect a=1 AND b=2: %q", sql)
	}
	if !contains(sql, "AND") {
		t.Fatalf("missing AND: %q", sql)
	}
}

// TestDSLToSQL_BoolShould bool.should -> OR
func TestDSLToSQL_BoolShould(t *testing.T) {
	body := map[string]interface{}{
		"query": map[string]interface{}{
			"bool": map[string]interface{}{
				"should": []interface{}{
					map[string]interface{}{"term": map[string]interface{}{"a": 1}},
					map[string]interface{}{"term": map[string]interface{}{"b": 2}},
				},
			},
		},
	}
	sql := DSLToSQL(body, "t")
	if !contains(sql, "OR") {
		t.Fatalf("expect OR: %q", sql)
	}
}

// TestDSLToSQL_LimitOrder LIMIT + ORDER BY
func TestDSLToSQL_LimitOrder(t *testing.T) {
	body := map[string]interface{}{
		"query": map[string]interface{}{"match_all": map[string]interface{}{}},
		"size":  10,
		"from":  5,
		"sort": []interface{}{
			map[string]interface{}{"price": "desc"},
		},
	}
	sql := DSLToSQL(body, "t")
	if !contains(sql, "ORDER BY") {
		t.Fatalf("expect ORDER BY: %q", sql)
	}
	if !contains(sql, "DESC") {
		t.Fatalf("expect DESC: %q", sql)
	}
	if !contains(sql, "LIMIT 10") {
		t.Fatalf("expect LIMIT 10: %q", sql)
	}
	if !contains(sql, "OFFSET 5") {
		t.Fatalf("expect OFFSET 5: %q", sql)
	}
}

// TestDSLToSQL_Terms terms -> IN
func TestDSLToSQL_Terms(t *testing.T) {
	body := map[string]interface{}{
		"query": map[string]interface{}{
			"terms": map[string]interface{}{
				"cat": []interface{}{"a", "b", "c"},
			},
		},
	}
	sql := DSLToSQL(body, "t")
	if !contains(sql, "IN (") {
		t.Fatalf("expect IN: %q", sql)
	}
}

// ---------------------------------------------------------------------------
// Round-trip
// ---------------------------------------------------------------------------

// TestRoundTrip SQL -> DSL -> SQL round-trip
func TestRoundTrip(t *testing.T) {
	cases := []string{
		"SELECT * FROM articles",
		"SELECT * FROM articles WHERE cat = 'news'",
		"SELECT * FROM articles WHERE price > 10",
		"SELECT * FROM articles LIMIT 10",
		"SELECT * FROM articles LIMIT 10 OFFSET 5",
	}
	for _, sql := range cases {
		body, err := SQLToDSL(sql)
		if err != nil {
			t.Fatalf("%q: %v", sql, err)
		}
		// 序列化为标准 map 再转回(走 json round-trip 保证类型稳定)
		raw, _ := json.Marshal(body)
		var body2 map[string]interface{}
		_ = json.Unmarshal(raw, &body2)
		out := DSLToSQL(body2, "articles")
		if out == "" {
			t.Fatalf("%q: empty round-trip", sql)
		}
		// 不能要求完全一致, 但至少 index + 关键词应保留
		if !contains(out, "FROM articles") {
			t.Fatalf("%q -> %q: missing FROM articles", sql, out)
		}
	}
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}

func containsStr(s, sub string) bool {
	return strings.Contains(s, sub)
}
