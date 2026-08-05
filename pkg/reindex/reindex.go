// Package reindex 提供Elasticsearch _reindex 能力
// 支持本地或远端源、查询过滤、Painless 脚本、切片并行,以及任务的轮询/取消
// 与 pkg/alias 配合,实现"零停机滚动迁移"
package reindex

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/esapi"
	"github.com/zixiliuyue/go_es/pkg/errors"
	"go.uber.org/zap"
)

// Manager reindex 管理器
type Manager struct {
	es  *elasticsearch.Client
	log *zap.Logger
}

// NewManager 创建一个新的 reindex 管理器
// 参数:
//
//	es: Elasticsearch客户端
//	log: 日志记录器(为nil时使用zap.NewProduction())
//
// 返回:
//
//	*Manager: reindex管理器实例
func NewManager(es *elasticsearch.Client, log *zap.Logger) *Manager {
	if log == nil {
		log, _ = zap.NewProduction()
	}
	return &Manager{es: es, log: log}
}

// Source reindex 的数据源
// Source 至少需要设置 Index 或 Remote 之一
type Source struct {
	// Index 源索引列表(本地)
	Index []string
	// Remote 远端源(例如另一个ES集群)
	Remote *RemoteSource
	// Query 过滤条件(DSL JSON)
	Query map[string]interface{}
	// Sort 排序
	Sort []map[string]interface{}
	// Slice 切片配置,用于并行 reindex
	Slice *SliceConfig
}

// RemoteSource 远端集群源
type RemoteSource struct {
	Host string                 `json:"host"`
	// Username/Password 可选
	Username string                 `json:"username,omitempty"`
	Password string                 `json:"password,omitempty"`
	Headers  map[string]interface{} `json:"headers,omitempty"`
	SocketTimeoutSeconds int      `json:"socket_timeout,omitempty"`
	ConnectTimeoutSeconds int     `json:"connect_timeout,omitempty"`
}

// SliceConfig 切片并行配置
type SliceConfig struct {
	// ID 任务ID(0 <= id < Max)
	ID int
	// Max 总切片数
	Max int
}

// Dest reindex 的目标
type Dest struct {
	// Index 目标索引名
	Index string
	// OpType index 或 create
	OpType string
	// VersionType internal/external/external_gt 等
	VersionType string
	// Pipeline 处理管道
	Pipeline string
}

// Request reindex 请求参数
type Request struct {
	// Source 数据源
	Source Source
	// Dest 目标
	Dest Dest
	// Script 可选的Painless脚本
	Script string
	// Params 脚本参数
	Params map[string]interface{}
	// WaitForCompletion 是否同步等待,默认false(异步)
	WaitForCompletion bool
	// Refresh 是否刷新目标索引
	Refresh bool
	// Conflicts 冲突处理策略: abort 或 proceed
	Conflicts string
	// SlicesAuto 自动切片(slices=auto),此时 Slice 字段被忽略
	SlicesAuto bool
}

// Response reindex 响应
// 当 WaitForCompletion=true 时,响应包含统计信息
// 当 WaitForCompletion=false 时,响应只包含 task 标识
type Response struct {
	// Took 总耗时(毫秒)
	Took int64 `json:"took"`
	// TimedOut 是否超时
	TimedOut bool `json:"timed_out"`
	// Total 文档总数
	Total int64 `json:"total"`
	// Updated 文档数
	Updated int64 `json:"updated"`
	// Created 文档数
	Created int64 `json:"created"`
	// Deleted 文档数
	Deleted int64 `json:"deleted"`
	// Batches 批次数
	Batches int `json:"batches"`
	// VersionConflicts 版本冲突数
	VersionConflicts int64 `json:"version_conflicts"`
	// Noops noop 数量
	Noops int64 `json:"noops"`
	// Failures 失败列表
	Failures []map[string]interface{} `json:"failures"`
	// Task 异步任务ID(WaitForCompletion=false 时存在)
	Task string `json:"task,omitempty"`
}

