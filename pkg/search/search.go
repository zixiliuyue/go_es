// Package search 提供Elasticsearch查询构建功能
// 支持构建各种类型的查询，包括匹配查询、布尔查询、范围查询等
package search

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/zixiliuyue/go_es/pkg/errors"
	"go.uber.org/zap"
)

// SearchRequest 搜索请求
type SearchRequest struct {
	Query        map[string]interface{} `json:"query"`
	From         int                    `json:"from,omitempty"`
	Size         int                    `json:"size,omitempty"`
	Sort         []map[string]string    `json:"sort,omitempty"`
	Aggregations map[string]interface{} `json:"aggregations,omitempty"`
	Source       interface{}            `json:"_source,omitempty"`
	Highlight    map[string]interface{} `json:"highlight,omitempty"`
}

// SearchResponse 搜索响应
type SearchResponse struct {
	Took         int             `json:"took"`
	TimedOut     bool            `json:"timed_out"`
	Hits         Hits            `json:"hits"`
	Aggregations json.RawMessage `json:"aggregations"`
}

// Hits 搜索命中结果
type Hits struct {
	Total Total `json:"total"`
	Hits  []Hit `json:"hits"`
}

// Total 总命中数
type Total struct {
	Value    int    `json:"value"`
	Relation string `json:"relation"`
}

// Hit 单个命中文档
type Hit struct {
	Index     string              `json:"_index"`
	ID        string              `json:"_id"`
	Score     float64             `json:"_score"`
	Source    json.RawMessage     `json:"_source"`
	Highlight map[string][]string `json:"highlight"`
}

// Builder 搜索查询构建器
type Builder struct {
	request SearchRequest
}

// NewSearch 创建一个新的搜索请求构建器
func NewSearch() *Builder {
	return &Builder{
		request: SearchRequest{
			Size: 10, // 默认返回10条
		},
	}
}

// Build 构建搜索请求JSON
func (b *Builder) Build() ([]byte, error) {
	return json.Marshal(b.request)
}

// Match 添加匹配查询
// 参数:
//
//	field: 字段名
//	query: 查询文本
//
// 返回:
//
//	*Builder: 构建器实例（支持链式调用）
func (b *Builder) Match(field, query string) *Builder {
	b.request.Query = map[string]interface{}{
		"match": map[string]interface{}{
			field: query,
		},
	}
	return b
}

// MatchPhrase 添加短语匹配查询
// 参数:
//
//	field: 字段名
//	query: 查询短语
//
// 返回:
//
//	*Builder: 构建器实例
func (b *Builder) MatchPhrase(field, query string) *Builder {
	b.request.Query = map[string]interface{}{
		"match_phrase": map[string]interface{}{
			field: query,
		},
	}
	return b
}

// Term 添加精确匹配查询
// 参数:
//
//	field: 字段名
//	value: 查询值
//
// 返回:
//
//	*Builder: 构建器实例
func (b *Builder) Term(field string, value interface{}) *Builder {
	b.request.Query = map[string]interface{}{
		"term": map[string]interface{}{
			field: value,
		},
	}
	return b
}

// Terms 添加多精确匹配查询
// 参数:
//
//	field: 字段名
//	values: 查询值列表
//
// 返回:
//
//	*Builder: 构建器实例
func (b *Builder) Terms(field string, values []interface{}) *Builder {
	b.request.Query = map[string]interface{}{
		"terms": map[string]interface{}{
			field: values,
		},
	}
	return b
}

// Range 添加范围查询
// 参数:
//
//	field: 字段名
//	gte: 大于等于（nil表示不限制）
//	lte: 小于等于（nil表示不限制）
//
// 返回:
//
//	*Builder: 构建器实例
func (b *Builder) Range(field string, gte, lte interface{}) *Builder {
	rangeQuery := make(map[string]interface{})
	if gte != nil {
		rangeQuery["gte"] = gte
	}
	if lte != nil {
		rangeQuery["lte"] = lte
	}
	b.request.Query = map[string]interface{}{
		"range": map[string]interface{}{
			field: rangeQuery,
		},
	}
	return b
}

// Bool 添加布尔查询
// 参数:
//
//	must: must条件列表
//	mustNot: must_not条件列表
//	should: should条件列表
//	filter: filter条件列表
//
// 返回:
//
//	*Builder: 构建器实例
func (b *Builder) Bool(
	must []map[string]interface{},
	mustNot []map[string]interface{},
	should []map[string]interface{},
	filter []map[string]interface{},
) *Builder {
	boolQuery := make(map[string]interface{})
	if len(must) > 0 {
		boolQuery["must"] = must
	}
	if len(mustNot) > 0 {
		boolQuery["must_not"] = mustNot
	}
	if len(should) > 0 {
		boolQuery["should"] = should
	}
	if len(filter) > 0 {
		boolQuery["filter"] = filter
	}
	b.request.Query = map[string]interface{}{
		"bool": boolQuery,
	}
	return b
}

