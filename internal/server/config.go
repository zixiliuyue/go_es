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

// ConfigFile 配置文件结构
type ConfigFile struct {
	Addr  string         `yaml:"addr"`
	Data  string         `yaml:"data"`
	Auth  AuthConfig     `yaml:"auth"`
	Limit LimitConfig    `yaml:"limit"`
	Log   string         `yaml:"log_level"`
	Watch time.Duration  `yaml:"watch_interval"`
	Extra map[string]any `yaml:"extra,omitempty"`
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
