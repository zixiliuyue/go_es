// Package search 包的单元测试
// 测试搜索查询构建功能
package search

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/stretchr/testify/assert"
	"github.com/zixiliuyue/go_es/pkg/client"
	"github.com/zixiliuyue/go_es/pkg/document"
	"github.com/zixiliuyue/go_es/pkg/index"
	"go.uber.org/zap"
)

func setupSearchTest(t *testing.T) (*Searcher, context.Context, string) {
	logger, _ := zap.NewDevelopment()

	cfg := client.Config{
		Addresses: []string{"http://localhost:9200"},
		Logger:    logger,
	}

	c, err := client.NewClient(cfg)
	if err != nil {
		t.Logf("Cannot connect to Elasticsearch: %v", err)
		t.Skip("Skipping test because Elasticsearch is not available")
	}

	// 创建测试索引
	indexManager := index.NewManager(c.GetES(), logger)
	indexName := "test_search"
	_ = indexManager.DeleteIndex(context.Background(), indexName)

	mapping := map[string]interface{}{
		"mappings": map[string]interface{}{
			"properties": map[string]interface{}{
				"title":    map[string]string{"type": "text"},
				"content":  map[string]string{"type": "text"},
				"category": map[string]string{"type": "keyword"},
				"price":    map[string]string{"type": "double"},
			},
		},
	}
	err = indexManager.CreateIndex(context.Background(), indexName, mapping)
	assert.NoError(t, err)

	// 插入测试数据
	docManager := document.NewManager(c.GetES(), logger)
	docs := []map[string]interface{}{
		{"title": "Apple iPhone", "content": "Apple iPhone 15 Pro Max", "category": "electronics", "price": 999.99},
		{"title": "Apple MacBook", "content": "Apple MacBook Pro laptop", "category": "electronics", "price": 1999.99},
		{"title": "Xiaomi Phone", "content": "Xiaomi 14 Pro smartphone", "category": "electronics", "price": 699.99},
		{"title": "T-Shirt", "content": "Cotton T-Shirt", "category": "clothing", "price": 19.99},
		{"title": "Blue Jeans", "content": "Denim Blue Jeans", "category": "clothing", "price": 49.99},
	}

	for i, doc := range docs {
		_, err := docManager.IndexWithID(context.Background(), indexName, fmt.Sprintf("%d", i+1), doc)
		assert.NoError(t, err)
	}

	// 刷新索引
	_, err = c.GetES().Indices.Refresh(c.GetES().Indices.Refresh.WithIndex(indexName))
	assert.NoError(t, err)

	searcher := NewSearcher(c.GetES(), logger)
	return searcher, context.Background(), indexName
}

func TestNewSearch(t *testing.T) {
	builder := NewSearch()
	assert.NotNil(t, builder)
}

func TestBuilder_Match(t *testing.T) {
	builder := NewSearch().Match("title", "apple")
	assert.NotNil(t, builder)

	bytes, err := builder.Build()
	assert.NoError(t, err)
	assert.Contains(t, string(bytes), `"match":`)
	assert.Contains(t, string(bytes), `"title":`)
	assert.Contains(t, string(bytes), `"apple"`)
}

func TestBuilder_MatchPhrase(t *testing.T) {
	builder := NewSearch().MatchPhrase("title", "apple iphone")
	assert.NotNil(t, builder)

	bytes, err := builder.Build()
	assert.NoError(t, err)
	assert.Contains(t, string(bytes), `"match_phrase":`)
	assert.Contains(t, string(bytes), `"title":`)
}

func TestBuilder_Term(t *testing.T) {
	builder := NewSearch().Term("category", "electronics")
	assert.NotNil(t, builder)

	bytes, err := builder.Build()
	assert.NoError(t, err)
	assert.Contains(t, string(bytes), `"term":`)
	assert.Contains(t, string(bytes), `"category":`)
	assert.Contains(t, string(bytes), `"electronics"`)
}

func TestBuilder_Terms(t *testing.T) {
	values := []interface{}{"electronics", "clothing"}
	builder := NewSearch().Terms("category", values)
	assert.NotNil(t, builder)

	bytes, err := builder.Build()
	assert.NoError(t, err)
	assert.Contains(t, string(bytes), `"terms":`)
	assert.Contains(t, string(bytes), `"category":`)
}

func TestBuilder_Range(t *testing.T) {
	builder := NewSearch().Range("price", 10.0, 100.0)
	assert.NotNil(t, builder)

	bytes, err := builder.Build()
	assert.NoError(t, err)
	assert.Contains(t, string(bytes), `"range":`)
	assert.Contains(t, string(bytes), `"gte":10`)
	assert.Contains(t, string(bytes), `"lte":100`)
}

func TestBuilder_Bool(t *testing.T) {
	must := []map[string]interface{}{
		MatchQuery("title", "apple"),
	}
	filter := []map[string]interface{}{
		TermQuery("category", "electronics"),
	}

	builder := NewSearch().Bool(must, nil, nil, filter)
	assert.NotNil(t, builder)

	bytes, err := builder.Build()
	assert.NoError(t, err)
	assert.Contains(t, string(bytes), `"bool":`)
	assert.Contains(t, string(bytes), `"must":`)
	assert.Contains(t, string(bytes), `"filter":`)
}

