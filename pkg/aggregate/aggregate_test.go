// Package aggregate 包的单元测试
// 测试聚合分析构建功能
package aggregate

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/zixiliuyue/go_es/pkg/client"
	"github.com/zixiliuyue/go_es/pkg/document"
	"github.com/zixiliuyue/go_es/pkg/index"
	"github.com/zixiliuyue/go_es/pkg/search"
	"go.uber.org/zap"
)

func setupAggregateTest(t *testing.T) (*search.Searcher, context.Context, string) {
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
	indexName := "test_aggregate"
	_ = indexManager.DeleteIndex(context.Background(), indexName)

	mapping := map[string]interface{}{
		"mappings": map[string]interface{}{
			"properties": map[string]interface{}{
				"category": map[string]string{"type": "keyword"},
				"price":    map[string]string{"type": "double"},
				"quantity": map[string]string{"type": "integer"},
			},
		},
	}
	err = indexManager.CreateIndex(context.Background(), indexName, mapping)
	assert.NoError(t, err)

	// 插入测试数据
	docManager := document.NewManager(c.GetES(), logger)
	docs := []map[string]interface{}{
		{"category": "electronics", "price": 100.0, "quantity": 5},
		{"category": "electronics", "price": 200.0, "quantity": 3},
		{"category": "electronics", "price": 300.0, "quantity": 2},
		{"category": "clothing", "price": 50.0, "quantity": 10},
		{"category": "clothing", "price": 80.0, "quantity": 8},
		{"category": "books", "price": 30.0, "quantity": 20},
	}

	for i, doc := range docs {
		_, err := docManager.IndexWithID(context.Background(), indexName, fmt.Sprintf("%d", i+1), doc)
		assert.NoError(t, err)
	}

	// 刷新索引
	_, err = c.GetES().Indices.Refresh(c.GetES().Indices.Refresh.WithIndex(indexName))
	assert.NoError(t, err)

	searcher := search.NewSearcher(c.GetES(), logger)
	return searcher, context.Background(), indexName
}

func TestNewTermsAggregation(t *testing.T) {
	agg := NewTermsAggregation("group_by_category", "category")
	assert.NotNil(t, agg)
	assert.Equal(t, "group_by_category", agg.Name())
}

func TestTermsAggregation_Build(t *testing.T) {
	agg := NewTermsAggregation("group_by_category", "category").
		SetSize(20).
		SetOrder("_count", "desc")

	result, err := agg.Build()
	assert.NoError(t, err)
	assert.NotNil(t, result)

	terms, ok := result["terms"]
	assert.True(t, ok)

	termsMap, ok := terms.(map[string]interface{})
	assert.True(t, ok)
	assert.Equal(t, "category", termsMap["field"])
	assert.Equal(t, 20, termsMap["size"])
}

func TestNewHistogramAggregation(t *testing.T) {
	agg := NewHistogramAggregation("price_histogram", "price", 50.0)
	assert.NotNil(t, agg)

	result, err := agg.Build()
	assert.NoError(t, err)
	assert.NotNil(t, result)

	histogram, ok := result["histogram"]
	assert.True(t, ok)

	histogramMap, ok := histogram.(map[string]interface{})
	assert.True(t, ok)
	assert.Equal(t, "price", histogramMap["field"])
	assert.Equal(t, 50.0, histogramMap["interval"])
}

func TestNewDateHistogramAggregation(t *testing.T) {
	agg := NewDateHistogramAggregation("group_by_month", "publish_date", "1M")
	assert.NotNil(t, agg)

	result, err := agg.Build()
	assert.NoError(t, err)
	assert.NotNil(t, result)

	dateHist, ok := result["date_histogram"]
	assert.True(t, ok)

	dateHistMap, ok := dateHist.(map[string]interface{})
	assert.True(t, ok)
	assert.Equal(t, "publish_date", dateHistMap["field"])
	assert.Equal(t, "1M", dateHistMap["calendar_interval"])
}

func TestNewRangeAggregation(t *testing.T) {
	agg := NewRangeAggregation("price_ranges", "price").
		AddRange(nil, 50.0).
		AddRange(50.0, 100.0).
		AddRange(100.0, nil)

	assert.NotNil(t, agg)
	assert.Equal(t, 3, len(agg.ranges))

	result, err := agg.Build()
	assert.NoError(t, err)
	assert.NotNil(t, result)

	rangeAgg, ok := result["range"]
	assert.True(t, ok)

	rangeAggMap, ok := rangeAgg.(map[string]interface{})
	assert.True(t, ok)
	assert.Equal(t, "price", rangeAggMap["field"])
}

