// Package aggregate 提供Elasticsearch聚合分析功能
// 支持构建各种类型的聚合，包括桶聚合、指标聚合等
package aggregate

import (
	"encoding/json"
)

// Aggregation 聚合接口
type Aggregation interface {
	// Build 构建聚合JSON
	Build() (map[string]interface{}, error)
	// Name 获取聚合名称
	Name() string
}

// TermsAggregation Terms桶聚合（分组聚合）
type TermsAggregation struct {
	name  string
	field string
	size  int
	order map[string]string
}

// NewTermsAggregation 创建Terms聚合
// 参数:
//
//	name: 聚合名称
//	field: 分组字段
//
// 返回:
//
//	*TermsAggregation: Terms聚合实例
func NewTermsAggregation(name, field string) *TermsAggregation {
	return &TermsAggregation{
		name:  name,
		field: field,
		size:  10, // 默认返回10个桶
	}
}

// SetSize 设置返回桶数量
func (a *TermsAggregation) SetSize(size int) *TermsAggregation {
	a.size = size
	return a
}

// SetOrder 设置排序方式
func (a *TermsAggregation) SetOrder(field, order string) *TermsAggregation {
	a.order = map[string]string{field: order}
	return a
}

// Name 获取聚合名称
func (a *TermsAggregation) Name() string {
	return a.name
}

// Build 构建聚合
func (a *TermsAggregation) Build() (map[string]interface{}, error) {
	terms := make(map[string]interface{})
	terms["field"] = a.field
	if a.size > 0 {
		terms["size"] = a.size
	}
	if a.order != nil {
		terms["order"] = a.order
	}

	return map[string]interface{}{
		"terms": terms,
	}, nil
}

// HistogramAggregation 直方图聚合
type HistogramAggregation struct {
	name        string
	field       string
	interval    float64
	minDocCount int
}

// NewHistogramAggregation 创建直方图聚合
// 参数:
//
//	name: 聚合名称
//	field: 字段
//	interval: 间隔
//
// 返回:
//
//	*HistogramAggregation: 直方图聚合实例
func NewHistogramAggregation(name, field string, interval float64) *HistogramAggregation {
	return &HistogramAggregation{
		name:        name,
		field:       field,
		interval:    interval,
		minDocCount: 1,
	}
}

// SetMinDocCount 设置最小文档数
func (a *HistogramAggregation) SetMinDocCount(minDocCount int) *HistogramAggregation {
	a.minDocCount = minDocCount
	return a
}

// Name 获取聚合名称
func (a *HistogramAggregation) Name() string {
	return a.name
}

// Build 构建聚合
func (a *HistogramAggregation) Build() (map[string]interface{}, error) {
	histogram := map[string]interface{}{
		"field":    a.field,
		"interval": a.interval,
	}
	if a.minDocCount > 0 {
		histogram["min_doc_count"] = a.minDocCount
	}

	return map[string]interface{}{
		"histogram": histogram,
	}, nil
}

// DateHistogramAggregation 日期直方图聚合
type DateHistogramAggregation struct {
	name             string
	field            string
	calendarInterval string
	fixedInterval    string
	minDocCount      int
}

// NewDateHistogramAggregation 创建日期直方图聚合
// 参数:
//
//	name: 聚合名称
//	field: 日期字段
//	interval: 时间间隔（如 1d, 1h, 1M）
//
// 返回:
//
//	*DateHistogramAggregation: 日期直方图聚合实例
func NewDateHistogramAggregation(name, field, interval string) *DateHistogramAggregation {
	return &DateHistogramAggregation{
		name:             name,
		field:            field,
		calendarInterval: interval,
		minDocCount:      1,
	}
}

// SetMinDocCount 设置最小文档数
func (a *DateHistogramAggregation) SetMinDocCount(minDocCount int) *DateHistogramAggregation {
	a.minDocCount = minDocCount
	return a
}

// Name 获取聚合名称
func (a *DateHistogramAggregation) Name() string {
	return a.name
}

// Build 构建聚合
func (a *DateHistogramAggregation) Build() (map[string]interface{}, error) {
	dateHistogram := map[string]interface{}{
		"field": a.field,
	}
	if a.calendarInterval != "" {
		dateHistogram["calendar_interval"] = a.calendarInterval
	}
	if a.fixedInterval != "" {
		dateHistogram["fixed_interval"] = a.fixedInterval
	}
	if a.minDocCount > 0 {
		dateHistogram["min_doc_count"] = a.minDocCount
	}

	return map[string]interface{}{
		"date_histogram": dateHistogram,
	}, nil
}

