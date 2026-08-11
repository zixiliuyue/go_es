// ILM 执行器单元测试
//
// 覆盖范围:
//   - parseDuration(秒/分/时/天/ms/非法字符串)
//   - orderedPhases / findPhase / nextPhase 等辅助函数
//   - parseActions(rollover/delete/forcemerge/set_priority)
//   - NewILMExecutor / Start / Stop 生命周期
//   - InitManagedIndex / GetILMState
//   - rollover 完整流程(建 policy → 建受管索引 → 触发 tick → 新索引出现 + 别名切换)
//   - delete 完整流程(建 delete phase policy → 触发 tick → 索引被清除)
//   - 未到期 phase 不应触发动作
//   - /_ilm/explain 真实状态返回(含错误信息持久化)
//   - switchAliases 别名切换逻辑
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zixiliuyue/go_es/internal/search"
	"github.com/zixiliuyue/go_es/internal/storage"
	"go.uber.org/zap"
)

// ---------------------------------------------------------------------------
// 辅助函数
// ---------------------------------------------------------------------------

// newILMTestServer 启动一个带 ILM 执行器(短 tick 间隔)的测试 server
// interval: ILM 扫描间隔(传 0 则默认 50ms 方便测试)
func newILMTestServer(t *testing.T, interval time.Duration) (*httptest.Server, *ILMExecutor, *storage.Store) {
	t.Helper()
	if interval <= 0 {
		interval = 50 * time.Millisecond
	}
	store, err := storage.Open("")
	require.NoError(t, err)
	engine := search.New(store)
	srv := NewWithOptions(store, engine, zap.NewNop(), ServerOptions{})
	// 用短间隔替换掉默认的 30s, 便于测试快速触发 tick
	if srv.ilmExecutor != nil {
		srv.ilmExecutor.ticker.Stop()
	}
	srv.ilmExecutor = NewILMExecutor(srv, zap.NewNop(), interval)
	srv.ilmExecutor.Start()
	srv.MarkStartupDone()
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		if srv.ilmExecutor != nil {
			srv.ilmExecutor.Stop()
		}
		_ = store.Close()
	})
	return ts, srv.ilmExecutor, store
}

// createIndex 创建一个索引(通过存储层, 绕开 httptest)
func createIndex(t *testing.T, store *storage.Store, engine *search.Engine, name string) {
	t.Helper()
	meta := IndexMeta{
		Name:      name,
		CreatedAt: time.Now().Unix(),
		Mapping:   map[string]interface{}{"properties": map[string]interface{}{"title": map[string]string{"type": "text"}}},
		Settings:  map[string]interface{}{},
	}
	require.NoError(t, store.Put(storage.MetaKey(name), meta))
	engine.CreateIndex(name)
}

// createILMPolicy 在 store 中写入一个 policy
func createILMPolicy(t *testing.T, store *storage.Store, name string, policy map[string]interface{}) {
	t.Helper()
	require.NoError(t, store.Put(storage.ILMKey(name), policy))
}

// putDoc 写入一个文档, 并在 engine 中登记
func putDoc(t *testing.T, store *storage.Store, engine *search.Engine, index, id string, body map[string]interface{}) {
	t.Helper()
	require.NoError(t, store.Put(storage.DocKey(index, id), body))
	engine.IndexDoc(index, id, body)
}

// decodeBody 解码响应 body 为 map
func decodeBody(t *testing.T, raw []byte) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &m))
	return m
}

// doSimple 发起一次请求(不注入 header)
func doSimple(t *testing.T, ts *httptest.Server, method, path string, body interface{}) (int, map[string]interface{}) {
	t.Helper()
	var rd interface{}
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		rd = b
	}
	var req *http.Request
	var err error
	if rd != nil {
		req, err = http.NewRequest(method, ts.URL+path, byteReader(rd.([]byte)))
	} else {
		req, err = http.NewRequest(method, ts.URL+path, nil)
	}
	require.NoError(t, err)
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	var raw map[string]interface{}
	if resp.ContentLength != 0 {
		body := make([]byte, 4096)
		n, _ := resp.Body.Read(body)
		raw = decodeBody(t, body[:n])
	}
	return resp.StatusCode, raw
}

// byteReader 简单包装
func byteReader(b []byte) *byteReaderImpl { return &byteReaderImpl{b: b} }

type byteReaderImpl struct{ b []byte }

func (r *byteReaderImpl) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, errEOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}

