// go_es 服务端 Prometheus 监控指标
//
// 暴露以下指标:
//   - go_es_http_requests_total{method,route,status}        请求总数
//   - go_es_http_request_duration_seconds{method,route}    请求耗时直方图
//   - go_es_http_in_flight_requests{method,route}          正在处理的请求数
//   - go_es_storage_txn_total{kind,status}                 存储事务数
//   - go_es_index_doc_count{index}                         每个索引的文档数
//   - go_es_start_time_seconds                             进程启动时间
//   - go_es_build_info{version,go_version}                 构建信息(常量 1)
//
// 路径打标使用"路由模板"(如 /{index}/_doc/{id})而非真实路径,
// 避免高基数索引名/文档 ID 撑爆 Prometheus。
package server

import (
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// ServerMetrics 服务端监控指标集合
type ServerMetrics struct {
	registry *prometheus.Registry

	httpRequestsTotal   *prometheus.CounterVec
	httpRequestDuration *prometheus.HistogramVec
	httpInFlight        *prometheus.GaugeVec

	storageTxnTotal  *prometheus.CounterVec
	indexDocCount    *prometheus.GaugeVec
	startTimeSeconds prometheus.Gauge
	buildInfo        *prometheus.GaugeVec

	// 新增 (M1.14)
	// WriteCoordinator
	wcInFlight     prometheus.Gauge
	wcTotalOK      *prometheus.CounterVec
	wcTotalFailed  *prometheus.CounterVec
	wcTotalBatches *prometheus.CounterVec
	// AccessLog
	accessLogWritten *prometheus.GaugeVec
	accessLogDropped *prometheus.GaugeVec
	accessLogBytes   *prometheus.GaugeVec
	// Segment
	segTotalSegments prometheus.Gauge
	segTotalFlushes  prometheus.Gauge
	segTotalBytes    prometheus.Gauge
	// Optimistic lock 冲突
	ocConflicts *prometheus.CounterVec
	// RBAC
	rbacAuthFail  *prometheus.CounterVec
	rbacForbidden *prometheus.CounterVec
	// SearchCache (#11)
	cacheHits   prometheus.Gauge
	cacheMisses prometheus.Gauge
	cacheSize   prometheus.Gauge

	// inFlight 状态保护并发计数
	inFlight sync.Map // map[inflightKey]*float64 -> gauge 指针
}

type inflightKey struct {
	method string
	route  string
}

// NewServerMetrics 创建并注册一个独立 registry,
// 避免与进程内其它 Prometheus 注册表互相干扰
func NewServerMetrics() *ServerMetrics {
	reg := prometheus.NewRegistry()
	m := &ServerMetrics{registry: reg}

	m.httpRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "go_es",
			Name:      "http_requests_total",
			Help:      "Total number of HTTP requests processed by go_es server.",
		},
		[]string{"method", "route", "status"},
	)

	m.httpRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "go_es",
			Name:      "http_request_duration_seconds",
			Help:      "Histogram of HTTP request latencies in seconds.",
			Buckets: []float64{
				0.0005, 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
			},
		},
		[]string{"method", "route"},
	)

	m.httpInFlight = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "go_es",
			Name:      "http_in_flight_requests",
			Help:      "Number of HTTP requests currently being handled.",
		},
		[]string{"method", "route"},
	)

	m.storageTxnTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "go_es",
			Name:      "storage_txn_total",
			Help:      "Total number of BadgerDB transactions executed.",
		},
		[]string{"kind", "status"},
	)

	m.indexDocCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "go_es",
			Name:      "index_doc_count",
			Help:      "Number of documents per index as observed by the search engine.",
		},
		[]string{"index"},
	)

	m.startTimeSeconds = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "go_es",
			Name:      "start_time_seconds",
			Help:      "Unix epoch time when the go_es server process started.",
		},
	)
	m.startTimeSeconds.Set(float64(time.Now().Unix()))

	m.buildInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "go_es",
			Name:      "build_info",
			Help:      "Constant 1; labels carry build metadata.",
		},
		[]string{"version", "go_version"},
	)
	m.buildInfo.WithLabelValues("0.1.0", "1.25").Set(1)

	// ----- 新增 (M1.14) -----
	// WriteCoordinator
	m.wcInFlight = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "go_es", Name: "wc_in_flight",
		Help: "Current in-flight write transactions.",
	})
	m.wcTotalOK = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "go_es", Name: "wc_ops_total",
		Help: "Total write operations succeeded.",
	}, []string{"kind"})
	m.wcTotalFailed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "go_es", Name: "wc_ops_failed_total",
		Help: "Total write operations failed.",
	}, []string{"reason"})
	m.wcTotalBatches = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "go_es", Name: "wc_batches_total",
		Help: "Total write batches processed.",
	}, []string{"status"})

	// AccessLog
	m.accessLogWritten = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "go_es", Name: "accesslog_written_total",
		Help: "Total access log entries written.",
	}, []string{"sink"})
	m.accessLogDropped = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "go_es", Name: "accesslog_dropped_total",
		Help: "Total access log entries dropped (buffer full).",
	}, []string{"sink"})
	m.accessLogBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "go_es", Name: "accesslog_bytes_total",
		Help: "Total bytes written to access log.",
	}, []string{"sink"})

	// Segment
	m.segTotalSegments = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "go_es", Name: "segment_total",
		Help: "Total segments currently in storage.",
	})
	m.segTotalFlushes = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "go_es", Name: "segment_flushes_total",
		Help: "Total segment flushes performed.",
	})
	m.segTotalBytes = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "go_es", Name: "segment_bytes_total",
		Help: "Total bytes occupied by all segments.",
	})

	// Optimistic lock
	m.ocConflicts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "go_es", Name: "optimistic_conflicts_total",
		Help: "Total optimistic lock conflicts (409).",
	}, []string{"op", "kind"})

	// RBAC
	m.rbacAuthFail = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "go_es", Name: "rbac_auth_failures_total",
		Help: "Total authentication failures.",
	}, []string{"reason"})
	m.rbacForbidden = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "go_es", Name: "rbac_forbidden_total",
		Help: "Total forbidden accesses.",
	}, []string{"action", "index"})

	// SearchCache (#11)
	m.cacheHits = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "go_es", Name: "search_cache_hits_total",
		Help: "Total number of search cache hits.",
	})
	m.cacheMisses = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "go_es", Name: "search_cache_misses_total",
		Help: "Total number of search cache misses.",
	})
	m.cacheSize = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "go_es", Name: "search_cache_size",
		Help: "Current number of entries in the search cache.",
	})

	reg.MustRegister(
		m.httpRequestsTotal,
		m.httpRequestDuration,
		m.httpInFlight,
		m.storageTxnTotal,
		m.indexDocCount,
		m.startTimeSeconds,
		m.buildInfo,
		m.wcInFlight, m.wcTotalOK, m.wcTotalFailed, m.wcTotalBatches,
		m.accessLogWritten, m.accessLogDropped, m.accessLogBytes,
		m.segTotalSegments, m.segTotalFlushes, m.segTotalBytes,
		m.ocConflicts,
		m.rbacAuthFail, m.rbacForbidden,
		m.cacheHits, m.cacheMisses, m.cacheSize,
	)
	// 预热 label, 让无样本的指标也能在 /metrics 出现
	m.wcTotalBatches.WithLabelValues("ok")
	m.wcTotalOK.WithLabelValues("total")
	m.wcTotalFailed.WithLabelValues("total")
	m.accessLogWritten.WithLabelValues("stdout")
	m.accessLogDropped.WithLabelValues("stdout")
	m.accessLogBytes.WithLabelValues("stdout")
	m.ocConflicts.WithLabelValues("write", "create")
	m.rbacAuthFail.WithLabelValues("no_user")
	m.rbacForbidden.WithLabelValues("read", "*")
	return m
}