// Reindex 执行一次 reindex
// 参数:
//
//	ctx: 上下文
//	req: reindex 请求
//
// 返回:
//
//	*Response: 响应结果
//	error: 执行错误
func (m *Manager) Reindex(ctx context.Context, req Request) (*Response, error) {
	body := m.buildBody(req)
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeMarshalJSON, "failed to marshal reindex body", err)
	}

	opts := []func(*esapi.ReindexRequest){
		m.es.Reindex.WithContext(ctx),
		m.es.Reindex.WithWaitForCompletion(req.WaitForCompletion),
		m.es.Reindex.WithRefresh(req.Refresh),
	}
	_ = opts

	// 实际请求:Reindex(body io.Reader, opts ...func(*ReindexRequest))
	res, err := m.es.Reindex(
		bytes.NewReader(payload),
		m.es.Reindex.WithContext(ctx),
		m.es.Reindex.WithWaitForCompletion(req.WaitForCompletion),
		m.es.Reindex.WithRefresh(req.Refresh),
	)
	if err != nil {
		m.log.Error("Failed to reindex", zap.Error(err))
		return nil, errors.Wrap(errors.ErrCodeUnknown, "failed to reindex", err)
	}
	_ = opts
	defer res.Body.Close()

	if res.IsError() {
		respBody, _ := io.ReadAll(res.Body)
		m.log.Error("Reindex returned error",
			zap.Int("status", res.StatusCode),
			zap.String("response", string(respBody)))
		return nil, errors.New(errors.ErrCodeUnknown, string(respBody))
	}

	var out Response
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// buildBody 将 Request 转为 _reindex 接口所需的 JSON 结构
func (m *Manager) buildBody(req Request) map[string]interface{} {
	src := map[string]interface{}{}
	if len(req.Source.Index) > 0 {
		src["index"] = req.Source.Index
	}
	if req.Source.Remote != nil {
		src["remote"] = req.Source.Remote
	}
	if req.Source.Query != nil {
		src["query"] = req.Source.Query
	}
	if len(req.Source.Sort) > 0 {
		src["sort"] = req.Source.Sort
	}
	if req.Source.Slice != nil {
		src["slice"] = map[string]interface{}{
			"id":  req.Source.Slice.ID,
			"max": req.Source.Slice.Max,
		}
	}

	dst := map[string]interface{}{"index": req.Dest.Index}
	if req.Dest.OpType != "" {
		dst["op_type"] = req.Dest.OpType
	}
	if req.Dest.VersionType != "" {
		dst["version_type"] = req.Dest.VersionType
	}
	if req.Dest.Pipeline != "" {
		dst["pipeline"] = req.Dest.Pipeline
	}

	body := map[string]interface{}{
		"source": src,
		"dest":   dst,
	}
	if req.Script != "" {
		script := map[string]interface{}{"source": req.Script}
		if len(req.Params) > 0 {
			script["params"] = req.Params
		}
		body["script"] = script
	}
	if req.Conflicts != "" {
		body["conflicts"] = req.Conflicts
	}
	if req.SlicesAuto {
		body["slices"] = "auto"
	}
	return body
}

// TaskInfo 任务信息
type TaskInfo struct {
	Completed bool                   `json:"completed"`
	Task      map[string]interface{} `json:"task"`
	Response  map[string]interface{} `json:"response,omitempty"`
	Error     map[string]interface{} `json:"error,omitempty"`
}

// GetTask 查询一个 reindex 任务的状态
// 参数:
//
//	ctx: 上下文
//	taskID: 任务ID
//
// 返回:
//
//	*TaskInfo: 任务信息
//	error: 查询错误
func (m *Manager) GetTask(ctx context.Context, taskID string) (*TaskInfo, error) {
	res, err := m.es.Tasks.Get(taskID, m.es.Tasks.Get.WithContext(ctx))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeUnknown, "failed to get task", err)
	}
	defer res.Body.Close()
	if res.IsError() {
		respBody, _ := io.ReadAll(res.Body)
		return nil, errors.New(errors.ErrCodeUnknown, string(respBody))
	}
	var out TaskInfo
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CancelTask 取消一个 reindex 任务
// 参数:
//
//	ctx: 上下文
//	taskID: 任务ID
//
// 返回:
//
//	error: 取消过程中的错误
func (m *Manager) CancelTask(ctx context.Context, taskID string) error {
	res, err := m.es.Tasks.Cancel(m.es.Tasks.Cancel.WithContext(ctx), m.es.Tasks.Cancel.WithTaskID(taskID))
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

// WaitForTask 轮询直到任务完成或达到超时
// 参数:
//
//	ctx: 上下文
//	taskID: 任务ID
//	interval: 轮询间隔
//	timeout: 总超时时间,0 表示不超时
//
// 返回:
//
//	*TaskInfo: 最终任务状态
//	error: 任务失败或超时
func (m *Manager) WaitForTask(ctx context.Context, taskID string, interval, timeout time.Duration) (*TaskInfo, error) {
	if interval <= 0 {
		interval = 2 * time.Second
	}

	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}

	for {
		info, err := m.GetTask(ctx, taskID)
		if err != nil {
			return nil, err
		}
		if info.Completed {
			if info.Error != nil {
				return info, errors.New(errors.ErrCodeUnknown, "reindex task failed")
			}
			return info, nil
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return info, errors.New(errors.ErrCodeUnknown, "wait for task timeout")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
}
