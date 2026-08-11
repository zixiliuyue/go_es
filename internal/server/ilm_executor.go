// ILM(Index Lifecycle Management) 执行器
//
// 设计:
//   - 后台 goroutine 按固定间隔(默认 30s)扫描所有受管索引
//   - 加载索引关联的 ILM policy, 按 phase 定义的 min_age + actions 判断是否应触发
//   - 支持的 phase: hot / warm / cold / delete
//   - 支持的 action: rollover(自动创建新索引+别名切换)、delete(删除过期索引)
//   - 状态持久化: 每个索引的 ILM 状态存储在 `ilm-state/<index>` 键, 重启后可恢复
//
// 存储格式(ilm-state/<index>):
//
//	{
//	  "policy_name": "my_policy",
//	  "phase": "hot",
//	  "phase_start_time": 1700000000,
//	  "rollover_count": 1,
//	  "managed": true,
//	  "last_checked": 1700000000
//	}
//
// Policy 格式(ilm/<policy>):
//
//	{
//	  "policy": {
//	    "phases": {
//	      "hot": {"min_age": "0ms", "actions": {"rollover": {"max_age": "1d", "max_docs": 10000}}},
//	      "warm": {"min_age": "30d", "actions": {}},
//	      "delete": {"min_age": "90d", "actions": {"delete": {}}}
//	    }
//	  }
//	}
package server

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zixiliuyue/go_es/internal/storage"
	"go.uber.org/zap"
)

// ILMStateKey 构造 ILM 状态存储键
func ILMStateKey(index string) []byte {
	return []byte("ilm-state/" + index)
}

// ILMState 索引的 ILM 运行时状态
type ILMState struct {
	PolicyName    string `json:"policy_name"`
	Phase         string `json:"phase"`
	PhaseStartTime int64  `json:"phase_start_time"`
	RolloverCount int    `json:"rollover_count"`
	Managed       bool   `json:"managed"`
	LastChecked   int64  `json:"last_checked"`
	Error         string `json:"error,omitempty"`
}

// ILMAction 单个 ILM 动作定义
type ILMAction struct {
	Rollover *RolloverAction `json:"rollover,omitempty"`
	Delete   *DeleteAction   `json:"delete,omitempty"`
	Forcemerge map[string]interface{} `json:"forcemerge,omitempty"`
	SetPriority map[string]interface{} `json:"set_priority,omitempty"`
}

// RolloverAction rollover 动作配置
type RolloverAction struct {
	MaxAge  string `json:"max_age,omitempty"`
	MaxSize string `json:"max_size,omitempty"`
	MaxDocs int    `json:"max_docs,omitempty"`
	MinDocs int    `json:"min_docs,omitempty"`
}

// DeleteAction 删除动作(通常为空对象)
type DeleteAction struct {
	Deleteable string `json:"deleteable,omitempty"`
}

// ILMPhase 单个 phase 定义
type ILMPhase struct {
	MinAge  string            `json:"min_age"`
	Actions map[string]interface{} `json:"actions"`
}

// ILMPolicy ILM policy 结构
type ILMPolicy struct {
	Policy struct {
		Phases map[string]ILMPhase `json:"phases"`
	} `json:"policy"`
}

// ILMExecutor ILM 执行器
type ILMExecutor struct {
	server   *Server
	logger   *zap.Logger
	interval time.Duration
	ticker   *time.Ticker
	stopCh   chan struct{}
	stopped  bool
	mu       sync.Mutex
}

// NewILMExecutor 创建 ILM 执行器
// tickInterval: 扫描间隔(默认 30s), 可设短值用于测试
func NewILMExecutor(s *Server, logger *zap.Logger, tickInterval time.Duration) *ILMExecutor {
	if tickInterval <= 0 {
		tickInterval = 30 * time.Second
	}
	return &ILMExecutor{
		server:   s,
		logger:   logger,
		interval: tickInterval,
		ticker:   time.NewTicker(tickInterval),
		stopCh:   make(chan struct{}),
	}
}

// Start 启动 ILM 执行器后台循环
func (e *ILMExecutor) Start() {
	go e.loop()
	e.logger.Info("ILM executor started", zap.Duration("interval", e.interval))
}

// Stop 停止 ILM 执行器
func (e *ILMExecutor) Stop() {
	e.mu.Lock()
	if e.stopped {
		e.mu.Unlock()
		return
	}
	e.stopped = true
	e.mu.Unlock()
	e.ticker.Stop()
	close(e.stopCh)
	e.logger.Info("ILM executor stopped")
}

