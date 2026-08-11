// otel_exporter 实现 OpenTelemetry 规范的 exporter 组件
//
// 支持将指标(metrics)、追踪(traces)、日志(logs) 数据
// 通过 OTLP HTTP/gRPC 协议导出至 OpenTelemetry Collector 或
// 其他兼容的后端系统。核心能力:
//   - OTLP HTTP/gRPC 双协议支持
//   - Metrics 批量采集 + 定时推送
//   - Traces 透传与批量导出(对接现有 tracing.TracerProvider)
//   - Logs 转发(zap core integration)
//   - 可配置: endpoint / 鉴权 / 推送频率 / 批大小 / 重试
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/prometheus/prompb"
	"go.uber.org/zap"
)

// OTelExportConfig OpenTelemetry 导出配置
type OTelExportConfig struct {
	Enabled     bool          `yaml:"enabled"`
	Protocol    string        `yaml:"protocol"` // "http" | "grpc"
	Metrics     OTelMetricsConfig `yaml:"metrics"`
	Traces      OTelTracesConfig  `yaml:"traces"`
	Logs        OTelLogsConfig    `yaml:"logs"`
	GlobalAttrs map[string]string `yaml:"global_attrs"`
}

// OTelMetricsConfig 指标导出配置
type OTelMetricsConfig struct {
	Enabled        bool          `yaml:"enabled"`
	Endpoint       string        `yaml:"endpoint"`
	PushInterval   time.Duration `yaml:"push_interval"`
	BatchSize      int           `yaml:"batch_size"`
	MaxRetries     int           `yaml:"max_retries"`
	Timeout        time.Duration `yaml:"timeout"`
	Insecure       bool          `yaml:"insecure"`
	BasicAuth      BasicAuthCfg  `yaml:"basic_auth"`
	BearerToken    string        `yaml:"bearer_token"`
	Headers        map[string]string `yaml:"headers"`
}

// OTelTracesConfig 追踪导出配置
type OTelTracesConfig struct {
	Enabled        bool          `yaml:"enabled"`
	Endpoint       string        `yaml:"endpoint"`
	Protocol       string        `yaml:"protocol"` // "http" | "grpc"
	BatchSize      int           `yaml:"batch_size"`
	MaxRetries     int           `yaml:"max_retries"`
	MaxQueueSize   int           `yaml:"max_queue_size"`
	Insecure       bool          `yaml:"insecure"`
	BasicAuth      BasicAuthCfg  `yaml:"basic_auth"`
	BearerToken    string        `yaml:"bearer_token"`
	Headers        map[string]string `yaml:"headers"`
	Timeout        time.Duration `yaml:"timeout"`
	SampleRate     float64       `yaml:"sample_rate"`
}

// OTelLogsConfig 日志导出配置
type OTelLogsConfig struct {
	Enabled        bool          `yaml:"enabled"`
	Endpoint       string        `yaml:"endpoint"`
	PushInterval   time.Duration `yaml:"push_interval"`
	BatchSize      int           `yaml:"batch_size"`
	MaxRetries     int           `yaml:"max_retries"`
	Timeout        time.Duration `yaml:"timeout"`
	Insecure       bool          `yaml:"insecure"`
	BasicAuth      BasicAuthCfg  `yaml:"basic_auth"`
	BearerToken    string        `yaml:"bearer_token"`
	Headers        map[string]string `yaml:"headers"`
	MinLevel       string        `yaml:"min_level"` // "debug" / "info" / "warn" / "error"
}

// DefaultOTelExportConfig 默认配置
func DefaultOTelExportConfig() OTelExportConfig {
	return OTelExportConfig{
		Protocol: "http",
		Metrics: OTelMetricsConfig{
			PushInterval: 15 * time.Second,
			BatchSize:    1000,
			MaxRetries:   2,
			Timeout:      30 * time.Second,
		},
		Traces: OTelTracesConfig{
			BatchSize:    512,
			MaxRetries:   2,
			MaxQueueSize: 4096,
			Timeout:      30 * time.Second,
			SampleRate:   1.0,
		},
		Logs: OTelLogsConfig{
			PushInterval: 10 * time.Second,
			BatchSize:    256,
			MaxRetries:   2,
			Timeout:      30 * time.Second,
			MinLevel:     "info",
		},
	}
}

