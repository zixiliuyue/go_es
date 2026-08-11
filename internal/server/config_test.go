// ConfigLoader 单元测试
package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestConfigLoader_BasicLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go_es.yaml")
	yamlContent := `addr: ":9200"
data: "./data"
auth:
  enabled: true
  basic:
    admin: "secret"
limit:
  max_body_bytes: 1048576
  rate_per_second: 100
log_level: "info"
`
	if err := os.WriteFile(path, []byte(yamlContent), 0644); err != nil {
		t.Fatal(err)
	}
	l := NewConfigLoader(path)
	if err := l.Load(); err != nil {
		t.Fatal(err)
	}
	cfg := l.Get()
	assert.Equal(t, ":9200", cfg.Addr)
	assert.Equal(t, "./data", cfg.Data)
	assert.True(t, cfg.Auth.Enabled)
	assert.Equal(t, "secret", cfg.Auth.Basic["admin"])
	assert.Equal(t, int64(1048576), cfg.Limit.MaxBodyBytes)
	assert.Equal(t, 100.0, cfg.Limit.RatePerSecond)
}

func TestConfigLoader_EmptyPath(t *testing.T) {
	l := NewConfigLoader("")
	assert.NoError(t, l.Load())
	assert.Equal(t, "", l.Get().Addr)
}

func TestConfigLoader_BadYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go_es.yaml")
	if err := os.WriteFile(path, []byte("not valid: [yaml"), 0644); err != nil {
		t.Fatal(err)
	}
	l := NewConfigLoader(path)
	err := l.Load()
	assert.Error(t, err)
}

func TestConfigLoader_ReloadOnChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go_es.yaml")
	if err := os.WriteFile(path, []byte("addr: \":9200\""), 0644); err != nil {
		t.Fatal(err)
	}
	l := NewConfigLoader(path)
	if err := l.Load(); err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, ":9200", l.Get().Addr)

	// 模拟 1s 后的修改
	time.Sleep(20 * time.Millisecond)
	newContent := "addr: \":9999\"\n"
	if err := os.WriteFile(path, []byte(newContent), 0644); err != nil {
		t.Fatal(err)
	}
	if err := l.Load(); err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, ":9999", l.Get().Addr)
}

func TestConfigLoader_ChangeCallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go_es.yaml")
	if err := os.WriteFile(path, []byte("addr: \":9200\""), 0644); err != nil {
		t.Fatal(err)
	}
	l := NewConfigLoader(path)
	called := 0
	l.SetOnChange(func(old, new *ConfigFile) { called++ })
	_ = l.Load()
	_ = l.Load() // 第二次调用, mtime 相同但仍触发回调
	assert.GreaterOrEqual(t, called, 1)
}