// RangeAggregation 范围聚合
type RangeAggregation struct {
	name   string
	field  string
	ranges []Range
}

// Range 范围定义
type Range struct {
	From interface{} `json:"from,omitempty"`
	To   interface{} `json:"to,omitempty"`
}

// NewRangeAggregation 创建范围聚合
// 参数:
//
//	name: 聚合名称
//	field: 字段
//
// 返回:
//
//	*RangeAggregation: 范围聚合实例
func NewRangeAggregation(name, field string) *RangeAggregation {
	return &RangeAggregation{
		name:  name,
		field: field,
	}
}

// AddRange 添加范围
func (a *RangeAggregation) AddRange(from, to interface{}) *RangeAggregation {
	a.ranges = append(a.ranges, Range{From: from, To: to})
	return a
}

// Name 获取聚合名称
func (a *RangeAggregation) Name() string {
	return a.name
}

// Build 构建聚合
func (a *RangeAggregation) Build() (map[string]interface{}, error) {
	return map[string]interface{}{
		"range": map[string]interface{}{
			"field":  a.field,
			"ranges": a.ranges,
		},
	}, nil
}

// AvgAggregation 平均值聚合
type AvgAggregation struct {
	name  string
	field string
}

// NewAvgAggregation 创建平均值聚合
// 参数:
//
//	name: 聚合名称
//	field: 字段
//
// 返回:
//
//	*AvgAggregation: 平均值聚合实例
func NewAvgAggregation(name, field string) *AvgAggregation {
	return &AvgAggregation{
		name:  name,
		field: field,
	}
}

// Name 获取聚合名称
func (a *AvgAggregation) Name() string {
	return a.name
}

// Build 构建聚合
func (a *AvgAggregation) Build() (map[string]interface{}, error) {
	return map[string]interface{}{
		"avg": map[string]interface{}{
			"field": a.field,
		},
	}, nil
}

// SumAggregation 求和聚合
type SumAggregation struct {
	name  string
	field string
}

// NewSumAggregation 创建求和聚合
// 参数:
//
//	name: 聚合名称
//	field: 字段
//
// 返回:
//
//	*SumAggregation: 求和聚合实例
func NewSumAggregation(name, field string) *SumAggregation {
	return &SumAggregation{
		name:  name,
		field: field,
	}
}

// Name 获取聚合名称
func (a *SumAggregation) Name() string {
	return a.name
}

// Build 构建聚合
func (a *SumAggregation) Build() (map[string]interface{}, error) {
	return map[string]interface{}{
		"sum": map[string]interface{}{
			"field": a.field,
		},
	}, nil
}

// MinAggregation 最小值聚合
type MinAggregation struct {
	name  string
	field string
}

// NewMinAggregation 创建最小值聚合
// 参数:
//
//	name: 聚合名称
//	field: 字段
//
// 返回:
//
//	*MinAggregation: 最小值聚合实例
func NewMinAggregation(name, field string) *MinAggregation {
	return &MinAggregation{
		name:  name,
		field: field,
	}
}

// Name 获取聚合名称
func (a *MinAggregation) Name() string {
	return a.name
}

// Build 构建聚合
func (a *MinAggregation) Build() (map[string]interface{}, error) {
	return map[string]interface{}{
		"min": map[string]interface{}{
			"field": a.field,
		},
	}, nil
}

// MaxAggregation 最大值聚合
type MaxAggregation struct {
	name  string
	field string
}

// NewMaxAggregation 创建最大值聚合
// 参数:
//
//	name: 聚合名称
//	field: 字段
//
// 返回:
//
//	*MaxAggregation: 最大值聚合实例
func NewMaxAggregation(name, field string) *MaxAggregation {
	return &MaxAggregation{
		name:  name,
		field: field,
	}
}

// Name 获取聚合名称
func (a *MaxAggregation) Name() string {
	return a.name
}

// Build 构建聚合
func (a *MaxAggregation) Build() (map[string]interface{}, error) {
	return map[string]interface{}{
		"max": map[string]interface{}{
			"field": a.field,
		},
	}, nil
}

// StatsAggregation 统计聚合（同时返回count, min, max, avg, sum）
type StatsAggregation struct {
	name  string
	field string
}

// NewStatsAggregation 创建统计聚合
// 参数:
//
//	name: 聚合名称
//	field: 字段
//
// 返回:
//
//	*StatsAggregation: 统计聚合实例
func NewStatsAggregation(name, field string) *StatsAggregation {
	return &StatsAggregation{
		name:  name,
		field: field,
	}
}

// Name 获取聚合名称
func (a *StatsAggregation) Name() string {
	return a.name
}