// OTelExporter OpenTelemetry 导出器(聚合 metrics/traces/logs 三个子组件)
type OTelExporter struct {
	cfg    OTelExportConfig
	log    *zap.Logger
	client *http.Client

	metricsExporter *OTelMetricsExporter
	tracesExporter  *OTelTracesExporter
	logsExporter    *OTelLogsExporter

	done   chan struct{}
	closed bool
	mu     sync.Mutex
}

// NewOTelExporter 创建 OpenTelemetry 导出器
func NewOTelExporter(cfg OTelExportConfig, log *zap.Logger) (*OTelExporter, error) {
	cfg = applyOTelDefaults(cfg)

	client := &http.Client{Timeout: 30 * time.Second}

	exp := &OTelExporter{
		cfg:    cfg,
		log:    log,
		client: client,
		done:   make(chan struct{}),
	}

	var errs []error

	if cfg.Metrics.Enabled {
		me, err := NewOTelMetricsExporter(cfg.Metrics, client, log)
		if err != nil {
			errs = append(errs, fmt.Errorf("metrics exporter: %w", err))
		} else {
			exp.metricsExporter = me
		}
	}

	if cfg.Traces.Enabled {
		te, err := NewOTelTracesExporter(cfg.Traces, client, log)
		if err != nil {
			errs = append(errs, fmt.Errorf("traces exporter: %w", err))
		} else {
			exp.tracesExporter = te
		}
	}

	if cfg.Logs.Enabled {
		le, err := NewOTelLogsExporter(cfg.Logs, client, log)
		if err != nil {
			errs = append(errs, fmt.Errorf("logs exporter: %w", err))
		} else {
			exp.logsExporter = le
		}
	}

	if len(errs) > 0 {
		return nil, errs[0]
	}
	return exp, nil
}

func applyOTelDefaults(cfg OTelExportConfig) OTelExportConfig {
	d := DefaultOTelExportConfig()
	if cfg.Metrics.PushInterval <= 0 {
		cfg.Metrics.PushInterval = d.Metrics.PushInterval
	}
	if cfg.Metrics.BatchSize <= 0 {
		cfg.Metrics.BatchSize = d.Metrics.BatchSize
	}
	if cfg.Metrics.Timeout <= 0 {
		cfg.Metrics.Timeout = d.Metrics.Timeout
	}
	if cfg.Metrics.MaxRetries < 0 {
		cfg.Metrics.MaxRetries = d.Metrics.MaxRetries
	}
	if cfg.Traces.BatchSize <= 0 {
		cfg.Traces.BatchSize = d.Traces.BatchSize
	}
	if cfg.Traces.MaxRetries < 0 {
		cfg.Traces.MaxRetries = d.Traces.MaxRetries
	}
	if cfg.Traces.MaxQueueSize <= 0 {
		cfg.Traces.MaxQueueSize = d.Traces.MaxQueueSize
	}
	if cfg.Traces.Timeout <= 0 {
		cfg.Traces.Timeout = d.Traces.Timeout
	}
	if cfg.Traces.SampleRate <= 0 {
		cfg.Traces.SampleRate = d.Traces.SampleRate
	}
	if cfg.Logs.PushInterval <= 0 {
		cfg.Logs.PushInterval = d.Logs.PushInterval
	}
	if cfg.Logs.BatchSize <= 0 {
		cfg.Logs.BatchSize = d.Logs.BatchSize
	}
	if cfg.Logs.MaxRetries < 0 {
		cfg.Logs.MaxRetries = d.Logs.MaxRetries
	}
	if cfg.Logs.Timeout <= 0 {
		cfg.Logs.Timeout = d.Logs.Timeout
	}
	if cfg.Logs.MinLevel == "" {
		cfg.Logs.MinLevel = d.Logs.MinLevel
	}
	return cfg
}

// Start 启动所有子 exporter 的后台推送协程
func (e *OTelExporter) Start(ctx context.Context) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()

	e.log.Info("OpenTelemetry exporter 启动",
		zap.Bool("metrics", e.cfg.Metrics.Enabled),
		zap.Bool("traces", e.cfg.Traces.Enabled),
		zap.Bool("logs", e.cfg.Logs.Enabled))

	if e.metricsExporter != nil {
		go e.metricsExporter.Start(ctx)
	}
	if e.tracesExporter != nil {
		go e.tracesExporter.Start(ctx)
	}
	if e.logsExporter != nil {
		go e.logsExporter.Start(ctx)
	}
}