// 验证 tls 块能从 yaml 正确解析, 并区分 "未设置" 与 "显式 false"
func TestConfigLoader_TLSBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go_es.yaml")
	// 显式给 cert/key/enable_http2
	yamlFull := `addr: ":9200"
tls:
  cert: "/tmp/server.crt"
  key:  "/tmp/server.key"
  enable_http2: false
`
	if err := os.WriteFile(path, []byte(yamlFull), 0644); err != nil {
		t.Fatal(err)
	}
	l := NewConfigLoader(path)
	if err := l.Load(); err != nil {
		t.Fatal(err)
	}
	cfg := l.Get()
	assert.Equal(t, "/tmp/server.crt", cfg.TLS.CertFile)
	assert.Equal(t, "/tmp/server.key", cfg.TLS.KeyFile)
	assert.True(t, cfg.TLS.Enabled(), "cert+key 都给时应 Enabled")
	assert.NotNil(t, cfg.TLS.EnableHTTP2)
	assert.False(t, cfg.TLS.H2Enabled(), "显式 false 时 H2Enabled 应为 false")

	// 不写 tls 块: EnableHTTP2 应为 nil, H2Enabled 默认 true, Enabled() 为 false
	path2 := filepath.Join(dir, "no_tls.yaml")
	if err := os.WriteFile(path2, []byte("addr: \":9200\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	l2 := NewConfigLoader(path2)
	if err := l2.Load(); err != nil {
		t.Fatal(err)
	}
	cfg2 := l2.Get()
	assert.False(t, cfg2.TLS.Enabled(), "无 cert/key 时 Enabled() 为 false")
	assert.Nil(t, cfg2.TLS.EnableHTTP2)
	assert.True(t, cfg2.TLS.H2Enabled(), "未设置时 H2Enabled 默认 true")

	// 只给 cert 不给 key: schema 校验应拒绝(单边配置启动会失败)
	path3 := filepath.Join(dir, "half_tls.yaml")
	if err := os.WriteFile(path3, []byte("tls:\n  cert: \"/x\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	l3 := NewConfigLoader(path3)
	err := l3.Load()
	assert.Error(t, err, "单边 TLS 配置应被 schema 校验拒绝")
	assert.Contains(t, err.Error(), "tls.cert", "错误应指向 tls.cert 路径")
	assert.Contains(t, err.Error(), "必须同时配置", "错误应说明需同时配置 cert+key")
}

// ---------- Schema 校验单元测试 ----------

// 通用辅助: 写一个 yaml 配置文件, 调 Load, 返回 (err, body)
func writeCfg(t *testing.T, content string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "go_es.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path, NewConfigLoader(path).Load()
}

// 6) 完整正常配置应通过
func TestSchema_ValidFullConfig(t *testing.T) {
	yaml := `addr: ":9200"
data: "./data"
auth:
  enabled: true
  basic:
    admin: "secret"
  api_keys:
    - "key1"
limit:
  max_body_bytes: 104857600
  rate_per_second: 1000
  burst: 1000
tls:
  cert: "/etc/go_es/server.crt"
  key:  "/etc/go_es/server.key"
  enable_http2: true
log_level: "info"
watch_interval: 5s
`
	_, err := writeCfg(t, yaml)
	assert.NoError(t, err, "完整合法配置应通过 schema 校验")
}

// 7) 必填项: addr 不合法格式应报错并定位
func TestSchema_BadAddrFormat(t *testing.T) {
	yaml := `addr: "localhost:9200"` // 缺冒号前缀
	_, err := writeCfg(t, yaml)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "addr", "错误应指向 addr")
	assert.Contains(t, err.Error(), ":port", "错误应说明格式要求(:port 形式)")
}

// 8) 范围越界: limit.max_body_bytes 超过 1GiB
func TestSchema_RangeViolation(t *testing.T) {
	yaml := `addr: ":9200"
limit:
  max_body_bytes: 9999999999999
`
	_, err := writeCfg(t, yaml)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "limit.max_body_bytes", "错误应指向具体路径")
	assert.Contains(t, err.Error(), "1 GiB", "错误应说明范围")
	assert.Contains(t, err.Error(), "实际值", "错误应展示实际值")
}

// 9) 类型不匹配: log_level 写成数字
func TestSchema_TypeMismatch(t *testing.T) {
	yaml := `addr: ":9200"
log_level: 123
`
	_, err := writeCfg(t, yaml)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "log_level", "错误应指向 log_level")
}

// 10) 枚举不合法: log_level 不在 debug/info/warn/error
func TestSchema_EnumViolation(t *testing.T) {
	yaml := `addr: ":9200"
log_level: "trace"
`
	_, err := writeCfg(t, yaml)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "log_level")
	assert.Contains(t, err.Error(), "debug/info/warn/error", "应列出合法枚举")
}

// 11) 业务规则: auth.enabled=true 但无凭据
func TestSchema_AuthEnabledNoCredentials(t *testing.T) {
	yaml := `addr: ":9200"
auth:
  enabled: true
`
	_, err := writeCfg(t, yaml)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "auth.enabled", "错误应指向 auth.enabled")
	assert.Contains(t, err.Error(), "凭据", "错误应说明缺少凭据")
}

// 12) 业务规则: auth.enabled=true 但有 api_keys
func TestSchema_AuthEnabledWithApiKeys(t *testing.T) {
	yaml := `addr: ":9200"
auth:
  enabled: true
  api_keys:
    - "k1"
`
	_, err := writeCfg(t, yaml)
	assert.NoError(t, err, "启用 auth + api_keys 应通过")
}

// 13) 业务规则: TLS cert 单边
func TestSchema_TLSSingleSide_CertOnly(t *testing.T) {
	yaml := `addr: ":9200"
tls:
  cert: "/etc/go_es/c.crt"
`
	_, err := writeCfg(t, yaml)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "必须同时配置", "应说明 cert+key 须同配")
}

// 14) 业务规则: TLS key 单边
func TestSchema_TLSSingleSide_KeyOnly(t *testing.T) {
	yaml := `addr: ":9200"
tls:
  key: "/etc/go_es/k.key"
`
	_, err := writeCfg(t, yaml)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "必须同时配置")
}

// 14b) mTLS 业务规则: 设 client_ca 但 client_auth=none -> 违反
func TestSchema_MTLSSingleSide_CAWithoutAuth(t *testing.T) {
	yaml := `addr: ":9200"
tls:
  cert: "/etc/go_es/c.crt"
  key: "/etc/go_es/c.key"
  client_ca: "/etc/go_es/ca.crt"
  client_auth: "none"
`
	_, err := writeCfg(t, yaml)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "必须配对", "应说明 mTLS client_ca/client_auth 必须配对")
}