// errEOF 避免引入 io 包的额外 import
var errEOF = eofError{}

type eofError struct{}

func (eofError) Error() string { return "EOF" }

// ---------------------------------------------------------------------------
// parseDuration
// ---------------------------------------------------------------------------

func TestILM_ParseDuration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		err  bool
	}{
		{"", 0, false},
		{"1s", time.Second, false},
		{"30s", 30 * time.Second, false},
		{"100ms", 100 * time.Millisecond, false},
		{"1m", time.Minute, false},
		{"5m", 5 * time.Minute, false},
		{"2h", 2 * time.Hour, false},
		{"1d", 24 * time.Hour, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"1x", 0, true},
		{"abc", 0, true},
	}
	for _, c := range cases {
		d, err := parseDuration(c.in)
		if c.err {
			assert.Error(t, err, "in=%s", c.in)
			continue
		}
		assert.NoError(t, err, "in=%s", c.in)
		assert.Equal(t, c.want, d, "in=%s", c.in)
	}
}

// ---------------------------------------------------------------------------
// orderedPhases / findPhase / nextPhase
// ---------------------------------------------------------------------------

func TestILM_OrderedPhases(t *testing.T) {
	policy := &ILMPolicy{
		Policy: struct {
			Phases map[string]ILMPhase `json:"phases"`
		}{Phases: map[string]ILMPhase{
			"delete": {MinAge: "30d", Actions: map[string]interface{}{"delete": map[string]interface{}{}}},
			"hot":    {MinAge: "0s", Actions: map[string]interface{}{"rollover": map[string]interface{}{"max_age": "1d"}}},
			"warm":   {MinAge: "7d", Actions: map[string]interface{}{}},
		}},
	}
	phases := orderedPhases(policy)
	names := make([]string, 0, len(phases))
	for _, p := range phases {
		names = append(names, p.Name)
	}
	assert.Equal(t, []string{"hot", "warm", "delete"}, names)
}

func TestILM_FindAndNextPhase(t *testing.T) {
	policy := &ILMPolicy{
		Policy: struct {
			Phases map[string]ILMPhase `json:"phases"`
		}{Phases: map[string]ILMPhase{
			"hot":  {MinAge: "0s", Actions: map[string]interface{}{}},
			"warm": {MinAge: "7d", Actions: map[string]interface{}{}},
		}},
	}
	e := &ILMExecutor{}
	phases := orderedPhases(policy)

	// findPhase: 存在/不存在
	assert.NotNil(t, e.findPhase(phases, "hot"))
	assert.Nil(t, e.findPhase(phases, "delete"))

	// nextPhase: hot -> warm; warm -> warm(已是最后)
	assert.Equal(t, "warm", nextPhase(phases, "hot"))
	assert.Equal(t, "warm", nextPhase(phases, "warm"))
}

// ---------------------------------------------------------------------------
// parseActions
// ---------------------------------------------------------------------------

func TestILM_ParseActions(t *testing.T) {
	raw := map[string]interface{}{
		"rollover": map[string]interface{}{
			"max_age":  "1h",
			"max_docs": float64(1000),
		},
		"delete":       map[string]interface{}{},
		"forcemerge":   map[string]interface{}{"max_num_segments": 1},
		"set_priority": map[string]interface{}{"priority": 100},
	}
	actions, err := parseActions(raw)
	require.NoError(t, err)
	require.Len(t, actions, 4)

	// 检查 rollover 的 MaxDocs(float64 来自 JSON)
	var foundRollover bool
	for _, a := range actions {
		if a.Rollover != nil {
			foundRollover = true
			assert.Equal(t, "1h", a.Rollover.MaxAge)
			assert.Equal(t, 1000, a.Rollover.MaxDocs)
		}
		if a.Delete != nil {
			assert.NotNil(t, a.Delete)
		}
	}
	assert.True(t, foundRollover)
}

// ---------------------------------------------------------------------------
// 生命周期
// ---------------------------------------------------------------------------

func TestILMExecutor_Lifecycle(t *testing.T) {
	store, err := storage.Open("")
	require.NoError(t, err)
	defer store.Close()
	logger := zap.NewNop()

	e := NewILMExecutor(nil, logger, 0) // 默认回退 30s
	assert.Equal(t, 30*time.Second, e.interval)

	e = NewILMExecutor(nil, logger, 10*time.Millisecond)
	assert.Equal(t, 10*time.Millisecond, e.interval)

	e.Start()
	e.Stop() // 第二次 stop 幂等
	e.Stop()
}