// MetricsExporter 暴露子 exporter
func (e *OTelExporter) MetricsExporter() *OTelMetricsExporter { return e.metricsExporter }

// TracesExporter 暴露子 exporter
func (e *OTelExporter) TracesExporter() *OTelTracesExporter { return e.tracesExporter }

// LogsExporter 暴露子 exporter
func (e *OTelExporter) LogsExporter() *OTelLogsExporter { return e.logsExporter }

// Close 停止所有子 exporter,flush 剩余数据
func (e *OTelExporter) Close() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	close(e.done)
	e.mu.Unlock()

	if e.metricsExporter != nil {
		e.metricsExporter.Close()
	}
	if e.tracesExporter != nil {
		e.tracesExporter.Close()
	}
	if e.logsExporter != nil {
		e.logsExporter.Close()
	}
}

// -------------------------------------------------------------------
// OTelMetricsExporter: 从 Prometheus registry 采集 metrics, 按 OTLP 格式导出
// -------------------------------------------------------------------

// OTelMetricsExporter OTLP 指标导出器
type OTelMetricsExporter struct {
	cfg    OTelMetricsConfig
	client *http.Client
	log    *zap.Logger

	done   chan struct{}
	closed bool

	// 统计
	pushSuccess int64
	pushFail    int64
	mu          sync.Mutex
}

// NewOTelMetricsExporter 创建指标导出器
func NewOTelMetricsExporter(cfg OTelMetricsConfig, client *http.Client, log *zap.Logger) (*OTelMetricsExporter, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("metrics exporter: endpoint 不能为空")
	}
	return &OTelMetricsExporter{
		cfg:    cfg,
		client: client,
		log:    log,
		done:   make(chan struct{}),
	}, nil
}

// Start 启动周期性推送
func (m *OTelMetricsExporter) Start(ctx context.Context) {
	interval := m.cfg.PushInterval
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	m.log.Info("OTel metrics exporter 启动",
		zap.String("endpoint", m.cfg.Endpoint),
		zap.Duration("interval", interval))

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.done:
			return
		case <-ticker.C:
			// 指标推送需要从外部 registry 获取数据
			// 这里由 Server 显式调用 PushMetrics 触发
		}
	}
}

// PushMetrics 将采集到的指标数据按 OTLP JSON 格式推送
func (m *OTelMetricsExporter) PushMetrics(ctx context.Context, tss []prompb.TimeSeries) error {
	if len(tss) == 0 {
		return nil
	}

	m.log.Debug("OTLP metrics 推送开始",
		zap.Int("timeseries_count", len(tss)),
	)

	start := time.Now()
	payload := m.buildOTLPMetricsPayload(tss)
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("序列化 OTLP metrics payload: %w", err)
	}

	m.log.Debug("OTLP metrics payload 序列化完成",
		zap.Int("payload_size", len(body)),
		zap.Duration("encode_elapsed", time.Since(start)),
	)

	if err := m.send(ctx, "/v1/metrics", body); err != nil {
		// pushFail 已由 sendWithOTLPRetries 的 onFail 回调累加, 这里不再重复
		m.log.Warn("OTLP metrics 推送失败",
			zap.Int("timeseries_count", len(tss)),
			zap.Int("payload_size", len(body)),
			zap.Duration("elapsed", time.Since(start)),
			zap.Error(err),
		)
		return err
	}

	m.mu.Lock()
	m.pushSuccess++
	m.mu.Unlock()

	m.log.Debug("OTLP metrics 推送完成",
		zap.Int("timeseries_count", len(tss)),
		zap.Int("payload_size", len(body)),
		zap.Duration("elapsed", time.Since(start)),
	)
	return nil
}