// Pagination 设置分页
// 参数:
//
//	from: 偏移量
//	size: 每页大小
//
// 返回:
//
//	*Builder: 构建器实例
func (b *Builder) Pagination(from, size int) *Builder {
	b.request.From = from
	b.request.Size = size
	return b
}

// Sort 添加排序
// 参数:
//
//	field: 排序字段
//	order: 排序方向（asc/desc）
//
// 返回:
//
//	*Builder: 构建器实例
func (b *Builder) Sort(field, order string) *Builder {
	b.request.Sort = append(b.request.Sort, map[string]string{
		field: order,
	})
	return b
}

// AddAggregation 添加聚合
// 参数:
//
//	name: 聚合名称
//	agg: 聚合定义
//
// 返回:
//
//	*Builder: 构建器实例
func (b *Builder) AddAggregation(name string, agg map[string]interface{}) *Builder {
	if b.request.Aggregations == nil {
		b.request.Aggregations = make(map[string]interface{})
	}
	b.request.Aggregations[name] = agg
	return b
}

// SourceFilter 设置_source过滤
// 参数:
//
//	includes: 需要包含的字段列表
//	excludes: 需要排除的字段列表
//
// 返回:
//
//	*Builder: 构建器实例
func (b *Builder) SourceFilter(includes, excludes []string) *Builder {
	if len(includes) == 0 && len(excludes) == 0 {
		return b
	}
	sourceFilter := make(map[string][]string)
	if len(includes) > 0 {
		sourceFilter["includes"] = includes
	}
	if len(excludes) > 0 {
		sourceFilter["excludes"] = excludes
	}
	b.request.Source = sourceFilter
	return b
}

// Highlight 添加高亮
// 参数:
//
//	fields: 需要高亮的字段配置
//	preTags: 前置标签
//	postTags: 后置标签
//
// 返回:
//
//	*Builder: 构建器实例
func (b *Builder) Highlight(fields map[string]interface{}, preTags, postTags []string) *Builder {
	highlight := make(map[string]interface{})
	if len(preTags) > 0 {
		highlight["pre_tags"] = preTags
	}
	if len(postTags) > 0 {
		highlight["post_tags"] = postTags
	}
	highlight["fields"] = fields
	b.request.Highlight = highlight
	return b
}

// Searcher 搜索引擎
type Searcher struct {
	es  *elasticsearch.Client
	log *zap.Logger
}

// NewSearcher 创建一个新的搜索引擎
// 参数:
//
//	es: Elasticsearch客户端
//	log: 日志记录器
//
// 返回:
//
//	*Searcher: 搜索引擎实例
func NewSearcher(es *elasticsearch.Client, log *zap.Logger) *Searcher {
	if log == nil {
		log, _ = zap.NewProduction()
	}
	return &Searcher{
		es:  es,
		log: log,
	}
}

// Search 执行搜索
// 参数:
//
//	ctx: 上下文
//	index: 索引名称
//	builder: 搜索请求构建器
//
// 返回:
//
//	*SearchResponse: 搜索响应
//	error: 操作错误
func (s *Searcher) Search(ctx context.Context, index string, builder *Builder) (*SearchResponse, error) {
	reqBytes, err := builder.Build()
	if err != nil {
		s.log.Error("Failed to build search request", zap.Error(err))
		return nil, errors.Wrap(errors.ErrCodeMarshalJSON,
			"failed to build search request", err)
	}

	res, err := s.es.Search(
		s.es.Search.WithContext(ctx),
		s.es.Search.WithIndex(index),
		s.es.Search.WithBody(bytes.NewReader(reqBytes)),
	)
	if err != nil {
		s.log.Error("Failed to execute search",
			zap.String("index", index),
			zap.Error(err))
		return nil, errors.Wrap(errors.ErrCodeSearch,
			"failed to execute search", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		s.log.Error("Search returned error",
			zap.String("index", index),
			zap.Int("status", res.StatusCode),
			zap.String("response", string(body)))
		return nil, errors.New(errors.ErrCodeSearch, string(body))
	}

	var response SearchResponse
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		s.log.Error("Failed to decode search response",
			zap.String("index", index),
			zap.Error(err))
		return nil, err
	}

	s.log.Debug("Search completed",
		zap.String("index", index),
		zap.Int("took_ms", response.Took),
		zap.Int("total_hits", response.Hits.Total.Value),
		zap.Int("returned", len(response.Hits.Hits)))

	return &response, nil
}

