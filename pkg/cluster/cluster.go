// Package cluster 提供Elasticsearch集群运维相关能力
// 包含:集群健康检查(支持wait_for)、节点/索引概览、快照仓库CRUD、快照创建/恢复/查询
// 与 pkg/index 等模块结合,实现"日常巡检+灾备"完整工作流
package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/esapi"
	"github.com/zixiliuyue/go_es/pkg/errors"
	"go.uber.org/zap"
)

// Manager 集群/快照管理器
type Manager struct {
	es  *elasticsearch.Client
	log *zap.Logger
}

// NewManager 创建一个新的集群管理器
// 参数:
//
//	es: Elasticsearch客户端
//	log: 日志记录器(为nil时使用zap.NewProduction())
//
// 返回:
//
//	*Manager: 集群管理器实例
func NewManager(es *elasticsearch.Client, log *zap.Logger) *Manager {
	if log == nil {
		log, _ = zap.NewProduction()
	}
	return &Manager{es: es, log: log}
}

// HealthStatus 集群健康状态
type HealthStatus string

// 集群状态枚举
const (
	HealthGreen  HealthStatus = "green"
	HealthYellow HealthStatus = "yellow"
	HealthRed    HealthStatus = "red"
)

// HealthResult 集群健康详情
type HealthResult struct {
	ClusterName                 string                 `json:"cluster_name"`
	Status                      HealthStatus           `json:"status"`
	TimedOut                    bool                   `json:"timed_out"`
	NumberOfNodes               int                    `json:"number_of_nodes"`
	NumberOfDataNodes           int                    `json:"number_of_data_nodes"`
	ActivePrimaryShards         int                    `json:"active_primary_shards"`
	ActiveShards                int                    `json:"active_shards"`
	RelocatingShards            int                    `json:"relocating_shards"`
	InitializingShards          int                    `json:"initializing_shards"`
	UnassignedShards            int                    `json:"unassigned_shards"`
	DelayedUnassignedShards     int                    `json:"delayed_unassigned_shards"`
	NumberOfPendingTasks        int                    `json:"number_of_pending_tasks"`
	NumberOfInFlightFetch       int                    `json:"number_of_in_flight_fetch"`
	TaskMaxWaitingInQueueMillis int                    `json:"task_max_waiting_in_queue_millis"`
	ActiveShardsPercentAsNumber float64                `json:"active_shards_percent_as_number"`
	Indices                     map[string]IndexHealth `json:"indices,omitempty"`
}

// IndexHealth 单索引健康
type IndexHealth struct {
	Status              HealthStatus `json:"status"`
	NumberOfShards      int          `json:"number_of_shards"`
	NumberOfReplicas    int          `json:"number_of_replicas"`
	ActivePrimaryShards int          `json:"active_primary_shards"`
	ActiveShards        int          `json:"active_shards"`
	UnassignedShards    int          `json:"unassigned_shards"`
}