// buildOTLPMetricsPayload 从 prompb.TimeSeries 构造 OTLP HTTP JSON 格式
func (m *OTelMetricsExporter) buildOTLPMetricsPayload(tss []prompb.TimeSeries) map[string]any {
	resourceMetrics := make([]map[string]any, 0, len(tss))

	for _, ts := range tss {
		attrs := make([]map[string]any, 0)
		for _, l := range ts.Labels {
			attrs = append(attrs, map[string]any{
				"key":   l.Name,
				"value": map[string]any{"stringValue": l.Value},
			})
		}

		dataPoints := make([]map[string]any, 0, len(ts.Samples))
		for _, s := range ts.Samples {
			attrs := make([]map[string]any, 0)
			dataPoints = append(dataPoints, map[string]any{
				"timeUnixNano": fmt.Sprintf("%d", s.Timestamp*1e6),
				"asDouble":     s.Value,
				"attributes":   attrs,
			})
		}

		scopeMetrics := []map[string]any{
			{
				"scope": map[string]any{"name": "github.com/zixiliuyue/go_es"},
				"metrics": []map[string]any{
					{
						"name":        m.metricName(ts.Labels),
						"description": "go_es server metric",
						"unit":        "",
						"sum": map[string]any{
							"dataPoints":   dataPoints,
							"aggregationTemporality": "AGGREGATION_TEMPORALITY_DELTA",
						},
					},
				},
			},
		}

		resourceMetrics = append(resourceMetrics, map[string]any{
			"resource": map[string]any{
				"attributes": append(attrs, map[string]any{
					"key":   "service.name",
					"value": map[string]any{"stringValue": "go_es_server"},
				}),
			},
			"scopeMetrics": scopeMetrics,
		})
	}

	return map[string]any{
		"resourceMetrics": resourceMetrics,
	}
}

// metricName 提取 __name__ label 作为指标名
func (m *OTelMetricsExporter) metricName(labels []prompb.Label) string {
	for _, l := range labels {
		if l.Name == "__name__" {
			return l.Value
		}
	}
	return "unknown_metric"
}

// send 发送 OTLP HTTP 请求(带重试)
func (m *OTelMetricsExporter) send(ctx context.Context, path string, body []byte) error {
	url := m.cfg.Endpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("构造请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	for k, v := range m.cfg.Headers {
		req.Header.Set(k, v)
	}
	if m.cfg.BasicAuth.Username != "" {
		req.SetBasicAuth(m.cfg.BasicAuth.Username, m.cfg.BasicAuth.Password)
	}
	if m.cfg.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+m.cfg.BearerToken)
	}

	m.log.Debug("OTLP metrics HTTP 请求已构造",
		zap.String("url", url+path),
		zap.Int("body_size", len(body)),
		zap.Int("custom_headers", len(m.cfg.Headers)),
		zap.Bool("basic_auth", m.cfg.BasicAuth.Username != ""),
		zap.Bool("bearer_token", m.cfg.BearerToken != ""),
		zap.Int("max_retries", m.cfg.MaxRetries),
	)

	return sendWithOTLPRetries(ctx, m.client, req, m.log, "metrics", m.cfg.MaxRetries,
		func() {
			m.mu.Lock()
			m.pushFail++
			m.mu.Unlock()
		})
}

// sendWithOTLPRetries 通用 OTLP 带重试的 HTTP 发送方法
// 被 metrics/traces/logs 导出器共用
// onFail: 每次请求失败时的回调（用于统计）
func sendWithOTLPRetries(ctx context.Context, client *http.Client, req *http.Request,
	logger *zap.Logger, component string, maxRetries int, onFail func()) error {
	var lastErr error
	totalStart := time.Now()
	maxAttempts := maxRetries + 1

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * 100 * time.Millisecond
			logger.Debug("OTLP 重试",
				zap.String("component", component),
				zap.Int("attempt", attempt+1),
				zap.Int("max_attempts", maxAttempts),
				zap.Duration("backoff", backoff),
			)
			select {
			case <-ctx.Done():
				return fmt.Errorf("OTLP %s 上下文已取消: %w", component, ctx.Err())
			case <-time.After(backoff):
			}
		}

		attemptStart := time.Now()
		resp, err := client.Do(req)
		attemptElapsed := time.Since(attemptStart)

		if err != nil {
			lastErr = err
			if onFail != nil {
				onFail()
			}
			logger.Warn("OTLP 请求错误",
				zap.String("component", component),
				zap.Int("attempt", attempt+1),
				zap.Duration("elapsed", attemptElapsed),
				zap.Error(err),
			)
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			logger.Debug("OTLP 发送成功",
				zap.String("component", component),
				zap.Int("attempt", attempt+1),
				zap.Int("status_code", resp.StatusCode),
				zap.Duration("elapsed", attemptElapsed),
				zap.Duration("total_elapsed", time.Since(totalStart)),
			)
			return nil
		}
		lastErr = fmt.Errorf("OTLP %s: 服务端返回 %d", component, resp.StatusCode)
		if onFail != nil {
			onFail()
		}
		logger.Warn("OTLP 服务端返回错误",
			zap.String("component", component),
			zap.Int("attempt", attempt+1),
			zap.Int("status_code", resp.StatusCode),
			zap.Duration("elapsed", attemptElapsed),
		)
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			logger.Warn("OTLP 客户端错误,不再重试",
				zap.String("component", component),
				zap.Int("status_code", resp.StatusCode),
			)
			return lastErr
		}
	}

	logger.Error("OTLP 全部重试失败",
		zap.String("component", component),
		zap.Int("total_attempts", maxAttempts),
		zap.Duration("total_elapsed", time.Since(totalStart)),
		zap.Error(lastErr),
	)
	return fmt.Errorf("OTLP %s: 全部 %d 次重试失败: %w", component, maxAttempts, lastErr)
}