// ---------------------------------------------------------------------------
// InitManagedIndex / GetILMState
// ---------------------------------------------------------------------------

func TestILMExecutor_InitAndGetState(t *testing.T) {
	store, err := storage.Open("")
	require.NoError(t, err)
	defer store.Close()
	engine := search.New(store)

	createIndex(t, store, engine, "logs")
	srv := New(store, engine, zap.NewNop())
	exec := NewILMExecutor(srv, zap.NewNop(), time.Minute)

	// 未初始化 GetILMState 应报错
	_, err = exec.GetILMState("logs")
	assert.Error(t, err)

	// 初始化受管状态
	require.NoError(t, exec.InitManagedIndex("logs", "my-policy"))

	state, err := exec.GetILMState("logs")
	require.NoError(t, err)
	assert.True(t, state.Managed)
	assert.Equal(t, "my-policy", state.PolicyName)
	assert.Equal(t, "hot", state.Phase)
	assert.NotZero(t, state.PhaseStartTime)
}

// ---------------------------------------------------------------------------
// rollover 流程
// ---------------------------------------------------------------------------

func TestILMExecutor_Rollover_Integration(t *testing.T) {
	ts, exec, store := newILMTestServer(t, 80*time.Millisecond)
	defer ts.Close()

	engine := search.New(store)

	// 1. 写入 policy(hot phase: 0s min_age, rollover max_age=0s)
	policy := map[string]interface{}{
		"policy": map[string]interface{}{
			"phases": map[string]interface{}{
				"hot": map[string]interface{}{
					"min_age": "0s",
					"actions": map[string]interface{}{
						"rollover": map[string]interface{}{
							"max_age":  "0s",
							"max_docs": float64(2),
						},
					},
				},
				"warm": map[string]interface{}{
					"min_age": "100d",
				},
			},
		},
	}
	createILMPolicy(t, store, "rollover-policy", policy)

	// 2. 创建源索引 + 2 个 doc(达到 max_docs)
	createIndex(t, store, engine, "logs")
	putDoc(t, store, engine, "logs", "1", map[string]interface{}{"msg": "hello"})
	putDoc(t, store, engine, "logs", "2", map[string]interface{}{"msg": "world"})

	// 把 DocCount 更新到 meta
	meta, found, err := loadIndexMetaDirect(store, "logs")
	require.True(t, found)
	require.NoError(t, err)
	meta.DocCount = 2
	require.NoError(t, store.Put(storage.MetaKey("logs"), meta))

	// 3. 初始化受管索引
	require.NoError(t, exec.InitManagedIndex("logs", "rollover-policy"))

	// 4. 等待 tick 触发(rollover 50ms tick + 处理时间, 等 500ms 足够)
	assert.Eventually(t, func() bool {
		// 新索引 logs-1 应出现
		found, _ := store.Exists(storage.MetaKey("logs-1"))
		return found
	}, 2*time.Second, 50*time.Millisecond, "rollover should create new index")

	// 5. 验证源索引状态
	state, err := exec.GetILMState("logs")
	require.NoError(t, err)
	assert.Equal(t, "warm", state.Phase, "should move to warm after rollover")
	assert.Equal(t, 1, state.RolloverCount)
}

// loadIndexMetaDirect 直接从 store 读取 meta, 便于测试
func loadIndexMetaDirect(store *storage.Store, index string) (IndexMeta, bool, error) {
	var meta IndexMeta
	found, err := store.Get(storage.MetaKey(index), &meta)
	return meta, found, err
}

// ---------------------------------------------------------------------------
// delete 流程
// ---------------------------------------------------------------------------