// loop 主循环: 周期性扫描受管索引
func (e *ILMExecutor) loop() {
	for {
		select {
		case <-e.ticker.C:
			e.runTick()
		case <-e.stopCh:
			return
		}
	}
}

// runTick 执行一次 ILM 检查周期
func (e *ILMExecutor) runTick() {
	ctx := context.Background()
	e.logger.Debug("ILM tick - checking managed indices")

	indices := e.listManagedIndices()
	for _, idx := range indices {
		select {
		case <-e.stopCh:
			return
		default:
		}
		e.processIndex(ctx, idx)
	}
}

// listManagedIndices 列出所有受管索引
func (e *ILMExecutor) listManagedIndices() []string {
	var indices []string
	_ = e.server.store.Scan([]byte("ilm-state/"), func(k, v []byte) error {
		rest := strings.TrimPrefix(string(k), "ilm-state/")
		var state ILMState
		if err := jsonUnmarshal(v, &state); err == nil && state.Managed {
			indices = append(indices, rest)
		}
		return nil
	})
	return indices
}

// processIndex 处理单个索引的 ILM 状态检查
func (e *ILMExecutor) processIndex(ctx context.Context, index string) {
	state, found, err := e.loadState(index)
	if err != nil || !found {
		if err != nil {
			e.logger.Error("ILM load state failed", zap.String("index", index), zap.Error(err))
		}
		return
	}

	policy, err := e.loadPolicy(state.PolicyName)
	if err != nil {
		e.logger.Error("ILM load policy failed",
			zap.String("index", index),
			zap.String("policy", state.PolicyName),
			zap.Error(err))
		state.Error = err.Error()
		_ = e.saveState(index, state)
		return
	}

	// 获取索引创建时间(用元信息)
	meta, metaFound, _ := e.loadIndexMeta(index)
	var createdAt int64
	if metaFound {
		createdAt = meta.CreatedAt
	} else {
		createdAt = state.PhaseStartTime
	}

	// 遍历 phase, 从当前 phase 开始检查
	phases := orderedPhases(policy)
	currentPhase := state.Phase
	if currentPhase == "" {
		currentPhase = "hot"
		state.Phase = currentPhase
		state.PhaseStartTime = createdAt
	}

	phase := e.findPhase(phases, currentPhase)
	if phase == nil {
		e.logger.Warn("ILM phase not found in policy",
			zap.String("index", index),
			zap.String("phase", currentPhase),
			zap.String("policy", state.PolicyName))
		return
	}

	// 计算 min_age 是否已到期
	minAge, _ := parseDuration(phase.MinAge)
	if minAge > 0 && time.Since(time.Unix(createdAt, 0)) < minAge {
		// 未到期, 更新 last_checked 后返回
		state.LastChecked = time.Now().Unix()
		_ = e.saveState(index, state)
		return
	}

	// 执行 phase 的 actions
	actions, err := parseActions(phase.Actions)
	if err != nil {
		state.Error = err.Error()
		_ = e.saveState(index, state)
		return
	}

	for _, action := range actions {
		if action.Rollover != nil {
			// 用当前 phase 开始时间作为 max_age 基准, 这样 rollover 后的新索引
			// 会重新计时(避免直接用 meta.CreatedAt 导致多阶段 rollover 无差别触发)
			if e.shouldRollover(index, action.Rollover, state.PhaseStartTime) {
				if err := e.doRollover(index, state, action.Rollover); err != nil {
					e.logger.Error("ILM rollover failed",
						zap.String("index", index),
						zap.Error(err))
					state.Error = err.Error()
					_ = e.saveState(index, state)
					return
				}
				state.RolloverCount++
				state.Phase = nextPhase(phases, currentPhase)
				state.PhaseStartTime = time.Now().Unix()
				e.logger.Info("ILM rollover completed",
					zap.String("index", index),
					zap.Int("rollover_count", state.RolloverCount))
			}
		}
		if action.Delete != nil {
			if err := e.doDelete(index); err != nil {
				e.logger.Error("ILM delete failed",
					zap.String("index", index),
					zap.Error(err))
				state.Error = err.Error()
				_ = e.saveState(index, state)
				return
			}
			e.logger.Info("ILM delete completed", zap.String("index", index))
			return
		}
	}

	state.LastChecked = time.Now().Unix()
	state.Error = ""
	_ = e.saveState(index, state)
}