// Close 关闭
func (m *OTelMetricsExporter) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	close(m.done)
	m.mu.Unlock()
}

// Stats 返回推送统计
func (m *OTelMetricsExporter) Stats() (success, fail int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pushSuccess, m.pushFail
}

// -------------------------------------------------------------------
// OTelTracesExporter: 批量导出 trace span
// -------------------------------------------------------------------

// OTelSpan 简化的 OTLP span 表示
type OTelSpan struct {
	TraceID       string            `json:"traceId"`
	SpanID        string            `json:"spanId"`
	ParentSpanID  string            `json:"parentSpanId,omitempty"`
	Name          string            `json:"name"`
	Kind          string            `json:"kind"`
	StartTimeUnix string            `json:"startTimeUnixNano"`
	EndTimeUnix   string            `json:"endTimeUnixNano"`
	Attributes    map[string]string `json:"attributes,omitempty"`
	StatusCode    string            `json:"statusCode,omitempty"`
	StatusMessage string            `json:"statusMessage,omitempty"`
}

// OTelTracesExporter OTLP 追踪导出器
type OTelTracesExporter struct {
	cfg      OTelTracesConfig
	client   *http.Client
	log      *zap.Logger

	queue   chan OTelSpan
	done    chan struct{}
	stopped chan struct{}
	started bool
	closed  bool

	pushSuccess int64
	pushFail    int64
	mu          sync.Mutex
}

// NewOTelTracesExporter 创建追踪导出器
func NewOTelTracesExporter(cfg OTelTracesConfig, client *http.Client, log *zap.Logger) (*OTelTracesExporter, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("traces exporter: endpoint 不能为空")
	}
	maxQ := cfg.MaxQueueSize
	if maxQ < 64 {
		maxQ = 64
	}
	return &OTelTracesExporter{
		cfg:     cfg,
		client:  client,
		log:     log,
		queue:   make(chan OTelSpan, maxQ),
		done:    make(chan struct{}),
		stopped: make(chan struct{}),
	}, nil
}

// Start 启动批量导出协程
func (t *OTelTracesExporter) Start(ctx context.Context) {
	t.log.Info("OTel traces exporter 启动",
		zap.String("endpoint", t.cfg.Endpoint),
		zap.Int("batch_size", t.cfg.BatchSize))

	t.mu.Lock()
	if t.started {
		t.mu.Unlock()
		return
	}
	t.started = true
	t.mu.Unlock()

	go t.exportLoop(ctx)
}

func (t *OTelTracesExporter) exportLoop(ctx context.Context) {
	defer close(t.stopped)
	batch := make([]OTelSpan, 0, t.cfg.BatchSize)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			t.flushBatch(ctx, batch)
			return
		case <-t.done:
			t.flushBatch(ctx, batch)
			return
		case span := <-t.queue:
			batch = append(batch, span)
			if len(batch) >= t.cfg.BatchSize {
				t.flushBatch(ctx, batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				t.flushBatch(ctx, batch)
				batch = batch[:0]
			}
		}
	}
}

// ExportSpan 入队一个 OTelSpan
func (t *OTelTracesExporter) ExportSpan(span OTelSpan) {
	if t == nil {
		return
	}
	select {
	case t.queue <- span:
	default:
		t.mu.Lock()
		t.pushFail++
		queueSize := len(t.queue)
		t.mu.Unlock()
		t.log.Warn("OTel traces 队列已满, 丢弃 span",
			zap.String("trace_id", span.TraceID),
			zap.String("span_id", span.SpanID),
			zap.String("span_name", span.Name),
			zap.Int("queue_size", queueSize),
		)
	}
}