func TestILMExecutor_Delete_Integration(t *testing.T) {
	store, err := storage.Open("")
	require.NoError(t, err)
	defer store.Close()
	engine := search.New(store)

	// 1. 写入 policy(含 delete phase, rollover+delete 动作)
	policy := map[string]interface{}{
		"policy": map[string]interface{}{
			"phases": map[string]interface{}{
				"hot": map[string]interface{}{
					"min_age": "0s",
					"actions": map[string]interface{}{
						"rollover": map[string]interface{}{
							"max_age":  "0s",
							"max_docs": float64(1),
						},
					},
				},
				"delete": map[string]interface{}{
					"min_age": "0s",
					"actions": map[string]interface{}{
						"delete": map[string]interface{}{},
					},
				},
			},
		},
	}
	createILMPolicy(t, store, "delete-policy", policy)

	// 2. 创建源索引 + 文档
	createIndex(t, store, engine, "events")
	putDoc(t, store, engine, "events", "1", map[string]interface{}{"msg": "hi"})

	// 3. 构造 state 在 delete phase, 直接触发删除
	srv := New(store, engine, zap.NewNop())
	exec := NewILMExecutor(srv, zap.NewNop(), time.Minute)
	state := ILMState{
		PolicyName:    "delete-policy",
		Phase:         "delete",
		PhaseStartTime: time.Now().Add(-time.Hour).Unix(),
		RolloverCount: 0,
		Managed:       true,
		LastChecked:   time.Now().Unix(),
	}
	require.NoError(t, store.Put(ILMStateKey("events"), state))

	exec.runTick()

	// 4. 验证索引及数据已被清除
	found, _ := store.Exists(storage.MetaKey("events"))
	assert.False(t, found, "events meta should be deleted")

	found, _ = store.Exists(storage.DocKey("events", "1"))
	assert.False(t, found, "events/1 doc should be deleted")

	found, _ = store.Exists(storage.DocTFKey("events", "1"))
	assert.False(t, found, "events/1 doc-tf should be deleted")

	found, _ = store.Exists(ILMStateKey("events"))
	assert.False(t, found, "events ILM state should be deleted")

	assert.Empty(t, engine.AllIDs("events"), "engine should have no docs for deleted index")
}

// ---------------------------------------------------------------------------
// 未到期 phase 不应触发
// ---------------------------------------------------------------------------

func TestILMExecutor_PhaseNotReady_NoAction(t *testing.T) {
	store, err := storage.Open("")
	require.NoError(t, err)
	defer store.Close()
	engine := search.New(store)

	createIndex(t, store, engine, "slow")
	createILMPolicy(t, store, "slow-policy", map[string]interface{}{
		"policy": map[string]interface{}{
			"phases": map[string]interface{}{
				"hot": map[string]interface{}{
					"min_age": "100d", // 很久之后才触发
					"actions": map[string]interface{}{
						"rollover": map[string]interface{}{
							"max_age": "100d",
						},
					},
				},
			},
		},
	})

	srv := New(store, engine, zap.NewNop())
	exec := NewILMExecutor(srv, zap.NewNop(), 50*time.Millisecond)
	require.NoError(t, exec.InitManagedIndex("slow", "slow-policy"))

	// 跑一次 tick, 不应该有任何动作
	exec.runTick()
	state, err := exec.GetILMState("slow")
	require.NoError(t, err)
	assert.Equal(t, "hot", state.Phase)
	assert.Equal(t, 0, state.RolloverCount)
	assert.Empty(t, state.Error)
}

// ---------------------------------------------------------------------------
// shouldRollover 边界条件
// ---------------------------------------------------------------------------

func TestILMExecutor_ShouldRollover(t *testing.T) {
	store, err := storage.Open("")
	require.NoError(t, err)
	defer store.Close()
	engine := search.New(store)

	createIndex(t, store, engine, "idx")
	// 写入 meta(无 doc_count, 用默认 0)
	srv := New(store, engine, zap.NewNop())
	exec := NewILMExecutor(srv, zap.NewNop(), time.Minute)

	// 1. 空动作 -> false
	assert.False(t, exec.shouldRollover("idx", &RolloverAction{}, time.Now().Unix()))

	// 2. max_age 已过期
	assert.True(t, exec.shouldRollover("idx", &RolloverAction{MaxAge: "0s"}, time.Now().Add(-time.Hour).Unix()))

	// 3. max_age 未过期
	assert.False(t, exec.shouldRollover("idx", &RolloverAction{MaxAge: "1h"}, time.Now().Unix()))

	// 4. 非法 duration 不应 panic
	assert.False(t, exec.shouldRollover("idx", &RolloverAction{MaxAge: "bad"}, time.Now().Add(-time.Hour).Unix()))

	// 5. max_docs 满足
	meta, _, _ := loadIndexMetaDirect(store, "idx")
	meta.DocCount = 10
	require.NoError(t, store.Put(storage.MetaKey("idx"), meta))
	assert.True(t, exec.shouldRollover("idx", &RolloverAction{MaxDocs: 10}, time.Now().Unix()))
	assert.False(t, exec.shouldRollover("idx", &RolloverAction{MaxDocs: 20}, time.Now().Unix()))

	// 6. 不存在的索引 -> false
	assert.False(t, exec.shouldRollover("no-such", &RolloverAction{MaxAge: "0s"}, time.Now().Unix()))

	// 7. phaseStart=0 时回退到 meta.CreatedAt(已过期)
	meta.CreatedAt = time.Now().Add(-time.Hour).Unix()
	meta.DocCount = 0
	require.NoError(t, store.Put(storage.MetaKey("idx"), meta))
	assert.True(t, exec.shouldRollover("idx", &RolloverAction{MaxAge: "0s"}, 0))
}