// Registry 暴露内部 registry(给 promhttp.HandlerFor 使用)
func (m *ServerMetrics) Registry() *prometheus.Registry { return m.registry }

// Collect 收集所有 gouge 统计到 Prometheus(由 /metrics scrape 前调用)
// s: server 句柄, 用于拿各子系统当前统计
func (m *ServerMetrics) Collect(s *Server) {
	if m == nil || s == nil {
		return
	}
	// WriteCoordinator
	if s.wc != nil {
		stats := s.wc.Stats()
		m.wcInFlight.Set(float64(stats.InFlight))
		m.wcTotalBatches.WithLabelValues("ok").Add(float64(stats.TotalBatches))
		m.wcTotalOK.WithLabelValues("total").Add(float64(stats.TotalOK))
		m.wcTotalFailed.WithLabelValues("total").Add(float64(stats.TotalFailed))
	}
	// AccessLog
	if s.accessLog != nil {
		stats := s.accessLog.Stats()
		sink := "stdout"
		if s.accessLog.cfg.FilePath != "" {
			sink = "file"
		}
		m.accessLogWritten.WithLabelValues(sink).Set(float64(stats.Written))
		m.accessLogDropped.WithLabelValues(sink).Set(float64(stats.Dropped))
		m.accessLogBytes.WithLabelValues(sink).Set(float64(stats.Bytes))
	}
	// Segment
	if s.seg != nil {
		stats := s.seg.Stats()
		m.segTotalSegments.Set(float64(stats.TotalSegments))
		m.segTotalFlushes.Set(float64(stats.TotalFlushes))
		m.segTotalBytes.Set(float64(stats.TotalBytes))
	}
	// SearchCache (#11)
	if s.searchCache != nil {
		stats := s.searchCache.Stats()
		m.cacheHits.Set(float64(stats.Hits))
		m.cacheMisses.Set(float64(stats.Misses))
		m.cacheSize.Set(float64(stats.Size))
	} else {
		m.cacheSize.Set(0)
	}
	// Index doc count
	if s.engine != nil {
		// 简化: 不逐个 index 拉, 后续可遍历 engine.docs
	}
}