// shouldRollover 判断是否应触发 rollover
//
// 语义(与 ES 行为一致):
//   - max_age 与 max_docs 之间为 OR 关系: 任一条件满足即触发
//   - 当只配置一个条件时, 只检查该条件
//   - maxAge=0 视为"立即到期", 可用于测试或强制 rollover 场景
//
// 参数:
//   - index: 索引名
//   - action: rollover 动作配置
//   - phaseStart: 当前 phase 开始的 unix 秒时间戳, 用于 max_age 计算
//     (使用 phase_start 而非 meta.created_at, 以便 rollover 后新索引重新计时)
//
// 返回:
//   - true: 满足 max_age 或 max_docs 任一条件
func (e *ILMExecutor) shouldRollover(index string, action *RolloverAction, phaseStart int64) bool {
	if action.MaxAge == "" && action.MaxDocs == 0 {
		return false
	}

	meta, found, _ := e.loadIndexMeta(index)
	if !found {
		return false
	}

	// 检查 max_age(与 max_docs 为 OR 关系, 任一满足即触发)
	if action.MaxAge != "" {
		maxAge, err := parseDuration(action.MaxAge)
		if err == nil {
			start := phaseStart
			if start <= 0 {
				start = meta.CreatedAt
			}
			if time.Since(time.Unix(start, 0)) >= maxAge {
				return true
			}
		}
	}

	// 检查 max_docs
	if action.MaxDocs > 0 && meta.DocCount >= int64(action.MaxDocs) {
		return true
	}

	return false
}

// doRollover 执行 rollover: 创建新索引 + 切换别名
func (e *ILMExecutor) doRollover(index string, state ILMState, action *RolloverAction) error {
	// 生成新索引名: <base>-<rollover+1>
	baseIndex := stripRolloverSuffix(index)
	newIndex := fmt.Sprintf("%s-%d", baseIndex, state.RolloverCount+1)

	// 确保新索引不存在
	exists, _ := e.server.store.Exists(storage.MetaKey(newIndex))
	if exists {
		return fmt.Errorf("rollover target index %s already exists", newIndex)
	}

	// 创建新索引(继承原索引的 settings/mappings)
	meta, found, _ := e.loadIndexMeta(index)
	if !found {
		return fmt.Errorf("source index %s not found", index)
	}

	newMeta := IndexMeta{
		Name:      newIndex,
		CreatedAt: time.Now().Unix(),
		Mapping:   meta.Mapping,
		Settings:  meta.Settings,
	}
	if err := e.server.store.Put(storage.MetaKey(newIndex), newMeta); err != nil {
		return fmt.Errorf("create rollover target: %w", err)
	}

	// 同步 engine 索引
	e.server.engine.CreateIndex(newIndex)

	// 原子切换别名(如果索引有别名)
	// 查找指向旧索引的所有别名, 重新映射到新索引
	e.switchAliases(index, newIndex)

	e.logger.Info("ILM rollover successful",
		zap.String("old_index", index),
		zap.String("new_index", newIndex))
	return nil
}

// switchAliases 把指向 oldIndex 的别名改为指向 newIndex
//
// 逻辑:
//  1. 遍历所有 alias/<name> 键, 解码后找出包含 oldIndex 的别名
//  2. 对每个匹配的别名, 移除 oldIndex 并追加 newIndex, 写回存储
//
// 参数:
//   - oldIndex: rollover 前的源索引名
//   - newIndex: rollover 后新创建的目标索引名
func (e *ILMExecutor) switchAliases(oldIndex, newIndex string) {
	var aliasesToUpdate [][]byte
	_ = e.server.store.Scan([]byte("alias/"), func(k, v []byte) error {
		var list []string
		if err := jsonUnmarshal(v, &list); err != nil {
			return nil
		}
		for _, idx := range list {
			if idx == oldIndex {
				// 拷贝一份, 避免循环中复用底层数组导致切片共享
				cp := make([]byte, len(k))
				copy(cp, k)
				aliasesToUpdate = append(aliasesToUpdate, cp)
				break
			}
		}
		return nil
	})

	for _, aliasKey := range aliasesToUpdate {
		raw, found, err := e.server.store.GetRaw(aliasKey)
		if err != nil || !found {
			continue
		}
		var list []string
		if err := jsonUnmarshal(raw, &list); err != nil {
			continue
		}
		// 移除 oldIndex, 添加 newIndex
		newList := make([]string, 0, len(list)+1)
		for _, idx := range list {
			if idx != oldIndex {
				newList = append(newList, idx)
			}
		}
		newList = append(newList, newIndex)
		_ = e.server.store.PutRaw(aliasKey, mustJSON(newList))
	}
}

