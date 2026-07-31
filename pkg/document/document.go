// Package document 提供Elasticsearch文档操作功能
// 负责文档的增删改查(CRUD)和批量操作
package document

import (
	"bytes"
	"context"
	"encoding/json"
	"io"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/esapi"
	"github.com/elastic/go-elasticsearch/v8/esutil"
	"github.com/zixiliuyue/go_es/pkg/errors"
	"go.uber.org/zap"
)

// Manager 文档管理器
type Manager struct {
	es  *elasticsearch.Client
	log *zap.Logger
}

// NewManager 创建一个新的文档管理器
// 参数:
//
//	es: Elasticsearch客户端
//	log: 日志记录器
//
// 返回:
//
//	*Manager: 文档管理器实例
func NewManager(es *elasticsearch.Client, log *zap.Logger) *Manager {
	if log == nil {
		log, _ = zap.NewProduction()
	}
	return &Manager{
		es:  es,
		log: log,
	}
}

// IndexResponse 索引操作响应
type IndexResponse struct {
	Index   string `json:"_index"`
	ID      string `json:"_id"`
	Version int    `json:"_version"`
	Result  string `json:"result"`
	Created bool   `json:"created"`
}

// GetResponse 获取文档响应
type GetResponse struct {
	Index   string          `json:"_index"`
	ID      string          `json:"_id"`
	Version int             `json:"_version"`
	Source  json.RawMessage `json:"_source"`
	Found   bool            `json:"found"`
}

// Index 创建或更新文档（自动生成ID）
// 参数:
//
//	ctx: 上下文
//	index: 索引名称
//	doc: 文档数据
//
// 返回:
//
//	*IndexResponse: 响应结果
//	error: 操作错误
func (m *Manager) Index(ctx context.Context, index string, doc interface{}) (*IndexResponse, error) {
	return m.IndexWithID(ctx, index, "", doc)
}

// IndexWithID 创建或更新文档（指定ID）
// 参数:
//
//	ctx: 上下文
//	index: 索引名称
//	docID: 文档ID，如果为空则自动生成
//	doc: 文档数据
//
// 返回:
//
//	*IndexResponse: 响应结果
//	error: 操作错误
func (m *Manager) IndexWithID(ctx context.Context, index, docID string, doc interface{}) (*IndexResponse, error) {
	// 序列化文档
	docBytes, err := json.Marshal(doc)
	if err != nil {
		m.log.Error("Failed to marshal document",
			zap.String("index", index),
			zap.String("doc_id", docID),
			zap.Error(err))
		return nil, errors.Wrap(errors.ErrCodeMarshalJSON,
			"failed to marshal document", err)
	}

	var res *esapi.Response
	if docID == "" {
		res, err = m.es.Index(
			index,
			bytes.NewReader(docBytes),
			m.es.Index.WithContext(ctx),
		)
	} else {
		res, err = m.es.Index(
			index,
			bytes.NewReader(docBytes),
			m.es.Index.WithContext(ctx),
			m.es.Index.WithDocumentID(docID),
		)
	}

	if err != nil {
		m.log.Error("Failed to index document",
			zap.String("index", index),
			zap.String("doc_id", docID),
			zap.Error(err))
		return nil, errors.Wrap(errors.ErrCodeDocumentCreate,
			"failed to index document", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		m.log.Error("Index document returned error",
			zap.String("index", index),
			zap.String("doc_id", docID),
			zap.Int("status", res.StatusCode),
			zap.String("response", string(body)))
		return nil, errors.New(errors.ErrCodeDocumentCreate, string(body))
	}

	var resp IndexResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		m.log.Error("Failed to decode index response",
			zap.String("index", index),
			zap.String("doc_id", docID),
			zap.Error(err))
		return nil, err
	}

	m.log.Debug("Document indexed successfully",
		zap.String("index", index),
		zap.String("doc_id", resp.ID))

	return &resp, nil
}

