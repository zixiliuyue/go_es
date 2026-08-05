// Package template 提供Elasticsearch索引模板的管理能力
// 支持 Composable Index Template 与 Component Template 的创建、查询、删除和模拟渲染
// 与 pkg/ilm 配合,可以一次性配置"自动建索引 + 自动绑定生命周期"
package template

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/zixiliuyue/go_es/pkg/errors"
	"go.uber.org/zap"
)

// Manager 索引模板管理器
type Manager struct {
	es  *elasticsearch.Client
	log *zap.Logger
}

// NewManager 创建一个新的模板管理器
// 参数:
//
//	es: Elasticsearch客户端
//	log: 日志记录器(为nil时使用zap.NewProduction())
//
// 返回:
//
//	*Manager: 模板管理器实例
func NewManager(es *elasticsearch.Client, log *zap.Logger) *Manager {
	if log == nil {
		log, _ = zap.NewProduction()
	}
	return &Manager{es: es, log: log}
}

// ComponentTemplate 组件模板定义
type ComponentTemplate struct {
	// Template 模板核心内容,可包含 mappings/settings/aliases
	Template map[string]interface{} `json:"template"`
	// Version 版本号
	Version int `json:"version,omitempty"`
	// Meta 附加元信息
	Meta map[string]interface{} `json:"_meta,omitempty"`
}

// IndexTemplate Composable Index Template
type IndexTemplate struct {
	// IndexPatterns 匹配的索引名模式,例如 ["logs-*"]
	IndexPatterns []string `json:"index_patterns"`
	// Template 模板核心内容
	Template map[string]interface{} `json:"template,omitempty"`
	// ComposedOf 引用的组件模板名称
	ComposedOf []string `json:"composed_of,omitempty"`
	// Priority 优先级,数值大者优先生效
	Priority int `json:"priority,omitempty"`
	// Version 版本号
	Version int `json:"version,omitempty"`
	// Meta 附加元信息
	Meta map[string]interface{} `json:"_meta,omitempty"`
}

