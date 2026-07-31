// Package main 示例程序
// 展示如何使用go_es包进行Elasticsearch操作
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/zixiliuyue/go_es/pkg/aggregate"
	"github.com/zixiliuyue/go_es/pkg/client"
	"github.com/zixiliuyue/go_es/pkg/document"
	"github.com/zixiliuyue/go_es/pkg/index"
	"github.com/zixiliuyue/go_es/pkg/search"
	"go.uber.org/zap"
)

// Article 示例文档结构
type Article struct {
	ID        int64  `json:"id"`
	Title     string `json:"title"`
	Content   string `json:"content"`
	Author    string `json:"author"`
	Category  string `json:"category"`
	PublishAt int64  `json:"publish_at"`
	Views     int    `json:"views"`
}

func main() {
	// 初始化日志
	logger, err := zap.NewDevelopment()
	if err != nil {
		log.Fatalf("Failed to init logger: %v", err)
	}
	defer logger.Sync()

	// 1. 创建客户端
	cfg := client.Config{
		Addresses: []string{"http://localhost:9200"},
		Username:  "",
		Password:  "",
		Logger:    logger,
	}

	esClient, err := client.NewClient(cfg)
	if err != nil {
		logger.Fatal("Failed to create client", zap.Error(err))
	}
	defer esClient.Close()

	// 检查连接是否可用
	ok, err := esClient.Ping()
	if err != nil || !ok {
		logger.Fatal("Failed to ping Elasticsearch", zap.Error(err))
	}
	logger.Info("Connected to Elasticsearch successfully")

	// 2. 创建索引管理器
	indexManager := index.NewManager(esClient.GetES(), logger)
	ctx := context.Background()

	// 删除已存在的索引（示例中总是重建）
	_ = indexManager.DeleteIndex(ctx, "articles")

	// 创建索引映射
	mapping := map[string]interface{}{
		"mappings": map[string]interface{}{
			"properties": map[string]interface{}{
				"id":         map[string]string{"type": "integer"},
				"title":      map[string]string{"type": "text", "analyzer": "ik_max_word"},
				"content":    map[string]string{"type": "text", "analyzer": "ik_max_word"},
				"author":     map[string]string{"type": "keyword"},
				"category":   map[string]string{"type": "keyword"},
				"publish_at": map[string]string{"type": "date"},
				"views":      map[string]string{"type": "integer"},
			},
		},
		"settings": map[string]interface{}{
			"number_of_shards":   1,
			"number_of_replicas": 0,
		},
	}

	err = indexManager.CreateIndex(ctx, "articles", mapping)
	if err != nil {
		logger.Fatal("Failed to create index", zap.Error(err))
	}
	logger.Info("Index created: articles")

	// 3. 创建文档管理器
	docManager := document.NewManager(esClient.GetES(), logger)

	// 批量插入示例数据
	articles := []Article{
		{ID: 1, Title: "Elasticsearch入门教程", Content: "这是一篇关于Elasticsearch入门教程，介绍了基本概念和使用方法", Author: "张三", Category: "技术", PublishAt: 1672531200, Views: 1000},
		{ID: 2, Title: "Go语言并发编程", Content: "本文介绍Go语言的并发编程模型，包括goroutine和channel", Author: "李四", Category: "技术", PublishAt: 1675209600, Views: 800},
		{ID: 3, Title: "Elasticsearch聚合分析", Content: "深入探讨Elasticsearch的聚合分析功能，包括桶聚合和指标聚合", Author: "张三", Category: "技术", PublishAt: 1677888000, Views: 1500},
		{ID: 4, Title: "旅行游记", Content: "这是一篇旅行游记，分享了我在云南的美好经历", Author: "王五", Category: "生活", PublishAt: 1680566400, Views: 300},
		{ID: 5, Title: "美食推荐", Content: "推荐几家北京好吃的餐厅，让你的味蕾享受", Author: "赵六", Category: "生活", PublishAt: 1683158400, Views: 450},
	}

	bulkOps := make([]document.BulkOperation, 0, len(articles))
	for _, article := range articles {
		bulkOps = append(bulkOps, document.BulkOperation{
			Operation: "index",
			Index:     "articles",
			ID:        fmt.Sprintf("%d", article.ID),
			Doc:       article,
		})
	}

	success, failed, err := docManager.Bulk(ctx, bulkOps)
	if err != nil {
		logger.Fatal("Bulk insert failed", zap.Error(err))
	}
	logger.Info("Bulk insert completed",
		zap.Int("total", len(bulkOps)),
		zap.Int("success", success),
		zap.Int("failed", failed))

	// 4. 测试CRUD操作
	// 获取单个文档
	var article Article
	err = docManager.GetInto(ctx, "articles", "1", &article)
	if err != nil {
		logger.Fatal("Failed to get document", zap.Error(err))
	}
	logger.Info("Got document", zap.Any("article", article))

	// 更新文档
	article.Views = 1050
	_, err = docManager.Update(ctx, "articles", "1", map[string]interface{}{"views": 1050})
	if err != nil {
		logger.Fatal("Failed to update document", zap.Error(err))
	}
	logger.Info("Document updated")

	// 5. 搜索测试
	searcher := search.NewSearcher(esClient.GetES(), logger)

	// 关键词搜索
	builder := search.NewSearch().
		Match("title", "Elasticsearch").
		Pagination(0, 10).
		Sort("publish_at", "desc")

	resp, err := searcher.Search(ctx, "articles", builder)
	if err != nil {
		logger.Fatal("Search failed", zap.Error(err))
	}

	logger.Info("Search completed",
		zap.Int("total", resp.Hits.Total.Value),
		zap.Int("returned", len(resp.Hits.Hits)))

	for i, hit := range resp.Hits.Hits {
		var result Article
		_ = json.Unmarshal(hit.Source, &result)
		fmt.Printf("%d. %s by %s - views: %d\n", i+1, result.Title, result.Author, result.Views)
	}

	// 6. 布尔组合查询测试
	fmt.Println("\n=== Boolean query example ===")
	builder = search.NewSearch().
		Bool(
			[]map[string]interface{}{search.MatchQuery("content", "聚合")},
			nil,
			nil,
			[]map[string]interface{}{search.TermQuery("category", "技术")},
		).
		Pagination(0, 10)

	resp, err = searcher.Search(ctx, "articles", builder)
	if err != nil {
		logger.Fatal("Boolean search failed", zap.Error(err))
	}
	fmt.Printf("Found %d documents matching '聚合' in category '技术'\n", resp.Hits.Total.Value)

	// 7. 聚合分析测试
	fmt.Println("\n=== Aggregation example (group by category) ===")
	// 按category分组统计
	categoryAgg := aggregate.NewTermsAggregation("group_by_category", "category").
		SetSize(10).
		SetOrder("_count", "desc")

	// 在分组基础上统计平均阅读量
	avgViewsAgg := aggregate.NewAvgAggregation("avg_views", "views")
	nestedAgg := aggregate.NewNestedAggregation(categoryAgg, []aggregate.Aggregation{avgViewsAgg})

	builder = search.NewSearch().
		Pagination(0, 0) // 只需要聚合结果，不需要返回文档

	nestedMap, err := nestedAgg.Build()
	if err != nil {
		logger.Fatal("Failed to build aggregation", zap.Error(err))
	}
	for name, aggDef := range nestedMap {
		builder.AddAggregation(name, aggDef.(map[string]interface{}))
	}

	resp, err = searcher.Search(ctx, "articles", builder)
	if err != nil {
		logger.Fatal("Aggregation search failed", zap.Error(err))
	}

	// 解析聚合结果
	termsResult, err := aggregate.ParseTermsResult(resp.Aggregations, "group_by_category")
	if err != nil {
		logger.Fatal("Failed to parse aggregation result", zap.Error(err))
	}

	for _, bucket := range termsResult.Buckets {
		fmt.Printf("Category: %v, Doc count: %d\n", bucket.Key, bucket.DocCount)
	}

	// 8. 范围聚合测试
	fmt.Println("\n=== Range aggregation example (by views) ===")
	rangeAgg := aggregate.NewRangeAggregation("views_distribution", "views").
		AddRange(nil, 500).
		AddRange(500, 1000).
		AddRange(1000, nil)

	builder = search.NewSearch().Pagination(0, 0)
	rangeMap, _ := rangeAgg.Build()
	for name, aggDef := range rangeMap {
		builder.AddAggregation(name, aggDef.(map[string]interface{}))
	}

	resp, err = searcher.Search(ctx, "articles", builder)
	if err != nil {
		logger.Fatal("Range aggregation failed", zap.Error(err))
	}

	fmt.Println("Views distribution:")
	fmt.Println("  0-500, 500-1000, 1000+")

	// 统计信息聚合
	fmt.Println("\n=== Stats aggregation (views) ===")
	statsAgg := aggregate.NewStatsAggregation("stats_views", "views")
	builder = search.NewSearch().Pagination(0, 0)
	statsMap, _ := statsAgg.Build()
	for name, aggDef := range statsMap {
		builder.AddAggregation(name, aggDef.(map[string]interface{}))
	}
	resp, err = searcher.Search(ctx, "articles", builder)
	if err != nil {
		logger.Fatal("Stats aggregation failed", zap.Error(err))
	}

	statsResult, err := aggregate.ParseStatsResult(resp.Aggregations, "stats_views")
	if err != nil {
		logger.Fatal("Failed to parse stats result", zap.Error(err))
	}

	fmt.Printf("Count: %d\nMin: %.2f\nMax: %.2f\nAvg: %.2f\nSum: %.2f\n",
		statsResult.Count, statsResult.Min, statsResult.Max, statsResult.Avg, statsResult.Sum)

	fmt.Println("\n=== Demo completed successfully ===")
}