// Get 获取文档
// 参数:
//
//	ctx: 上下文
//	index: 索引名称
//	docID: 文档ID
//
// 返回:
//
//	*GetResponse: 响应结果
//	error: 操作错误
func (m *Manager) Get(ctx context.Context, index, docID string) (*GetResponse, error) {
	res, err := m.es.Get(
		index,
		docID,
		m.es.Get.WithContext(ctx),
	)
	if err != nil {
		m.log.Error("Failed to get document",
			zap.String("index", index),
			zap.String("doc_id", docID),
			zap.Error(err))
		return nil, errors.Wrap(errors.ErrCodeDocumentGet,
			"failed to get document", err)
	}
	defer res.Body.Close()

	if res.StatusCode == 404 {
		return nil, errors.ErrDocumentNotFound
	}

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		m.log.Error("Get document returned error",
			zap.String("index", index),
			zap.String("doc_id", docID),
			zap.Int("status", res.StatusCode),
			zap.String("response", string(body)))
		return nil, errors.New(errors.ErrCodeDocumentGet, string(body))
	}

	var resp GetResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		m.log.Error("Failed to decode get response",
			zap.String("index", index),
			zap.String("doc_id", docID),
			zap.Error(err))
		return nil, err
	}

	if !resp.Found {
		return nil, errors.ErrDocumentNotFound
	}

	return &resp, nil
}

// GetInto 获取文档并解析到指定对象
// 参数:
//
//	ctx: 上下文
//	index: 索引名称
//	docID: 文档ID
//	out: 输出对象指针
//
// 返回:
//
//	error: 操作错误
func (m *Manager) GetInto(ctx context.Context, index, docID string, out interface{}) error {
	resp, err := m.Get(ctx, index, docID)
	if err != nil {
		return err
	}

	if err := json.Unmarshal(resp.Source, out); err != nil {
		m.log.Error("Failed to unmarshal document source",
			zap.String("index", index),
			zap.String("doc_id", docID),
			zap.Error(err))
		return errors.Wrap(errors.ErrCodeUnmarshalJSON,
			"failed to unmarshal document source", err)
	}

	return nil
}

// Update 更新文档
// 参数:
//
//	ctx: 上下文
//	index: 索引名称
//	docID: 文档ID
//	doc: 更新数据
//
// 返回:
//
//	*IndexResponse: 响应结果
//	error: 操作错误
func (m *Manager) Update(ctx context.Context, index, docID string, doc interface{}) (*IndexResponse, error) {
	// Elasticsearch要求update使用doc包装
	updateBody := map[string]interface{}{"doc": doc}
	docBytes, err := json.Marshal(updateBody)
	if err != nil {
		m.log.Error("Failed to marshal update document",
			zap.String("index", index),
			zap.String("doc_id", docID),
			zap.Error(err))
		return nil, errors.Wrap(errors.ErrCodeMarshalJSON,
			"failed to marshal update document", err)
	}

	res, err := m.es.Update(
		index,
		docID,
		bytes.NewReader(docBytes),
		m.es.Update.WithContext(ctx),
	)
	if err != nil {
		m.log.Error("Failed to update document",
			zap.String("index", index),
			zap.String("doc_id", docID),
			zap.Error(err))
		return nil, errors.Wrap(errors.ErrCodeDocumentUpdate,
			"failed to update document", err)
	}
	defer res.Body.Close()

	if res.StatusCode == 404 {
		return nil, errors.ErrDocumentNotFound
	}

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		m.log.Error("Update document returned error",
			zap.String("index", index),
			zap.String("doc_id", docID),
			zap.Int("status", res.StatusCode),
			zap.String("response", string(body)))
		return nil, errors.New(errors.ErrCodeDocumentUpdate, string(body))
	}

	var resp IndexResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		m.log.Error("Failed to decode update response",
			zap.String("index", index),
			zap.String("doc_id", docID),
			zap.Error(err))
		return nil, err
	}

	m.log.Debug("Document updated successfully",
		zap.String("index", index),
		zap.String("doc_id", docID))

	return &resp, nil
}