// doDelete 删除索引
//
// 清理顺序:
//  1. 移除 ILM 状态键
//  2. 移除索引元信息 meta/<index>
//  3. 批量删除 doc/<index>/ 下的所有文档(持久层)
//  4. 批量删除 doc-tf/<index>/ 下的分词落盘
//  5. 批量删除 doc-meta/<index>/ 下的版本元数据
//  6. 删除 postings-version/<index> 倒排版本号
//  7. 通知 engine 清理内存态(docs + 倒排 + scorer 统计)
//
// 注意: 该函数为 best-effort, 错误仅记录日志, 不让上层阻塞
func (e *ILMExecutor) doDelete(index string) error {
	// 1. 删除 ILM 状态
	_ = e.server.store.Delete(ILMStateKey(index))

	// 2. 删除索引元数据
	_ = e.server.store.Delete(storage.MetaKey(index))

	// 3. 删除所有文档 (doc/<index>/...)
	_ = e.server.store.DeletePrefix(storage.DocPrefix(index))

	// 4. 删除 doc-tf 分词结果 (doc-tf/<index>/...)
	_ = e.server.store.DeletePrefix(storage.DocTFPrefix(index))

	// 5. 删除 doc-meta 版本号 (doc-meta/<index>/...)
	_ = e.server.store.DeletePrefix([]byte("doc-meta/" + index + "/"))

	// 6. 删除倒排版本号
	_ = e.server.store.Delete(storage.PostingsVersionKey(index))

	// 7. 通知 engine 从内存中移除
	e.server.engine.DeleteIndex(index)

	return nil
}

// loadState 加载索引的 ILM 状态
func (e *ILMExecutor) loadState(index string) (ILMState, bool, error) {
	var state ILMState
	found, err := e.server.store.Get(ILMStateKey(index), &state)
	return state, found, err
}

// saveState 保存索引的 ILM 状态
func (e *ILMExecutor) saveState(index string, state ILMState) error {
	return e.server.store.Put(ILMStateKey(index), state)
}

// loadPolicy 加载 ILM policy
func (e *ILMExecutor) loadPolicy(name string) (*ILMPolicy, error) {
	var policy ILMPolicy
	found, err := e.server.store.Get(storage.ILMKey(name), &policy)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("policy %s not found", name)
	}
	return &policy, nil
}

// loadIndexMeta 加载索引元信息
func (e *ILMExecutor) loadIndexMeta(index string) (IndexMeta, bool, error) {
	var meta IndexMeta
	found, err := e.server.store.Get(storage.MetaKey(index), &meta)
	return meta, found, err
}

// InitManagedIndex 初始化索引的 ILM 受管状态
// 当用户通过 index template + ILM policy 创建索引时调用
func (e *ILMExecutor) InitManagedIndex(index, policyName string) error {
	state := ILMState{
		PolicyName: policyName,
		Phase:      "hot",
		Managed:    true,
		LastChecked: time.Now().Unix(),
	}
	// 用索引创建时间作为 phase_start
	meta, found, _ := e.loadIndexMeta(index)
	if found {
		state.PhaseStartTime = meta.CreatedAt
	} else {
		state.PhaseStartTime = time.Now().Unix()
	}
	return e.saveState(index, state)
}

// GetILMState 获取索引的 ILM 状态(供/_ilm/explain 使用)
func (e *ILMExecutor) GetILMState(index string) (ILMState, error) {
	state, found, err := e.loadState(index)
	if err != nil {
		return ILMState{}, err
	}
	if !found {
		return ILMState{}, fmt.Errorf("no ILM state for index %s", index)
	}
	return state, nil
}

// ---------- 辅助函数 ----------

// orderedPhases 返回按固定顺序排列的 phase 列表(hot -> warm -> cold -> delete)
func orderedPhases(policy *ILMPolicy) []struct {
	Name  string
	Phase ILMPhase
} {
	order := []string{"hot", "warm", "cold", "frozen", "delete"}
	var result []struct {
		Name  string
		Phase ILMPhase
	}
	for _, name := range order {
		if phase, ok := policy.Policy.Phases[name]; ok {
			result = append(result, struct {
				Name  string
				Phase ILMPhase
			}{Name: name, Phase: phase})
		}
	}
	return result
}

