// otel_exporter_test.go OpenTelemetry 导出器单元测试
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/prometheus/prompb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// TestOTelMetricsExporter_New_Validation 验证指标导出器配置
func TestOTelMetricsExporter_New_Validation(t *testing.T) {
	log, _ := zap.NewDevelopment()

	t.Run("empty endpoint", func(t *testing.T) {
		_, err := NewOTelMetricsExporter(OTelMetricsConfig{}, &http.Client{}, log)
		assert.Error(t, err)
	})

	t.Run("valid config", func(t *testing.T) {
		cfg := DefaultOTelExportConfig()
		cfg.Metrics.Enabled = true
		cfg.Metrics.Endpoint = "http://localhost:4318"
		m, err := NewOTelMetricsExporter(cfg.Metrics, &http.Client{}, log)
		require.NoError(t, err)
		assert.NotNil(t, m)
		m.Close()
	})
}

// TestOTelMetricsExporter_PushMetrics 验证指标推送
func TestOTelMetricsExporter_PushMetrics(t *testing.T) {
	log, _ := zap.NewDevelopment()

	var receivedBody []byte
	var receivedPath string
	var receivedContentType string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedContentType = r.Header.Get("Content-Type")
		receivedBody = make([]byte, 4096)
		n, _ := r.Body.Read(receivedBody)
		receivedBody = receivedBody[:n]
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := DefaultOTelExportConfig()
	cfg.Metrics.Enabled = true
	cfg.Metrics.Endpoint = server.URL
	m, err := NewOTelMetricsExporter(cfg.Metrics, &http.Client{}, log)
	require.NoError(t, err)
	defer m.Close()

	ctx := context.Background()
	tss := []prompb.TimeSeries{
		{
			Labels: []prompb.Label{
				{Name: "__name__", Value: "test_counter"},
				{Name: "method", Value: "GET"},
			},
			Samples: []prompb.Sample{
				{Value: 42.0, Timestamp: time.Now().UnixMilli()},
			},
		},
	}

	err = m.PushMetrics(ctx, tss)
	require.NoError(t, err)

	assert.Equal(t, "/v1/metrics", receivedPath)
	assert.Equal(t, "application/json", receivedContentType)

	var payload map[string]any
	err = json.Unmarshal(receivedBody, &payload)
	require.NoError(t, err)

	rm, ok := payload["resourceMetrics"].([]any)
	assert.True(t, ok)
	assert.Greater(t, len(rm), 0)
}

// TestOTelMetricsExporter_Stats 验证指标统计
func TestOTelMetricsExporter_Stats(t *testing.T) {
	log, _ := zap.NewDevelopment()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := DefaultOTelExportConfig()
	cfg.Metrics.Endpoint = server.URL
	m, err := NewOTelMetricsExporter(cfg.Metrics, &http.Client{}, log)
	require.NoError(t, err)
	defer m.Close()

	ctx := context.Background()

	success, fail := m.Stats()
	assert.Equal(t, int64(0), success)
	assert.Equal(t, int64(0), fail)

	tss := []prompb.TimeSeries{
		{
			Labels:  []prompb.Label{{Name: "__name__", Value: "stats_test"}},
			Samples: []prompb.Sample{{Value: 1.0, Timestamp: time.Now().UnixMilli()}},
		},
	}
	_ = m.PushMetrics(ctx, tss)

	success, fail = m.Stats()
	assert.Equal(t, int64(1), success)
	assert.Equal(t, int64(0), fail)

	badCfg := cfg
	badCfg.Metrics.Endpoint = "http://localhost:19999"
	badCfg.Metrics.Timeout = 500 * time.Millisecond
	m2, _ := NewOTelMetricsExporter(badCfg.Metrics, &http.Client{Timeout: badCfg.Metrics.Timeout}, log)
	defer m2.Close()
	_ = m2.PushMetrics(ctx, tss)

	_, fail2 := m2.Stats()
	assert.GreaterOrEqual(t, fail2, int64(1))
}