// Health 集群健康检查
// 参数:
//
//	ctx: 上下文
//	level: cluster/indices/shards,控制返回详细程度
//
// 返回:
//
//	*HealthResult: 健康结果
//	error: 检查错误
func (m *Manager) Health(ctx context.Context, level string) (*HealthResult, error) {
	res, err := m.es.Cluster.Health(
		m.es.Cluster.Health.WithContext(ctx),
		m.es.Cluster.Health.WithLevel(level),
	)
	if err != nil {
		m.log.Error("Failed to get cluster health", zap.Error(err))
		return nil, errors.Wrap(errors.ErrCodeUnknown, "failed to get cluster health", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		respBody, _ := io.ReadAll(res.Body)
		return nil, errors.New(errors.ErrCodeUnknown, string(respBody))
	}

	var out HealthResult
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// WaitForHealth 阻塞直到集群达到目标状态或超时
// 参数:
//
//	ctx: 上下文
//	level: 详细级别
//	status: 目标状态(green/yellow)
//	timeout: 最大等待时间
//
// 返回:
//
//	*HealthResult: 最终健康结果
//	error: 超时或检查错误
func (m *Manager) WaitForHealth(ctx context.Context, level string, status HealthStatus, timeout time.Duration) (*HealthResult, error) {
	res, err := m.es.Cluster.Health(
		m.es.Cluster.Health.WithContext(ctx),
		m.es.Cluster.Health.WithLevel(level),
		m.es.Cluster.Health.WithWaitForStatus(string(status)),
		m.es.Cluster.Health.WithTimeout(timeout),
	)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeUnknown, "failed to wait for health", err)
	}
	defer res.Body.Close()
	if res.IsError() {
		respBody, _ := io.ReadAll(res.Body)
		return nil, errors.New(errors.ErrCodeUnknown, string(respBody))
	}
	var out HealthResult
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// NodeInfo 节点信息(精简)
type NodeInfo struct {
	Name             string  `json:"name"`
	Host             string  `json:"host"`
	IP               string  `json:"ip"`
	Role             string  `json:"role"`
	HeapPercent      float64 `json:"heap.percent"`
	RAMPercent       float64 `json:"ram.percent"`
	CPU              string  `json:"cpu"`
	LoadAverage      string  `json:"load_average"`
	NodeRole         string  `json:"nodeRole"`
	Master           string  `json:"master"`
	CurrentMaster    string  `json:"cm"`
	Version          string  `json:"version"`
}

// ListNodes 列出节点
// 参数:
//
//	ctx: 上下文
//	metric: nodes/nodes,默认 "nodes"
//
// 返回:
//
//	[]NodeInfo: 节点列表
//	error: 错误
func (m *Manager) ListNodes(ctx context.Context) ([]NodeInfo, error) {
	res, err := m.es.Cat.Nodes(
		m.es.Cat.Nodes.WithContext(ctx),
		m.es.Cat.Nodes.WithFormat("json"),
		m.es.Cat.Nodes.WithH("name,host,ip,role,heap.percent,ram.percent,cpu,load_average,nodeRole,master,cm,version"),
	)
	if err != nil {
		m.log.Error("Failed to list nodes", zap.Error(err))
		return nil, errors.Wrap(errors.ErrCodeUnknown, "failed to list nodes", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		respBody, _ := io.ReadAll(res.Body)
		return nil, errors.New(errors.ErrCodeUnknown, string(respBody))
	}

	var out []NodeInfo
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// SnapshotRepository 仓库配置
type SnapshotRepository struct {
	// Type 仓库类型: fs / s3 / azure / gcs ...
	Type string `json:"type"`
	// Settings 仓库具体设置
	Settings map[string]interface{} `json:"settings"`
}

// PutRepository 创建/更新快照仓库
// 参数:
//
//	ctx: 上下文
//	name: 仓库名
//	repo: 仓库配置
//
// 返回:
//
//	error: 失败错误
func (m *Manager) PutRepository(ctx context.Context, name string, repo SnapshotRepository) error {
	if name == "" {
		return errors.New(errors.ErrCodeUnknown, "repository name must not be empty")
	}
	if repo.Type == "" {
		return errors.New(errors.ErrCodeUnknown, "repository type must not be empty")
	}

	payload, err := json.Marshal(repo)
	if err != nil {
		return errors.Wrap(errors.ErrCodeMarshalJSON, "failed to marshal repository", err)
	}

	res, err := m.es.Snapshot.CreateRepository(
		name,
		bytes.NewReader(payload),
		m.es.Snapshot.CreateRepository.WithContext(ctx),
	)
	if err != nil {
		m.log.Error("Failed to put snapshot repository", zap.String("name", name), zap.Error(err))
		return errors.Wrap(errors.ErrCodeUnknown, "failed to put snapshot repository", err)
	}
	defer res.Body.Close()
	if res.IsError() {
		respBody, _ := io.ReadAll(res.Body)
		return errors.New(errors.ErrCodeUnknown, string(respBody))
	}
	m.log.Info("Snapshot repository created", zap.String("name", name))
	return nil
}

// DeleteRepository 删除快照仓库
// 参数:
//
//	ctx: 上下文
//	name: 仓库名
//
// 返回:
//
//	error: 失败错误
func (m *Manager) DeleteRepository(ctx context.Context, name string) error {
	res, err := m.es.Snapshot.DeleteRepository(
		[]string{name},
		m.es.Snapshot.DeleteRepository.WithContext(ctx),
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

// CreateSnapshot 创建一次快照
// 由于 esapi.SnapshotCreate 不支持 WithIndices/WithIncludeGlobalState,
// 这里把全部参数通过 WithBody 传给 _snapshot 端点
// 参数:
//
//	ctx: 上下文
//	repository: 仓库名
//	snapshot: 快照名
//	indices: 要备份的索引(空表示全部)
//	includeGlobalState: 是否包含全局状态
//	waitForCompletion: 是否同步等待
//
// 返回:
//
//	error: 失败错误
func (m *Manager) CreateSnapshot(ctx context.Context, repository, snapshot string, indices []string, includeGlobalState, waitForCompletion bool) error {
	if repository == "" || snapshot == "" {
		return errors.New(errors.ErrCodeUnknown, "repository and snapshot name must not be empty")
	}

	body := map[string]interface{}{
		"include_global_state": includeGlobalState,
	}
	if len(indices) > 0 {
		body["indices"] = strings.Join(indices, ",")
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return errors.Wrap(errors.ErrCodeMarshalJSON, "failed to marshal snapshot body", err)
	}

	res, err := m.es.Snapshot.Create(
		repository, snapshot,
		m.es.Snapshot.Create.WithContext(ctx),
		m.es.Snapshot.Create.WithBody(bytes.NewReader(payload)),
		m.es.Snapshot.Create.WithWaitForCompletion(waitForCompletion),
	)
	if err != nil {
		m.log.Error("Failed to create snapshot",
			zap.String("repository", repository), zap.String("snapshot", snapshot), zap.Error(err))
		return errors.Wrap(errors.ErrCodeUnknown, "failed to create snapshot", err)
	}
	defer res.Body.Close()
	if res.IsError() {
		respBody, _ := io.ReadAll(res.Body)
		return errors.New(errors.ErrCodeUnknown, string(respBody))
	}
	m.log.Info("Snapshot created", zap.String("repository", repository), zap.String("snapshot", snapshot))
	return nil
}

// RestoreSnapshot 恢复一次快照
// 通过 WithBody 把 indices 传给 _restore 端点
// 参数:
//
//	ctx: 上下文
//	repository: 仓库名
//	snapshot: 快照名
//	indices: 要恢复的索引(空表示全部)
//	waitForCompletion: 是否同步等待
//
// 返回:
//
//	error: 失败错误
func (m *Manager) RestoreSnapshot(ctx context.Context, repository, snapshot string, indices []string, waitForCompletion bool) error {
	if repository == "" || snapshot == "" {
		return errors.New(errors.ErrCodeUnknown, "repository and snapshot name must not be empty")
	}

	var payload []byte
	if len(indices) > 0 {
		body := map[string]interface{}{"indices": strings.Join(indices, ",")}
		payload, _ = json.Marshal(body)
	}

	var res *esapi.Response
	var err error
	if len(payload) > 0 {
		res, err = m.es.Snapshot.Restore(
			repository, snapshot,
			m.es.Snapshot.Restore.WithContext(ctx),
			m.es.Snapshot.Restore.WithBody(bytes.NewReader(payload)),
			m.es.Snapshot.Restore.WithWaitForCompletion(waitForCompletion),
		)
	} else {
		res, err = m.es.Snapshot.Restore(
			repository, snapshot,
			m.es.Snapshot.Restore.WithContext(ctx),
			m.es.Snapshot.Restore.WithWaitForCompletion(waitForCompletion),
		)
	}
	if err != nil {
		return errors.Wrap(errors.ErrCodeUnknown, "failed to restore snapshot", err)
	}
	defer res.Body.Close()
	if res.IsError() {
		respBody, _ := io.ReadAll(res.Body)
		return errors.New(errors.ErrCodeUnknown, string(respBody))
	}
	m.log.Info("Snapshot restored", zap.String("repository", repository), zap.String("snapshot", snapshot))
	return nil
}

// SnapshotInfo 快照信息
type SnapshotInfo struct {
	State        string                 `json:"state"`
	StartTime    string                 `json:"start_time"`
	EndTime      string                 `json:"end_time"`
	DurationInMillis int64             `json:"duration_in_millis"`
	Failures     []map[string]interface{} `json:"failures"`
	Indices      []string               `json:"indices"`
	IncludeGlobalState bool             `json:"include_global_state"`
}

// GetSnapshot 获取快照详情
// 参数:
//
//	ctx: 上下文
//	repository: 仓库名
//	snapshot: 快照名
//
// 返回:
//
//	*SnapshotInfo: 快照信息
//	error: 失败错误
func (m *Manager) GetSnapshot(ctx context.Context, repository, snapshot string) (*SnapshotInfo, error) {
	res, err := m.es.Snapshot.Get(
		repository,
		[]string{snapshot},
		m.es.Snapshot.Get.WithContext(ctx),
	)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeUnknown, "failed to get snapshot", err)
	}
	defer res.Body.Close()
	if res.IsError() {
		respBody, _ := io.ReadAll(res.Body)
		return nil, errors.New(errors.ErrCodeUnknown, string(respBody))
	}

	var raw struct {
		Snapshots []SnapshotInfo `json:"snapshots"`
	}
	if err := json.NewDecoder(res.Body).Decode(&raw); err != nil {
		return nil, err
	}
	if len(raw.Snapshots) == 0 {
		return nil, errors.New(errors.ErrCodeIndexNotFound, "snapshot not found")
	}
	return &raw.Snapshots[0], nil
}
