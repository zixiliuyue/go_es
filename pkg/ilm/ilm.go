// Package ilm 提供Elasticsearch索引生命周期管理(ILM)能力
// 支持ILM Policy的创建、查询、删除,以及与索引模板的绑定
// 常见用途:日志/时序数据按 hot -> warm -> cold -> delete 自动流转
package ilm

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

// Manager ILM 管理器
type Manager struct {
	es  *elasticsearch.Client
	log *zap.Logger
}

// NewManager 创建一个新的ILM管理器
// 参数:
//
//	es: Elasticsearch客户端
//	log: 日志记录器(为nil时使用zap.NewProduction())
//
// 返回:
//
//	*Manager: ILM管理器实例
func NewManager(es *elasticsearch.Client, log *zap.Logger) *Manager {
	if log == nil {
		log, _ = zap.NewProduction()
	}
	return &Manager{es: es, log: log}
}

// Phase ILM 阶段定义
type Phase struct {
	// MinAge 进入该阶段的最小年龄,例如 "1d"、"7d"、"30d"
	MinAge string `json:"min_age"`
	// Actions 该阶段执行的动作,具体格式参考ES文档
	// 例如: {"rollover": {"max_age": "1d"}, "set_priority": {"priority": 50}}
	Actions map[string]interface{} `json:"actions"`
}

// Policy 索引生命周期策略
type Policy struct {
	Phases map[string]Phase `json:"phases"`
}

// PutPolicy 创建或更新一个ILM Policy
// 参数:
//
//	ctx: 上下文
//	name: 策略名
//	policy: 策略定义
//
// 返回:
//
//	error: 失败时返回错误
func (m *Manager) PutPolicy(ctx context.Context, name string, policy Policy) error {
	if name == "" {
		return errors.New(errors.ErrCodeUnknown, "policy name must not be empty")
	}
	if len(policy.Phases) == 0 {
		return errors.New(errors.ErrCodeUnknown, "policy phases must not be empty")
	}

	body := map[string]interface{}{"policy": policy}
	payload, err := json.Marshal(body)
	if err != nil {
		m.log.Error("Failed to marshal ilm policy", zap.String("name", name), zap.Error(err))
		return errors.Wrap(errors.ErrCodeMarshalJSON, "failed to marshal ilm policy", err)
	}

	res, err := m.es.ILM.PutLifecycle(
		name,
		m.es.ILM.PutLifecycle.WithContext(ctx),
		m.es.ILM.PutLifecycle.WithBody(bytes.NewReader(payload)),
	)
	if err != nil {
		m.log.Error("Failed to put ilm policy", zap.String("name", name), zap.Error(err))
		return errors.Wrap(errors.ErrCodeUnknown, "failed to put ilm policy", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		respBody, _ := io.ReadAll(res.Body)
		m.log.Error("Put ilm policy returned error",
			zap.String("name", name),
			zap.Int("status", res.StatusCode),
			zap.String("response", string(respBody)))
		return errors.New(errors.ErrCodeUnknown, string(respBody))
	}

	m.log.Info("ILM policy created/updated", zap.String("name", name))
	return nil
}

// GetPolicy 获取一个已存在的ILM Policy
// 参数:
//
//	ctx: 上下文
//	name: 策略名
//
// 返回:
//
//	*Policy: 策略内容
//	error: 失败时返回错误
func (m *Manager) GetPolicy(ctx context.Context, name string) (*Policy, error) {
	res, err := m.es.ILM.GetLifecycle(
		m.es.ILM.GetLifecycle.WithContext(ctx),
	)
	if err != nil {
		m.log.Error("Failed to get ilm policy", zap.String("name", name), zap.Error(err))
		return nil, errors.Wrap(errors.ErrCodeUnknown, "failed to get ilm policy", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		respBody, _ := io.ReadAll(res.Body)
		return nil, errors.New(errors.ErrCodeUnknown, string(respBody))
	}

	// 响应格式: {"<name>": {"policy": {...}}}
	var raw map[string]struct {
		Policy Policy `json:"policy"`
	}
	if err := json.NewDecoder(res.Body).Decode(&raw); err != nil {
		return nil, err
	}
	// 优先按 name 精确匹配;若响应里没有该 name,返回 ErrIndexNotFound
	if _, ok := raw[name]; !ok {
		return nil, errors.New(errors.ErrCodeIndexNotFound,
			fmt.Sprintf("ilm policy not found: %s", name))
	}
	p := raw[name].Policy
	return &p, nil
}

// DeletePolicy 删除ILM Policy
// 参数:
//
//	ctx: 上下文
//	name: 策略名
//
// 返回:
//
//	error: 失败时返回错误
func (m *Manager) DeletePolicy(ctx context.Context, name string) error {
	res, err := m.es.ILM.DeleteLifecycle(
		name,
		m.es.ILM.DeleteLifecycle.WithContext(ctx),
	)
	if err != nil {
		m.log.Error("Failed to delete ilm policy", zap.String("name", name), zap.Error(err))
		return errors.Wrap(errors.ErrCodeUnknown, "failed to delete ilm policy", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		respBody, _ := io.ReadAll(res.Body)
		return errors.New(errors.ErrCodeUnknown, string(respBody))
	}

	m.log.Info("ILM policy deleted", zap.String("name", name))
	return nil
}

// ExplainIndex 查询某个索引当前的ILM执行状态
// 参数:
//
//	ctx: 上下文
//	index: 索引名
//
// 返回:
//
//	map[string]interface{}: ILM返回的原始信息(managed/phase/action/step等)
//	error: 失败时返回错误
func (m *Manager) ExplainIndex(ctx context.Context, index string) (map[string]interface{}, error) {
	res, err := m.es.ILM.ExplainLifecycle(
		index,
		m.es.ILM.ExplainLifecycle.WithContext(ctx),
	)
	if err != nil {
		m.log.Error("Failed to explain ilm lifecycle",
			zap.String("index", index), zap.Error(err))
		return nil, errors.Wrap(errors.ErrCodeUnknown, "failed to explain ilm lifecycle", err)
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

// BuildTimedRolloverPolicy 构造一个按时间滚动的"日志类"策略
// hot阶段在 maxAge 后滚动;warm/cold/delete 按 min_age 推进
// 参数:
//
//	rolloverMaxAge: hot 阶段最大保留时间(例如 "1d")
//	warmMinAge: 进入 warm 阶段的时间(例如 "7d")
//	deleteMinAge: 进入 delete 阶段的时间(例如 "30d")
//
// 返回:
//
//	Policy: 策略对象
func BuildTimedRolloverPolicy(rolloverMaxAge, warmMinAge, deleteMinAge string) Policy {
	return Policy{
		Phases: map[string]Phase{
			"hot": {
				MinAge: "0ms",
				Actions: map[string]interface{}{
					"rollover": map[string]interface{}{
						"max_age": rolloverMaxAge,
					},
					"set_priority": map[string]interface{}{"priority": 100},
				},
			},
			"warm": {
				MinAge: warmMinAge,
				Actions: map[string]interface{}{
					"set_priority": map[string]interface{}{"priority": 50},
					"forcemerge":   map[string]interface{}{"max_num_segments": 1},
				},
			},
			"delete": {
				MinAge: deleteMinAge,
				Actions: map[string]interface{}{
					"delete": map[string]interface{}{},
				},
			},
		},
	}
}
