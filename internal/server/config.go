// 服务端配置文件加载与热更新
//
// 配置文件格式(YAML):
//
//   addr: ":9200"
//   data: "./data"
//   auth:
//     enabled: true
//     basic:
//       admin: "secret"
//   limit:
//     max_body_bytes: 104857600     # 100 MiB
//     rate_per_second: 1000
//   log_level: "info"
//
// 通过 -config <path> 启动, 服务端每 WatchInterval 轮询 mtime,
// 检测到变化后重新解析并应用(只对可热更新项生效, 如 auth/limit;
// addr/data 启动后不可变, 改动需重启).
package server

import (
	"fmt"
	"os"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// SlowLogConfig 慢请求日志配置(支持 YAML 热更新)
type SlowLogConfig struct {
	// ThresholdMs 慢请求阈值(毫秒), 超过该耗时的请求记为慢请求, 默认 500ms
	ThresholdMs int64 `yaml:"threshold_ms"`
	// Log5xx 是否对 5xx 响应独立输出 WARN 日志, 默认 true
	Log5xx *bool `yaml:"log_5xx,omitempty"`
}

// ConfigFile 配置文件结构
type ConfigFile struct {
	Addr      string         `yaml:"addr"`
	Data      string         `yaml:"data"`
	Auth      AuthConfig     `yaml:"auth"`
	Limit     LimitConfig    `yaml:"limit"`
	TLS       TLSConfig      `yaml:"tls"`
	Session   SessionConfig  `yaml:"session"`
	Tracing   TracingConfig  `yaml:"tracing"`
	SlowLog   SlowLogConfig  `yaml:"slowlog"`
	RemoteWrite RemoteWriteConfig `yaml:"remote_write"`
	OTelExport OTelExportConfig `yaml:"otel_export"`
	Log       string         `yaml:"log_level"`
	Watch     time.Duration  `yaml:"watch_interval"`
	Extra     map[string]any `yaml:"extra,omitempty"`
}

// TLSConfig TLS 监听配置.
// 证书路径只在启动时生效(和 addr/data 一样), 修改需重启.
//
// mTLS(mTLS) 模式: 同时配置 CertFile/KeyFile/ClientCAFile 时启用.
// ClientAuth 控制对客户端证书的强制级别:
//   - "none"           : 不要求, 标准 TLS(默认)
//   - "request"        : 握手时请求, 但不强制/不验证(降级兼容)
//   - "require_any"    : 强制要求, 但不验证证书链
//   - "require_verify" : 强制要求 且 验证证书链(生产用, 默认 mTLS 推荐)
type TLSConfig struct {
	// CertFile PEM 编码的证书链路径
	CertFile string `yaml:"cert"`
	// KeyFile PEM 编码的私钥路径
	KeyFile string `yaml:"key"`
	// EnableHTTP2 true 时在 TLS 上协商 h2(默认 true, 需要 -tls.cert/-tls.key 同时设置)
	// 用指针以便区分 "未设置" 与 "显式 false"
	EnableHTTP2 *bool `yaml:"enable_http2,omitempty"`
	// ClientCAFile PEM 编码的 CA 池, 用于校验客户端证书(mTLS 用).
	// 路径只在启动时生效.
	ClientCAFile string `yaml:"client_ca,omitempty"`
	// ClientAuth 客户端证书强制级别, 枚举: none/request/require_any/require_verify.
	// 用指针区分 "未设置(= none)" 与 "显式设置"; 默认 mTLS 推荐 require_verify.
	ClientAuth *string `yaml:"client_auth,omitempty"`
}

// ClientAuthKind 客户端证书强制级别枚举
type ClientAuthKind string

const (
	ClientAuthNone          ClientAuthKind = "none"
	ClientAuthRequest       ClientAuthKind = "request"
	ClientAuthRequireAny    ClientAuthKind = "require_any"
	ClientAuthRequireVerify ClientAuthKind = "require_verify"
)

// AuthKind 返回 client_auth 字段的枚举值, 缺省 none.
func (t TLSConfig) AuthKind() ClientAuthKind {
	if t.ClientAuth == nil {
		return ClientAuthNone
	}
	return ClientAuthKind(*t.ClientAuth)
}

// Enabled 当 cert+key 都配置时启用 TLS
func (t TLSConfig) Enabled() bool {
	return t.CertFile != "" && t.KeyFile != ""
}

// MTLSEnabled mTLS 模式: cert+key+client_ca 全部配置 且 auth != none
func (t TLSConfig) MTLSEnabled() bool {
	return t.Enabled() && t.ClientCAFile != "" && t.AuthKind() != ClientAuthNone
}

// H2Enabled 返回 h2 是否启用, 默认为 true
func (t TLSConfig) H2Enabled() bool {
	if t.EnableHTTP2 == nil {
		return true
	}
	return *t.EnableHTTP2
}

// DefaultWatchInterval 默认轮询间隔
const DefaultWatchInterval = 5 * time.Second

// ConfigLoader 配置文件加载器
type ConfigLoader struct {
	mu       sync.RWMutex
	path     string
	current  ConfigFile
	lastMTime time.Time
	// 变更回调
	onChange func(old, new *ConfigFile)
	stop     chan struct{}
}

// NewConfigLoader 创建 loader
func NewConfigLoader(path string) *ConfigLoader {
	return &ConfigLoader{path: path, stop: make(chan struct{})}
}

// Load 同步加载一次, 解析到 current
// 解析流程: read -> yaml.Unmarshal -> schema.Validate -> 提交 current
// 任何一步失败都返回错误, cmd/server/main.go 的 log.Fatalf 统一处理
func (l *ConfigLoader) Load() error {
	if l.path == "" {
		return nil
	}
	data, err := os.ReadFile(l.path)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var cfg ConfigFile
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	// Schema 校验: 在 unmarshal 之后, 提交 current 之前.
	// 校验失败 = 启动期报错(与 parse 错误同级别), 不修改 current.
	if verrs := DefaultConfigSchema().Validate(&cfg); len(verrs) > 0 {
		return fmt.Errorf("validate config: %w", verrs)
	}
	// 附加健全性检查(端口号范围)
	if err := SanityCheck(&cfg); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}
	if cfg.Watch == 0 {
		cfg.Watch = DefaultWatchInterval
	}
	l.mu.Lock()
	old := l.current
	l.current = cfg
	st, _ := os.Stat(l.path)
	if st != nil {
		l.lastMTime = st.ModTime()
	}
	l.mu.Unlock()
	if l.onChange != nil {
		l.onChange(&old, &cfg)
	}
	return nil
}