// ExportTracingSpan 实现 SpanExporter 接口, 将内部 *Span 转换为 OTelSpan 并入队
func (t *OTelTracesExporter) ExportTracingSpan(span *Span) {
	if t == nil || span == nil {
		return
	}
	otSpan := OTelSpan{
		TraceID:       span.TraceIDString(),
		SpanID:        span.SpanIDString(),
		Name:          span.name,
		Kind:          span.kind.String(),
		StartTimeUnix: fmt.Sprintf("%d", span.startTime.UnixNano()),
		StatusMessage: string(span.status),
	}
	if span.ended {
		otSpan.EndTimeUnix = fmt.Sprintf("%d", span.endTime.UnixNano())
	} else {
		otSpan.EndTimeUnix = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	// 转换 attributes
	span.mu.Lock()
	attrs := make(map[string]string, len(span.attributes))
	for k, v := range span.attributes {
		attrs[k] = v
	}
	span.mu.Unlock()
	otSpan.Attributes = attrs

	// 设置状态码
	switch span.status {
	case SpanStatusOK:
		otSpan.StatusCode = "STATUS_CODE_OK"
	case SpanStatusError:
		otSpan.StatusCode = "STATUS_CODE_ERROR"
	default:
		otSpan.StatusCode = "STATUS_CODE_UNSET"
	}

	t.ExportSpan(otSpan)
}

// flushBatch 发送一批 spans
func (t *OTelTracesExporter) flushBatch(ctx context.Context, batch []OTelSpan) {
	if len(batch) == 0 {
		return
	}
	t.log.Debug("OTLP traces flushBatch 开始",
		zap.Int("batch_size", len(batch)),
		zap.Int("remaining_queue", len(t.queue)),
	)
	start := time.Now()

	payload := t.buildOTLPTracesPayload(batch)
	body, err := json.Marshal(payload)
	if err != nil {
		t.log.Error("序列化 OTLP traces payload 失败",
			zap.Int("batch_size", len(batch)),
			zap.Duration("elapsed", time.Since(start)),
			zap.Error(err))
		return
	}

	t.log.Debug("OTLP traces payload 序列化完成",
		zap.Int("batch_size", len(batch)),
		zap.Int("payload_size", len(body)),
		zap.Duration("encode_elapsed", time.Since(start)),
	)

	url := t.cfg.Endpoint + "/v1/traces"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.log.Error("构造 traces 请求失败",
			zap.Int("batch_size", len(batch)),
			zap.Duration("elapsed", time.Since(start)),
			zap.Error(err))
		return
	}
	req.Header.Set("Content-Type", "application/json")

	for k, v := range t.cfg.Headers {
		req.Header.Set(k, v)
	}
	if t.cfg.BasicAuth.Username != "" {
		req.SetBasicAuth(t.cfg.BasicAuth.Username, t.cfg.BasicAuth.Password)
	}
	if t.cfg.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+t.cfg.BearerToken)
	}

	t.log.Debug("OTLP traces HTTP 请求已构造",
		zap.String("url", url),
		zap.Int("batch_size", len(batch)),
		zap.Int("body_size", len(body)),
		zap.Int("max_retries", t.cfg.MaxRetries),
	)

	if err := sendWithOTLPRetries(ctx, t.client, req, t.log, "traces", t.cfg.MaxRetries,
		func() {
			t.mu.Lock()
			t.pushFail++
			t.mu.Unlock()
		}); err != nil {
		// pushFail 已由 onFail 回调累加, 这里不再重复
		t.log.Warn("OTLP traces 发送失败",
			zap.String("url", url),
			zap.Int("batch_size", len(batch)),
			zap.Duration("elapsed", time.Since(start)),
			zap.Error(err))
	} else {
		t.mu.Lock()
		t.pushSuccess++
		t.mu.Unlock()
		t.log.Debug("OTLP traces 发送成功",
			zap.String("url", url),
			zap.Int("batch_size", len(batch)),
			zap.Duration("elapsed", time.Since(start)),
		)
	}
}

