// ConfigLoader 单元测试
package server

import (
	"os"
	"path/filepath"
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