// 14c) mTLS 业务规则: 设 client_auth 非 none 但 client_ca 为空 -> 违反
func TestSchema_MTLSSingleSide_AuthWithoutCA(t *testing.T) {
	yaml := `addr: ":9200"
tls:
  cert: "/etc/go_es/c.crt"
  key: "/etc/go_es/c.key"
  client_auth: "require_verify"
`
	_, err := writeCfg(t, yaml)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "必须配对", "应说明 mTLS client_ca/client_auth 必须配对")
}

// 14d) mTLS 完整配置: 合法 (CA + auth=require_verify)
func TestSchema_MTLSValidFull(t *testing.T) {
	yaml := `addr: ":9200"
tls:
  cert: "/etc/go_es/c.crt"
  key: "/etc/go_es/c.key"
  client_ca: "/etc/go_es/ca.crt"
  client_auth: "require_verify"
`
	_, err := writeCfg(t, yaml)
	assert.NoError(t, err, "mTLS 完整配置应通过")
}

// 14e) mTLS client_auth 枚举非法值
func TestSchema_MTLSClientAuthInvalidEnum(t *testing.T) {
	yaml := `addr: ":9200"
tls:
  cert: "/etc/go_es/c.crt"
  key: "/etc/go_es/c.key"
  client_ca: "/etc/go_es/ca.crt"
  client_auth: "demand"  # 非法
`
	_, err := writeCfg(t, yaml)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "client_auth", "错误应指向 client_auth 路径")
}

// 15) 错误聚合: 多个错误一次性报
func TestSchema_AggregateMultipleErrors(t *testing.T) {
	yaml := `addr: ":9200"
log_level: "trace"
limit:
  max_body_bytes: -1
  rate_per_second: -100
`
	_, err := writeCfg(t, yaml)
	assert.Error(t, err)
	msg := err.Error()
	// 至少 3 条 schema violation
	assert.Contains(t, msg, "schema violation", "应报告 schema 违例")
	assert.Contains(t, msg, "log_level", "应包含 log_level 错误")
	assert.Contains(t, msg, "limit.max_body_bytes", "应包含 max_body_bytes 错误")
	assert.Contains(t, msg, "limit.rate_per_second", "应包含 rate_per_second 错误")
}

// 16) 错误格式: 路径 + 期望 + 实际值
func TestSchema_ErrorMessageFormat(t *testing.T) {
	yaml := `addr: ":9200"
limit:
  max_body_bytes: -5
`
	_, err := writeCfg(t, yaml)
	assert.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "config: limit.max_body_bytes", "路径前缀")
	assert.Contains(t, msg, "0 ~", "范围提示")
	assert.Contains(t, msg, "实际值=", "实际值")
}

// 17) 与 yaml 解析错误同级: 校验失败用 fmt.Errorf wrap 同样的 "load config" 路径
func TestSchema_WrappedAsLoadError(t *testing.T) {
	yaml := `addr: ":9200"
log_level: "bogus"
`
	_, err := writeCfg(t, yaml)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "validate config:", "应 wrap 为 validate config 错误")
}

// 18) 验证失败时不修改 current(防止热更新场景下损坏 in-memory 状态)
func TestSchema_FailureLeavesCurrentUnchanged(t *testing.T) {
	// 1) 先加载一个合法配置
	dir := t.TempDir()
	path := filepath.Join(dir, "go_es.yaml")
	good := `addr: ":9200"
log_level: "info"
`
	if err := os.WriteFile(path, []byte(good), 0644); err != nil {
		t.Fatal(err)
	}
	l := NewConfigLoader(path)
	if err := l.Load(); err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, "info", l.Get().Log)

	// 2) 模拟文件被改坏(用户手贱)
	bad := `addr: ":9200"
log_level: "fatal"
`
	if err := os.WriteFile(path, []byte(bad), 0644); err != nil {
		t.Fatal(err)
	}
	err := l.Load()
	assert.Error(t, err)

	// 3) current 仍是上一个合法值
	assert.Equal(t, "info", l.Get().Log, "校验失败不应回写 current")
}

// 19) 范围下限: 0 应被接受
func TestSchema_RangeLowerBoundInclusive(t *testing.T) {
	yaml := `addr: ":9200"
limit:
  max_body_bytes: 0
  rate_per_second: 0
  burst: 0
`
	_, err := writeCfg(t, yaml)
	assert.NoError(t, err, "0 应当作合法下限")
}

// 20) Pattern 规则: addr 形如 ":abc" 应被拒绝
func TestSchema_PatternRejectsNonNumericPort(t *testing.T) {
	yaml := `addr: ":abc"
`
	_, err := writeCfg(t, yaml)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "addr")
}

// 21) *bool 字段: 显式 false 是合法值(非 zero 跳过)
func TestSchema_BoolPointerExplicitFalse(t *testing.T) {
	yaml := `addr: ":9200"
tls:
  cert: "/x"
  key:  "/y"
  enable_http2: false
`
	_, err := writeCfg(t, yaml)
	assert.NoError(t, err, "显式 false 应通过(与 nil 区分)")
}

