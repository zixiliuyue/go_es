// Package index 提供Elasticsearch索引管理功能
// 负责索引的创建、删除、检查存在性等操作
package index

import (
	"bytes"
	"context"
	"encoding/json"
	"io"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/esapi"
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

// CreateTemplate 创建索引模板
// 参数:
//
//	ctx: 上下文
//	name: 模板名称
//	template: 模板定义（包含mappings, settings等）
//
// 返回:
//
//	error: 创建失败返回错误
func (m *Manager) CreateTemplate(ctx context.Context, name string, template map[string]interface{}) error {
	templateBytes, err := json.Marshal(template)
	if err != nil {
		m.log.Error("Failed to marshal template",
			zap.String("name", name),
			zap.Error(err))
		return errors.Wrap(errors.ErrCodeMarshalJSON,
			"failed to marshal template", err)
	}

	res, err := m.es.Indices.PutIndexTemplate(
		name,
		bytes.NewReader(templateBytes),
		m.es.Indices.PutIndexTemplate.WithContext(ctx),
	)
	if err != nil {
		m.log.Error("Failed to create index template",
			zap.String("name", name),
			zap.Error(err))
		return err
	}
	defer res.Body.Close()

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		m.log.Error("Create index template returned error",
			zap.String("name", name),
			zap.Int("status", res.StatusCode),
			zap.String("response", string(body)))
		return errors.New(errors.ErrCodeCreateIndex, string(body))
	}

	m.log.Info("Index template created successfully", zap.String("name", name))
	return nil
}

// DeleteTemplate 删除索引模板
// 参数:
//
//	ctx: 上下文
//	name: 模板名称
//
// 返回:
//
//	error: 删除失败返回错误
func (m *Manager) DeleteTemplate(ctx context.Context, name string) error {
	res, err := m.es.Indices.DeleteIndexTemplate(name,
		m.es.Indices.DeleteIndexTemplate.WithContext(ctx),
	)
	if err != nil {
		m.log.Error("Failed to delete index template",
			zap.String("name", name),
			zap.Error(err))
		return err
	}
	defer res.Body.Close()

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		m.log.Error("Delete index template returned error",
			zap.String("name", name),
			zap.Int("status", res.StatusCode),
			zap.String("response", string(body)))
		return errors.New(errors.ErrCodeDeleteIndex, string(body))
	}

	m.log.Info("Index template deleted successfully", zap.String("name", name))
	return nil
}

// TemplateExists 检查索引模板是否存在
// 参数:
//
//	ctx: 上下文
//	name: 模板名称
//
// 返回:
//
//	bool: 模板是否存在
//	error: 检查失败返回错误
func (m *Manager) TemplateExists(ctx context.Context, name string) (bool, error) {
	res, err := m.es.Indices.ExistsIndexTemplate(
		name,
		m.es.Indices.ExistsIndexTemplate.WithContext(ctx),
	)
	if err != nil {
		m.log.Error("Failed to check if template exists",
			zap.String("name", name),
			zap.Error(err))
		return false, err
	}
	defer res.Body.Close()

	// 200 means exists, 404 means not exists
	return res.StatusCode == 200, nil
}

// GetTemplate 获取索引模板
// 参数:
//
//	ctx: 上下文
//	name: 模板名称
//
// 返回:
//
//	map[string]interface{}: 模板定义
//	error: 获取失败返回错误
func (m *Manager) GetTemplate(ctx context.Context, name string) (map[string]interface{}, error) {
	res, err := m.es.Indices.GetIndexTemplate(
		m.es.Indices.GetIndexTemplate.WithName(name),
		m.es.Indices.GetIndexTemplate.WithContext(ctx),
	)
	if err != nil {
		m.log.Error("Failed to get index template",
			zap.String("name", name),
			zap.Error(err))
		return nil, err
	}
	defer res.Body.Close()

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		m.log.Error("Get index template returned error",
			zap.String("name", name),
			zap.Int("status", res.StatusCode),
			zap.String("response", string(body)))
		return nil, errors.New(errors.ErrCodeCreateIndex, string(body))
	}

	var result map[string]interface{}
	if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
		m.log.Error("Failed to decode template response",
			zap.String("name", name),
			zap.Error(err))
		return nil, err
	}

	return result, nil
}

