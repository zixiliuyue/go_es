// 运行时 Profiling + 配置热加载
//
// 设计:
//   - /_debug/pprof/*: Go 性能剖析端点(类似 net/http/pprof)
//     * index: pprof 索引页
//     * cmdline: 命令行参数
//     * profile: CPU profile
//     * symbols: 符号表
//     * goroutine: goroutine 堆栈
//     * heap: 内存堆栈
//     * threadcreate: 线程创建历史
//     * allocs: 内存分配历史
//     * block: 阻塞历史
//     * mutex: 互斥锁历史
//   - /_config/reload: 手动触发配置热加载
package server

import (
	"fmt"
	"net/http"
	stdpprof "net/http/pprof"
	"runtime"
	rtpprof "runtime/pprof"
	"strconv"
	"time"
)

// handlePprofIndex GET /_debug/pprof
func (s *Server) handlePprofIndex(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, `go_es server pprof endpoints:
	/_debug/pprof/cmdline
	/_debug/pprof/profile
	/_debug/pprof/symbol
	/_debug/pprof/goroutine
	/_debug/pprof/heap
	/_debug/pprof/threadcreate
	/_debug/pprof/allocs
	/_debug/pprof/block
	/_debug/pprof/mutex
`)
}

// handlePprofCmdline GET /_debug/pprof/cmdline
func (s *Server) handlePprofCmdline(w http.ResponseWriter, r *http.Request) {
	stdpprof.Cmdline(w, r)
}

// handlePprofProfile GET /_debug/pprof/profile
func (s *Server) handlePprofProfile(w http.ResponseWriter, r *http.Request) {
	stdpprof.Profile(w, r)
}

// handlePprofSymbols GET /_debug/pprof/symbols
func (s *Server) handlePprofSymbols(w http.ResponseWriter, r *http.Request) {
	stdpprof.Symbol(w, r)
}

// handlePprofGoroutine GET /_debug/pprof/goroutine
func (s *Server) handlePprofGoroutine(w http.ResponseWriter, r *http.Request) {
	prof := rtpprof.Lookup("goroutine")
	if prof == nil {
		http.NotFound(w, r)
		return
	}
	_ = prof.WriteTo(w, 1)
}

// handlePprofHeap GET /_debug/pprof/heap
func (s *Server) handlePprofHeap(w http.ResponseWriter, r *http.Request) {
	prof := rtpprof.Lookup("heap")
	if prof == nil {
		http.NotFound(w, r)
		return
	}
	_ = prof.WriteTo(w, 1)
}

// handlePprofThreadcreate GET /_debug/pprof/threadcreate
func (s *Server) handlePprofThreadcreate(w http.ResponseWriter, r *http.Request) {
	prof := rtpprof.Lookup("threadcreate")
	if prof == nil {
		http.NotFound(w, r)
		return
	}
	_ = prof.WriteTo(w, 1)
}

// handlePprofAllocs GET /_debug/pprof/allocs
func (s *Server) handlePprofAllocs(w http.ResponseWriter, r *http.Request) {
	runtime.MemProfileRate = 1
	prof := rtpprof.Lookup("allocs")
	if prof == nil {
		http.NotFound(w, r)
		return
	}
	_ = prof.WriteTo(w, 1)
}

// handlePprofBlock GET /_debug/pprof/block
func (s *Server) handlePprofBlock(w http.ResponseWriter, r *http.Request) {
	runtime.SetBlockProfileRate(1)
	prof := rtpprof.Lookup("block")
	if prof == nil {
		http.NotFound(w, r)
		return
	}
	_ = prof.WriteTo(w, 1)
}

// handlePprofMutex GET /_debug/pprof/mutex
func (s *Server) handlePprofMutex(w http.ResponseWriter, r *http.Request) {
	runtime.SetMutexProfileFraction(1)
	prof := rtpprof.Lookup("mutex")
	if prof == nil {
		http.NotFound(w, r)
		return
	}
	_ = prof.WriteTo(w, 1)
}

// handleConfigReload POST /_config/reload
// 手动触发配置热加载
func (s *Server) handleConfigReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST", "")
		return
	}

	// 强制触发配置重载(如果 ConfigLoader 已初始化)
	if s.configLoader != nil {
		if err := s.configLoader.ForceReload(); err != nil {
			writeError(w, http.StatusInternalServerError, "config_reload_failed", err.Error(), "")
			return
		}
	}

	// 同时返回当前配置(如果已初始化)
	var addr, logLevel string
	var authEnabled bool
	if s.configLoader != nil {
		cfg := s.configLoader.Get()
		addr = cfg.Addr
		logLevel = cfg.Log
		authEnabled = cfg.Auth.Enabled
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":      "reloaded",
		"addr":        addr,
		"auth":        map[string]interface{}{"enabled": authEnabled},
		"log_level":   logLevel,
		"reloaded_at": time.Now().UTC().Format(time.RFC3339),
	})
}

// GetRuntimeStats 获取运行时统计信息
func GetRuntimeStats() map[string]interface{} {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	return map[string]interface{}{
		"goroutines": runtime.NumGoroutine(),
		"memory": map[string]interface{}{
			"alloc":        m.Alloc,
			"total_alloc":  m.TotalAlloc,
			"sys":          m.Sys,
			"lookups":      m.Lookups,
			"mallocs":      m.Mallocs,
			"frees":        m.Frees,
			"heap_alloc":   m.HeapAlloc,
			"heap_sys":     m.HeapSys,
			"heap_idle":    m.HeapIdle,
			"heap_inuse":   m.HeapInuse,
			"stack_inuse":  m.StackInuse,
			"stack_sys":    m.StackSys,
			"num_gc":       uint64(m.NumGC),
			"next_gc":      m.NextGC,
			"gc_pause_ns":  m.PauseTotalNs,
		},
		"go_version": runtime.Version(),
		"num_cpu":    runtime.NumCPU(),
	}
}

// handleRuntimeStats GET /_stats 扩展端点,返回运行时统计
func (s *Server) handleRuntimeStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET", "")
		return
	}
	writeJSON(w, http.StatusOK, GetRuntimeStats())
}

// parseInt 辅助函数: 字符串转 int
func parseInt(s string) (int, error) {
	return strconv.Atoi(s)
}