// Delete 删除文档
// 参数:
//
//	ctx: 上下文
//	index: 索引名称
//	docID: 文档ID
//
// 返回:
//
//	error: 操作错误
func (m *Manager) Delete(ctx context.Context, index, docID string) error {
	res, err := m.es.Delete(
		index,
		docID,
		m.es.Delete.WithContext(ctx),
	)
	if err != nil {
		m.log.Error("Failed to delete document",
			zap.String("index", index),
			zap.String("doc_id", docID),
			zap.Error(err))
		return errors.Wrap(errors.ErrCodeDocumentDelete,
			"failed to delete document", err)
	}
	defer res.Body.Close()

	if res.StatusCode == 404 {
		return errors.ErrDocumentNotFound
	}

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		m.log.Error("Delete document returned error",
			zap.String("index", index),
			zap.String("doc_id", docID),
			zap.Int("status", res.StatusCode),
			zap.String("response", string(body)))
		return errors.New(errors.ErrCodeDocumentDelete, string(body))
	}

	m.log.Debug("Document deleted successfully",
		zap.String("index", index),
		zap.String("doc_id", docID))

	return nil
}

// BulkOperation 批量操作
type BulkOperation struct {
	Operation string      // index, create, update, delete
	Index     string      // 索引名称
	ID        string      // 文档ID
	Doc       interface{} // 文档数据
}

// Bulk 批量操作
// 参数:
//
//	ctx: 上下文
//	ops: 批量操作列表
//
// 返回:
//
//	int: 成功操作数量
//	int: 失败操作数量
//	error: 操作错误
func (m *Manager) Bulk(ctx context.Context, ops []BulkOperation) (int, int, error) {
	bulk, err := esutil.NewBulkIndexer(esutil.BulkIndexerConfig{
		Client: m.es,
	})
	if err != nil {
		m.log.Error("Failed to create bulk indexer", zap.Error(err))
		return 0, 0, errors.Wrap(errors.ErrCodeBulkOperation, "failed to create bulk indexer", err)
	}

	var successful int
	var failed int

	for _, op := range ops {
		// 序列化文档
		docBytes, err := json.Marshal(op.Doc)
		if err != nil {
			m.log.Warn("Failed to marshal document in bulk operation",
				zap.String("operation", op.Operation),
				zap.String("index", op.Index),
				zap.String("doc_id", op.ID),
				zap.Error(err))
			failed++
			continue
		}

		item := esutil.BulkIndexerItem{
			Index:      op.Index,
			DocumentID: op.ID,
			Body:       bytes.NewReader(docBytes),
			OnSuccess: func(_ context.Context, _ esutil.BulkIndexerItem, _ esutil.BulkIndexerResponseItem) {
				successful++
			},
			OnFailure: func(_ context.Context, _ esutil.BulkIndexerItem, item esutil.BulkIndexerResponseItem, err error) {
				m.log.Warn("Bulk operation failed",
					zap.String("operation", op.Operation),
					zap.String("index", op.Index),
					zap.String("doc_id", op.ID),
					zap.Error(err),
					zap.String("error_type", item.Error.Type),
					zap.String("error_reason", item.Error.Reason))
				failed++
			},
		}

		// 根据操作类型设置op_type
		switch op.Operation {
		case "delete":
			item.Body = nil
		}

		if err := bulk.Add(ctx, item); err != nil {
			m.log.Warn("Failed to add item to bulk",
				zap.String("operation", op.Operation),
				zap.String("index", op.Index),
				zap.String("doc_id", op.ID),
				zap.Error(err))
			failed++
		}
	}

	if err := bulk.Close(ctx); err != nil {
		m.log.Error("Failed to close bulk indexer",
			zap.Error(err))
		return successful, failed, errors.Wrap(errors.ErrCodeBulkOperation,
			"failed to close bulk indexer", err)
	}

	m.log.Info("Bulk operation completed",
		zap.Int("total", len(ops)),
		zap.Int("successful", successful),
		zap.Int("failed", failed))

	return successful, failed, nil
}