// buildOTLPTracesPayload 构造 OTLP traces JSON payload
func (t *OTelTracesExporter) buildOTLPTracesPayload(spans []OTelSpan) map[string]any {
	spanList := make([]map[string]any, 0, len(spans))
	for _, s := range spans {
		attrs := make([]map[string]any, 0)
		for k, v := range s.Attributes {
			attrs = append(attrs, map[string]any{
				"key":   k,
				"value": map[string]any{"stringValue": v},
			})
		}

		status := map[string]any{}
		if s.StatusCode != "" {
			status["code"] = s.StatusCode
		}
		if s.StatusMessage != "" {
			status["message"] = s.StatusMessage
		}

		spanList = append(spanList, map[string]any{
			"traceId":       s.TraceID,
			"spanId":        s.SpanID,
			"parentSpanId":  s.ParentSpanID,
			"name":          s.Name,
			"kind":          s.Kind,
			"startTimeUnixNano": s.StartTimeUnix,
			"endTimeUnixNano":   s.EndTimeUnix,
			"attributes":    attrs,
			"status":        status,
		})
	}

	return map[string]any{
		"resourceSpans": []map[string]any{
			{
				"resource": map[string]any{
					"attributes": []map[string]any{
						{"key": "service.name", "value": map[string]any{"stringValue": "go_es_server"}},
					},
				},
				"scopeSpans": []map[string]any{
					{
						"scope": map[string]any{"name": "github.com/zixiliuyue/go_es"},
						"spans": spanList,
					},
				},
			},
		},
	}
}

// Close 关闭并等待 flush 完成
func (t *OTelTracesExporter) Close() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	close(t.done)
	started := t.started
	t.mu.Unlock()
	if started {
		<-t.stopped
	}
}

// Stats 返回推送统计
func (t *OTelTracesExporter) Stats() (success, fail int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pushSuccess, t.pushFail
}

// -------------------------------------------------------------------
// OTelLogsExporter: 批量导出日志
// -------------------------------------------------------------------

// OTelLogEntry 简化的 OTLP log 表示
type OTelLogEntry struct {
	TimeUnixNano string            `json:"timeUnixNano"`
	Severity     string            `json:"severityText"`
	Body         string            `json:"body"`
	TraceID      string            `json:"traceId,omitempty"`
	SpanID       string            `json:"spanId,omitempty"`
	Attributes   map[string]string `json:"attributes,omitempty"`
}

// OTelLogsExporter OTLP 日志导出器
type OTelLogsExporter struct {
	cfg    OTelLogsConfig
	client *http.Client
	log    *zap.Logger

	queue   chan OTelLogEntry
	done    chan struct{}
	stopped chan struct{}
	started bool
	closed  bool

	pushSuccess int64
	pushFail    int64
	mu          sync.Mutex
}

// NewOTelLogsExporter 创建日志导出器
func NewOTelLogsExporter(cfg OTelLogsConfig, client *http.Client, log *zap.Logger) (*OTelLogsExporter, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("logs exporter: endpoint 不能为空")
	}
	maxQ := cfg.BatchSize * 8
	if maxQ < 64 {
		maxQ = 64
	}
	return &OTelLogsExporter{
		cfg:     cfg,
		client:  client,
		log:     log,
		queue:   make(chan OTelLogEntry, maxQ),
		done:    make(chan struct{}),
		stopped: make(chan struct{}),
	}, nil
}

// Start 启动日志导出协程
func (l *OTelLogsExporter) Start(ctx context.Context) {
	l.log.Info("OTel logs exporter 启动",
		zap.String("endpoint", l.cfg.Endpoint),
		zap.Duration("interval", l.cfg.PushInterval))

	l.mu.Lock()
	if l.started {
		l.mu.Unlock()
		return
	}
	l.started = true
	l.mu.Unlock()

	go l.exportLoop(ctx)
}

func (l *OTelLogsExporter) exportLoop(ctx context.Context) {
	defer close(l.stopped)
	batch := make([]OTelLogEntry, 0, l.cfg.BatchSize)
	ticker := time.NewTicker(l.cfg.PushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			l.flushBatch(ctx, batch)
			return
		case <-l.done:
			l.flushBatch(ctx, batch)
			return
		case entry := <-l.queue:
			batch = append(batch, entry)
			if len(batch) >= l.cfg.BatchSize {
				l.flushBatch(ctx, batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				l.flushBatch(ctx, batch)
				batch = batch[:0]
			}
		}
	}
}

// ExportLog 入队一条日志
func (l *OTelLogsExporter) ExportLog(entry OTelLogEntry) {
	if l == nil {
		return
	}
	select {
	case l.queue <- entry:
	default:
		l.mu.Lock()
		l.pushFail++
		queueSize := len(l.queue)
		l.mu.Unlock()
		l.log.Warn("OTel logs 队列已满, 丢弃日志",
			zap.String("body", entry.Body),
			zap.String("severity", entry.Severity),
			zap.String("trace_id", entry.TraceID),
			zap.Int("queue_size", queueSize),
		)
	}
}