// SearchWithQuery 使用原生查询JSON执行搜索
// 参数:
//
//	ctx: 上下文
//	index: 索引名称
//	query: 查询JSON
//
// 返回:
//
//	*SearchResponse: 搜索响应
//	error: 操作错误
func (s *Searcher) SearchWithQuery(ctx context.Context, index string, query []byte) (*SearchResponse, error) {
	res, err := s.es.Search(
		s.es.Search.WithContext(ctx),
		s.es.Search.WithIndex(index),
		s.es.Search.WithBody(bytes.NewReader(query)),
	)
	if err != nil {
		s.log.Error("Failed to execute search with raw query",
			zap.String("index", index),
			zap.Error(err))
		return nil, errors.Wrap(errors.ErrCodeSearch,
			"failed to execute search", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		s.log.Error("Search returned error",
			zap.String("index", index),
			zap.Int("status", res.StatusCode),
			zap.String("response", string(body)))
		return nil, errors.New(errors.ErrCodeSearch, string(body))
	}

	var response SearchResponse
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		s.log.Error("Failed to decode search response",
			zap.String("index", index),
			zap.Error(err))
		return nil, err
	}

	return &response, nil
}

// Count 获取文档数量
// 参数:
//
//	ctx: 上下文
//	index: 索引名称
//	query: 查询条件
//
// 返回:
//
//	int64: 文档数量
//	error: 操作错误
func (s *Searcher) Count(ctx context.Context, index string, query map[string]interface{}) (int64, error) {
	var queryBytes []byte
	var err error
	if query != nil {
		queryBytes, err = json.Marshal(map[string]interface{}{"query": query})
		if err != nil {
			return 0, errors.Wrap(errors.ErrCodeMarshalJSON,
				"failed to marshal count query", err)
		}
	}

	res, err := s.es.Count(
		s.es.Count.WithContext(ctx),
		s.es.Count.WithIndex(index),
		s.es.Count.WithBody(bytes.NewReader(queryBytes)),
	)
	if err != nil {
		s.log.Error("Failed to count documents",
			zap.String("index", index),
			zap.Error(err))
		return 0, err
	}
	defer res.Body.Close()

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		s.log.Error("Count returned error",
			zap.String("index", index),
			zap.Int("status", res.StatusCode),
			zap.String("response", string(body)))
		return 0, errors.New(errors.ErrCodeSearch, string(body))
	}

	var countResult struct {
		Count int64 `json:"count"`
	}
	if err := json.NewDecoder(res.Body).Decode(&countResult); err != nil {
		s.log.Error("Failed to decode count response",
			zap.String("index", index),
			zap.Error(err))
		return 0, err
	}

	return countResult.Count, nil
}

// Scroll 滚动搜索（用于大批量数据导出）
// 由于实现复杂，这里只提供基础支持
// keepAlive 格式如 "1m", "5m" 表示1分钟，5分钟
func (s *Searcher) Scroll(ctx context.Context, index string, size int, keepAlive string) (*SearchResponse, error) {
	// 完整的滚动搜索需要维护scroll_id，这里简化处理
	d, err := time.ParseDuration(keepAlive)
	if err != nil {
		d = 1 * time.Minute // 默认1分钟
	}
	res, err := s.es.Search(
		s.es.Search.WithContext(ctx),
		s.es.Search.WithIndex(index),
		s.es.Search.WithSize(size),
		s.es.Search.WithScroll(d),
	)
	if err != nil {
		s.log.Error("Failed to init scroll",
			zap.String("index", index),
			zap.Error(err))
		return nil, err
	}
	defer res.Body.Close()

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		return nil, errors.New(errors.ErrCodeSearch, string(body))
	}

	var response SearchResponse
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		return nil, err
	}

	return &response, nil
}

// MatchAll 创建match_all查询结构
func MatchAll() map[string]interface{} {
	return map[string]interface{}{
		"match_all": map[string]interface{}{},
	}
}

// MatchQuery 创建match查询结构
func MatchQuery(field, query string) map[string]interface{} {
	return map[string]interface{}{
		"match": map[string]interface{}{
			field: query,
		},
	}
}

// TermQuery 创建term查询结构
func TermQuery(field string, value interface{}) map[string]interface{} {
	return map[string]interface{}{
		"term": map[string]interface{}{
			field: value,
		},
	}
}

// RangeQuery 创建range查询结构
func RangeQuery(field string, gte, lte interface{}) map[string]interface{} {
	rangeQuery := make(map[string]interface{})
	if gte != nil {
		rangeQuery["gte"] = gte
	}
	if lte != nil {
		rangeQuery["lte"] = lte
	}
	return map[string]interface{}{
		"range": map[string]interface{}{
			field: rangeQuery,
		},
	}
}
