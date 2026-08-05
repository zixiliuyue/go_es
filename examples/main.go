// Package main 示例程序
// 展示如何使用go_es包进行Elasticsearch操作
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/zixiliuyue/go_es/pkg/aggregate"
	"github.com/zixiliuyue/go_es/pkg/alias"
	"github.com/zixiliuyue/go_es/pkg/client"
	"github.com/zixiliuyue/go_es/pkg/cluster"
	"github.com/zixiliuyue/go_es/pkg/document"
	"github.com/zixiliuyue/go_es/pkg/ilm"
	"github.com/zixiliuyue/go_es/pkg/index"
	"github.com/zixiliuyue/go_es/pkg/ingest"
	"github.com/zixiliuyue/go_es/pkg/reindex"
	"github.com/zixiliuyue/go_es/pkg/search"
	"github.com/zixiliuyue/go_es/pkg/template"
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
	addr := os.Getenv("ES_ADDR")
	if addr == "" {
		addr = "http://localhost:9200"
	}
	cfg := client.Config{
		Addresses: []string{addr},
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
	// 注: ik_max_word 需要安装 IK 插件;本示例默认走 standard analyzer,
	//    你可以视需要改回 ik_max_word 并安装对应版本插件
	mapping := map[string]interface{}{
		"mappings": map[string]interface{}{
			"properties": map[string]interface{}{
				"id":         map[string]string{"type": "integer"},
				"title":      map[string]string{"type": "text"},
				"content":    map[string]string{"type": "text"},
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

	// === 以下演示新增的 6 个模块:alias / ilm / template / reindex / ingest / cluster ===
	// 它们全部复用同一个 esClient,与现有功能无缝衔接
	// 集成度要求:不影响原有功能,演示失败时只打印警告,不中断整体流程

	// 9. 集群健康与节点列表(cluster)
	fmt.Println("\n=== Cluster health & nodes ===")
	clusterManager := cluster.NewManager(esClient.GetES(), logger)
	if h, err := clusterManager.Health(ctx, "cluster"); err != nil {
		logger.Warn("cluster health failed", zap.Error(err))
	} else {
		fmt.Printf("Cluster=%s Status=%s Nodes=%d ActiveShards=%d\n",
			h.ClusterName, h.Status, h.NumberOfNodes, h.ActiveShards)
	}
	if nodes, err := clusterManager.ListNodes(ctx); err != nil {
		logger.Warn("list nodes failed", zap.Error(err))
	} else {
		for _, n := range nodes {
			fmt.Printf("Node: name=%s version=%s heap=%.0f%%\n", n.Name, n.Version, n.HeapPercent)
		}
	}

	// 10. 别名管理(alias):演示 Add / Get / Switch / Remove
	fmt.Println("\n=== Alias: add / switch / remove ===")
	aliasManager := alias.NewManager(esClient.GetES(), logger)
	aliasName := "articles_alias"
	_ = aliasManager.AddAlias(ctx, "articles", aliasName)
	if bound, err := aliasManager.GetAlias(ctx, aliasName); err != nil {
		logger.Warn("get alias failed", zap.Error(err))
	} else {
		fmt.Printf("Alias %s bound to: %v\n", aliasName, bound)
	}
	// 把别名从 articles 切换到一个新索引 articles_v2(若不存在则创建)
	_ = indexManager.DeleteIndex(ctx, "articles_v2")
	_ = indexManager.CreateIndex(ctx, "articles_v2", mapping)
	if err := aliasManager.SwitchAlias(ctx, aliasName, "articles", "articles_v2"); err != nil {
		logger.Warn("switch alias failed", zap.Error(err))
	} else {
		fmt.Printf("Alias %s switched to articles_v2\n", aliasName)
	}
	// 还原:切回 articles
	_ = aliasManager.SwitchAlias(ctx, aliasName, "articles_v2", "articles")
	_ = indexManager.DeleteIndex(ctx, "articles_v2")

	// 11. 索引模板(template):Composable + Component + Simulate
	fmt.Println("\n=== Index template: create / get / simulate ===")
	tplManager := template.NewManager(esClient.GetES(), logger)
	compName := "demo_settings_component"
	idxTplName := "demo_articles_tpl"
	_ = tplManager.DeleteIndexTemplate(ctx, idxTplName)
	_ = tplManager.DeleteComponentTemplate(ctx, compName)

	// 创建一个组件模板:统一 settings
	if err := tplManager.PutComponentTemplate(ctx, compName, template.ComponentTemplate{
		Template: map[string]interface{}{
			"settings": map[string]interface{}{"number_of_shards": 1, "number_of_replicas": 0},
		},
		Version: 1,
	}); err != nil {
		logger.Warn("put component template failed", zap.Error(err))
	}

	// 创建一个组合模板:引用上面的组件 + 自定义 mapping
	if err := tplManager.PutIndexTemplate(ctx, idxTplName, template.IndexTemplate{
		IndexPatterns: []string{"demo-*"},
		Priority:     100,
		ComposedOf:   []string{compName},
		Template: map[string]interface{}{
			"mappings": map[string]interface{}{
				"properties": map[string]interface{}{
					"title": map[string]string{"type": "text"},
				},
			},
		},
	}); err != nil {
		logger.Warn("put index template failed", zap.Error(err))
	} else {
		fmt.Printf("Index template %s ready\n", idxTplName)
	}

	// 模拟渲染一个匹配 demo-xxx 的索引将得到的最终配置
	if sim, err := tplManager.Simulate(ctx, "demo-2024", nil); err != nil {
		logger.Warn("simulate failed", zap.Error(err))
	} else {
		fmt.Printf("Simulated template has keys: %v\n", keysOf(sim))
	}

	// 12. ILM:创建一个7天->30天删除的策略
	fmt.Println("\n=== ILM: put / get / delete policy ===")
	ilmManager := ilm.NewManager(esClient.GetES(), logger)
	policyName := "demo_articles_policy"
	_ = ilmManager.DeletePolicy(ctx, policyName)
	policy := ilm.BuildTimedRolloverPolicy("1d", "7d", "30d")
	if err := ilmManager.PutPolicy(ctx, policyName, policy); err != nil {
		logger.Warn("put ilm policy failed", zap.Error(err))
	} else {
		fmt.Printf("ILM policy %s phases=%d\n", policyName, len(policy.Phases))
	}

	// 13. Ingest Pipeline:创建一个简单的 set 管道并模拟
	fmt.Println("\n=== Ingest: put / simulate pipeline ===")
	ingestManager := ingest.NewManager(esClient.GetES(), logger)
	pipeName := "demo_upper_pipe"
	_ = ingestManager.DeletePipeline(ctx, pipeName)
	if err := ingestManager.PutPipeline(ctx, pipeName, ingest.Pipeline{
		Description: "demo: set tag",
		Processors: []map[string]interface{}{
			{"set": map[string]interface{}{"field": "source", "value": "demo"}},
		},
	}); err != nil {
		logger.Warn("put pipeline failed", zap.Error(err))
	}
	if sim, err := ingestManager.Simulate(ctx, ingest.Pipeline{
		Processors: []map[string]interface{}{
			{"set": map[string]interface{}{"field": "tag", "value": "v1"}},
		},
	}, []map[string]interface{}{{"_source": map[string]interface{}{"x": 1}}}); err != nil {
		logger.Warn("simulate pipeline failed", zap.Error(err))
	} else if len(sim.Docs) > 0 {
		fmt.Printf("Simulated doc tag=%v\n", sim.Docs[0].Doc["_source"].(map[string]interface{})["tag"])
	}

	// 14. Reindex:同步模式把 articles 拷贝到 articles_reidx
	fmt.Println("\n=== Reindex: copy articles -> articles_reidx ===")
	reidxManager := reindex.NewManager(esClient.GetES(), logger)
	_ = indexManager.DeleteIndex(ctx, "articles_reidx")
	_ = indexManager.CreateIndex(ctx, "articles_reidx", mapping)
	if resp, err := reidxManager.Reindex(ctx, reindex.Request{
		Source:           reindex.Source{Index: []string{"articles"}},
		Dest:             reindex.Dest{Index: "articles_reidx", OpType: "index"},
		WaitForCompletion: true,
		Refresh:          true,
	}); err != nil {
		logger.Warn("reindex failed", zap.Error(err))
	} else {
		fmt.Printf("Reindex took=%dms total=%d created=%d\n", resp.Took, resp.Total, resp.Created)
	}
	_ = indexManager.DeleteIndex(ctx, "articles_reidx")

	fmt.Println("\n=== Demo completed successfully ===")
}

// keysOf 辅助函数:返回 map 的所有 key,仅用于示例输出
func keysOf(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