// Get 当前配置(只读快照)
func (l *ConfigLoader) Get() ConfigFile {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.current
}

// SetOnChange 设置变更回调
func (l *ConfigLoader) SetOnChange(fn func(old, new *ConfigFile)) {
	l.onChange = fn
}

// Watch 启动轮询协程, 检测到 mtime 变化就重新加载
// 阻塞直到 Stop()
func (l *ConfigLoader) Watch() {
	if l.path == "" {
		return
	}
	// 初次加载
	if err := l.Load(); err != nil {
		// 加载失败也启动 watch, 让用户修复配置后能自愈
		fmt.Fprintf(os.Stderr, "[config] initial load failed: %v\n", err)
	}
	interval := l.Get().Watch
	if interval <= 0 {
		interval = DefaultWatchInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			st, err := os.Stat(l.path)
			if err != nil {
				continue
			}
			l.mu.RLock()
			mt := l.lastMTime
			l.mu.RUnlock()
			if st.ModTime().After(mt) {
				if err := l.Load(); err != nil {
					fmt.Fprintf(os.Stderr, "[config] reload failed: %v\n", err)
					continue
				}
				fmt.Fprintf(os.Stderr, "[config] reloaded from %s\n", l.path)
			}
		}
	}
}

// Stop 停止 watch
func (l *ConfigLoader) Stop() {
	if l.stop != nil {
		select {
		case <-l.stop:
			return
		default:
			close(l.stop)
		}
	}
}

// ForceReload 强制重新加载配置(忽略 mtime 检查)
// 用于 /_config/reload 端点的手动触发场景.
func (l *ConfigLoader) ForceReload() error {
	if l.path == "" {
		return nil
	}
	data, err := os.ReadFile(l.path)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var cfg ConfigFile
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	if verrs := DefaultConfigSchema().Validate(&cfg); len(verrs) > 0 {
		return fmt.Errorf("validate config: %w", verrs)
	}
	if err := SanityCheck(&cfg); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}
	if cfg.Watch == 0 {
		cfg.Watch = DefaultWatchInterval
	}
	l.mu.Lock()
	old := l.current
	l.current = cfg
	if st, _ := os.Stat(l.path); st != nil {
		l.lastMTime = st.ModTime()
	}
	l.mu.Unlock()
	if l.onChange != nil {
		l.onChange(&old, &cfg)
	}
	return nil
}

// ToServerOptions 将 ConfigFile 转换为 ServerOptions
func (c *ConfigFile) ToServerOptions() ServerOptions {
	return ServerOptions{
		Auth:        c.Auth,
		Limit:       c.Limit,
		Session:     c.Session,
		Tracing:     c.Tracing,
		RemoteWrite: c.RemoteWrite,
		OTelExport:  c.OTelExport,
	}
}
