// Package suggest 提供Elasticsearch搜索建议功能
// 支持自动完成、短语建议、术语纠正等搜索推荐功能
package suggest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/zixiliuyue/go_es/pkg/errors"
	"go.uber.org/zap"
)

// SuggestType 建议类型
type SuggestType string

const (
	// CompletionSuggestion 完成建议（自动补全）
	CompletionSuggestion SuggestType = "completion"
	// PhraseSuggestion 短语建议
	PhraseSuggestion SuggestType = "phrase"
	// TermSuggestion 术语建议（拼写纠正）
	TermSuggestion SuggestType = "term"
)

// Completion 完成建议配置
type Completion struct {
	Field string `json:"field"`
	Size  int    `json:"size,omitempty"`
	Prefix string `json:"prefix"`
	Fuzzy  *bool  `json:"fuzzy,omitempty"` // 是否启用模糊匹配
}

// Phrase 短语建议配置
type Phrase struct {
	Field           string  `json:"field"`
	Size            int     `json:"size,omitempty"`
	MaxErrors       float64 `json:"max_errors,omitempty"`
	Confidence      float64 `json:"confidence,omitempty"`
	TokenLength     int     `json:"token_length,omitempty"`
}

// Term 术语建议配置
type Term struct {
	Field     string `json:"field"`
	Size      int    `json:"size,omitempty"`
	SuggestMode string `json:"suggest_mode,omitempty"` // missing, popular, always
}

// Suggestion 单个建议结果
type Suggestion struct {
	Text    string  `json:"text"`
	Score   float64 `json:"score"`
	ScoreRaw float64 `json:"_score,omitempty"`
}

// CompletionOption 完成建议选项
type CompletionOption struct {
	Text        string                 `json:"text"`
	Score       float64                `json:"_score"`
	Source      map[string]interface{} `json:"_source,omitempty"`
}

// SuggestRequest 搜索建议请求
type SuggestRequest struct {
	Text   string                     `json:"text"`
	Suggest map[string]interface{}    `json:"suggest"`
	Size   int                        `json:"size,omitempty"`
}

// SuggestResponse 搜索建议响应
type SuggestResponse struct {
	Text string `json:"text"`
	// 按建议名称分组的结果
	Suggestions map[string][]SuggestionResult `json:"-"`
}

// SuggestionResult 单个建议结果
type SuggestionResult struct {
	Text         string        `json:"text"`
	Offset       int           `json:"offset"`
	Length       int           `json:"length"`
	Options      []Suggestion `json:"options"`
}

// Suggester 搜索建议管理器
type Suggester struct {
	es  *elasticsearch.Client
	log *zap.Logger
}

// NewSuggester 创建一个新的搜索建议管理器
func NewSuggester(es *elasticsearch.Client, log *zap.Logger) *Suggester {
	if log == nil {
		log, _ = zap.NewProduction()
	}
	return &Suggester{
		es:  es,
		log: log,
	}
}