// PutIndexTemplate 创建或更新一个 Composable Index Template
// 参数:
//
//	ctx: 上下文
//	name: 模板名
//	tpl: 模板定义
//
// 返回:
//
//	error: 失败时返回错误
func (m *Manager) PutIndexTemplate(ctx context.Context, name string, tpl IndexTemplate) error {
	if name == "" {
		return errors.New(errors.ErrCodeUnknown, "template name must not be empty")
	}
	if len(tpl.IndexPatterns) == 0 {
		return errors.New(errors.ErrCodeUnknown, "index_patterns must not be empty")
	}

	payload, err := json.Marshal(tpl)
	if err != nil {
		m.log.Error("Failed to marshal index template", zap.String("name", name), zap.Error(err))
		return errors.Wrap(errors.ErrCodeMarshalJSON, "failed to marshal index template", err)
	}

	res, err := m.es.Indices.PutIndexTemplate(
		name,
		bytes.NewReader(payload),
		m.es.Indices.PutIndexTemplate.WithContext(ctx),
	)
	if err != nil {
		m.log.Error("Failed to put index template", zap.String("name", name), zap.Error(err))
		return errors.Wrap(errors.ErrCodeUnknown, "failed to put index template", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		respBody, _ := io.ReadAll(res.Body)
		m.log.Error("Put index template returned error",
			zap.String("name", name),
			zap.Int("status", res.StatusCode),
			zap.String("response", string(respBody)))
		return errors.New(errors.ErrCodeUnknown, string(respBody))
	}

	m.log.Info("Index template created/updated", zap.String("name", name))
	return nil
}

// GetIndexTemplate 获取一个 Composable Index Template
// 参数:
//
//	ctx: 上下文
//	name: 模板名
//
// 返回:
//
//	*IndexTemplate: 模板内容
//	error: 失败时返回错误
func (m *Manager) GetIndexTemplate(ctx context.Context, name string) (*IndexTemplate, error) {
	res, err := m.es.Indices.GetIndexTemplate(
		m.es.Indices.GetIndexTemplate.WithContext(ctx),
		m.es.Indices.GetIndexTemplate.WithName(name),
	)
	if err != nil {
		m.log.Error("Failed to get index template", zap.String("name", name), zap.Error(err))
		return nil, errors.Wrap(errors.ErrCodeUnknown, "failed to get index template", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		respBody, _ := io.ReadAll(res.Body)
		return nil, errors.New(errors.ErrCodeUnknown, string(respBody))
	}

	// 响应: {"index_templates": [{"name": "...", "index_template": {...}}]}
	var raw struct {
		IndexTemplates []struct {
			Name          string        `json:"name"`
			IndexTemplate IndexTemplate `json:"index_template"`
		} `json:"index_templates"`
	}
	if err := json.NewDecoder(res.Body).Decode(&raw); err != nil {
		return nil, err
	}

	for _, it := range raw.IndexTemplates {
		if it.Name == name {
			tpl := it.IndexTemplate
			return &tpl, nil
		}
	}
	return nil, errors.New(errors.ErrCodeIndexNotFound,
		fmt.Sprintf("index template not found: %s", name))
}

// DeleteIndexTemplate 删除 Composable Index Template
// 参数:
//
//	ctx: 上下文
//	name: 模板名
//
// 返回:
//
//	error: 失败时返回错误
func (m *Manager) DeleteIndexTemplate(ctx context.Context, name string) error {
	res, err := m.es.Indices.DeleteIndexTemplate(
		name,
		m.es.Indices.DeleteIndexTemplate.WithContext(ctx),
	)
	if err != nil {
		m.log.Error("Failed to delete index template", zap.String("name", name), zap.Error(err))
		return errors.Wrap(errors.ErrCodeUnknown, "failed to delete index template", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		respBody, _ := io.ReadAll(res.Body)
		return errors.New(errors.ErrCodeUnknown, string(respBody))
	}

	m.log.Info("Index template deleted", zap.String("name", name))
	return nil
}

// IndexTemplateExists 判断 Composable Index Template 是否存在
// 参数:
//
//	ctx: 上下文
//	name: 模板名
//
// 返回:
//
//	bool: 是否存在
//	error: 查询错误
func (m *Manager) IndexTemplateExists(ctx context.Context, name string) (bool, error) {
	res, err := m.es.Indices.ExistsIndexTemplate(
		name,
		m.es.Indices.ExistsIndexTemplate.WithContext(ctx),
	)
	if err != nil {
		return false, err
	}
	defer res.Body.Close()
	if res.StatusCode == 200 {
		return true, nil
	}
	if res.StatusCode == 404 {
		return false, nil
	}
	return false, nil
}

// PutComponentTemplate 创建或更新一个 Component Template
// 参数:
//
//	ctx: 上下文
//	name: 组件模板名
//	tpl: 组件模板内容
//
// 返回:
//
//	error: 失败时返回错误
func (m *Manager) PutComponentTemplate(ctx context.Context, name string, tpl ComponentTemplate) error {
	if name == "" {
		return errors.New(errors.ErrCodeUnknown, "component template name must not be empty")
	}

	payload, err := json.Marshal(tpl)
	if err != nil {
		m.log.Error("Failed to marshal component template", zap.String("name", name), zap.Error(err))
		return errors.Wrap(errors.ErrCodeMarshalJSON, "failed to marshal component template", err)
	}

	res, err := m.es.Cluster.PutComponentTemplate(
		name,
		bytes.NewReader(payload),
		m.es.Cluster.PutComponentTemplate.WithContext(ctx),
	)
	if err != nil {
		m.log.Error("Failed to put component template", zap.String("name", name), zap.Error(err))
		return errors.Wrap(errors.ErrCodeUnknown, "failed to put component template", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		respBody, _ := io.ReadAll(res.Body)
		m.log.Error("Put component template returned error",
			zap.String("name", name),
			zap.Int("status", res.StatusCode),
			zap.String("response", string(respBody)))
		return errors.New(errors.ErrCodeUnknown, string(respBody))
	}

	m.log.Info("Component template created/updated", zap.String("name", name))
	return nil
}

// DeleteComponentTemplate 删除 Component Template
// 参数:
//
//	ctx: 上下文
//	name: 组件模板名
//
// 返回:
//
//	error: 失败时返回错误
func (m *Manager) DeleteComponentTemplate(ctx context.Context, name string) error {
	res, err := m.es.Cluster.DeleteComponentTemplate(
		name,
		m.es.Cluster.DeleteComponentTemplate.WithContext(ctx),
	)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.IsError() {
		respBody, _ := io.ReadAll(res.Body)
		return errors.New(errors.ErrCodeUnknown, string(respBody))
	}
	return nil
}

// Simulate 模拟渲染: 给定一个候选索引名,返回最终生效的 settings/mappings
// 常用于模板调试
// 参数:
//
//	ctx: 上下文
//	indexName: 候选索引名(用于匹配模板)
//	template: 可选,临时模板定义(若提供,会先创建再模拟)
//
// 返回:
//
//	map[string]interface{}: 渲染结果
//	error: 失败时返回错误
func (m *Manager) Simulate(ctx context.Context, indexName string, template map[string]interface{}) (map[string]interface{}, error) {
	if indexName == "" {
		return nil, errors.New(errors.ErrCodeUnknown, "indexName must not be empty")
	}
	body := map[string]interface{}{}
	if template != nil {
		body["template"] = template
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeMarshalJSON, "failed to marshal simulate body", err)
	}

	res, err := m.es.Indices.SimulateIndexTemplate(
		indexName,
		m.es.Indices.SimulateIndexTemplate.WithContext(ctx),
		m.es.Indices.SimulateIndexTemplate.WithBody(bytes.NewReader(payload)),
	)
	if err != nil {
		m.log.Error("Failed to simulate index template", zap.String("index", indexName), zap.Error(err))
		return nil, errors.Wrap(errors.ErrCodeUnknown, "failed to simulate index template", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		respBody, _ := io.ReadAll(res.Body)
		return nil, errors.New(errors.ErrCodeUnknown, string(respBody))
	}

	var out map[string]interface{}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}
