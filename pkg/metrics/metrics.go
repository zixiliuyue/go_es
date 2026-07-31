// Package metrics 提供Prometheus监控指标收集
// 支持操作耗时、请求计数等监控，可选集成，不使用时不增加依赖
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics ES操作监控指标
type Metrics struct {
	// 操作请求计数器
	requestsTotal *prometheus.CounterVec
	// 操作耗时直方图
	requestDuration *prometheus.HistogramVec
	// 错误计数器
	errorsTotal *prometheus.CounterVec
	// 文档总数
	documentCount prometheus.Gauge
	// 是否已经注册
	registered bool
}

// MetricLabels 指标标签
const (
	LabelOperation = "operation"
	LabelIndex    = "index"
	LabelStatus   = "status"
)

// Status 操作状态
const (
	StatusSuccess = "success"
	StatusError   = "error"
)

var (
	defaultBuckets = []float64{
		0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
	}
)

// NewMetrics 创建一个新的metrics实例
func NewMetrics(namespace string) *Metrics {
	m := &Metrics{}

	m.requestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "es_requests_total",
			Help:      "Total number of ES requests processed",
		},
		[]string{LabelOperation, LabelIndex, LabelStatus},
	)

	m.requestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "es_request_duration_seconds",
			Help:      "request processing latency in seconds",
			Buckets:   defaultBuckets,
		},
		[]string{LabelOperation, LabelIndex},
	)

	m.errorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "es_errors_total",
			Help:      "Total number of ES errors",
		},
		[]string{LabelOperation, LabelIndex},
	)

	m.documentCount = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "es_documents_total",
			Help:      "Total number of documents in ES",
		},
	)

	return m
}

// Register 注册指标到Prometheus注册表
func (m *Metrics) Register(registry prometheus.Registerer) error {
	if m.registered {
		return nil // already registered
	}

	if err := registry.Register(m.requestsTotal); err != nil {
		return err
	}
	if err := registry.Register(m.requestDuration); err != nil {
		return err
	}
	if err := registry.Register(m.errorsTotal); err != nil {
		return err
	}
	if err := registry.Register(m.documentCount); err != nil {
		return err
	}

	m.registered = true
	return nil
}

// ObserveRequest 观察一个请求
func (m *Metrics) ObserveRequest(operation, index string, status string, duration time.Duration) {
	if m == nil {
		return // metrics not enabled
	}

	m.requestsTotal.WithLabelValues(operation, index, status).Inc()
	m.requestDuration.WithLabelValues(operation, index).Observe(duration.Seconds())

	if status == StatusError {
		m.errorsTotal.WithLabelValues(operation, index).Inc()
	}
}

// IncError 增加错误计数
func (m *Metrics) IncError(operation, index string) {
	if m == nil {
		return
	}
	m.errorsTotal.WithLabelValues(operation, index).Inc()
}

// SetDocumentCount 设置文档总数
func (m *Metrics) SetDocumentCount(count int64) {
	if m == nil {
		return
	}
	m.documentCount.Set(float64(count))
}

// NoopMetrics 空实现，用于不启用metrics时
var NoopMetrics = (*Metrics)(nil)

// Instrumented 包装一个函数，自动记录指标
type Instrumented struct {
	metrics *Metrics
}

// NewInstrumented 创建一个instrumented包装器
func NewInstrumented(metrics *Metrics) *Instrumented {
	return &Instrumented{
		metrics: metrics,
	}
}

// Observe 观察请求
func (i *Instrumented) Observe(operation, index string, start time.Time) {
	duration := time.Since(start)
	status := StatusSuccess
	i.metrics.ObserveRequest(operation, index, status, duration)
}

// ObserveError 观察错误请求
func (i *Instrumented) ObserveError(operation, index string, start time.Time) {
	duration := time.Since(start)
	status := StatusError
	i.metrics.ObserveRequest(operation, index, status, duration)
}