// ---------------------------------------------------------------------------
// processIndex 错误路径
// ---------------------------------------------------------------------------

func TestILMExecutor_ProcessIndex_ErrorPaths(t *testing.T) {
	store, err := storage.Open("")
	require.NoError(t, err)
	defer store.Close()
	engine := search.New(store)
	srv := New(store, engine, zap.NewNop())
	exec := NewILMExecutor(srv, zap.NewNop(), time.Minute)

	// 1. 无 state -> 跳过
	exec.processIndex(nil, "ghost")

	// 2. 有 state 但 policy 不存在 -> state 应带 error
	state := ILMState{
		PolicyName: "missing",
		Phase:      "hot",
		Managed:    true,
	}
	require.NoError(t, store.Put(ILMStateKey("broken"), state))
	exec.processIndex(nil, "broken")
	state, err = exec.GetILMState("broken")
	require.NoError(t, err)
	assert.Contains(t, state.Error, "policy")

	// 3. phase 不存在 -> 无 error, 记录 warn
	createIndexDirect(t, store, "orphan")
	state = ILMState{
		PolicyName:    "p",
		Phase:         "ghost-phase",
		PhaseStartTime: time.Now().Unix(),
		Managed:       true,
	}
	require.NoError(t, store.Put(storage.ILMKey("p"), map[string]interface{}{
		"policy": map[string]interface{}{
			"phases": map[string]interface{}{"hot": map[string]interface{}{}},
		},
	}))
	require.NoError(t, store.Put(ILMStateKey("orphan"), state))
	exec.processIndex(nil, "orphan")
	state, err = exec.GetILMState("orphan")
	require.NoError(t, err)
	assert.Empty(t, state.Error)

	// 4. min_age 未到期 -> 不应触发 action
	require.NoError(t, store.Put(storage.ILMKey("p2"), map[string]interface{}{
		"policy": map[string]interface{}{
			"phases": map[string]interface{}{
				"hot": map[string]interface{}{
					"min_age": "100d",
					"actions": map[string]interface{}{
						"rollover": map[string]interface{}{"max_age": "100d"},
					},
				},
			},
		},
	}))
	state = ILMState{
		PolicyName:    "p2",
		Phase:         "hot",
		PhaseStartTime: time.Now().Unix(), // 刚开始
		Managed:       true,
	}
	require.NoError(t, store.Put(ILMStateKey("young"), state))
	createIndexDirect(t, store, "young")
	exec.processIndex(nil, "young")
	state, err = exec.GetILMState("young")
	require.NoError(t, err)
	assert.Equal(t, "hot", state.Phase)
	assert.Empty(t, state.Error)
}

func TestILM_ExplainEndpoint(t *testing.T) {
	ts, exec, store := newILMTestServer(t, time.Minute)
	defer ts.Close()

	// 1. 未受管索引(通过真实 HTTPClient)
	resp, err := http.Get(ts.URL + "/noindex/_ilm/explain")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, 200, resp.StatusCode)
	var out map[string]interface{}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	indices, ok := out["indices"].(map[string]interface{})
	require.True(t, ok)
	info, ok := indices["noindex"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, false, info["managed"])

	// 2. 受管索引(手动写入 state)
	createIndexDirect(t, store, "myindex")
	state := ILMState{
		PolicyName:    "p1",
		Phase:         "warm",
		PhaseStartTime: time.Now().Add(-time.Hour).Unix(),
		RolloverCount: 2,
		Managed:       true,
		LastChecked:   time.Now().Unix(),
		Error:         "",
	}
	require.NoError(t, store.Put(ILMStateKey("myindex"), state))

	resp, err = http.Get(ts.URL + "/myindex/_ilm/explain")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	indices = out["indices"].(map[string]interface{})
	info = indices["myindex"].(map[string]interface{})
	assert.Equal(t, true, info["managed"])
	assert.Equal(t, "p1", info["policy"])
	assert.Equal(t, "warm", info["phase"])
	assert.Equal(t, float64(2), info["rollover_count"])

	// 3. policy 不存在 -> explain 仍返回 managed, 运行一次 tick 后应带错误信息
	createIndexDirect(t, store, "broken")
	require.NoError(t, exec.InitManagedIndex("broken", "missing-policy"))
	exec.runTick()
	state, err = exec.GetILMState("broken")
	require.NoError(t, err)
	assert.Contains(t, state.Error, "policy")
}

