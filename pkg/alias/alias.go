// Package alias 提供Elasticsearch索引别名(Alias)管理能力
// 支持别名的批量创建、删除、原子切换以及与索引的绑定关系查询
// 别名常用于零停机滚动重建索引(Blue-Green)、多索引聚合查询等场景
package alias

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

// Manager 别名管理器
type Manager struct {
	es  *elasticsearch.Client
	log *zap.Logger
}

// NewManager 创建一个新的别名管理器
// 参数:
//
//	es: Elasticsearch客户端
//	log: 日志记录器(为nil时使用zap.NewProduction())
//
// 返回:
//
//	*Manager: 别名管理器实例
func NewManager(es *elasticsearch.Client, log *zap.Logger) *Manager {
	if log == nil {
		log, _ = zap.NewProduction()
	}
	return &Manager{
		es:  es,
		log: log,
	}
}

// AliasAction 别名原子操作动作
// 用于通过 _aliases 端点批量执行 add/remove 操作
type AliasAction struct {
	// Action 操作类型: add 或 remove
	Action string
	// Index 目标索引
	Index string
	// Alias 别名
	Alias string
	// Filter 可选,基于查询的别名过滤(仅在Action=add时生效)
	Filter map[string]interface{}
	// IsWriteIndex 是否将索引设置为该别名的写索引
	IsWriteIndex bool
}

// AddAction 创建一个"添加别名"的动作
// 参数:
//
//	index: 索引名
//	alias: 别名
//
// 返回:
//
//	*AliasAction: 别名动作实例
func AddAction(index, alias string) *AliasAction {
	return &AliasAction{Action: "add", Index: index, Alias: alias}
}

// RemoveAction 创建一个"删除别名"的动作
// 参数:
//
//	index: 索引名
//	alias: 别名
//
// 返回:
//
//	*AliasAction: 别名动作实例
func RemoveAction(index, alias string) *AliasAction {
	return &AliasAction{Action: "remove", Index: index, Alias: alias}
}

// WithFilter 设置别名过滤(仅对add生效)
func (a *AliasAction) WithFilter(filter map[string]interface{}) *AliasAction {
	a.Filter = filter
	return a
}

// WithWriteIndex 设置该索引为别名的写索引(仅对add生效)
func (a *AliasAction) WithWriteIndex(isWrite bool) *AliasAction {
	a.IsWriteIndex = isWrite
	return a
}

// toMap 序列化为 _aliases 接口所需的 action 对象
func (a *AliasAction) toMap() map[string]interface{} {
	body := map[string]interface{}{
		"index": a.Index,
		"alias": a.Alias,
	}
	if a.Filter != nil {
		body["filter"] = a.Filter
	}
	if a.IsWriteIndex {
		body["is_write_index"] = true
	}
	return map[string]interface{}{a.Action: body}
}

// AddAlias 给指定索引添加一个别名(走 _aliases 端点,原子操作)
// 参数:
//
//	ctx: 上下文
//	index: 索引名
//	alias: 别名
//
// 返回:
//
//	error: 失败时返回错误
func (m *Manager) AddAlias(ctx context.Context, index, alias string) error {
	return m.UpdateAliases(ctx, []AliasAction{*AddAction(index, alias)})
}

// RemoveAlias 删除指定索引上的某个别名
// 参数:
//
//	ctx: 上下文
//	index: 索引名
//	alias: 别名
//
// 返回:
//
//	error: 失败时返回错误
func (m *Manager) RemoveAlias(ctx context.Context, index, alias string) error {
	return m.UpdateAliases(ctx, []AliasAction{*RemoveAction(index, alias)})
}

