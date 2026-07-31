// Package index 提供Elasticsearch索引管理功能
// 负责索引的创建、删除、检查存在性等操作
package index

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/zixiliuyue/go_es/pkg/errors"
	"go.uber.org/zap"
)

// Manager 索引管理器
type Manager struct {
	es  *elasticsearch.Client
	log *zap.Logger
}

// NewManager 创建一个新的索引管理器
// 参数:
//
//	es: Elasticsearch客户端
//	log: 日志记录器
//
// 返回:
//
//	*Manager: 索引管理器实例
func NewManager(es *elasticsearch.Client, log *zap.Logger) *Manager {
	if log == nil {
		log, _ = zap.NewProduction()
	}
	return &Manager{
		es:  es,
		log: log,
	}
}

// CreateIndex 创建索引
// 参数:
//
//	ctx: 上下文
//	indexName: 索引名称
//	mapping: 索引映射（JSON格式）
//
// 返回:
//
//	error: 创建失败返回错误，成功返回nil
func (m *Manager) CreateIndex(ctx context.Context, indexName string, mapping map[string]interface{}) error {
	// 先检查索引是否已存在
	exists, err := m.IndexExists(ctx, indexName)
	if err != nil {
		m.log.Error("Failed to check index existence",
			zap.String("index", indexName),
			zap.Error(err))
		return errors.Wrap(errors.ErrCodeIndexExistsCheck,
			"failed to check index existence", err)
	}

	if exists {
		m.log.Warn("Index already exists", zap.String("index", indexName))
		return errors.ErrIndexExists
	}

	// 序列化mapping
	var mappingBytes []byte
	if mapping != nil {
		mappingBytes, err = json.Marshal(mapping)
		if err != nil {
			m.log.Error("Failed to marshal mapping",
				zap.String("index", indexName),
				zap.Error(err))
			return errors.Wrap(errors.ErrCodeMarshalJSON,
				"failed to marshal mapping", err)
		}
	}

	// 创建索引
	res, err := m.es.Indices.Create(indexName,
		m.es.Indices.Create.WithContext(ctx),
		m.es.Indices.Create.WithBody(bytes.NewReader(mappingBytes)),
	)
	if err != nil {
		m.log.Error("Failed to create index",
			zap.String("index", indexName),
			zap.Error(err))
		return errors.Wrap(errors.ErrCodeCreateIndex,
			"failed to create index", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		m.log.Error("Create index returned error",
			zap.String("index", indexName),
			zap.Int("status", res.StatusCode),
			zap.String("response", res.String()))
		return errors.New(errors.ErrCodeCreateIndex,
			res.String())
	}

	m.log.Info("Index created successfully", zap.String("index", indexName))
	return nil
}

// DeleteIndex 删除索引
// 参数:
//
//	ctx: 上下文
//	indexName: 索引名称
//
// 返回:
//
//	error: 删除失败返回错误，成功返回nil
func (m *Manager) DeleteIndex(ctx context.Context, indexName string) error {
	// 先检查索引是否存在
	exists, err := m.IndexExists(ctx, indexName)
	if err != nil {
		m.log.Error("Failed to check index existence",
			zap.String("index", indexName),
			zap.Error(err))
		return errors.Wrap(errors.ErrCodeIndexExistsCheck,
			"failed to check index existence", err)
	}

	if !exists {
		m.log.Warn("Index does not exist, skip deletion",
			zap.String("index", indexName))
		return errors.ErrIndexNotFound
	}

	// 删除索引
	res, err := m.es.Indices.Delete([]string{indexName},
		m.es.Indices.Delete.WithContext(ctx),
	)
	if err != nil {
		m.log.Error("Failed to delete index",
			zap.String("index", indexName),
			zap.Error(err))
		return errors.Wrap(errors.ErrCodeDeleteIndex,
			"failed to delete index", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		m.log.Error("Delete index returned error",
			zap.String("index", indexName),
			zap.Int("status", res.StatusCode),
			zap.String("response", res.String()))
		return errors.New(errors.ErrCodeDeleteIndex,
			res.String())
	}

	m.log.Info("Index deleted successfully", zap.String("index", indexName))
	return nil
}

// IndexExists 检查索引是否存在
// 参数:
//
//	ctx: 上下文
//	indexName: 索引名称
//
// 返回:
//
//	bool: 索引是否存在
//	error: 检查过程中的错误
func (m *Manager) IndexExists(ctx context.Context, indexName string) (bool, error) {
	res, err := m.es.Indices.Exists([]string{indexName},
		m.es.Indices.Exists.WithContext(ctx),
	)
	if err != nil {
		m.log.Error("Failed to check index existence",
			zap.String("index", indexName),
			zap.Error(err))
		return false, err
	}
	defer res.Body.Close()

	// 200-299 表示存在，404 表示不存在
	return res.StatusCode == 200, nil
}

// GetMapping 获取索引映射
// 参数:
//
//	ctx: 上下文
//	indexName: 索引名称
//
// 返回:
//
//	map[string]interface{}: 索引映射
//	error: 获取失败返回错误
func (m *Manager) GetMapping(ctx context.Context, indexName string) (map[string]interface{}, error) {
	exists, err := m.IndexExists(ctx, indexName)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errors.ErrIndexNotFound
	}

	res, err := m.es.Indices.GetMapping(
		m.es.Indices.GetMapping.WithContext(ctx),
		m.es.Indices.GetMapping.WithIndex(indexName),
	)
	if err != nil {
		m.log.Error("Failed to get mapping",
			zap.String("index", indexName),
			zap.Error(err))
		return nil, err
	}
	defer res.Body.Close()

	if res.IsError() {
		return nil, errors.New(errors.ErrCodeCreateIndex, res.String())
	}

	var result map[string]interface{}
	if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
		m.log.Error("Failed to decode mapping response",
			zap.String("index", indexName),
			zap.Error(err))
		return nil, err
	}

	return result, nil
}