// Exists 检查文档是否存在
// 参数:
//
//	ctx: 上下文
//	index: 索引名称
//	docID: 文档ID
//
// 返回:
//
//	bool: 文档是否存在
//	error: 操作错误
func (m *Manager) Exists(ctx context.Context, index, docID string) (bool, error) {
	res, err := m.es.Exists(
		index,
		docID,
		m.es.Exists.WithContext(ctx),
	)
	if err != nil {
		m.log.Error("Failed to check document existence",
			zap.String("index", index),
			zap.String("doc_id", docID),
			zap.Error(err))
		return false, err
	}
	defer res.Body.Close()

	return res.StatusCode == 200, nil
}

// UpdateByQueryResponse 更新按查询响应
type UpdateByQueryResponse struct {
	Total      int64 `json:"total"`
	Updated    int64 `json:"updated"`
	Deleted    int64 `json:"deleted"`
	Batches    int   `json:"batches"`
	VersionConflicts int64 `json:"version_conflicts"`
	Failures   []map[string]interface{} `json:"failures"`
}

// UpdateByQuery 根据查询条件批量更新文档
// 使用inline脚本更新匹配文档
// 参数:
//
//	ctx: 上下文
//	index: 索引名称
//	query: 查询条件（匹配要更新的文档）
//	script: 更新脚本（Painless语法）
//
// 返回:
//
//	*UpdateByQueryResponse: 更新结果统计
//	error: 操作错误
func (m *Manager) UpdateByQuery(
	ctx context.Context,
	index string,
	query map[string]interface{},
	script string,
) (*UpdateByQueryResponse, error) {
	// 构建请求体
	request := map[string]interface{}{
		"query": query,
		"script": map[string]interface{}{
			"source": script,
		},
	}

	reqBytes, err := json.Marshal(request)
	if err != nil {
		m.log.Error("Failed to marshal update by query request",
			zap.String("index", index),
			zap.Error(err))
		return nil, errors.Wrap(errors.ErrCodeMarshalJSON,
			"failed to marshal update by query request", err)
	}

	res, err := m.es.UpdateByQuery(
		[]string{index},
		m.es.UpdateByQuery.WithContext(ctx),
		m.es.UpdateByQuery.WithBody(bytes.NewReader(reqBytes)),
		m.es.UpdateByQuery.WithConflicts("proceed"),
	)
	if err != nil {
		m.log.Error("Failed to execute update by query",
			zap.String("index", index),
			zap.Error(err))
		return nil, errors.Wrap(errors.ErrCodeDocumentUpdate,
			"failed to execute update by query", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		m.log.Error("Update by query returned error",
			zap.String("index", index),
			zap.Int("status", res.StatusCode),
			zap.String("response", string(body)))
		return nil, errors.New(errors.ErrCodeDocumentUpdate, string(body))
	}

	var response UpdateByQueryResponse
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		m.log.Error("Failed to decode update by query response",
			zap.String("index", index),
			zap.Error(err))
		return nil, err
	}

	m.log.Debug("Update by query completed",
		zap.String("index", index),
		zap.Int64("total", response.Total),
		zap.Int64("updated", response.Updated),
		zap.Int64("conflicts", response.VersionConflicts))

	return &response, nil
}

// DeleteByQueryResponse 删除按查询响应
type DeleteByQueryResponse struct {
	Total      int64 `json:"total"`
	Deleted    int64 `json:"deleted"`
	Batches    int   `json:"batches"`
	VersionConflicts int64 `json:"version_conflicts"`
	Failures   []map[string]interface{} `json:"failures"`
}