// IncOptimisticConflict 优化锁冲突计数
func (m *ServerMetrics) IncOptimisticConflict(op, kind string) {
	if m == nil {
		return
	}
	m.ocConflicts.WithLabelValues(op, kind).Inc()
}

// IncRbacAuthFail 认证失败计数
func (m *ServerMetrics) IncRbacAuthFail(reason string) {
	if m == nil {
		return
	}
	m.rbacAuthFail.WithLabelValues(reason).Inc()
}

// IncRbacForbidden 鉴权拒绝计数
func (m *ServerMetrics) IncRbacForbidden(action, index string) {
	if m == nil {
		return
	}
	m.rbacForbidden.WithLabelValues(action, index).Inc()
}

// ObserveRequest 记录一次 HTTP 请求的指标
// method: HTTP 方法
// route: 路由模板(如 /{index}/_doc/{id}), 未知时传 "<other>"
// status: HTTP 状态码
// dur: 请求耗时
func (m *ServerMetrics) ObserveRequest(method, route string, status int, dur time.Duration) {
	if m == nil {
		return
	}
	if route == "" {
		route = "<other>"
	}
	statusStr := strconv.Itoa(status)
	m.httpRequestsTotal.WithLabelValues(method, route, statusStr).Inc()
	m.httpRequestDuration.WithLabelValues(method, route).Observe(dur.Seconds())
}

// InflightInc 请求开始时调用,返回的函数在请求结束时调用
func (m *ServerMetrics) InflightInc(method, route string) func() {
	if m == nil {
		return func() {}
	}
	if route == "" {
		route = "<other>"
	}
	g := m.httpInFlight.WithLabelValues(method, route)
	g.Inc()
	return func() { g.Dec() }
}

// ObserveStorageTxn 记录一次存储事务
// kind: "read" | "write" | "scan" | "delete_prefix"
// status: "ok" | "error"
func (m *ServerMetrics) ObserveStorageTxn(kind, status string) {
	if m == nil {
		return
	}
	m.storageTxnTotal.WithLabelValues(kind, status).Inc()
}

// SetIndexDocCount 设置索引文档数
func (m *ServerMetrics) SetIndexDocCount(index string, count int64) {
	if m == nil || index == "" {
		return
	}
	m.indexDocCount.WithLabelValues(index).Set(float64(count))
}