// findPhase 在有序 phase 列表中查找指定名称的 phase
func (e *ILMExecutor) findPhase(phases []struct {
	Name  string
	Phase ILMPhase
}, name string) *ILMPhase {
	for _, p := range phases {
		if p.Name == name {
			return &p.Phase
		}
	}
	return nil
}

// nextPhase 找到当前 phase 的下一个 phase(按顺序)
func nextPhase(phases []struct {
	Name  string
	Phase ILMPhase
}, current string) string {
	for i, p := range phases {
		if p.Name == current && i+1 < len(phases) {
			return phases[i+1].Name
		}
	}
	return current
}

// parseActions 解析 phase 的 actions 配置
func parseActions(raw map[string]interface{}) ([]ILMAction, error) {
	var actions []ILMAction
	for kind, def := range raw {
		switch kind {
		case "rollover":
			if rollover, ok := def.(map[string]interface{}); ok {
				action := ILMAction{}
				ra := &RolloverAction{}
				if v, ok := rollover["max_age"].(string); ok {
					ra.MaxAge = v
				}
				if v, ok := rollover["max_docs"].(float64); ok {
					ra.MaxDocs = int(v)
				}
				if v, ok := rollover["max_docs"].(int); ok {
					ra.MaxDocs = v
				}
				action.Rollover = ra
				actions = append(actions, action)
			}
		case "delete":
			action := ILMAction{Delete: &DeleteAction{}}
			actions = append(actions, action)
		case "forcemerge":
			actions = append(actions, ILMAction{Forcemerge: def.(map[string]interface{})})
		case "set_priority":
			actions = append(actions, ILMAction{SetPriority: def.(map[string]interface{})})
		}
	}
	return actions, nil
}

// parseDuration 解析时间字符串(如 "1d", "1h", "30m", "100ms")
func parseDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	// 尝试标准 Go duration
	d, err := time.ParseDuration(s)
	if err == nil {
		return d, nil
	}

	// 尝试 ES 风格: "1d" -> 24h, "1m" -> 1m
	patterns := []struct {
		re    *regexp.Regexp
		mult  time.Duration
	}{
		{regexp.MustCompile(`^(\d+)d$`), 24 * time.Hour},
		{regexp.MustCompile(`^(\d+)h$`), time.Hour},
		{regexp.MustCompile(`^(\d+)m$`), time.Minute},
		{regexp.MustCompile(`^(\d+)s$`), time.Second},
		{regexp.MustCompile(`^(\d+)ms$`), time.Millisecond},
	}

	for _, p := range patterns {
		matches := p.re.FindStringSubmatch(s)
		if matches != nil {
			n, err := strconv.Atoi(matches[1])
			if err != nil {
				return 0, err
			}
			return time.Duration(n) * p.mult, nil
		}
	}

	return 0, fmt.Errorf("invalid duration: %s", s)
}

// stripRolloverSuffix 从索引名中去掉 rollover 序号后缀
// 例如 "logs-000001" -> "logs"
func stripRolloverSuffix(index string) string {
	parts := strings.Split(index, "-")
	if len(parts) >= 2 {
		last := parts[len(parts)-1]
		if _, err := strconv.Atoi(last); err == nil {
			return strings.Join(parts[:len(parts)-1], "-")
		}
	}
	return index
}

// ILMStateResponse /_ilm/explain 响应结构
type ILMStateResponse struct {
	Indices map[string]ILMExplainInfo `json:"indices"`
}

// ILMExplainInfo 单个索引的 explain 信息
type ILMExplainInfo struct {
	Managed    bool   `json:"managed"`
	Policy     string `json:"policy,omitempty"`
	Phase      string `json:"phase,omitempty"`
	Action     string `json:"action,omitempty"`
	Step       string `json:"step,omitempty"`
	RolloverCount int  `json:"rollover_count,omitempty"`
	Error      string `json:"error,omitempty"`
}

// BuildILMExplainResponse 构建 /_ilm/explain 的响应
func BuildILMExplainResponse(index string, state ILMState) ILMStateResponse {
	info := ILMExplainInfo{
		Managed:       state.Managed,
		Policy:        state.PolicyName,
		Phase:         state.Phase,
		Action:        "complete",
		Step:          "complete",
		RolloverCount: state.RolloverCount,
		Error:         state.Error,
	}
	return ILMStateResponse{
		Indices: map[string]ILMExplainInfo{
			index: info,
		},
	}
}