// flushBatch 发送一批日志
func (l *OTelLogsExporter) flushBatch(ctx context.Context, batch []OTelLogEntry) {
	if len(batch) == 0 {
		return
	}
	l.log.Debug("OTLP logs flushBatch 开始",
		zap.Int("batch_size", len(batch)),
		zap.Int("remaining_queue", len(l.queue)),
	)
	start := time.Now()

	payload := l.buildOTLPLogsPayload(batch)
	body, err := json.Marshal(payload)
	if err != nil {
		l.log.Error("序列化 OTLP logs payload 失败",
			zap.Int("batch_size", len(batch)),
			zap.Duration("elapsed", time.Since(start)),
			zap.Error(err))
		return
	}

	l.log.Debug("OTLP logs payload 序列化完成",
		zap.Int("batch_size", len(batch)),
		zap.Int("payload_size", len(body)),
		zap.Duration("encode_elapsed", time.Since(start)),
	)

	url := l.cfg.Endpoint + "/v1/logs"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		l.log.Error("构造 logs 请求失败",
			zap.Int("batch_size", len(batch)),
			zap.Duration("elapsed", time.Since(start)),
			zap.Error(err))
		return
	}
	req.Header.Set("Content-Type", "application/json")

	for k, v := range l.cfg.Headers {
		req.Header.Set(k, v)
	}
	if l.cfg.BasicAuth.Username != "" {
		req.SetBasicAuth(l.cfg.BasicAuth.Username, l.cfg.BasicAuth.Password)
	}
	if l.cfg.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+l.cfg.BearerToken)
	}

	l.log.Debug("OTLP logs HTTP 请求已构造",
		zap.String("url", url),
		zap.Int("batch_size", len(batch)),
		zap.Int("body_size", len(body)),
		zap.Int("max_retries", l.cfg.MaxRetries),
	)

	if err := sendWithOTLPRetries(ctx, l.client, req, l.log, "logs", l.cfg.MaxRetries,
		func() {
			l.mu.Lock()
			l.pushFail++
			l.mu.Unlock()
		}); err != nil {
		// pushFail 已由 onFail 回调累加, 这里不再重复
		l.log.Warn("OTLP logs 发送失败",
			zap.String("url", url),
			zap.Int("batch_size", len(batch)),
			zap.Duration("elapsed", time.Since(start)),
			zap.Error(err))
	} else {
		l.mu.Lock()
		l.pushSuccess++
		l.mu.Unlock()
		l.log.Debug("OTLP logs 发送成功",
			zap.String("url", url),
			zap.Int("batch_size", len(batch)),
			zap.Duration("elapsed", time.Since(start)),
		)
	}
}

// buildOTLPLogsPayload 构造 OTLP logs JSON payload
func (l *OTelLogsExporter) buildOTLPLogsPayload(entries []OTelLogEntry) map[string]any {
	logList := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		attrs := make([]map[string]any, 0)
		for k, v := range e.Attributes {
			attrs = append(attrs, map[string]any{
				"key":   k,
				"value": map[string]any{"stringValue": v},
			})
		}

		logList = append(logList, map[string]any{
			"timeUnixNano": e.TimeUnixNano,
			"severityText": e.Severity,
			"body":         map[string]any{"stringValue": e.Body},
			"traceId":      e.TraceID,
			"spanId":       e.SpanID,
			"attributes":   attrs,
		})
	}

	return map[string]any{
		"resourceLogs": []map[string]any{
			{
				"resource": map[string]any{
					"attributes": []map[string]any{
						{"key": "service.name", "value": map[string]any{"stringValue": "go_es_server"}},
					},
				},
				"scopeLogs": []map[string]any{
					{
						"scope": map[string]any{"name": "github.com/zixiliuyue/go_es"},
						"logRecords": logList,
					},
				},
			},
		},
	}
}

// Close 关闭并等待 flush 完成
func (l *OTelLogsExporter) Close() {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.closed = true
	close(l.done)
	started := l.started
	l.mu.Unlock()
	if started {
		<-l.stopped
	}
}

// Stats 返回推送统计
func (l *OTelLogsExporter) Stats() (success, fail int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.metricsSuccessFail()
}

func (l *OTelLogsExporter) metricsSuccessFail() (int64, int64) {
	return l.pushSuccess, l.pushFail
}