func TestNewAvgAggregation(t *testing.T) {
	agg := NewAvgAggregation("avg_price", "price")
	assert.NotNil(t, agg)

	result, err := agg.Build()
	assert.NoError(t, err)
	assert.NotNil(t, result)

	avg, ok := result["avg"]
	assert.True(t, ok)

	avgMap, ok := avg.(map[string]interface{})
	assert.True(t, ok)
	assert.Equal(t, "price", avgMap["field"])
}

func TestNewSumAggregation(t *testing.T) {
	agg := NewSumAggregation("total_quantity", "quantity")
	assert.NotNil(t, agg)

	result, err := agg.Build()
	assert.NoError(t, err)
	assert.NotNil(t, result)

	sum, ok := result["sum"]
	assert.True(t, ok)

	sumMap, ok := sum.(map[string]interface{})
	assert.True(t, ok)
	assert.Equal(t, "quantity", sumMap["field"])
}

func TestNewStatsAggregation(t *testing.T) {
	agg := NewStatsAggregation("stats_price", "price")
	assert.NotNil(t, agg)

	result, err := agg.Build()
	assert.NoError(t, err)
	assert.NotNil(t, result)

	stats, ok := result["stats"]
	assert.True(t, ok)

	statsMap, ok := stats.(map[string]interface{})
	assert.True(t, ok)
	assert.Equal(t, "price", statsMap["field"])
}

func TestNewCardinalityAggregation(t *testing.T) {
	agg := NewCardinalityAggregation("unique_categories", "category")
	assert.NotNil(t, agg)

	result, err := agg.Build()
	assert.NoError(t, err)
	assert.NotNil(t, result)

	card, ok := result["cardinality"]
	assert.True(t, ok)

	cardMap, ok := card.(map[string]interface{})
	assert.True(t, ok)
	assert.Equal(t, "category", cardMap["field"])
}

func TestNestedAggregation_Build(t *testing.T) {
	// 创建父聚合（Terms按category分组）
	termsAgg := NewTermsAggregation("group_by_category", "category")

	// 创建子聚合（平均价格）
	avgPriceAgg := NewAvgAggregation("avg_price", "price")

	// 创建嵌套聚合
	nested := NewNestedAggregation(termsAgg, []Aggregation{avgPriceAgg})

	result, err := nested.Build()
	assert.NoError(t, err)
	assert.NotNil(t, result)

	// 结果结构应该是 {"terms": ..., "aggs": {...}}
	terms, ok := result["terms"]
	assert.True(t, ok)

	termsMap, ok := terms.(map[string]interface{})
	assert.True(t, ok)

	// 应该包含 aggs 字段
	aggs, ok := termsMap["aggs"]
	assert.True(t, ok)
	assert.NotNil(t, aggs)
}

func TestTermsAggregation_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	searcher, ctx, indexName := setupAggregateTest(t)
	defer func() {}()

	// 构建terms聚合 - 按category分组
	builder := search.NewSearch().Pagination(0, 0)

	agg := NewTermsAggregation("group_by_category", "category")
	aggMap, err := agg.Build()
	assert.NoError(t, err)

	for name, aggDef := range aggMap {
		// agg.Build() 返回 map[string]interface{}, 但 range 出来的 value 是 interface{}
		// AddAggregation 需要 map[string]interface{}, 需要显式断言
		m, ok := aggDef.(map[string]interface{})
		assert.True(t, ok, "聚合定义必须是 map[string]interface{}, got %T", aggDef)
		builder.AddAggregation(name, m)
	}

	resp, err := searcher.Search(ctx, indexName, builder)
	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.NotEmpty(t, resp.Aggregations)

	// 解析结果
	termsResult, err := ParseTermsResult(resp.Aggregations, "group_by_category")
	assert.NoError(t, err)
	assert.NotNil(t, termsResult)
	assert.Equal(t, 3, len(termsResult.Buckets)) // 3 categories
}