// doSimpleHTTP 简化请求
func doSimpleHTTP(t *testing.T, ts *httptest.Server, method, path string, body interface{}) (int, map[string]interface{}) {
	t.Helper()
	var rd interface{}
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		rd = b
	}
	var req *http.Request
	var err error
	if rd != nil {
		req, err = http.NewRequest(method, ts.URL+path, byteReader(rd.([]byte)))
	} else {
		req, err = http.NewRequest(method, ts.URL+path, nil)
	}
	require.NoError(t, err)
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw := make([]byte, 4096)
	n, _ := resp.Body.Read(raw)
	if n == 0 {
		return resp.StatusCode, nil
	}
	var out map[string]interface{}
	_ = json.Unmarshal(raw[:n], &out)
	return resp.StatusCode, out
}

// createIndexDirect 仅在 store 中创建索引 meta(不启动 engine)
func createIndexDirect(t *testing.T, store *storage.Store, name string) {
	t.Helper()
	meta := IndexMeta{
		Name:      name,
		CreatedAt: time.Now().Unix(),
		Mapping:   map[string]interface{}{},
		Settings:  map[string]interface{}{},
	}
	require.NoError(t, store.Put(storage.MetaKey(name), meta))
}

// ---------------------------------------------------------------------------
// switchAliases 行为
// ---------------------------------------------------------------------------

func TestILMExecutor_SwitchAliases(t *testing.T) {
	store, err := storage.Open("")
	require.NoError(t, err)
	defer store.Close()
	engine := search.New(store)

	createIndex(t, store, engine, "src")
	// 建立别名 myalias -> [src]
	require.NoError(t, store.PutRaw(storage.AliasKey("myalias"), []byte(`["src"]`)))

	srv := New(store, engine, zap.NewNop())
	exec := NewILMExecutor(srv, zap.NewNop(), time.Minute)

	exec.switchAliases("src", "dst")

	// 断言 myalias 现在指向 dst
	raw, found, err := store.GetRaw(storage.AliasKey("myalias"))
	require.NoError(t, err)
	require.True(t, found)
	var list []string
	require.NoError(t, json.Unmarshal(raw, &list))
	assert.Equal(t, []string{"dst"}, list)

	// 另一个不包含 src 的别名应保持不变
	require.NoError(t, store.PutRaw(storage.AliasKey("other"), []byte(`["unrelated"]`)))
	exec.switchAliases("src", "dst") // 没有任何 alias 含 src
	raw, found, _ = store.GetRaw(storage.AliasKey("other"))
	require.True(t, found)
	require.Equal(t, `["unrelated"]`, string(raw))
}

// ---------------------------------------------------------------------------
// BuildILMExplainResponse
// ---------------------------------------------------------------------------

func TestBuildILMExplainResponse(t *testing.T) {
	state := ILMState{
		PolicyName:    "p1",
		Phase:         "hot",
		RolloverCount: 3,
		Managed:       true,
		Error:         "",
	}
	resp := BuildILMExplainResponse("idx", state)
	assert.Equal(t, 1, len(resp.Indices))
	info, ok := resp.Indices["idx"]
	require.True(t, ok)
	assert.True(t, info.Managed)
	assert.Equal(t, "p1", info.Policy)
	assert.Equal(t, "hot", info.Phase)
	assert.Equal(t, "complete", info.Action)
	assert.Equal(t, "complete", info.Step)
	assert.Equal(t, 3, info.RolloverCount)
}

// ---------------------------------------------------------------------------
// stripRolloverSuffix
// ---------------------------------------------------------------------------

func TestStripRolloverSuffix(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"logs-1", "logs"},
		{"logs-000001", "logs"}, // 尾部全数字段(零填充也视为 rollover 序号, 整体剥离)
		{"logs", "logs"},
		{"my-app-3", "my-app"},
		{"logs-with-number-0", "logs-with-number"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, stripRolloverSuffix(c.in), "in=%s", c.in)
	}
}