// Complete 执行自动完成建议
// 参数:
//
//	ctx: 上下文
//	index: 索引名称
//	field: 完成字段名（通常是xxx.completion）
//	prefix: 输入前缀
//	size: 返回结果数量
//	fuzzy: 是否启用模糊匹配
//
// 返回:
//
//	[]CompletionOption: 建议选项列表
//	error: 操作错误
func (s *Suggester) Complete(
	ctx context.Context,
	index string,
	field string,
	prefix string,
	size int,
	fuzzy bool,
) ([]CompletionOption, error) {
	if size <= 0 {
		size = 10
	}

	// 构建completion suggest
	completion := map[string]interface{}{
		"completion": map[string]interface{}{
			"field": field,
			"size":  size,
		},
	}

	if fuzzy {
		completion["completion"].(map[string]interface{})["fuzzy"] = map[string]interface{}{}
	}

	req := map[string]interface{}{
		"suggest": map[string]interface{}{
			"completion_suggest": completion,
		},
		"text": prefix,
	}

	reqBytes, err := json.Marshal(req)
	if err != nil {
		s.log.Error("Failed to marshal completion suggest request",
			zap.String("index", index),
			zap.String("prefix", prefix),
			zap.Error(err))
		return nil, errors.Wrap(errors.ErrCodeMarshalJSON,
			"failed to marshal completion suggest request", err)
	}

	res, err := s.es.Search(
		s.es.Search.WithContext(ctx),
		s.es.Search.WithIndex(index),
		s.es.Search.WithBody(bytes.NewReader(reqBytes)),
		s.es.Search.WithSize(0), // 不需要返回匹配的文档
	)
	if err != nil {
		s.log.Error("Failed to execute completion suggest",
			zap.String("index", index),
			zap.String("prefix", prefix),
			zap.Error(err))
		return nil, errors.Wrap(errors.ErrCodeSearch,
			"failed to execute completion suggest", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		s.log.Error("Completion suggest returned error",
			zap.String("index", index),
			zap.Int("status", res.StatusCode),
			zap.String("response", string(body)))
		return nil, errors.New(errors.ErrCodeSearch, string(body))
	}

	var result struct {
		Suggest struct {
			CompletionSuggest []struct {
				Options []CompletionOption `json:"options"`
			} `json:"completion_suggest"`
		} `json:"suggest"`
	}

	if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
		s.log.Error("Failed to decode completion suggest response",
			zap.String("index", index),
			zap.Error(err))
		return nil, err
	}

	if len(result.Suggest.CompletionSuggest) == 0 {
		return []CompletionOption{}, nil
	}

	return result.Suggest.CompletionSuggest[0].Options, nil
}

// TermSuggest 执行术语建议（拼写纠正）
// 参数:
//
//	ctx: 上下文
//	index: 索引名称
//	text: 输入文本
//	field: 建议字段
//
// 返回:
//
//	map[string][]Suggestion: 按词语分组的建议列表
//	error: 操作错误
func (s *Suggester) TermSuggest(
	ctx context.Context,
	index string,
	text string,
	field string,
) (map[string][]Suggestion, error) {
	req := map[string]interface{}{
		"suggest": map[string]interface{}{
			"term_suggest": map[string]interface{}{
				"term": map[string]interface{}{
					"field": field,
					"size":  5,
				},
			},
		},
		"text": text,
	}

	reqBytes, err := json.Marshal(req)
	if err != nil {
		s.log.Error("Failed to marshal term suggest request",
			zap.String("index", index),
			zap.String("text", text),
			zap.Error(err))
		return nil, errors.Wrap(errors.ErrCodeMarshalJSON,
			"failed to marshal term suggest request", err)
	}

	res, err := s.es.Search(
		s.es.Search.WithContext(ctx),
		s.es.Search.WithIndex(index),
		s.es.Search.WithBody(bytes.NewReader(reqBytes)),
		s.es.Search.WithSize(0),
	)
	if err != nil {
		s.log.Error("Failed to execute term suggest",
			zap.String("index", index),
			zap.String("text", text),
			zap.Error(err))
		return nil, errors.Wrap(errors.ErrCodeSearch,
			"failed to execute term suggest", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		s.log.Error("Term suggest returned error",
			zap.String("index", index),
			zap.Int("status", res.StatusCode),
			zap.String("response", string(body)))
		return nil, errors.New(errors.ErrCodeSearch, string(body))
	}

	var result struct {
		Suggest map[string][]struct {
			Text    string        `json:"text"`
			Options []Suggestion `json:"options"`
		} `json:"suggest"`
	}

	if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
		s.log.Error("Failed to decode term suggest response",
			zap.String("index", index),
			zap.Error(err))
		return nil, err
	}

	suggestions := make(map[string][]Suggestion)
	if termResults, ok := result.Suggest["term_suggest"]; ok && len(termResults) > 0 {
		for _, termResult := range termResults {
			suggestions[termResult.Text] = termResult.Options
		}
	}

	return suggestions, nil
}