// 22) SanityCheck: 端口号超出范围
func TestSchema_SanityCheckBadPort(t *testing.T) {
	cfg := &ConfigFile{Addr: "99999"} // 缺冒号且数字越界
	err := SanityCheck(cfg)
	// 缺冒号先被 net.SplitHostPort 抓到
	assert.Error(t, err)
}

// ---------- #15 新增: slowlog schema 规则 ----------

// 23) slowlog.threshold_ms 合法值(0 默认)通过
func TestSchema_SlowLog_ThresholdValid(t *testing.T) {
	yaml := `addr: ":9200"
slowlog:
  threshold_ms: 0
`
	_, err := writeCfg(t, yaml)
	assert.NoError(t, err, "threshold_ms=0 表示默认, 应通过")

	yaml = `addr: ":9200"
slowlog:
  threshold_ms: 1000
`
	_, err = writeCfg(t, yaml)
	assert.NoError(t, err, "threshold_ms=1000 应通过")

	yaml = `addr: ":9200"
slowlog:
  threshold_ms: 60000
`
	_, err = writeCfg(t, yaml)
	assert.NoError(t, err, "threshold_ms=60000(上限) 应通过")
}

// 24) slowlog.threshold_ms 超出上限报错
func TestSchema_SlowLog_ThresholdOverLimit(t *testing.T) {
	yaml := `addr: ":9200"
slowlog:
  threshold_ms: 60001
`
	_, err := writeCfg(t, yaml)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "slowlog.threshold_ms", "错误路径应指向 slowlog.threshold_ms")
	assert.Contains(t, err.Error(), "60000", "应提示上限 60000")
}

// 25) slowlog.log_5xx true/false 合法
func TestSchema_SlowLog_Log5xxBoolValid(t *testing.T) {
	yaml := `addr: ":9200"
slowlog:
  log_5xx: true
`
	_, err := writeCfg(t, yaml)
	assert.NoError(t, err, "log_5xx=true 应通过")

	yaml = `addr: ":9200"
slowlog:
  log_5xx: false
`
	_, err = writeCfg(t, yaml)
	assert.NoError(t, err, "log_5xx=false 应通过(显式 false 合法, 非 nil 跳过)")
}

// 26) slowlog.log_5xx 类型不匹配(填了字符串)报错
func TestSchema_SlowLog_Log5xxTypeMismatch(t *testing.T) {
	// 注意: YAML 1.1 把 yes/no/on/off/true/false 自动解析为 bool
	// 这里用 yaml.v3 明确会解析为字符串的内容, 使 *bool 收到非 bool 原始值后触发
	// 类型规则校验: 我们通过 yaml.v3 的 Strict(false) 行为, 当 struct 是 *bool 而
	// YAML 值是数字时, 解析会出错, 因此这里用 123 触发 yaml parse 错误更直接
	yaml := `addr: ":9200"
slowlog:
  log_5xx: 123
`
	_, err := writeCfg(t, yaml)
	assert.Error(t, err)
	// 要么是 yaml 解析阶段报了类型错误, 要么是 schema 抓到 log_5xx 异常
	msg := err.Error()
	assert.True(t,
		strings.Contains(msg, "slowlog.log_5xx") || strings.Contains(msg, "yaml:") ||
			strings.Contains(msg, "unmarshal") || strings.Contains(msg, "type"),
		"应报告 log_5xx 或 yaml 类型错, 实际: %s", msg)
}

// 27) slowlog 完整配置 + tracing/session 共存应通过(回归)
func TestSchema_SlowLog_FullBlockValid(t *testing.T) {
	yaml := `addr: ":9200"
data: "./data"
auth:
  enabled: true
  basic:
    admin: "secret"
slowlog:
  threshold_ms: 250
  log_5xx: true
tracing:
  enabled: true
  service_name: "go_es_test"
  service_version: "v0.7.0"
  propagation: "both"
  sampling_rate: 1.0
log_level: "info"
watch_interval: 10s
`
	// 先写文件, 通过 writeCfg 做 schema 校验
	dir := t.TempDir()
	path := filepath.Join(dir, "go_es.yaml")
	assert.NoError(t, os.WriteFile(path, []byte(yaml), 0644))
	l := NewConfigLoader(path)
	assert.NoError(t, l.Load(), "slowlog + tracing + session 同配置应通过")

	// 再检查 SlowLog 字段值
	cfg := l.Get()
	assert.EqualValues(t, 250, cfg.SlowLog.ThresholdMs, "threshold_ms 应读到 250")
	assert.NotNil(t, cfg.SlowLog.Log5xx, "log_5xx 应为非 nil *bool")
	assert.True(t, *cfg.SlowLog.Log5xx, "log_5xx 应等于 true")
}
