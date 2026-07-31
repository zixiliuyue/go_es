# Go Elasticsearch 学习项目

这是一个基于Go语言的Elasticsearch学习项目，严格遵循字节跳动工程标准进行开发，包含完整的ES核心功能实现、详细的中文注释和全面的单元测试。

## 项目结构

```
go_es/
├── go.mod              # Go模块定义
├── README.md           # 项目说明文档
├── examples/
│   └── main.go         # 示例程序，展示完整使用流程
└── pkg/
    ├── client/         # 客户端连接管理
    │   ├── client.go
    │   └── client_test.go
    ├── errors/         # 统一错误定义和处理
    │   ├── errors.go
    │   └── errors_test.go
    ├── index/          # 索引管理功能
    │   ├── index.go
    │   └── index_test.go
    ├── document/       # 文档CRUD操作
    │   ├── document.go
    │   └── document_test.go
    ├── search/         # 查询构建功能
    │   ├── search.go
    │   └── search_test.go
    └── aggregate/     # 聚合分析功能
        ├── aggregate.go
        └── aggregate_test.go
```

## 功能特性

### 1. 客户端连接管理
- 支持多个ES节点
- 支持用户名密码认证
- 连接健康检查（Ping）
- 集成zap日志记录

### 2. 索引管理
- 创建索引（支持自定义mapping）
- 删除索引
- 检查索引是否存在
- 获取索引映射

### 3. 文档CRUD操作
- 创建文档（自动生成ID或指定ID）
- 获取文档
- 更新文档
- 删除文档
- 批量操作
- 检查文档是否存在
- 强类型文档解析（GetInto）

### 4. 查询构建
- 链式调用构建查询
- 支持多种查询类型：
  - Match查询（全文搜索）
  - MatchPhrase查询（短语匹配）
  - Term查询（精确匹配）
  - Terms查询（多值精确匹配）
  - Range查询（范围查询）
  - Bool查询（布尔组合查询）
- 分页支持
- 排序支持
- Source字段过滤
- 高亮显示支持

### 5. 聚合分析
- 支持多种聚合类型：
  - Terms聚合（分组聚合）
  - Histogram聚合（直方图）
  - DateHistogram聚合（日期直方图）
  - Range聚合（范围聚合）
  - Avg聚合（平均值）
  - Sum聚合（求和）
  - Min聚合（最小值）
  - Max聚合（最大值）
  - Stats聚合（统计聚合，同时返回count/min/max/avg/sum）
  - Cardinality聚合（去重计数）
- 支持嵌套聚合（在桶聚合中嵌套指标聚合）
- 便捷的结果解析

## 环境要求

- Go 1.19+
- Elasticsearch 8.x
- （可选）Docker 用于运行ES测试环境

## 依赖安装

```bash
# 克隆项目
git clone https://github.com/zixiliuyue/go_es.git
cd go_es

# 安装依赖
go mod download
```

## 快速开始

### 1. 启动Elasticsearch

使用Docker快速启动：

```bash
docker run -d \
  --name elasticsearch \
  -p 9200:9200 \
  -e "discovery.type=single-node" \
  -e "xpack.security.enabled=false" \
  docker.elastic.co/elasticsearch/elasticsearch:8.14.0
```

### 2. 运行示例程序

```bash
go run examples/main.go
```

### 3. 代码示例

#### 初始化客户端

```go
package main

import (
	"github.com/zixiliuyue/go_es/pkg/client"
	"go.uber.org/zap"
)

func main() {
	logger, _ := zap.NewDevelopment()
	
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
}
```

#### 创建索引

```go
import (
	"github.com/zixiliuyue/go_es/pkg/index"
	"context"
)

indexManager := index.NewManager(esClient.GetES(), logger)

mapping := map[string]interface{}{
	"mappings": map[string]interface{}{
		"properties": map[string]interface{}{
			"title":   map[string]string{"type": "text"},
			"content": map[string]string{"type": "text"},
			"author":  map[string]string{"type": "keyword"},
			"category": map[string]string{"type": "keyword"},
			"publish_at": map[string]string{"type": "date"},
			"views": map[string]string{"type": "integer"},
		},
	},
}

err := indexManager.CreateIndex(context.Background(), "articles", mapping)
if err != nil {
	logger.Fatal("Failed to create index", zap.Error(err))
}
```