// PhraseSuggest 执行短语建议
// 参数:
//
//	ctx: 上下文
//	index: 索引名称
//	text: 输入文本
//	field: 建议字段（通常是text字段的phrase）
//	size: 返回结果数量
//
// 返回:
//
//	[]Suggestion: 短语建议列表
//	error: 操作错误
func (s *Suggester) PhraseSuggest(
	ctx context.Context,
	index string,
	text string,
	field string,
	size int,
) ([]Suggestion, error) {
	if size <= 0 {
		size = 5
	}

	req := map[string]interface{}{
		"suggest": map[string]interface{}{
			"phrase_suggest": map[string]interface{}{
				"phrase": map[string]interface{}{
					"field": field,
					"size":  size,
				},
			},
		},
		"text": text,
	}

	reqBytes, err := json.Marshal(req)
	if err != nil {
		s.log.Error("Failed to marshal phrase suggest request",
			zap.String("index", index),
			zap.String("text", text),
			zap.Error(err))
		return nil, errors.Wrap(errors.ErrCodeMarshalJSON,
			"failed to marshal phrase suggest request", err)
	}

	res, err := s.es.Search(
		s.es.Search.WithContext(ctx),
		s.es.Search.WithIndex(index),
		s.es.Search.WithBody(bytes.NewReader(reqBytes)),
		s.es.Search.WithSize(0),
	)
	if err != nil {
		s.log.Error("Failed to execute phrase suggest",
			zap.String("index", index),
			zap.String("text", text),
			zap.Error(err))
		return nil, errors.Wrap(errors.ErrCodeSearch,
			"failed to execute phrase suggest", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		s.log.Error("Phrase suggest returned error",
			zap.String("index", index),
			zap.Int("status", res.StatusCode),
			zap.String("response", string(body)))
		return nil, errors.New(errors.ErrCodeSearch, string(body))
	}

	var result struct {
		Suggest map[string][]struct {
			Text    string        `json:"text"`
			Options []Suggestion `json:"options"`
		} `json:"suggest"`
	}

	if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
		s.log.Error("Failed to decode phrase suggest response",
			zap.String("index", index),
			zap.Error(err))
		return nil, err
	}

	if phraseResults, ok := result.Suggest["phrase_suggest"]; ok && len(phraseResults) > 0 {
		return phraseResults[0].Options, nil
	}

	return []Suggestion{}, nil
}

// MultiSuggest 执行多种类型的建议
// 可以同时获取完成建议、短语建议、术语建议
func (s *Suggester) MultiSuggest(
	ctx context.Context,
	index string,
	text string,
	suggestConfigs map[string]map[string]interface{},
) (map[string][]SuggestionResult, error) {
	req := map[string]interface{}{
		"suggest": suggestConfigs,
		"text":    text,
	}

	reqBytes, err := json.Marshal(req)
	if err != nil {
		s.log.Error("Failed to marshal multi suggest request",
			zap.String("index", index),
			zap.String("text", text),
			zap.Error(err))
		return nil, errors.Wrap(errors.ErrCodeMarshalJSON,
			"failed to marshal multi suggest request", err)
	}

	res, err := s.es.Search(
		s.es.Search.WithContext(ctx),
		s.es.Search.WithIndex(index),
		s.es.Search.WithBody(bytes.NewReader(reqBytes)),
		s.es.Search.WithSize(0),
	)
	if err != nil {
		s.log.Error("Failed to execute multi suggest",
			zap.String("index", index),
			zap.String("text", text),
			zap.Error(err))
		return nil, errors.Wrap(errors.ErrCodeSearch,
			"failed to execute multi suggest", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		s.log.Error("Multi suggest returned error",
			zap.String("index", index),
			zap.Int("status", res.StatusCode),
			zap.String("response", string(body)))
		return nil, errors.New(errors.ErrCodeSearch, string(body))
	}

	var result struct {
		Suggest map[string][]SuggestionResult `json:"suggest"`
	}

	if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
		s.log.Error("Failed to decode multi suggest response",
			zap.String("index", index),
			zap.Error(err))
		return nil, err
	}

	return result.Suggest, nil
}