// DeleteByQuery 根据查询条件批量删除文档
// 参数:
//
//	ctx: 上下文
//	index: 索引名称
//	query: 查询条件（匹配要删除的文档）
//
// 返回:
//
//	*DeleteByQueryResponse: 删除结果统计
//	error: 操作错误
func (m *Manager) DeleteByQuery(
	ctx context.Context,
	index string,
	query map[string]interface{},
) (*DeleteByQueryResponse, error) {
	// 构建请求体
	request := map[string]interface{}{
		"query": query,
	}

	reqBytes, err := json.Marshal(request)
	if err != nil {
		m.log.Error("Failed to marshal delete by query request",
			zap.String("index", index),
			zap.Error(err))
		return nil, errors.Wrap(errors.ErrCodeMarshalJSON,
			"failed to marshal delete by query request", err)
	}

	res, err := m.es.DeleteByQuery(
		[]string{index},
		bytes.NewReader(reqBytes),
		m.es.DeleteByQuery.WithContext(ctx),
		m.es.DeleteByQuery.WithConflicts("proceed"),
	)
	if err != nil {
		m.log.Error("Failed to execute delete by query",
			zap.String("index", index),
			zap.Error(err))
		return nil, errors.Wrap(errors.ErrCodeDocumentDelete,
			"failed to execute delete by query", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		m.log.Error("Delete by query returned error",
			zap.String("index", index),
			zap.Int("status", res.StatusCode),
			zap.String("response", string(body)))
		return nil, errors.New(errors.ErrCodeDocumentDelete, string(body))
	}

	var response DeleteByQueryResponse
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		m.log.Error("Failed to decode delete by query response",
			zap.String("index", index),
			zap.Error(err))
		return nil, err
	}

	m.log.Debug("Delete by query completed",
		zap.String("index", index),
		zap.Int64("total", response.Total),
		zap.Int64("deleted", response.Deleted))

	return &response, nil
}

// UpdateByQueryWithParams 根据查询条件批量更新文档（带参数）
// 参数:
//
//	ctx: 上下文
//	index: 索引名称
//	query: 查询条件
//	scriptSource: 脚本源码
//	params: 脚本参数
//
// 返回:
//
//	*UpdateByQueryResponse: 更新结果统计
//	error: 操作错误
func (m *Manager) UpdateByQueryWithParams(
	ctx context.Context,
	index string,
	query map[string]interface{},
	scriptSource string,
	params map[string]interface{},
) (*UpdateByQueryResponse, error) {
	// 构建请求体
	request := map[string]interface{}{
		"query": query,
		"script": map[string]interface{}{
			"source": scriptSource,
			"params": params,
		},
	}

	reqBytes, err := json.Marshal(request)
	if err != nil {
		m.log.Error("Failed to marshal update by query request",
			zap.String("index", index),
			zap.Error(err))
		return nil, errors.Wrap(errors.ErrCodeMarshalJSON,
			"failed to marshal update by query request", err)
	}

	res, err := m.es.UpdateByQuery(
		[]string{index},
		m.es.UpdateByQuery.WithContext(ctx),
		m.es.UpdateByQuery.WithBody(bytes.NewReader(reqBytes)),
		m.es.UpdateByQuery.WithConflicts("proceed"),
	)
	if err != nil {
		m.log.Error("Failed to execute update by query",
			zap.String("index", index),
			zap.Error(err))
		return nil, errors.Wrap(errors.ErrCodeDocumentUpdate,
			"failed to execute update by query", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		m.log.Error("Update by query returned error",
			zap.String("index", index),
			zap.Int("status", res.StatusCode),
			zap.String("response", string(body)))
		return nil, errors.New(errors.ErrCodeDocumentUpdate, string(body))
	}

	var response UpdateByQueryResponse
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		m.log.Error("Failed to decode update by query response",
			zap.String("index", index),
			zap.Error(err))
		return nil, err
	}

	return &response, nil
}