// TestOTelTracesExporter_New_Validation 验证追踪导出器配置
func TestOTelTracesExporter_New_Validation(t *testing.T) {
	log, _ := zap.NewDevelopment()

	t.Run("empty endpoint", func(t *testing.T) {
		_, err := NewOTelTracesExporter(OTelTracesConfig{}, &http.Client{}, log)
		assert.Error(t, err)
	})

	t.Run("valid config with defaults", func(t *testing.T) {
		cfg := DefaultOTelExportConfig()
		cfg.Traces.Enabled = true
		cfg.Traces.Endpoint = "http://localhost:4318"
		tracesExp, err := NewOTelTracesExporter(cfg.Traces, &http.Client{}, log)
		require.NoError(t, err)
		assert.NotNil(t, tracesExp)
		assert.Equal(t, 512, tracesExp.cfg.BatchSize)
		assert.Equal(t, 4096, tracesExp.cfg.MaxQueueSize)
		assert.Equal(t, 1.0, tracesExp.cfg.SampleRate)
		tracesExp.Close()
	})

	t.Run("custom batch size", func(t *testing.T) {
		cfg := DefaultOTelExportConfig()
		cfg.Traces.Endpoint = "http://localhost:4318"
		cfg.Traces.BatchSize = 64
		cfg.Traces.MaxQueueSize = 128
		tracesExp, err := NewOTelTracesExporter(cfg.Traces, &http.Client{}, log)
		require.NoError(t, err)
		assert.Equal(t, 64, tracesExp.cfg.BatchSize)
		assert.Equal(t, 128, tracesExp.cfg.MaxQueueSize)
		tracesExp.Close()
	})
}