func TestStatsAggregation_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	searcher, ctx, indexName := setupAggregateTest(t)
	defer func() {}()

	// 构建stats聚合
	builder := search.NewSearch().Pagination(0, 0)

	agg := NewStatsAggregation("stats_price", "price")
	aggMap, err := agg.Build()
	assert.NoError(t, err)

	for name, aggDef := range aggMap {
		m, ok := aggDef.(map[string]interface{})
		assert.True(t, ok, "聚合定义必须是 map[string]interface{}, got %T", aggDef)
		builder.AddAggregation(name, m)
	}

	resp, err := searcher.Search(ctx, indexName, builder)
	assert.NoError(t, err)

	// 解析结果
	statsResult, err := ParseStatsResult(resp.Aggregations, "stats_price")
	assert.NoError(t, err)
	assert.NotNil(t, statsResult)
	assert.Equal(t, 6, statsResult.Count)
	// 最小值应该是30
	assert.InDelta(t, 30.0, statsResult.Min, 0.001)
	// 最大值应该是300
	assert.InDelta(t, 300.0, statsResult.Max, 0.001)
	// 总和: 100+200+300+50+80+30 = 760
	assert.InDelta(t, 760.0, statsResult.Sum, 0.001)
	// 平均值 760/6 ≈ 126.666667
	assert.InDelta(t, 126.666667, statsResult.Avg, 0.1)
}

func TestNestedAggregation_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	searcher, ctx, indexName := setupAggregateTest(t)
	defer func() {}()

	// 按category分组，然后统计每个分组的平均价格和总数量
	categoryAgg := NewTermsAggregation("group_by_category", "category")
	avgPriceAgg := NewAvgAggregation("avg_price", "price")
	totalQuantityAgg := NewSumAggregation("total_quantity", "quantity")

	nestedAgg := NewNestedAggregation(categoryAgg, []Aggregation{avgPriceAgg, totalQuantityAgg})
	aggMap, err := nestedAgg.Build()
	assert.NoError(t, err)

	builder := search.NewSearch().Pagination(0, 0)
	for name, aggDef := range aggMap {
		m, ok := aggDef.(map[string]interface{})
		assert.True(t, ok, "聚合定义必须是 map[string]interface{}, got %T", aggDef)
		builder.AddAggregation(name, m)
	}

	resp, err := searcher.Search(ctx, indexName, builder)
	assert.NoError(t, err)
	assert.NotNil(t, resp)

	// 解析分组结果
	termsResult, err := ParseTermsResult(resp.Aggregations, "group_by_category")
	assert.NoError(t, err)
	assert.NotNil(t, termsResult)
	assert.Equal(t, 3, len(termsResult.Buckets)) // electronics, clothing, books
}

func TestRangeAggregation_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	searcher, ctx, indexName := setupAggregateTest(t)
	defer func() {}()

	// 价格范围聚合
	agg := NewRangeAggregation("price_ranges", "price").
		AddRange(nil, 50.0).
		AddRange(50.0, 200.0).
		AddRange(200.0, nil)

	aggMap, err := agg.Build()
	assert.NoError(t, err)

	builder := search.NewSearch().Pagination(0, 0)
	for name, aggDef := range aggMap {
		m, ok := aggDef.(map[string]interface{})
		assert.True(t, ok, "聚合定义必须是 map[string]interface{}, got %T", aggDef)
		builder.AddAggregation(name, m)
	}

	resp, err := searcher.Search(ctx, indexName, builder)
	assert.NoError(t, err)
	assert.NotNil(t, resp)
}

func TestNewMinMaxAggregation(t *testing.T) {
	minAgg := NewMinAggregation("min_price", "price")
	assert.NotNil(t, minAgg)
	result, err := minAgg.Build()
	assert.NoError(t, err)
	_, ok := result["min"]
	assert.True(t, ok)

	maxAgg := NewMaxAggregation("max_price", "price")
	assert.NotNil(t, maxAgg)
	result, err = maxAgg.Build()
	assert.NoError(t, err)
	_, ok = result["max"]
	assert.True(t, ok)
}

func TestParseTermsResult_Empty(t *testing.T) {
	// 测试空聚合解析
	raw := json.RawMessage(`{}`)
	result, err := ParseTermsResult(raw, "non_existent")
	assert.NoError(t, err)
	assert.Nil(t, result)
}

func TestParseStatsResult_Empty(t *testing.T) {
	// 测试空聚合解析
	raw := json.RawMessage(`{}`)
	result, err := ParseStatsResult(raw, "non_existent")
	assert.NoError(t, err)
	assert.Nil(t, result)
}