// CreateIndexWithTemplate 使用模板创建索引
// 参数:
//
//	ctx: 上下文
//	indexName: 索引名称
//
// 返回:
//
//	error: 创建失败返回错误
func (m *Manager) CreateIndexWithTemplate(ctx context.Context, indexName string) error {
	// 先检查索引是否已经存在
	exists, err := m.IndexExists(ctx, indexName)
	if err != nil {
		return err
	}
	if exists {
		m.log.Warn("Index already exists", zap.String("index", indexName))
		return errors.ErrIndexExists
	}

	// 创建索引，会自动应用匹配的模板
	res, err := m.es.Indices.Create(
		indexName,
		m.es.Indices.Create.WithContext(ctx),
	)
	if err != nil {
		m.log.Error("Failed to create index with template",
			zap.String("index", indexName),
			zap.Error(err))
		return errors.Wrap(errors.ErrCodeCreateIndex,
			"failed to create index", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		m.log.Error("Create index with template returned error",
			zap.String("index", indexName),
			zap.Int("status", res.StatusCode),
			zap.String("response", string(body)))
		return errors.New(errors.ErrCodeCreateIndex, string(body))
	}

	m.log.Info("Index created with template successfully", zap.String("index", indexName))
	return nil
}

// ListIndices 列出所有索引
// 参数:
//
//	ctx: 上下文
//	pattern: 索引名称匹配模式（如 "*", "log-*"），为空则匹配所有
//
// 返回:
//
//	[]string: 索引名称列表
//	error: 获取失败返回错误
func (m *Manager) ListIndices(ctx context.Context, pattern string) ([]string, error) {
	if pattern == "" {
		pattern = "*"
	}

	res, err := m.es.Cat.Indices(
		m.es.Cat.Indices.WithContext(ctx),
		m.es.Cat.Indices.WithIndex(pattern),
		m.es.Cat.Indices.WithFormat("json"),
	)
	if err != nil {
		m.log.Error("Failed to list indices",
			zap.String("pattern", pattern),
			zap.Error(err))
		return nil, errors.Wrap(errors.ErrCodeUnknown,
			"failed to list indices", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		m.log.Error("List indices returned error",
			zap.String("pattern", pattern),
			zap.Int("status", res.StatusCode),
			zap.String("response", string(body)))
		return nil, errors.New(errors.ErrCodeUnknown, string(body))
	}

	var indices []struct {
		Index string `json:"index"`
	}

	if err := json.NewDecoder(res.Body).Decode(&indices); err != nil {
		m.log.Error("Failed to decode indices response",
			zap.String("pattern", pattern),
			zap.Error(err))
		return nil, err
	}

	result := make([]string, 0, len(indices))
	for _, idx := range indices {
		result = append(result, idx.Index)
	}

	return result, nil
}

// IndexStats 获取索引统计信息
// 参数:
//
//	ctx: 上下文
//	indexName: 索引名称
//
// 返回:
//
//	map[string]interface{}: 统计信息
//	error: 获取失败返回错误
func (m *Manager) IndexStats(ctx context.Context, indexName string) (map[string]interface{}, error) {
	exists, err := m.IndexExists(ctx, indexName)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errors.ErrIndexNotFound
	}

	res, err := m.es.Indices.Stats(
		m.es.Indices.Stats.WithContext(ctx),
		m.es.Indices.Stats.WithIndex(indexName),
	)
	if err != nil {
		m.log.Error("Failed to get index stats",
			zap.String("index", indexName),
			zap.Error(err))
		return nil, errors.Wrap(errors.ErrCodeUnknown,
			"failed to get index stats", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		m.log.Error("Get index stats returned error",
			zap.String("index", indexName),
			zap.Int("status", res.StatusCode),
			zap.String("response", string(body)))
		return nil, errors.New(errors.ErrCodeUnknown, string(body))
	}

	var result map[string]interface{}
	if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
		m.log.Error("Failed to decode index stats response",
			zap.String("index", indexName),
			zap.Error(err))
		return nil, err
	}

	return result, nil
}

// CloseIndex 关闭索引
// 参数:
//
//	ctx: 上下文
//	indexName: 索引名称
//
// 返回:
//
//	error: 关闭失败返回错误
func (m *Manager) CloseIndex(ctx context.Context, indexName string) error {
	exists, err := m.IndexExists(ctx, indexName)
	if err != nil {
		return err
	}
	if !exists {
		return errors.ErrIndexNotFound
	}

	res, err := m.es.Indices.Close(
		[]string{indexName},
		m.es.Indices.Close.WithContext(ctx),
	)
	if err != nil {
		m.log.Error("Failed to close index",
			zap.String("index", indexName),
			zap.Error(err))
		return errors.Wrap(errors.ErrCodeUnknown,
			"failed to close index", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		m.log.Error("Close index returned error",
			zap.String("index", indexName),
			zap.Int("status", res.StatusCode),
			zap.String("response", string(body)))
		return errors.New(errors.ErrCodeUnknown, string(body))
	}

	m.log.Info("Index closed successfully", zap.String("index", indexName))
	return nil
}

// OpenIndex 打开已关闭的索引
// 参数:
//
//	ctx: 上下文
//	indexName: 索引名称
//
// 返回:
//
//	error: 打开失败返回错误
func (m *Manager) OpenIndex(ctx context.Context, indexName string) error {
	exists, err := m.IndexExists(ctx, indexName)
	if err != nil {
		return err
	}
	if !exists {
		return errors.ErrIndexNotFound
	}

	res, err := m.es.Indices.Open(
		[]string{indexName},
		m.es.Indices.Open.WithContext(ctx),
	)
	if err != nil {
		m.log.Error("Failed to open index",
			zap.String("index", indexName),
			zap.Error(err))
		return errors.Wrap(errors.ErrCodeUnknown,
			"failed to open index", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		m.log.Error("Open index returned error",
			zap.String("index", indexName),
			zap.Int("status", res.StatusCode),
			zap.String("response", string(body)))
		return errors.New(errors.ErrCodeUnknown, string(body))
	}

	m.log.Info("Index opened successfully", zap.String("index", indexName))
	return nil
}

// ForceMerge 强制合并索引分段
// 参数:
//
//	ctx: 上下文
//	indexName: 索引名称
//	maxNumSegments: 最大分段数
//
// 返回:
//
//	error: 合并失败返回错误
func (m *Manager) ForceMerge(ctx context.Context, indexName string, maxNumSegments int) error {
	exists, err := m.IndexExists(ctx, indexName)
	if err != nil {
		return err
	}
	if !exists {
		return errors.ErrIndexNotFound
	}

	var res *esapi.Response
	if maxNumSegments > 0 {
		res, err = m.es.Indices.Forcemerge(
			m.es.Indices.Forcemerge.WithContext(ctx),
			m.es.Indices.Forcemerge.WithIndex(indexName),
			m.es.Indices.Forcemerge.WithMaxNumSegments(maxNumSegments),
		)
	} else {
		res, err = m.es.Indices.Forcemerge(
			m.es.Indices.Forcemerge.WithContext(ctx),
			m.es.Indices.Forcemerge.WithIndex(indexName),
		)
	}
	if err != nil {
		m.log.Error("Failed to force merge index",
			zap.String("index", indexName),
			zap.Error(err))
		return errors.Wrap(errors.ErrCodeUnknown,
			"failed to force merge index", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		m.log.Error("Force merge returned error",
			zap.String("index", indexName),
			zap.Int("status", res.StatusCode),
			zap.String("response", string(body)))
		return errors.New(errors.ErrCodeUnknown, string(body))
	}

	m.log.Info("Index force merged successfully",
		zap.String("index", indexName),
		zap.Int("max_segments", maxNumSegments))
	return nil
}

// ClearCache 清除索引缓存
// 参数:
//
//	ctx: 上下文
//	indexName: 索引名称
//
// 返回:
//
//	error: 清除失败返回错误
func (m *Manager) ClearCache(ctx context.Context, indexName string) error {
	exists, err := m.IndexExists(ctx, indexName)
	if err != nil {
		return err
	}
	if !exists {
		return errors.ErrIndexNotFound
	}

	res, err := m.es.Indices.ClearCache(
		m.es.Indices.ClearCache.WithContext(ctx),
		m.es.Indices.ClearCache.WithIndex(indexName),
	)
	if err != nil {
		m.log.Error("Failed to clear cache",
			zap.String("index", indexName),
			zap.Error(err))
		return errors.Wrap(errors.ErrCodeUnknown,
			"failed to clear cache", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		m.log.Error("Clear cache returned error",
			zap.String("index", indexName),
			zap.Int("status", res.StatusCode),
			zap.String("response", string(body)))
		return errors.New(errors.ErrCodeUnknown, string(body))
	}

	m.log.Info("Index cache cleared", zap.String("index", indexName))
	return nil
}

// Refresh 刷新索引，使最近的更改可搜索
// 参数:
//
//	ctx: 上下文
//	indexName: 索引名称
//
// 返回:
//
//	error: 刷新失败返回错误
func (m *Manager) Refresh(ctx context.Context, indexName string) error {
	exists, err := m.IndexExists(ctx, indexName)
	if err != nil {
		return err
	}
	if !exists {
		return errors.ErrIndexNotFound
	}

	res, err := m.es.Indices.Refresh(
		m.es.Indices.Refresh.WithContext(ctx),
		m.es.Indices.Refresh.WithIndex(indexName),
	)
	if err != nil {
		m.log.Error("Failed to refresh index",
			zap.String("index", indexName),
			zap.Error(err))
		return errors.Wrap(errors.ErrCodeUnknown,
			"failed to refresh index", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		m.log.Error("Refresh returned error",
			zap.String("index", indexName),
			zap.Int("status", res.StatusCode),
			zap.String("response", string(body)))
		return errors.New(errors.ErrCodeUnknown, string(body))
	}

	return nil
}

// Flush 将数据刷新到磁盘
// 参数:
//
//	ctx: 上下文
//	indexName: 索引名称
//
// 返回:
//
//	error: 刷新失败返回错误
func (m *Manager) Flush(ctx context.Context, indexName string) error {
	exists, err := m.IndexExists(ctx, indexName)
	if err != nil {
		return err
	}
	if !exists {
		return errors.ErrIndexNotFound
	}

	res, err := m.es.Indices.Flush(
		m.es.Indices.Flush.WithContext(ctx),
		m.es.Indices.Flush.WithIndex(indexName),
	)
	if err != nil {
		m.log.Error("Failed to flush index",
			zap.String("index", indexName),
			zap.Error(err))
		return errors.Wrap(errors.ErrCodeUnknown,
			"failed to flush index", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		m.log.Error("Flush returned error",
			zap.String("index", indexName),
			zap.Int("status", res.StatusCode),
			zap.String("response", string(body)))
		return errors.New(errors.ErrCodeUnknown, string(body))
	}

	return nil
}

// UpdateSettings 更新索引设置
// 参数:
//
//	ctx: 上下文
//	indexName: 索引名称
//	settings: 设置map
//
// 返回:
//
//	error: 更新失败返回错误
func (m *Manager) UpdateSettings(ctx context.Context, indexName string, settings map[string]interface{}) error {
	exists, err := m.IndexExists(ctx, indexName)
	if err != nil {
		return err
	}
	if !exists {
		return errors.ErrIndexNotFound
	}

	settingsBytes, err := json.Marshal(settings)
	if err != nil {
		m.log.Error("Failed to marshal settings",
			zap.String("index", indexName),
			zap.Error(err))
		return errors.Wrap(errors.ErrCodeMarshalJSON,
			"failed to marshal settings", err)
	}

	res, err := m.es.Indices.PutSettings(
		bytes.NewReader(settingsBytes),
		m.es.Indices.PutSettings.WithContext(ctx),
		m.es.Indices.PutSettings.WithIndex(indexName),
	)
	if err != nil {
		m.log.Error("Failed to update index settings",
			zap.String("index", indexName),
			zap.Error(err))
		return errors.Wrap(errors.ErrCodeUnknown,
			"failed to update index settings", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		m.log.Error("Update settings returned error",
			zap.String("index", indexName),
			zap.Int("status", res.StatusCode),
			zap.String("response", string(body)))
		return errors.New(errors.ErrCodeUnknown, string(body))
	}

	m.log.Info("Index settings updated", zap.String("index", indexName))
	return nil
}