func TestBuilder_Pagination(t *testing.T) {
	builder := NewSearch().Match("title", "test").Pagination(10, 20)
	assert.NotNil(t, builder)

	bytes, err := builder.Build()
	assert.NoError(t, err)

	var req SearchRequest
	err = json.Unmarshal(bytes, &req)
	assert.NoError(t, err)
	assert.Equal(t, 10, req.From)
	assert.Equal(t, 20, req.Size)
}

func TestBuilder_Sort(t *testing.T) {
	builder := NewSearch().Match("title", "test").Sort("price", "desc")
	assert.NotNil(t, builder)

	bytes, err := builder.Build()
	assert.NoError(t, err)

	var req SearchRequest
	err = json.Unmarshal(bytes, &req)
	assert.NoError(t, err)
	assert.Len(t, req.Sort, 1)
	assert.Equal(t, "desc", req.Sort[0]["price"])
}

func TestBuilder_SourceFilter(t *testing.T) {
	includes := []string{"title", "price"}
	excludes := []string{"content"}
	builder := NewSearch().Match("title", "test").SourceFilter(includes, excludes)
	assert.NotNil(t, builder)

	bytes, err := builder.Build()
	assert.NoError(t, err)
	assert.Contains(t, string(bytes), `"includes":["title","price"]`)
	assert.Contains(t, string(bytes), `"excludes":["content"]`)
}

func TestBuilder_Highlight(t *testing.T) {
	fields := map[string]interface{}{
		"title":   map[string]interface{}{},
		"content": map[string]interface{}{},
	}
	preTags := []string{"<em>"}
	postTags := []string{"</em>"}

	builder := NewSearch().Match("title", "test").Highlight(fields, preTags, postTags)
	assert.NotNil(t, builder)

	bytes, err := builder.Build()
	assert.NoError(t, err)
	assert.Contains(t, string(bytes), `"highlight":`)
	assert.Contains(t, string(bytes), `"pre_tags":["<em>"]`)
}

func TestSearcher_Search_Match(t *testing.T) {
	searcher, ctx, indexName := setupSearchTest(t)
	defer func() {
		// Cleanup handled in next test setup
	}()

	builder := NewSearch().
		Match("title", "Apple").
		Pagination(0, 10).
		Sort("price", "asc")

	resp, err := searcher.Search(ctx, indexName, builder)
	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.GreaterOrEqual(t, resp.Hits.Total.Value, 2)
}

func TestSearcher_Search_Term(t *testing.T) {
	searcher, ctx, indexName := setupSearchTest(t)
	defer func() {}()

	builder := NewSearch().
		Term("category", "electronics").
		Pagination(0, 10)

	resp, err := searcher.Search(ctx, indexName, builder)
	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, 3, resp.Hits.Total.Value)
}

func TestSearcher_Search_Range(t *testing.T) {
	searcher, ctx, indexName := setupSearchTest(t)
	defer func() {}()

	builder := NewSearch().
		Range("price", 0.0, 50.0).
		Pagination(0, 10)

	resp, err := searcher.Search(ctx, indexName, builder)
	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, 2, resp.Hits.Total.Value) // 19.99 and 49.99
}

func TestSearcher_Search_Bool(t *testing.T) {
	searcher, ctx, indexName := setupSearchTest(t)
	defer func() {}()

	builder := NewSearch().
		Bool(
			[]map[string]interface{}{MatchQuery("title", "Apple")},
			nil,
			nil,
			[]map[string]interface{}{TermQuery("category", "electronics")},
		).
		Pagination(0, 10)

	resp, err := searcher.Search(ctx, indexName, builder)
	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, 2, resp.Hits.Total.Value)
}

func TestSearcher_Count(t *testing.T) {
	searcher, ctx, indexName := setupSearchTest(t)
	defer func() {}()

	query := TermQuery("category", "electronics")
	count, err := searcher.Count(ctx, indexName, map[string]interface{}{"bool": map[string]interface{}{"filter": []map[string]interface{}{query}}})
	assert.NoError(t, err)
	assert.Equal(t, int64(3), count)
}

func TestSearcher_Count_All(t *testing.T) {
	searcher, ctx, indexName := setupSearchTest(t)
	defer func() {}()

	count, err := searcher.Count(ctx, indexName, nil)
	assert.NoError(t, err)
	assert.Equal(t, int64(5), count)
}

func TestHelperFunctions(t *testing.T) {
	// Test MatchAll
	ma := MatchAll()
	assert.NotNil(t, ma)
	_, ok := ma["match_all"]
	assert.True(t, ok)

	// Test MatchQuery
	mq := MatchQuery("title", "test")
	assert.NotNil(t, mq)
	_, ok = mq["match"]
	assert.True(t, ok)

	// Test TermQuery
	tq := TermQuery("category", "test")
	assert.NotNil(t, tq)
	_, ok = tq["term"]
	assert.True(t, ok)

	// Test RangeQuery
	rq := RangeQuery("price", 10, 100)
	assert.NotNil(t, rq)
	_, ok = rq["range"]
	assert.True(t, ok)
}

func TestNewSearcher(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	es, _ := elasticsearch.NewClient(elasticsearch.Config{
		Addresses: []string{"http://localhost:9200"},
	})

	searcher := NewSearcher(es, logger)
	assert.NotNil(t, searcher)
}