// UpdateAliases 原子地执行一组别名动作(单次请求全部生效)
// 常用于"零停机切换别名"场景:同一事务中 add 新索引、remove 旧索引
// 参数:
//
//	ctx: 上下文
//	actions: 别名动作列表
//
// 返回:
//
//	error: 失败时返回错误
func (m *Manager) UpdateAliases(ctx context.Context, actions []AliasAction) error {
	if len(actions) == 0 {
		return errors.New(errors.ErrCodeUnknown, "actions must not be empty")
	}

	// 组装 _aliases 请求体 {"actions":[{add:{...}},{remove:{...}}]}
	body := map[string]interface{}{"actions": make([]map[string]interface{}, 0, len(actions))}
	for i := range actions {
		body["actions"] = append(body["actions"].([]map[string]interface{}), actions[i].toMap())
	}

	payload, err := json.Marshal(body)
	if err != nil {
		m.log.Error("Failed to marshal alias actions", zap.Error(err))
		return errors.Wrap(errors.ErrCodeMarshalJSON, "failed to marshal alias actions", err)
	}

	res, err := m.es.Indices.UpdateAliases(
		bytes.NewReader(payload),
		m.es.Indices.UpdateAliases.WithContext(ctx),
	)
	if err != nil {
		m.log.Error("Failed to update aliases", zap.Error(err))
		return errors.Wrap(errors.ErrCodeUnknown, "failed to update aliases", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		respBody, _ := io.ReadAll(res.Body)
		m.log.Error("Update aliases returned error",
			zap.Int("status", res.StatusCode),
			zap.String("response", string(respBody)))
		return errors.New(errors.ErrCodeUnknown, string(respBody))
	}

	m.log.Info("Aliases updated successfully", zap.Int("count", len(actions)))
	return nil
}

// SwitchAlias 零停机切换:在一次原子操作中把别名从旧索引迁移到新索引
// 内部等价于同时 add(newIndex, alias) 与 remove(oldIndex, alias)
// 参数:
//
//	ctx: 上下文
//	alias: 别名
//	oldIndex: 当前指向的旧索引
//	newIndex: 要切换到的新索引
//
// 返回:
//
//	error: 失败时返回错误
func (m *Manager) SwitchAlias(ctx context.Context, alias, oldIndex, newIndex string) error {
	return m.UpdateAliases(ctx, []AliasAction{
		*AddAction(newIndex, alias),
		*RemoveAction(oldIndex, alias),
	})
}

// GetAlias 查询别名绑定的索引(列表)
// 参数:
//
//	ctx: 上下文
//	alias: 别名
//
// 返回:
//
//	[]string: 绑定到该别名的索引列表
//	error: 失败时返回错误
func (m *Manager) GetAlias(ctx context.Context, alias string) ([]string, error) {
	res, err := m.es.Indices.GetAlias(
		m.es.Indices.GetAlias.WithContext(ctx),
		m.es.Indices.GetAlias.WithName(alias),
	)
	if err != nil {
		m.log.Error("Failed to get alias", zap.String("alias", alias), zap.Error(err))
		return nil, errors.Wrap(errors.ErrCodeUnknown, "failed to get alias", err)
	}
	defer res.Body.Close()

	if res.StatusCode == 404 {
		return nil, nil
	}
	if res.IsError() {
		respBody, _ := io.ReadAll(res.Body)
		return nil, errors.New(errors.ErrCodeUnknown, string(respBody))
	}

	// 响应格式: { "<index>": { "aliases": { "<alias>": {...} } }, ... }
	var raw map[string]map[string]interface{}
	if err := json.NewDecoder(res.Body).Decode(&raw); err != nil {
		m.log.Error("Failed to decode alias response",
			zap.String("alias", alias), zap.Error(err))
		return nil, err
	}

	indices := make([]string, 0, len(raw))
	for indexName, content := range raw {
		if _, ok := content["aliases"]; ok {
			indices = append(indices, indexName)
		}
	}
	return indices, nil
}

// Exists 判断别名是否存在
// 参数:
//
//	ctx: 上下文
//	alias: 别名
//
// 返回:
//
//	bool: 别名是否存在
//	error: 查询错误
func (m *Manager) Exists(ctx context.Context, alias string) (bool, error) {
	res, err := m.es.Indices.ExistsAlias(
		[]string{alias},
		m.es.Indices.ExistsAlias.WithContext(ctx),
	)
	if err != nil {
		m.log.Error("Failed to check alias existence", zap.String("alias", alias), zap.Error(err))
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

// PutAlias 直接对单个索引设置/更新别名(走 _alias 端点)
// 与 UpdateAliases 的区别:此方法会覆盖该索引上同名别名的设置
// 由于 esapi.IndicesPutAlias 没有 WithIsWriteIndex,这里用 WithBody 传递 is_write_index
// 参数:
//
//	ctx: 上下文
//	index: 索引名
//	alias: 别名
//	isWriteIndex: 是否设置为写索引
//
// 返回:
//
//	error: 失败时返回错误
func (m *Manager) PutAlias(ctx context.Context, index, alias string, isWriteIndex bool) error {
	var res *esapi.Response
	var err error
	if isWriteIndex {
		body := map[string]interface{}{"is_write_index": true}
		payload, _ := json.Marshal(body)
		res, err = m.es.Indices.PutAlias(
			[]string{index},
			alias,
			m.es.Indices.PutAlias.WithContext(ctx),
			m.es.Indices.PutAlias.WithBody(bytes.NewReader(payload)),
		)
	} else {
		res, err = m.es.Indices.PutAlias(
			[]string{index},
			alias,
			m.es.Indices.PutAlias.WithContext(ctx),
		)
	}
	if err != nil {
		m.log.Error("Failed to put alias",
			zap.String("index", index), zap.String("alias", alias), zap.Error(err))
		return errors.Wrap(errors.ErrCodeUnknown, "failed to put alias", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		respBody, _ := io.ReadAll(res.Body)
		return errors.New(errors.ErrCodeUnknown, string(respBody))
	}
	return nil
}