// TestOTelTracesExporter_ExportSpan 验证 span 入队和批量导出
func TestOTelTracesExporter_ExportSpan(t *testing.T) {
	log, _ := zap.NewDevelopment()

	bodyCh := make(chan []byte, 1)
	var receivedSpans int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/traces" {
			atomic.AddInt64(&receivedSpans, 1)
			buf := make([]byte, 16384)
			n, _ := r.Body.Read(buf)
			select {
			case bodyCh <- buf[:n]:
			default:
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := DefaultOTelExportConfig()
	cfg.Traces.Endpoint = server.URL
	cfg.Traces.BatchSize = 2
	tracesExp, err := NewOTelTracesExporter(cfg.Traces, &http.Client{}, log)
	require.NoError(t, err)
	defer tracesExp.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tracesExp.Start(ctx)

	span1 := OTelSpan{
		TraceID:       "0102030405060708090a0b0c0d0e0f10",
		SpanID:        "0102030405060708",
		Name:          "HTTP GET /api/users",
		Kind:          "server",
		StartTimeUnix: fmt.Sprintf("%d", time.Now().Add(-1*time.Second).UnixNano()),
		EndTimeUnix:   fmt.Sprintf("%d", time.Now().UnixNano()),
		StatusCode:    "STATUS_CODE_OK",
		Attributes:    map[string]string{"http.method": "GET"},
	}

	span2 := OTelSpan{
		TraceID:       "0102030405060708090a0b0c0d0e0f10",
		SpanID:        "a102030405060708",
		ParentSpanID:  "0102030405060708",
		Name:          "db.query",
		Kind:          "client",
		StartTimeUnix: fmt.Sprintf("%d", time.Now().Add(-500*time.Millisecond).UnixNano()),
		EndTimeUnix:   fmt.Sprintf("%d", time.Now().UnixNano()),
		StatusCode:    "STATUS_CODE_OK",
		Attributes:    map[string]string{"db.system": "badger"},
	}

	tracesExp.ExportSpan(span1)
	tracesExp.ExportSpan(span2)

	var receivedBody []byte
	select {
	case receivedBody = <-bodyCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for trace export")
	}

	assert.True(t, atomic.LoadInt64(&receivedSpans) > 0)

	var payload map[string]any
	err = json.Unmarshal(receivedBody, &payload)
	require.NoError(t, err)

	rs, ok := payload["resourceSpans"].([]any)
	assert.True(t, ok)
	assert.Greater(t, len(rs), 0)
}

// TestOTelTracesExporter_ExportTracingSpan 验证 SpanExporter 接口实现
func TestOTelTracesExporter_ExportTracingSpan(t *testing.T) {
	log, _ := zap.NewDevelopment()

	var receivedSpans int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/traces" {
			atomic.AddInt64(&receivedSpans, 1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := DefaultOTelExportConfig()
	cfg.Traces.Endpoint = server.URL
	cfg.Traces.BatchSize = 2
	tracesExp, err := NewOTelTracesExporter(cfg.Traces, &http.Client{}, log)
	require.NoError(t, err)
	defer tracesExp.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tracesExp.Start(ctx)

	tp := NewTracerProvider(DefaultTracingConfig())
	tracer := tp.Tracer("test")
	_, span := tracer.StartSpan(context.Background(), "test_span")
	span.SetAttribute("key", "value")
	span.SetStatus(SpanStatusError)
	tracesExp.ExportTracingSpan(span)

	time.Sleep(2 * time.Second)

	assert.True(t, atomic.LoadInt64(&receivedSpans) > 0)
}

// TestOTelLogsExporter_New_Validation 验证日志导出器配置
func TestOTelLogsExporter_New_Validation(t *testing.T) {
	log, _ := zap.NewDevelopment()

	t.Run("empty endpoint", func(t *testing.T) {
		_, err := NewOTelLogsExporter(OTelLogsConfig{}, &http.Client{}, log)
		assert.Error(t, err)
	})

	t.Run("valid config", func(t *testing.T) {
		cfg := DefaultOTelExportConfig()
		cfg.Logs.Enabled = true
		cfg.Logs.Endpoint = "http://localhost:4318"
		logsExp, err := NewOTelLogsExporter(cfg.Logs, &http.Client{}, log)
		require.NoError(t, err)
		assert.NotNil(t, logsExp)
		assert.Equal(t, "info", logsExp.cfg.MinLevel)
		logsExp.Close()
	})
}

// TestOTelLogsExporter_ExportLog 验证日志入队和批量导出
func TestOTelLogsExporter_ExportLog(t *testing.T) {
	log, _ := zap.NewDevelopment()

	bodyCh := make(chan []byte, 1)
	var receivedLogs int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/logs" {
			atomic.AddInt64(&receivedLogs, 1)
			buf := make([]byte, 16384)
			n, _ := r.Body.Read(buf)
			select {
			case bodyCh <- buf[:n]:
			default:
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := DefaultOTelExportConfig()
	cfg.Logs.Endpoint = server.URL
	cfg.Logs.BatchSize = 2 // 降低批大小, 触发批量刷新
	cfg.Logs.PushInterval = 500 * time.Millisecond
	logsExp, err := NewOTelLogsExporter(cfg.Logs, &http.Client{}, log)
	require.NoError(t, err)
	defer logsExp.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logsExp.Start(ctx)

	entries := []OTelLogEntry{
		{
			TimeUnixNano: fmt.Sprintf("%d", time.Now().UnixNano()),
			Severity:     "INFO",
			Body:         "Server started",
			TraceID:      "0102030405060708090a0b0c0d0e0f10",
			SpanID:       "0102030405060708",
			Attributes:   map[string]string{"component": "server"},
		},
		{
			TimeUnixNano: fmt.Sprintf("%d", time.Now().UnixNano()),
			Severity:     "WARN",
			Body:         "Slow request detected",
			Attributes:   map[string]string{"duration": "2.5s"},
		},
	}

	for _, e := range entries {
		logsExp.ExportLog(e)
	}

	var receivedBody []byte
	select {
	case receivedBody = <-bodyCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for log export")
	}

	assert.True(t, atomic.LoadInt64(&receivedLogs) > 0, "期望收到日志推送")

	var payload map[string]any
	err = json.Unmarshal(receivedBody, &payload)
	require.NoError(t, err)

	rl, ok := payload["resourceLogs"].([]any)
	assert.True(t, ok)
	assert.Greater(t, len(rl), 0)
}

// TestOTelExporter_New_Integration 验证完整 OTelExporter 创建
func TestOTelExporter_New_Integration(t *testing.T) {
	log, _ := zap.NewDevelopment()

	t.Run("all enabled", func(t *testing.T) {
		cfg := DefaultOTelExportConfig()
		cfg.Enabled = true
		cfg.Metrics.Enabled = true
		cfg.Metrics.Endpoint = "http://localhost:4318"
		cfg.Traces.Enabled = true
		cfg.Traces.Endpoint = "http://localhost:4318"
		cfg.Logs.Enabled = true
		cfg.Logs.Endpoint = "http://localhost:4318"

		exp, err := NewOTelExporter(cfg, log)
		require.NoError(t, err)
		assert.NotNil(t, exp)
		assert.NotNil(t, exp.MetricsExporter())
		assert.NotNil(t, exp.TracesExporter())
		assert.NotNil(t, exp.LogsExporter())
		exp.Close()
	})

	t.Run("only metrics", func(t *testing.T) {
		cfg := DefaultOTelExportConfig()
		cfg.Enabled = true
		cfg.Metrics.Enabled = true
		cfg.Metrics.Endpoint = "http://localhost:4318"

		exp, err := NewOTelExporter(cfg, log)
		require.NoError(t, err)
		assert.NotNil(t, exp)
		assert.NotNil(t, exp.MetricsExporter())
		assert.Nil(t, exp.TracesExporter())
		assert.Nil(t, exp.LogsExporter())
		exp.Close()
	})

	t.Run("invalid config returns error", func(t *testing.T) {
		cfg := DefaultOTelExportConfig()
		cfg.Enabled = true
		cfg.Metrics.Enabled = true
		_, err := NewOTelExporter(cfg, log)
		assert.Error(t, err)
	})
}

// TestOTelExporter_StartClose 验证完整生命周期
func TestOTelExporter_StartClose(t *testing.T) {
	log, _ := zap.NewDevelopment()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := DefaultOTelExportConfig()
	cfg.Enabled = true
	cfg.Metrics.Enabled = true
	cfg.Metrics.Endpoint = server.URL
	cfg.Traces.Enabled = true
	cfg.Traces.Endpoint = server.URL
	cfg.Logs.Enabled = true
	cfg.Logs.Endpoint = server.URL

	exp, err := NewOTelExporter(cfg, log)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	exp.Start(ctx)

	if exp.MetricsExporter() != nil {
		ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel2()
		_ = exp.MetricsExporter().PushMetrics(ctx2, []prompb.TimeSeries{
			{
				Labels:  []prompb.Label{{Name: "__name__", Value: "lifecycle_test"}},
				Samples: []prompb.Sample{{Value: 1.0, Timestamp: time.Now().UnixMilli()}},
			},
		})
	}

	if exp.TracesExporter() != nil {
		exp.TracesExporter().ExportSpan(OTelSpan{
			TraceID:       "0102030405060708090a0b0c0d0e0f10",
			SpanID:        "0102030405060708",
			Name:          "lifecycle_test_span",
			StartTimeUnix: fmt.Sprintf("%d", time.Now().UnixNano()),
			EndTimeUnix:   fmt.Sprintf("%d", time.Now().Add(time.Millisecond).UnixNano()),
		})
	}

	if exp.LogsExporter() != nil {
		exp.LogsExporter().ExportLog(OTelLogEntry{
			TimeUnixNano: fmt.Sprintf("%d", time.Now().UnixNano()),
			Severity:     "INFO",
			Body:         "lifecycle test log",
		})
	}

	exp.Close()

	success, fail := exp.MetricsExporter().Stats()
	assert.GreaterOrEqual(t, success, int64(0))
	assert.GreaterOrEqual(t, fail, int64(0))
}

// TestOTelBuildMetricsPayload 验证 OTLP metrics payload 构造
func TestOTelBuildMetricsPayload(t *testing.T) {
	log, _ := zap.NewDevelopment()
	cfg := DefaultOTelExportConfig()
	cfg.Metrics.Endpoint = "http://localhost:4318"
	m, err := NewOTelMetricsExporter(cfg.Metrics, &http.Client{}, log)
	require.NoError(t, err)
	defer m.Close()

	tss := []prompb.TimeSeries{
		{
			Labels: []prompb.Label{
				{Name: "__name__", Value: "request_counter"},
				{Name: "method", Value: "POST"},
			},
			Samples: []prompb.Sample{
				{Value: 10.0, Timestamp: 1700000000000},
				{Value: 20.0, Timestamp: 1700000001000},
			},
		},
	}

	payload := m.buildOTLPMetricsPayload(tss)

	// 序列化后反序列化以获取通用类型
	raw, err := json.Marshal(payload)
	require.NoError(t, err)

	var decoded map[string]any
	err = json.Unmarshal(raw, &decoded)
	require.NoError(t, err)

	rm := decoded["resourceMetrics"].([]any)
	require.True(t, okForSlice(decoded, "resourceMetrics"))
	assert.Equal(t, 1, len(rm))

	firstRM := rm[0].(map[string]any)
	resource := firstRM["resource"].(map[string]any)
	attrs := resource["attributes"].([]any)
	assert.GreaterOrEqual(t, len(attrs), 1)

	sm := firstRM["scopeMetrics"].([]any)
	assert.Equal(t, 1, len(sm))

	metrics := sm[0].(map[string]any)["metrics"].([]any)
	assert.Equal(t, 1, len(metrics))

	metric := metrics[0].(map[string]any)
	assert.Equal(t, "request_counter", metric["name"])

	sum := metric["sum"].(map[string]any)
	dp := sum["dataPoints"].([]any)
	assert.Equal(t, 2, len(dp))
}

func okForSlice(m map[string]any, key string) bool {
	_, ok := m[key].([]any)
	return ok
}

// TestOTelBuildTracesPayload 验证 OTLP traces payload 构造
func TestOTelBuildTracesPayload(t *testing.T) {
	log, _ := zap.NewDevelopment()
	cfg := DefaultOTelExportConfig()
	cfg.Traces.Endpoint = "http://localhost:4318"
	tracesExp, err := NewOTelTracesExporter(cfg.Traces, &http.Client{}, log)
	require.NoError(t, err)
	defer tracesExp.Close()

	spans := []OTelSpan{
		{
			TraceID:       "0102030405060708090a0b0c0d0e0f10",
			SpanID:        "0102030405060708",
			Name:          "test_operation",
			Kind:          "server",
			StartTimeUnix: "1700000000000000000",
			EndTimeUnix:   "1700000001000000000",
			StatusCode:    "STATUS_CODE_OK",
			Attributes:    map[string]string{"key": "value"},
		},
	}

	payload := tracesExp.buildOTLPTracesPayload(spans)

	raw, err := json.Marshal(payload)
	require.NoError(t, err)

	var decoded map[string]any
	err = json.Unmarshal(raw, &decoded)
	require.NoError(t, err)

	rs := decoded["resourceSpans"].([]any)
	require.True(t, okForSlice(decoded, "resourceSpans"))
	assert.Equal(t, 1, len(rs))

	firstRS := rs[0].(map[string]any)
	ss := firstRS["scopeSpans"].([]any)
	assert.Equal(t, 1, len(ss))

	spansList := ss[0].(map[string]any)["spans"].([]any)
	assert.Equal(t, 1, len(spansList))

	firstSpan := spansList[0].(map[string]any)
	assert.Equal(t, "0102030405060708090a0b0c0d0e0f10", firstSpan["traceId"])
	assert.Equal(t, "0102030405060708", firstSpan["spanId"])
	assert.Equal(t, "test_operation", firstSpan["name"])
	assert.Equal(t, "server", firstSpan["kind"])
}

// TestOTelBuildLogsPayload 验证 OTLP logs payload 构造
func TestOTelBuildLogsPayload(t *testing.T) {
	log, _ := zap.NewDevelopment()
	cfg := DefaultOTelExportConfig()
	cfg.Logs.Endpoint = "http://localhost:4318"
	logsExp, err := NewOTelLogsExporter(cfg.Logs, &http.Client{}, log)
	require.NoError(t, err)
	defer logsExp.Close()

	entries := []OTelLogEntry{
		{
			TimeUnixNano: "1700000000000000000",
			Severity:     "ERROR",
			Body:         "Something went wrong",
			TraceID:      "0102030405060708090a0b0c0d0e0f10",
			SpanID:       "0102030405060708",
			Attributes:   map[string]string{"error_type": "timeout"},
		},
	}

	payload := logsExp.buildOTLPLogsPayload(entries)

	raw, err := json.Marshal(payload)
	require.NoError(t, err)

	var decoded map[string]any
	err = json.Unmarshal(raw, &decoded)
	require.NoError(t, err)

	rl := decoded["resourceLogs"].([]any)
	require.True(t, okForSlice(decoded, "resourceLogs"))
	assert.Equal(t, 1, len(rl))

	firstRL := rl[0].(map[string]any)
	sl := firstRL["scopeLogs"].([]any)
	assert.Equal(t, 1, len(sl))

	logRecords := sl[0].(map[string]any)["logRecords"].([]any)
	assert.Equal(t, 1, len(logRecords))

	firstLog := logRecords[0].(map[string]any)
	assert.Equal(t, "1700000000000000000", firstLog["timeUnixNano"])
	assert.Equal(t, "ERROR", firstLog["severityText"])
	assert.Equal(t, "0102030405060708090a0b0c0d0e0f10", firstLog["traceId"])
	assert.Equal(t, "0102030405060708", firstLog["spanId"])

	body := firstLog["body"].(map[string]any)
	assert.Equal(t, "Something went wrong", body["stringValue"])
}

// -------------------------------------------------------------------
// OTel Retry Logging 验证测试
// -------------------------------------------------------------------

// otelBufferSyncer 用于捕获 zap 日志输出
type otelBufferSyncer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *otelBufferSyncer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *otelBufferSyncer) Sync() error { return nil }

func (b *otelBufferSyncer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newOTelBufferLogger 创建一个将日志写入 buffer 的 zap.Logger
func newOTelBufferLogger() (*zap.Logger, *otelBufferSyncer) {
	syncer := &otelBufferSyncer{}
	encoder := zapcore.NewJSONEncoder(zapcore.EncoderConfig{
		TimeKey:        "time",
		LevelKey:       "level",
		NameKey:        "caller",
		MessageKey:     "msg",
		StacktraceKey:  "stacktrace",
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeLevel:    zapcore.LowercaseLevelEncoder,
		EncodeTime:     zapcore.EpochTimeEncoder,
		EncodeDuration: zapcore.SecondsDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	})
	core := zapcore.NewCore(encoder, syncer, zapcore.DebugLevel)
	logger := zap.New(core, zap.AddCaller())
	return logger, syncer
}

// TestOTelMetrics_RetryLogging_500Error 验证 metrics 导出器在 500 错误时重试日志
func TestOTelMetrics_RetryLogging_500Error(t *testing.T) {
	logger, syncer := newOTelBufferLogger()

	var requestCount int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requestCount, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	cfg := DefaultOTelExportConfig()
	cfg.Metrics.Endpoint = server.URL
	cfg.Metrics.MaxRetries = 2 // 共 3 次尝试

	m, err := NewOTelMetricsExporter(cfg.Metrics, &http.Client{}, logger)
	require.NoError(t, err)
	defer m.Close()

	ctx := context.Background()
	tss := []prompb.TimeSeries{
		{
			Labels:  []prompb.Label{{Name: "__name__", Value: "otel_500_test"}},
			Samples: []prompb.Sample{{Value: 1.0, Timestamp: time.Now().UnixMilli()}},
		},
	}

	err = m.PushMetrics(ctx, tss)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "全部 3 次重试失败")

	assert.Equal(t, int64(3), atomic.LoadInt64(&requestCount), "应总共发送 3 次(1 初始 + 2 重试)")

	logOutput := syncer.String()
	assert.Contains(t, logOutput, "OTLP 重试", "应包含重试日志")
	assert.Contains(t, logOutput, "OTLP 服务端返回错误", "应包含 500 错误日志")
	assert.Contains(t, logOutput, "OTLP 全部重试失败", "应包含最终失败日志")
	assert.Contains(t, logOutput, `"attempt"`, "应包含 attempt 字段")
	assert.Contains(t, logOutput, `"total_attempts":3`, "应包含总尝试次数")
	assert.Contains(t, logOutput, `"total_elapsed"`, "应包含总耗时")

	// 验证重试 attempt 递增
	assert.Contains(t, logOutput, `"attempt":2`, "应记录第 2 次尝试")
	assert.Contains(t, logOutput, `"attempt":3`, "应记录第 3 次尝试")

	// 验证 backoff
	assert.Contains(t, logOutput, `"backoff"`, "应包含退避时间")

	// 验证 Stats
	success, fail := m.Stats()
	assert.Equal(t, int64(0), success)
	assert.Equal(t, int64(3), fail)

	t.Logf("OTel metrics 500 重试验证通过: 重试 %d 次, 错误: %s",
		atomic.LoadInt64(&requestCount), err.Error())
}

// TestOTelMetrics_RetryLogging_NetworkError 验证 metrics 导出器在网络错误时重试日志
func TestOTelMetrics_RetryLogging_NetworkError(t *testing.T) {
	logger, syncer := newOTelBufferLogger()

	cfg := DefaultOTelExportConfig()
	cfg.Metrics.Endpoint = "http://127.0.0.1:19999" // 未监听的端口
	cfg.Metrics.MaxRetries = 1                        // 共 2 次尝试
	cfg.Metrics.Timeout = 500 * time.Millisecond

	m, err := NewOTelMetricsExporter(cfg.Metrics, &http.Client{Timeout: cfg.Metrics.Timeout}, logger)
	require.NoError(t, err)
	defer m.Close()

	ctx := context.Background()
	tss := []prompb.TimeSeries{
		{
			Labels:  []prompb.Label{{Name: "__name__", Value: "otel_net_test"}},
			Samples: []prompb.Sample{{Value: 1.0, Timestamp: time.Now().UnixMilli()}},
		},
	}

	err = m.PushMetrics(ctx, tss)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "全部 2 次重试失败")

	logOutput := syncer.String()
	assert.Contains(t, logOutput, "OTLP 请求错误", "应包含网络错误日志")
	assert.Contains(t, logOutput, "OTLP 全部重试失败", "应包含最终失败日志")
	assert.Contains(t, logOutput, `"total_attempts":2`, "应包含总尝试次数")
	assert.Contains(t, logOutput, `"error"`, "应包含具体错误")

	// 验证错误类型
	assert.True(t, strings.Contains(logOutput, "connection refused") ||
		strings.Contains(logOutput, "i/o timeout") ||
		strings.Contains(logOutput, "dial tcp"),
		"错误日志应包含网络错误类型")

	success, fail := m.Stats()
	assert.Equal(t, int64(0), success)
	assert.Equal(t, int64(2), fail)

	t.Logf("OTel metrics 网络错误重试验证通过: 错误: %s", err.Error())
}

// TestOTelMetrics_RetryLogging_Success 验证 metrics 成功路径日志
func TestOTelMetrics_RetryLogging_Success(t *testing.T) {
	logger, _ := newOTelBufferLogger()

	var requestCount int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requestCount, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := DefaultOTelExportConfig()
	cfg.Metrics.Endpoint = server.URL
	cfg.Metrics.MaxRetries = 2

	m, err := NewOTelMetricsExporter(cfg.Metrics, &http.Client{}, logger)
	require.NoError(t, err)
	defer m.Close()

	ctx := context.Background()
	tss := []prompb.TimeSeries{
		{
			Labels:  []prompb.Label{{Name: "__name__", Value: "otel_success_test"}},
			Samples: []prompb.Sample{{Value: 42.0, Timestamp: time.Now().UnixMilli()}},
		},
	}

	err = m.PushMetrics(ctx, tss)
	assert.NoError(t, err)

	assert.Equal(t, int64(1), atomic.LoadInt64(&requestCount), "成功时应只发送 1 次")

	success, fail := m.Stats()
	assert.Equal(t, int64(1), success)
	assert.Equal(t, int64(0), fail)
}

// TestOTelTraces_RetryLogging_500Error 验证 traces flushBatch 在 500 错误时重试
func TestOTelTraces_RetryLogging_500Error(t *testing.T) {
	logger, syncer := newOTelBufferLogger()

	var requestCount int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requestCount, 1)
		if r.URL.Path == "/v1/traces" {
			w.WriteHeader(http.StatusInternalServerError)
		} else {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	cfg := DefaultOTelExportConfig()
	cfg.Traces.Endpoint = server.URL
	cfg.Traces.MaxRetries = 2

	tracesExp, err := NewOTelTracesExporter(cfg.Traces, &http.Client{}, logger)
	require.NoError(t, err)
	defer tracesExp.Close()

	ctx := context.Background()
	batch := []OTelSpan{
		{
			TraceID:       "0102030405060708090a0b0c0d0e0f10",
			SpanID:        "0102030405060708",
			Name:          "test_span",
			StartTimeUnix: fmt.Sprintf("%d", time.Now().UnixNano()),
			EndTimeUnix:   fmt.Sprintf("%d", time.Now().Add(time.Millisecond).UnixNano()),
			StatusCode:    "STATUS_CODE_OK",
		},
	}

	// 直接调用 flushBatch 触发发送
	tracesExp.flushBatch(ctx, batch)

	assert.Equal(t, int64(3), atomic.LoadInt64(&requestCount), "应发送 3 次(1 初始 + 2 重试)")

	logOutput := syncer.String()
	assert.Contains(t, logOutput, "OTLP 重试", "应包含重试日志")
	assert.Contains(t, logOutput, "OTLP 全部重试失败", "应包含最终失败日志")
	assert.Contains(t, logOutput, `"total_attempts":3`, "应包含总尝试次数")

	_, fail := tracesExp.Stats()
	assert.Equal(t, int64(3), fail)

	t.Logf("OTel traces 500 重试验证通过: 重试 %d 次", atomic.LoadInt64(&requestCount))
}

// TestOTelLogs_RetryLogging_500Error 验证 logs flushBatch 在 500 错误时重试
func TestOTelLogs_RetryLogging_500Error(t *testing.T) {
	logger, syncer := newOTelBufferLogger()

	var requestCount int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requestCount, 1)
		if r.URL.Path == "/v1/logs" {
			w.WriteHeader(http.StatusInternalServerError)
		} else {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	cfg := DefaultOTelExportConfig()
	cfg.Logs.Endpoint = server.URL
	cfg.Logs.MaxRetries = 2

	logsExp, err := NewOTelLogsExporter(cfg.Logs, &http.Client{}, logger)
	require.NoError(t, err)
	defer logsExp.Close()

	ctx := context.Background()
	batch := []OTelLogEntry{
		{
			TimeUnixNano: fmt.Sprintf("%d", time.Now().UnixNano()),
			Severity:     "ERROR",
			Body:         "test log for retry",
			TraceID:      "0102030405060708090a0b0c0d0e0f10",
			SpanID:       "0102030405060708",
		},
	}

	logsExp.flushBatch(ctx, batch)

	assert.Equal(t, int64(3), atomic.LoadInt64(&requestCount), "应发送 3 次(1 初始 + 2 重试)")

	logOutput := syncer.String()
	assert.Contains(t, logOutput, "OTLP 重试", "应包含重试日志")
	assert.Contains(t, logOutput, "OTLP 全部重试失败", "应包含最终失败日志")
	assert.Contains(t, logOutput, `"total_attempts":3`, "应包含总尝试次数")

	_, fail := logsExp.Stats()
	assert.Equal(t, int64(3), fail)

	t.Logf("OTel logs 500 重试验证通过: 重试 %d 次", atomic.LoadInt64(&requestCount))
}

// TestOTelTraces_RetryLogging_NetworkError 验证 traces 导出器在网络错误时重试日志
func TestOTelTraces_RetryLogging_NetworkError(t *testing.T) {
	logger, syncer := newOTelBufferLogger()

	cfg := DefaultOTelExportConfig()
	cfg.Traces.Endpoint = "http://127.0.0.1:19999" // 未监听端口
	cfg.Traces.MaxRetries = 1
	cfg.Traces.Timeout = 500 * time.Millisecond

	tracesExp, err := NewOTelTracesExporter(cfg.Traces, &http.Client{Timeout: cfg.Traces.Timeout}, logger)
	require.NoError(t, err)
	defer tracesExp.Close()

	ctx := context.Background()
	batch := []OTelSpan{
		{
			TraceID:       "0102030405060708090a0b0c0d0e0f10",
			SpanID:        "0102030405060708",
			Name:          "net_err_span",
			StartTimeUnix: fmt.Sprintf("%d", time.Now().UnixNano()),
			EndTimeUnix:   fmt.Sprintf("%d", time.Now().Add(time.Millisecond).UnixNano()),
		},
	}

	tracesExp.flushBatch(ctx, batch)

	logOutput := syncer.String()
	assert.Contains(t, logOutput, "OTLP 请求错误", "应包含网络错误日志")
	assert.Contains(t, logOutput, "OTLP 全部重试失败", "应包含最终失败日志")
	assert.Contains(t, logOutput, `"total_attempts":2`, "应包含总尝试次数")

	assert.True(t, strings.Contains(logOutput, "connection refused") ||
		strings.Contains(logOutput, "i/o timeout") ||
		strings.Contains(logOutput, "dial tcp"),
		"错误日志应包含网络错误类型")

	_, fail := tracesExp.Stats()
	assert.Equal(t, int64(2), fail)

	t.Logf("OTel traces 网络错误重试验证通过")
}

// TestOTelLogs_RetryLogging_NetworkError 验证 logs 导出器在网络错误时重试日志
func TestOTelLogs_RetryLogging_NetworkError(t *testing.T) {
	logger, syncer := newOTelBufferLogger()

	cfg := DefaultOTelExportConfig()
	cfg.Logs.Endpoint = "http://127.0.0.1:19999" // 未监听端口
	cfg.Logs.MaxRetries = 1
	cfg.Logs.Timeout = 500 * time.Millisecond

	logsExp, err := NewOTelLogsExporter(cfg.Logs, &http.Client{Timeout: cfg.Logs.Timeout}, logger)
	require.NoError(t, err)
	defer logsExp.Close()

	ctx := context.Background()
	batch := []OTelLogEntry{
		{
			TimeUnixNano: fmt.Sprintf("%d", time.Now().UnixNano()),
			Severity:     "ERROR",
			Body:         "network error test",
		},
	}

	logsExp.flushBatch(ctx, batch)

	logOutput := syncer.String()
	assert.Contains(t, logOutput, "OTLP 请求错误", "应包含网络错误日志")
	assert.Contains(t, logOutput, "OTLP 全部重试失败", "应包含最终失败日志")
	assert.Contains(t, logOutput, `"total_attempts":2`, "应包含总尝试次数")

	assert.True(t, strings.Contains(logOutput, "connection refused") ||
		strings.Contains(logOutput, "i/o timeout") ||
		strings.Contains(logOutput, "dial tcp"),
		"错误日志应包含网络错误类型")

	_, fail := logsExp.Stats()
	assert.Equal(t, int64(2), fail)

	t.Logf("OTel logs 网络错误重试验证通过")
}

// TestOTel_RetryLogging_4xxNoRetry 验证 OTel 4xx 错误不重试
func TestOTel_RetryLogging_4xxNoRetry(t *testing.T) {
	logger, syncer := newOTelBufferLogger()

	var requestCount int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requestCount, 1)
		w.WriteHeader(http.StatusBadRequest) // 400
	}))
	defer server.Close()

	cfg := DefaultOTelExportConfig()
	cfg.Metrics.Endpoint = server.URL
	cfg.Metrics.MaxRetries = 5 // 即使设了重试

	m, err := NewOTelMetricsExporter(cfg.Metrics, &http.Client{}, logger)
	require.NoError(t, err)
	defer m.Close()

	ctx := context.Background()
	tss := []prompb.TimeSeries{
		{
			Labels:  []prompb.Label{{Name: "__name__", Value: "otel_4xx_test"}},
			Samples: []prompb.Sample{{Value: 1.0, Timestamp: time.Now().UnixMilli()}},
		},
	}

	_ = m.PushMetrics(ctx, tss)

	// 4xx 不应重试,只发送一次
	assert.Equal(t, int64(1), atomic.LoadInt64(&requestCount), "4xx 错误不应重试,只发 1 次")

	logOutput := syncer.String()
	assert.Contains(t, logOutput, "OTLP 客户端错误,不再重试", "应包含 4xx 不重试日志")

	t.Logf("OTel 4xx 不重试验证通过: 仅发送 %d 次", atomic.LoadInt64(&requestCount))
}