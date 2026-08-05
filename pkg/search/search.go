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
	Sort         []map[string]interface{} `json:"sort,omitempty"`
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
// ES 8 严格模式不允许 "query": null,因此 query 为空时省略该字段
func (b *Builder) Build() ([]byte, error) {
	if b.request.Query == nil {
		// 直接序列化去掉 query 字段
		return json.Marshal(struct {
			From         int                      `json:"from,omitempty"`
			Size         int                      `json:"size,omitempty"`
			Sort         []map[string]interface{} `json:"sort,omitempty"`
			Aggregations map[string]interface{}   `json:"aggs,omitempty"`
		}{
			From:         b.request.From,
			Size:         b.request.Size,
			Sort:         b.request.Sort,
			Aggregations: b.request.Aggregations,
		})
	}
	return json.Marshal(b.request)
}

// SetQuery 设置查询条件
// 用于直接设置预定义的查询结构
func (b *Builder) SetQuery(query map[string]interface{}) *Builder {
	b.request.Query = query
	return b
}

// GetRequest 获取搜索请求（用于内部访问）
func (b *Builder) GetRequest() SearchRequest {
	return b.request
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

// MatchPhrasePrefix 添加短语前缀匹配查询（用于搜索建议）
// 参数:
//
//	field: 字段名
//	query: 查询前缀
//
// 返回:
//
//	*Builder: 构建器实例
func (b *Builder) MatchPhrasePrefix(field, query string) *Builder {
	b.request.Query = map[string]interface{}{
		"match_phrase_prefix": map[string]interface{}{
			field: query,
		},
	}
	return b
}

// MultiMatch 添加多字段匹配查询
// 参数:
//
//	query: 查询文本
//	fields: 要搜索的字段列表
//
// 返回:
//
//	*Builder: 构建器实例
func (b *Builder) MultiMatch(query string, fields []string) *Builder {
	b.request.Query = map[string]interface{}{
		"multi_match": map[string]interface{}{
			"query":  query,
			"fields": fields,
		},
	}
	return b
}

// Exists 添加存在性查询（查询字段存在的文档）
// 参数:
//
//	field: 字段名
//
// 返回:
//
//	*Builder: 构建器实例
func (b *Builder) Exists(field string) *Builder {
	b.request.Query = map[string]interface{}{
		"exists": map[string]interface{}{
			"field": field,
		},
	}
	return b
}

// Prefix 添加前缀查询
// 参数:
//
//	field: 字段名
//	prefix: 前缀值
//
// 返回:
//
//	*Builder: 构建器实例
func (b *Builder) Prefix(field string, prefix string) *Builder {
	b.request.Query = map[string]interface{}{
		"prefix": map[string]interface{}{
			field: prefix,
		},
	}
	return b
}

// Wildcard 添加通配符查询
// 参数:
//
//	field: 字段名
//	pattern: 通配符模式（支持*和?）
//
// 返回:
//
//	*Builder: 构建器实例
func (b *Builder) Wildcard(field string, pattern string) *Builder {
	b.request.Query = map[string]interface{}{
		"wildcard": map[string]interface{}{
			field: pattern,
		},
	}
	return b
}

// Fuzzy 添加模糊查询（支持拼写错误纠正）
// 参数:
//
//	field: 字段名
//	query: 查询文本
//
// 返回:
//
//	*Builder: 构建器实例
func (b *Builder) Fuzzy(field string, query string) *Builder {
	b.request.Query = map[string]interface{}{
		"fuzzy": map[string]interface{}{
			field: query,
		},
	}
	return b
}

// FuzzyWithFuzziness 添加模糊查询并指定模糊度
// 参数:
//
//	field: 字段名
//	query: 查询文本
//	fuzziness: 模糊度（如"AUTO"或1, 2）
//
// 返回:
//
//	*Builder: 构建器实例
func (b *Builder) FuzzyWithFuzziness(field string, query string, fuzziness interface{}) *Builder {
	b.request.Query = map[string]interface{}{
		"fuzzy": map[string]interface{}{
			field: map[string]interface{}{
				"value":      query,
				"fuzziness":  fuzziness,
			},
		},
	}
	return b
}

// MatchAll 添加匹配所有文档查询
// 返回:
//
//	*Builder: 构建器实例
func (b *Builder) MatchAll() *Builder {
	b.request.Query = map[string]interface{}{
		"match_all": map[string]interface{}{},
	}
	return b
}

// GeoDistance 添加地理距离查询（查找指定经纬度范围内的文档）
// 参数:
//
//	field: 地理字段名
//	lat: 纬度
//	lon: 经度
//	distance: 距离（如"10km", "5mi"）
//
// 返回:
//
//	*Builder: 构建器实例
func (b *Builder) GeoDistance(field string, lat, lon float64, distance string) *Builder {
	b.request.Query = map[string]interface{}{
		"geo_distance": map[string]interface{}{
			"distance": distance,
			field: []interface{}{lon, lat},
		},
	}
	return b
}

// GeoBoundingBox 添加地理边界框查询（查找指定矩形框内的文档）
// 参数:
//
//	field: 地理字段名
//	topLat: 左上角纬度
//	leftLon: 左上角经度
//	bottomLat: 右下角纬度
//	rightLon: 右下角经度
//
// 返回:
//
//	*Builder: 构建器实例
func (b *Builder) GeoBoundingBox(field string, topLat, leftLon, bottomLat, rightLon float64) *Builder {
	b.request.Query = map[string]interface{}{
		"geo_bounding_box": map[string]interface{}{
			field: map[string]interface{}{
				"top_left": []interface{}{leftLon, topLat},
				"bottom_right": []interface{}{rightLon, bottomLat},
			},
		},
	}
	return b
}

// TermsSet 添加terms_set查询（匹配至少/最多N个词）
// 参数:
//
//	field: 字段名
//	terms: 词列表
//	minShouldMatch: 最少匹配数量
//
// 返回:
//
//	*Builder: 构建器实例
func (b *Builder) TermsSet(field string, terms []string, minShouldMatch int) *Builder {
	b.request.Query = map[string]interface{}{
		"terms_set": map[string]interface{}{
			field: map[string]interface{}{
				"terms": terms,
				"minimum_should_match": minShouldMatch,
			},
		},
	}
	return b
}

// Regexp 添加正则表达式查询
// 参数:
//
//	field: 字段名
//	pattern: 正则表达式模式
//
// 返回:
//
//	*Builder: 构建器实例
func (b *Builder) Regexp(field string, pattern string) *Builder {
	b.request.Query = map[string]interface{}{
		"regexp": map[string]interface{}{
			field: pattern,
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

// Sort 添加简单排序
// 参数:
//
//	field: 排序字段
//	order: 排序方向（asc/desc）
//
// 返回:
//
//	*Builder: 构建器实例
func (b *Builder) Sort(field, order string) *Builder {
	b.request.Sort = append(b.request.Sort, map[string]interface{}{
		field: order,
	})
	return b
}

// SortWithMode 添加高级排序，支持自定义排序模式
// 参数:
//
//	field: 排序字段
//	order: 排序方向（asc/desc）
//	mode: 排序模式（min/max/avg/sum/median）
//
// 返回:
//
//	*Builder: 构建器实例
func (b *Builder) SortWithMode(field, order, mode string) *Builder {
	sortSpec := map[string]interface{}{
		"order": order,
		"mode":  mode,
	}
	b.request.Sort = append(b.request.Sort, map[string]interface{}{
		field: sortSpec,
	})
	return b
}

// SortGeoDistance 添加按地理距离排序
// 参数:
//
//	field: 地理字段名
//	lat: 纬度
//	lon: 经度
//	order: 排序方向（asc/desc）
//
// 返回:
//
//	*Builder: 构建器实例
func (b *Builder) SortGeoDistance(field string, lat, lon float64, order string) *Builder {
	geoSort := map[string]interface{}{
		field: []interface{}{lon, lat},
		"order": order,
		"unit":  "km",
	}
	b.request.Sort = append(b.request.Sort, map[string]interface{}{
		"_geo_distance": geoSort,
	})
	return b
}

// SortScript 添加脚本排序
// 参数:
//
//	source: 脚本源码
//	order: 排序方向（asc/desc）
//
// 返回:
//
//	*Builder: 构建器实例
func (b *Builder) SortScript(source string, order string) *Builder {
	scriptSort := map[string]interface{}{
		"script": map[string]interface{}{
			"source": source,
		},
		"order": order,
	}
	b.request.Sort = append(b.request.Sort, map[string]interface{}{
		"_script": scriptSort,
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
		s.log.Error("Failed to build search request",
			zap.String("index", index),
			zap.String("body", string(reqBytes)),
			zap.Error(err))
		return nil, errors.Wrap(errors.ErrCodeMarshalJSON,
			"failed to build search request", err)
	}
	s.log.Debug("search request body",
		zap.String("index", index),
		zap.String("body", string(reqBytes)))

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

// MatchPhraseQuery 创建match_phrase查询结构
func MatchPhraseQuery(field, query string) map[string]interface{} {
	return map[string]interface{}{
		"match_phrase": map[string]interface{}{
			field: query,
		},
	}
}

// MatchPhrasePrefixQuery 创建match_phrase_prefix查询结构
func MatchPhrasePrefixQuery(field, query string) map[string]interface{} {
	return map[string]interface{}{
		"match_phrase_prefix": map[string]interface{}{
			field: query,
		},
	}
}

// MultiMatchQuery 创建multi_match查询结构
func MultiMatchQuery(query string, fields []string) map[string]interface{} {
	return map[string]interface{}{
		"multi_match": map[string]interface{}{
			"query":  query,
			"fields": fields,
		},
	}
}

// ExistsQuery 创建exists查询结构
func ExistsQuery(field string) map[string]interface{} {
	return map[string]interface{}{
		"exists": map[string]interface{}{
			"field": field,
		},
	}
}

// PrefixQuery 创建prefix查询结构
func PrefixQuery(field string, prefix string) map[string]interface{} {
	return map[string]interface{}{
		"prefix": map[string]interface{}{
			field: prefix,
		},
	}
}

// WildcardQuery 创建wildcard查询结构
func WildcardQuery(field string, pattern string) map[string]interface{} {
	return map[string]interface{}{
		"wildcard": map[string]interface{}{
			field: pattern,
		},
	}
}

// FuzzyQuery 创建fuzzy查询结构
func FuzzyQuery(field string, query string) map[string]interface{} {
	return map[string]interface{}{
		"fuzzy": map[string]interface{}{
			field: query,
		},
	}
}

// FuzzyQueryWithFuzziness 创建fuzzy查询结构并指定模糊度
func FuzzyQueryWithFuzziness(field string, query string, fuzziness interface{}) map[string]interface{} {
	return map[string]interface{}{
		"fuzzy": map[string]interface{}{
			field: map[string]interface{}{
				"value":     query,
				"fuzziness": fuzziness,
			},
		},
	}
}

// BoolQuery 创建bool查询结构
func BoolQuery(
	must []map[string]interface{},
	mustNot []map[string]interface{},
	should []map[string]interface{},
	filter []map[string]interface{},
) map[string]interface{} {
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
	return map[string]interface{}{
		"bool": boolQuery,
	}
}

// MatchAllQuery 创建match_all查询结构
func MatchAllQuery() map[string]interface{} {
	return map[string]interface{}{
		"match_all": map[string]interface{}{},
	}
}

// GeoDistanceQuery 创建地理距离查询结构
func GeoDistanceQuery(field string, lat, lon float64, distance string) map[string]interface{} {
	return map[string]interface{}{
		"geo_distance": map[string]interface{}{
			"distance": distance,
			field: []interface{}{lon, lat},
		},
	}
}

// GeoBoundingBoxQuery 创建地理边界框查询结构
func GeoBoundingBoxQuery(field string, topLat, leftLon, bottomLat, rightLon float64) map[string]interface{} {
	return map[string]interface{}{
		"geo_bounding_box": map[string]interface{}{
			field: map[string]interface{}{
				"top_left": []interface{}{leftLon, topLat},
				"bottom_right": []interface{}{rightLon, bottomLat},
			},
		},
	}
}

// TermsSetQuery 创建terms_set查询结构
func TermsSetQuery(field string, terms []string, minShouldMatch int) map[string]interface{} {
	return map[string]interface{}{
		"terms_set": map[string]interface{}{
			field: map[string]interface{}{
				"terms": terms,
				"minimum_should_match": minShouldMatch,
			},
		},
	}
}

// RegexpQuery 创建正则表达式查询结构
func RegexpQuery(field string, pattern string) map[string]interface{} {
	return map[string]interface{}{
		"regexp": map[string]interface{}{
			field: pattern,
		},
	}
}

// ScrollIterator 滚动搜索迭代器
// 用于遍历大批量搜索结果，避免一次性加载所有结果到内存
type ScrollIterator struct {
	searcher      *Searcher
	index         string
	keepAlive     time.Duration
	builder       *Builder
	scrollID      string
	hasNext       bool
	currentResult *SearchResponse
	err           error
	ctx           context.Context
}

// NewScrollIterator 创建一个滚动搜索迭代器
func (s *Searcher) NewScrollIterator(ctx context.Context, index string, keepAlive string, builder *Builder) (*ScrollIterator, error) {
	d, err := time.ParseDuration(keepAlive)
	if err != nil {
		d = 5 * time.Minute // 默认5分钟
	}

	// 第一次搜索
	// builder已经构建好查询，我们只需要确保scroll配置正确
	// 第一次请求获取scrollID
	reqBytes, err := builder.Build()
	if err != nil {
		return nil, err
	}

	res, err := s.es.Search(
		s.es.Search.WithContext(ctx),
		s.es.Search.WithIndex(index),
		s.es.Search.WithBody(bytes.NewReader(reqBytes)),
		s.es.Search.WithScroll(d),
	)
	if err != nil {
		s.log.Error("Failed to init scroll iterator",
			zap.String("index", index),
			zap.Error(err))
		return nil, err
	}
	defer res.Body.Close()

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		return nil, errors.New(errors.ErrCodeSearch, string(body))
	}

	// 读取整个响应来获取scrollID
	bodyBytes, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	var full struct {
		ScrollID string `json:"_scroll_id"`
		SearchResponse
	}
	if err := json.Unmarshal(bodyBytes, &full); err != nil {
		return nil, err
	}

	return &ScrollIterator{
		searcher:      s,
		index:         index,
		keepAlive:     d,
		builder:       builder,
		scrollID:      full.ScrollID,
		hasNext:       len(full.Hits.Hits) > 0,
		currentResult:  &full.SearchResponse,
		ctx:           ctx,
	}, nil
}

// Next 是否还有下一批结果
func (it *ScrollIterator) Next() bool {
	if !it.hasNext || it.err != nil {
		return false
	}

	// 如果已经有currentResult，说明调用者还没获取当前批
	// 我们需要获取下一批
	if it.currentResult != nil && len(it.currentResult.Hits.Hits) == 0 {
		it.hasNext = false
		return false
	}

	// 使用scroll ID获取下一批
	res, err := it.searcher.es.Scroll(
		it.searcher.es.Scroll.WithContext(it.ctx),
		it.searcher.es.Scroll.WithScrollID(it.scrollID),
		it.searcher.es.Scroll.WithScroll(it.keepAlive),
	)
	if err != nil {
		it.err = err
		it.hasNext = false
		return false
	}
	defer res.Body.Close()

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		it.err = errors.New(errors.ErrCodeSearch, string(body))
		it.hasNext = false
		return false
	}

	var fullResponse struct {
		ScrollID string `json:"_scroll_id"`
		SearchResponse
	}
	if err := json.NewDecoder(res.Body).Decode(&fullResponse); err != nil {
		it.err = err
		it.hasNext = false
		return false
	}

	it.scrollID = fullResponse.ScrollID
	it.currentResult = &fullResponse.SearchResponse
	it.hasNext = len(fullResponse.Hits.Hits) > 0

	return true
}

// Result 获取当前批结果
func (it *ScrollIterator) Result() *SearchResponse {
	return it.currentResult
}

// Err 获取迭代过程中的错误
func (it *ScrollIterator) Err() error {
	return it.err
}

// Close 关闭滚动搜索，清理scroll
func (it *ScrollIterator) Close() error {
	if it.scrollID == "" {
		return nil
	}

	// 清除scroll
	res, err := it.searcher.es.ClearScroll(
		it.searcher.es.ClearScroll.WithScrollID(it.scrollID),
		it.searcher.es.ClearScroll.WithContext(it.ctx),
	)
	if err != nil {
		it.searcher.log.Warn("Failed to clear scroll", zap.Error(err))
		return err
	}
	defer res.Body.Close()

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		it.searcher.log.Warn("Clear scroll returned error", zap.String("response", string(body)))
		return errors.New(errors.ErrCodeSearch, string(body))
	}

	it.hasNext = false
	it.scrollID = ""
	return nil
}
