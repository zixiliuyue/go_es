// Package ingest 提供Elasticsearch Ingest Pipeline的管理能力
// 支持Pipeline的创建、查询、删除、模拟执行以及写入时附加Pipeline
// 常见用途:日志解析(split/grok)、字段加工(rename/script)、时间字段解析(date)
package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/zixiliuyue/go_es/pkg/errors"
	"go.uber.org/zap"
)

// Manager ingest 管理器
type Manager struct {
	es  *elasticsearch.Client
	log *zap.Logger
}

// NewManager 创建一个新的ingest管理器
// 参数:
//
//	es: Elasticsearch客户端
//	log: 日志记录器(为nil时使用zap.NewProduction())
//
// 返回:
//
//	*Manager: ingest管理器实例
func NewManager(es *elasticsearch.Client, log *zap.Logger) *Manager {
	if log == nil {
		log, _ = zap.NewProduction()
	}
	return &Manager{es: es, log: log}
}

// Pipeline 管道定义
// Description 与 Processors 字段顺序与ES一致
type Pipeline struct {
	Description string                   `json:"description,omitempty"`
	Version     int                      `json:"version,omitempty"`
	Processors  []map[string]interface{} `json:"processors"`
	OnFailure   []map[string]interface{} `json:"on_failure,omitempty"`
}