// Build 构建聚合
func (a *StatsAggregation) Build() (map[string]interface{}, error) {
	return map[string]interface{}{
		"stats": map[string]interface{}{
			"field": a.field,
		},
	}, nil
}

// CardinalityAggregation 基数聚合（去重计数）
type CardinalityAggregation struct {
	name  string
	field string
}

// NewCardinalityAggregation 创建基数聚合
// 参数:
//
//	name: 聚合名称
//	field: 字段
//
// 返回:
//
//	*CardinalityAggregation: 基数聚合实例
func NewCardinalityAggregation(name, field string) *CardinalityAggregation {
	return &CardinalityAggregation{
		name:  name,
		field: field,
	}
}

// Name 获取聚合名称
func (a *CardinalityAggregation) Name() string {
	return a.name
}

// Build 构建聚合
func (a *CardinalityAggregation) Build() (map[string]interface{}, error) {
	return map[string]interface{}{
		"cardinality": map[string]interface{}{
			"field": a.field,
		},
	}, nil
}

// NestedAggregation 嵌套聚合（在桶聚合中嵌套指标聚合）
type NestedAggregation struct {
	parent Aggregation
	sub    []Aggregation
}

// NewNestedAggregation 创建嵌套聚合
// 参数:
//
//	parent: 父聚合（桶聚合）
//	sub: 子聚合列表（指标聚合）
//
// 返回:
//
//	*NestedAggregation: 嵌套聚合实例
func NewNestedAggregation(parent Aggregation, sub []Aggregation) *NestedAggregation {
	return &NestedAggregation{
		parent: parent,
		sub:    sub,
	}
}

// Build 构建嵌套聚合
func (n *NestedAggregation) Build() (map[string]interface{}, error) {
	parentMap, err := n.parent.Build()
	if err != nil {
		return nil, err
	}

	// 添加子聚合
	if len(n.sub) > 0 {
		aggs := make(map[string]interface{})
		for _, subAgg := range n.sub {
			subMap, err := subAgg.Build()
			if err != nil {
				return nil, err
			}
			// subMap 的结构是 {aggType: aggDef}，需要提取出来
			for _, aggDef := range subMap {
				// 找到聚合定义，然后包装到 aggs 中
				_ = parentMap[n.parent.Name()]
				aggs[subAgg.Name()] = aggDef
			}
		}
		// 将子聚合添加到父聚合中
		for k, v := range parentMap {
			aggDef := v.(map[string]interface{})
			aggDef["aggs"] = aggs
			parentMap[k] = aggDef
		}
	}

	return parentMap, nil
}

// Name 获取聚合名称（使用父聚合名称）
func (n *NestedAggregation) Name() string {
	return n.parent.Name()
}

// TermsBucket Terms聚合桶
type TermsBucket struct {
	Key      interface{} `json:"key"`
	DocCount int         `json:"doc_count"`
}

// TermsAggregationResult Terms聚合结果
type TermsAggregationResult struct {
	Buckets []TermsBucket `json:"buckets"`
}

// HistogramBucket 直方图聚合桶
type HistogramBucket struct {
	Key      float64 `json:"key"`
	DocCount int     `json:"doc_count"`
}

// DateHistogramBucket 日期直方图聚合桶
type DateHistogramBucket struct {
	Key      int64 `json:"key"`
	DocCount int   `json:"doc_count"`
}

// StatsResult 统计聚合结果
type StatsResult struct {
	Count int     `json:"count"`
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
	Avg   float64 `json:"avg"`
	Sum   float64 `json:"sum"`
}

// ParseTermsResult 解析Terms聚合结果
// 参数:
//
//	raw: 原始聚合数据（来自SearchResponse.Aggregations）
//	aggName: 聚合名称
//
// 返回:
//
//	*TermsAggregationResult: 解析后的结果
//	error: 解析错误
func ParseTermsResult(raw json.RawMessage, aggName string) (*TermsAggregationResult, error) {
	var result map[string]TermsAggregationResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}

	aggResult, ok := result[aggName]
	if !ok {
		return nil, nil
	}

	return &aggResult, nil
}

// ParseStatsResult 解析统计聚合结果
// 参数:
//
//	raw: 原始聚合数据
//	aggName: 聚合名称
//
// 返回:
//
//	*StatsResult: 解析后的结果
//	error: 解析错误
func ParseStatsResult(raw json.RawMessage, aggName string) (*StatsResult, error) {
	var result map[string]StatsResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}

	aggResult, ok := result[aggName]
	if !ok {
		return nil, nil
	}

	return &aggResult, nil
}