#### 索引文档

```go
import (
	"github.com/zixiliuyue/go_es/pkg/document"
)

type Article struct {
	ID        int64  `json:"id"`
	Title     string `json:"title"`
	Content   string `json:"content"`
	Author    string `json:"author"`
	Category  string `json:"category"`
	PublishAt int64  `json:"publish_at"`
	Views     int    `json:"views"`
}

docManager := document.NewManager(esClient.GetES(), logger)

article := Article{
	ID:        1,
	Title:     "Elasticsearch入门教程",
	Content:   "这是一篇关于Elasticsearch入门教程",
	Author:    "张三",
	Category:  "技术",
	PublishAt: 1672531200,
	Views:     1000,
}

resp, err := docManager.IndexWithID(context.Background(), "articles", "1", article)
if err != nil {
	logger.Fatal("Failed to index document", zap.Error(err))
}
```

#### 获取文档

```go
var article Article
err := docManager.GetInto(context.Background(), "articles", "1", &article)
if err != nil {
	logger.Fatal("Failed to get document", zap.Error(err))
}
fmt.Printf("Got document: %+v\n", article)
```

#### 搜索

```go
import (
	"github.com/zixiliuyue/go_es/pkg/search"
)

searcher := search.NewSearcher(esClient.GetES(), logger)

// 关键词搜索，分页，排序
builder := search.NewSearch().
	Match("title", "Elasticsearch").
	Pagination(0, 10).
	Sort("publish_at", "desc")

resp, err := searcher.Search(context.Background(), "articles", builder)
if err != nil {
	logger.Fatal("Search failed", zap.Error(err))
}

fmt.Printf("Found %d documents\n", resp.Hits.Total.Value)
for _, hit := range resp.Hits.Hits {
	var article Article
	_ = json.Unmarshal(hit.Source, &article)
	fmt.Printf("- %s by %s\n", article.Title, article.Author)
}
```

#### 布尔组合查询

```go
builder := search.NewSearch().
	Bool(
		[]map[string]interface{}{search.MatchQuery("content", "聚合")},
		nil,
		nil,
		[]map[string]interface{}{search.TermQuery("category", "技术")},
	).
	Pagination(0, 10)

resp, err := searcher.Search(ctx, "articles", builder)
```

#### 聚合分析

```go
import (
	"github.com/zixiliuyue/go_es/pkg/aggregate"
)

// 按category分组统计
categoryAgg := aggregate.NewTermsAggregation("group_by_category", "category").
	SetSize(10).
	SetOrder("_count", "desc")

// 在分组基础上统计平均阅读量
avgViewsAgg := aggregate.NewAvgAggregation("avg_views", "views")
nestedAgg := aggregate.NewNestedAggregation(categoryAgg, []aggregate.Aggregation{avgViewsAgg})

builder := search.NewSearch().Pagination(0, 0) // 只获取聚合结果，不需要文档

aggMap, _ := nestedAgg.Build()
for name, aggDef := range aggMap {
	builder.AddAggregation(name, aggDef)
}

resp, err := searcher.Search(ctx, "articles", builder)

// 解析分组结果
termsResult, err := aggregate.ParseTermsResult(resp.Aggregations, "group_by_category")
for _, bucket := range termsResult.Buckets {
	fmt.Printf("Category: %v, Doc count: %d\n", bucket.Key, bucket.DocCount)
}
```

## 运行单元测试

```bash
# 运行所有测试
go test -v ./...

# 运行指定包测试
go test -v ./pkg/client
go test -v ./pkg/errors

# 生成覆盖率报告
go test -v ./... -coverprofile=coverage.out
go tool cover -html=coverage.out
```

**注意**: 集成测试需要Elasticsearch服务可用。如果ES没有运行，相关测试会自动跳过。

## 代码质量

- 所有代码都添加了详细的中文注释
- 遵循Go语言编码规范，使用`go fmt`格式化
- 完整的单元测试覆盖，核心功能测试覆盖率 > 80%
- 统一的错误处理和日志记录
- 清晰的包划分和模块组织，符合字节跳动工程规范

## 代码格式化

```bash
# 格式化所有代码
go fmt ./...
```

## 作者

zixiliuyue

## 许可证

MIT