// PutPipeline 创建或更新一个Pipeline
// 参数:
//
//	ctx: 上下文
//	name: 管道名
//	pipeline: 管道定义
//
// 返回:
//
//	error: 失败时返回错误
func (m *Manager) PutPipeline(ctx context.Context, name string, pipeline Pipeline) error {
	if name == "" {
		return errors.New(errors.ErrCodeUnknown, "pipeline name must not be empty")
	}
	if len(pipeline.Processors) == 0 {
		return errors.New(errors.ErrCodeUnknown, "pipeline processors must not be empty")
	}

	payload, err := json.Marshal(pipeline)
	if err != nil {
		m.log.Error("Failed to marshal pipeline", zap.String("name", name), zap.Error(err))
		return errors.Wrap(errors.ErrCodeMarshalJSON, "failed to marshal pipeline", err)
	}

	res, err := m.es.Ingest.PutPipeline(
		name,
		bytes.NewReader(payload),
		m.es.Ingest.PutPipeline.WithContext(ctx),
	)
	if err != nil {
		m.log.Error("Failed to put pipeline", zap.String("name", name), zap.Error(err))
		return errors.Wrap(errors.ErrCodeUnknown, "failed to put pipeline", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		respBody, _ := io.ReadAll(res.Body)
		m.log.Error("Put pipeline returned error",
			zap.String("name", name),
			zap.Int("status", res.StatusCode),
			zap.String("response", string(respBody)))
		return errors.New(errors.ErrCodeUnknown, string(respBody))
	}

	m.log.Info("Ingest pipeline created/updated", zap.String("name", name))
	return nil
}

// GetPipeline 获取一个Pipeline
// 参数:
//
//	ctx: 上下文
//	name: 管道名
//
// 返回:
//
//	*Pipeline: 管道定义
//	error: 失败时返回错误
func (m *Manager) GetPipeline(ctx context.Context, name string) (*Pipeline, error) {
	res, err := m.es.Ingest.GetPipeline(
		m.es.Ingest.GetPipeline.WithContext(ctx),
		m.es.Ingest.GetPipeline.WithPipelineID(name),
	)
	if err != nil {
		m.log.Error("Failed to get pipeline", zap.String("name", name), zap.Error(err))
		return nil, errors.Wrap(errors.ErrCodeUnknown, "failed to get pipeline", err)
	}
	defer res.Body.Close()

	if res.StatusCode == 404 {
		return nil, errors.ErrIndexNotFound
	}
	if res.IsError() {
		respBody, _ := io.ReadAll(res.Body)
		return nil, errors.New(errors.ErrCodeUnknown, string(respBody))
	}

	var raw map[string]Pipeline
	if err := json.NewDecoder(res.Body).Decode(&raw); err != nil {
		return nil, err
	}
	p, ok := raw[name]
	if !ok {
		return nil, errors.New(errors.ErrCodeIndexNotFound,
			"pipeline not found: "+name)
	}
	return &p, nil
}

// DeletePipeline 删除一个Pipeline
// 参数:
//
//	ctx: 上下文
//	name: 管道名
//
// 返回:
//
//	error: 失败时返回错误
func (m *Manager) DeletePipeline(ctx context.Context, name string) error {
	res, err := m.es.Ingest.DeletePipeline(
		name,
		m.es.Ingest.DeletePipeline.WithContext(ctx),
	)
	if err != nil {
		m.log.Error("Failed to delete pipeline", zap.String("name", name), zap.Error(err))
		return errors.Wrap(errors.ErrCodeUnknown, "failed to delete pipeline", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		respBody, _ := io.ReadAll(res.Body)
		return errors.New(errors.ErrCodeUnknown, string(respBody))
	}
	m.log.Info("Ingest pipeline deleted", zap.String("name", name))
	return nil
}

// SimulateResult Simulate 响应
type SimulateResult struct {
	Docs []SimulatedDoc `json:"docs"`
}

// SimulatedDoc 单文档模拟结果
type SimulatedDoc struct {
	Doc map[string]interface{} `json:"doc"`
}

// Simulate 模拟执行管道,返回处理后的文档
// 参数:
//
//	ctx: 上下文
//	pipeline: 待模拟的管道
//	docs: 原始文档列表,每个元素为一个 _source JSON
//
// 返回:
//
//	*SimulateResult: 模拟结果
//	error: 执行错误
func (m *Manager) Simulate(ctx context.Context, pipeline Pipeline, docs []map[string]interface{}) (*SimulateResult, error) {
	body := map[string]interface{}{
		"pipeline": pipeline,
		"docs":     docs,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeMarshalJSON, "failed to marshal simulate body", err)
	}

	res, err := m.es.Ingest.Simulate(
		bytes.NewReader(payload),
		m.es.Ingest.Simulate.WithContext(ctx),
	)
	if err != nil {
		m.log.Error("Failed to simulate pipeline", zap.Error(err))
		return nil, errors.Wrap(errors.ErrCodeUnknown, "failed to simulate pipeline", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		respBody, _ := io.ReadAll(res.Body)
		return nil, errors.New(errors.ErrCodeUnknown, string(respBody))
	}

	var out SimulateResult
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// IndexWithPipeline 索引文档并应用指定管道
// 与 pkg/document 的区别:这里直接调用 esapi.Index 并附加 ?pipeline=xxx
// 参数:
//
//	ctx: 上下文
//	index: 索引名
//	docID: 文档ID(空字符串时自动生成)
//	pipelineName: 要应用的管道名
//	doc: 文档内容
//
// 返回:
//
//	error: 写入错误
func (m *Manager) IndexWithPipeline(ctx context.Context, index, docID, pipelineName string, doc interface{}) error {
	if pipelineName == "" {
		return errors.New(errors.ErrCodeUnknown, "pipeline name must not be empty")
	}

	payload, err := json.Marshal(doc)
	if err != nil {
		m.log.Error("Failed to marshal document",
			zap.String("index", index), zap.Error(err))
		return errors.Wrap(errors.ErrCodeMarshalJSON, "failed to marshal document", err)
	}

	// 构造请求参数:带 ctx、pipeline、可选 docID
	res, err := m.es.Index(index, bytes.NewReader(payload),
		m.es.Index.WithContext(ctx),
		m.es.Index.WithPipeline(pipelineName),
	)
	if docID != "" {
		res, err = m.es.Index(index, bytes.NewReader(payload),
			m.es.Index.WithContext(ctx),
			m.es.Index.WithPipeline(pipelineName),
			m.es.Index.WithDocumentID(docID),
		)
	}
	if err != nil {
		return errors.Wrap(errors.ErrCodeDocumentCreate, "failed to index with pipeline", err)
	}
	defer res.Body.Close()
	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		return errors.New(errors.ErrCodeDocumentCreate, string(body))
	}
	return nil
}
