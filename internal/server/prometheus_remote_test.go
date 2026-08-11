// prometheus_remote_test.go Prometheus remote_write 客户端单元测试
package server

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang/snappy"
	"github.com/gogo/protobuf/proto"
	"github.com/prometheus/prometheus/prompb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// TestRemoteWriter_New_ClientConfig 验证客户端配置验证
func TestRemoteWriter_New_ClientConfig(t *testing.T) {
	log, _ := zap.NewDevelopment()

	t.Run("empty endpoint", func(t *testing.T) {
		_, err := NewRemoteWriter(RemoteWriteConfig{}, log)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "endpoint")
	})

	t.Run("invalid url", func(t *testing.T) {
		cfg := DefaultRemoteWriteConfig()
		cfg.Endpoint = "://invalid"
		_, err := NewRemoteWriter(cfg, log)
		assert.Error(t, err)
	})

	t.Run("basic auth mismatch", func(t *testing.T) {
		cfg := DefaultRemoteWriteConfig()
		cfg.Endpoint = "http://localhost:9090/api/v1/write"
		cfg.BasicAuth.Username = "user"
		_, err := NewRemoteWriter(cfg, log)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "password")
	})

	t.Run("valid config with defaults", func(t *testing.T) {
		cfg := DefaultRemoteWriteConfig()
		cfg.Endpoint = "http://localhost:9090/api/v1/write"
		rw, err := NewRemoteWriter(cfg, log)
		require.NoError(t, err)
		assert.NotNil(t, rw)
		assert.Equal(t, "http://localhost:9090/api/v1/write", rw.cfg.Endpoint)
		assert.Equal(t, 15*time.Second, rw.cfg.PushInterval)
		assert.Equal(t, 1000, rw.cfg.BatchSize)
		assert.Equal(t, 3, rw.cfg.MaxRetries)
		assert.Equal(t, 30*time.Second, rw.cfg.Timeout)
		rw.Close()
	})

	t.Run("valid config with custom values", func(t *testing.T) {
		cfg := DefaultRemoteWriteConfig()
		cfg.Endpoint = "http://example.com/api/v1/write"
		cfg.PushInterval = 5 * time.Second
		cfg.BatchSize = 500
		cfg.MaxRetries = 5
		cfg.Timeout = 10 * time.Second
		rw, err := NewRemoteWriter(cfg, log)
		require.NoError(t, err)
		assert.Equal(t, 5*time.Second, rw.cfg.PushInterval)
		assert.Equal(t, 500, rw.cfg.BatchSize)
		assert.Equal(t, 5, rw.cfg.MaxRetries)
		assert.Equal(t, 10*time.Second, rw.cfg.Timeout)
		rw.Close()
	})
}

// TestRemoteWriter_Enqueue 验证入队逻辑
func TestRemoteWriter_Enqueue(t *testing.T) {
	log, _ := zap.NewDevelopment()
	cfg := DefaultRemoteWriteConfig()
	cfg.Endpoint = "http://localhost:9090/api/v1/write"
	rw, err := NewRemoteWriter(cfg, log)
	require.NoError(t, err)
	defer rw.Close()

	// 空输入不应 panic
	rw.Enqueue()

	tss := []prompb.TimeSeries{
		{
			Labels: []prompb.Label{
				{Name: "__name__", Value: "test_metric"},
				{Name: "label1", Value: "value1"},
			},
			Samples: []prompb.Sample{
				{Value: 42.0, Timestamp: time.Now().UnixMilli()},
			},
		},
	}

	rw.Enqueue(tss...)
	assert.GreaterOrEqual(t, rw.PendingQueueSize(), 1)
}

// TestRemoteWriter_WriteRequest_Encoding 验证 WriteRequest 编码格式
func TestRemoteWriter_WriteRequest_Encoding(t *testing.T) {
	log, _ := zap.NewDevelopment()
	cfg := DefaultRemoteWriteConfig()
	cfg.Endpoint = "http://localhost:9090/api/v1/write"
	rw, err := NewRemoteWriter(cfg, log)
	require.NoError(t, err)
	defer rw.Close()

	// 构造测试数据
	tss := []prompb.TimeSeries{
		{
			Labels: []prompb.Label{
				{Name: "__name__", Value: "go_es_test_counter"},
				{Name: "method", Value: "GET"},
				{Name: "route", Value: "/test"},
			},
			Samples: []prompb.Sample{
				{Value: 100.0, Timestamp: time.Now().UnixMilli()},
			},
		},
		{
			Labels: []prompb.Label{
				{Name: "__name__", Value: "go_es_test_gauge"},
			},
			Samples: []prompb.Sample{
				{Value: 3.14, Timestamp: time.Now().UnixMilli()},
			},
		},
	}

	// 编码
	req := &prompb.WriteRequest{Timeseries: tss}
	raw, err := req.Marshal()
	require.NoError(t, err)

	compressed := snappy.Encode(nil, raw)

	// 解码验证
	decompressed, err := snappy.Decode(nil, compressed)
	require.NoError(t, err)

	var decodedReq prompb.WriteRequest
	err = decodedReq.Unmarshal(decompressed)
	require.NoError(t, err)

	assert.Equal(t, 2, len(decodedReq.Timeseries))
	assert.Equal(t, "go_es_test_counter", decodedReq.Timeseries[0].Labels[0].Value)
	assert.Equal(t, "GET", decodedReq.Timeseries[0].Labels[1].Value)
	assert.Equal(t, 100.0, decodedReq.Timeseries[0].Samples[0].Value)
	assert.Greater(t, req.Size(), 0) // 验证 Size() 返回正数
	assert.Greater(t, len(raw), 0)
	assert.Greater(t, len(compressed), 0)
}

// TestRemoteWriter_Write_Success 验证成功写入流程
func TestRemoteWriter_Write_Success(t *testing.T) {
	log, _ := zap.NewDevelopment()

	var receivedRequests int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&receivedRequests, 1)
		assert.Equal(t, "/api/v1/write", r.URL.Path)
		assert.Equal(t, "application/x-protobuf", r.Header.Get("Content-Type"))
		assert.Equal(t, "snappy", r.Header.Get("Content-Encoding"))

		// 验证 Authorization
		assert.Equal(t, "Basic dXNlcjpwYXNz", r.Header.Get("Authorization"))

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := DefaultRemoteWriteConfig()
	cfg.Endpoint = server.URL + "/api/v1/write"
	cfg.BasicAuth = BasicAuthCfg{Username: "user", Password: "pass"}
	cfg.MaxRetries = 0 // 不需要重试
	rw, err := NewRemoteWriter(cfg, log)
	require.NoError(t, err)
	defer rw.Close()

	ctx := context.Background()

	tss := []prompb.TimeSeries{
		{
			Labels: []prompb.Label{
				{Name: "__name__", Value: "test_metric"},
			},
			Samples: []prompb.Sample{
				{Value: 1.0, Timestamp: time.Now().UnixMilli()},
			},
		},
	}

	err = rw.writeBatch(ctx, tss)
	require.NoError(t, err)
	assert.Equal(t, int64(1), atomic.LoadInt64(&receivedRequests))
	assert.True(t, atomic.LoadInt64(&rw.pushSuccess) > 0)
}

// TestRemoteWriter_Write_Retry 验证重试机制
func TestRemoteWriter_Write_Retry(t *testing.T) {
	log, _ := zap.NewDevelopment()

	var attempts int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt64(&attempts, 1)
		if count <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := DefaultRemoteWriteConfig()
	cfg.Endpoint = server.URL + "/api/v1/write"
	cfg.MaxRetries = 3
	cfg.Timeout = 5 * time.Second
	rw, err := NewRemoteWriter(cfg, log)
	require.NoError(t, err)
	defer rw.Close()

	ctx := context.Background()

	tss := []prompb.TimeSeries{
		{
			Labels: []prompb.Label{
				{Name: "__name__", Value: "retry_metric"},
			},
			Samples: []prompb.Sample{
				{Value: 1.0, Timestamp: time.Now().UnixMilli()},
			},
		},
	}

	err = rw.writeBatch(ctx, tss)
	require.NoError(t, err)
	assert.Equal(t, int64(3), atomic.LoadInt64(&attempts))
}

// TestRemoteWriter_Write_ClientError_NoRetry 验证客户端错误不重试
func TestRemoteWriter_Write_ClientError_NoRetry(t *testing.T) {
	log, _ := zap.NewDevelopment()

	var attempts int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&attempts, 1)
		w.WriteHeader(http.StatusBadRequest) // 400 - 客户端错误
	}))
	defer server.Close()

	cfg := DefaultRemoteWriteConfig()
	cfg.Endpoint = server.URL + "/api/v1/write"
	cfg.MaxRetries = 3
	rw, err := NewRemoteWriter(cfg, log)
	require.NoError(t, err)
	defer rw.Close()

	ctx := context.Background()

	tss := []prompb.TimeSeries{
		{
			Labels: []prompb.Label{
				{Name: "__name__", Value: "bad_metric"},
			},
			Samples: []prompb.Sample{
				{Value: 1.0, Timestamp: time.Now().UnixMilli()},
			},
		},
	}

	err = rw.writeBatch(ctx, tss)
	require.Error(t, err)
	// 400 错误不应重试
	assert.Equal(t, int64(1), atomic.LoadInt64(&attempts))
}

// TestRemoteWriter_Write_WithHeaders 验证自定义请求头
func TestRemoteWriter_Write_WithHeaders(t *testing.T) {
	log, _ := zap.NewDevelopment()

	headerCh := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case headerCh <- r.Header.Clone():
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := DefaultRemoteWriteConfig()
	cfg.Endpoint = server.URL + "/api/v1/write"
	cfg.BearerToken = "my-bearer-token"
	cfg.Headers = map[string]string{
		"X-Custom-Header": "custom-value",
	}
	rw, err := NewRemoteWriter(cfg, log)
	require.NoError(t, err)
	defer rw.Close()

	ctx := context.Background()

	tss := []prompb.TimeSeries{
		{
			Labels:  []prompb.Label{{Name: "__name__", Value: "test"}},
			Samples: []prompb.Sample{{Value: 1.0, Timestamp: time.Now().UnixMilli()}},
		},
	}

	err = rw.writeBatch(ctx, tss)
	require.NoError(t, err)

	receivedHeaders := <-headerCh
	assert.Equal(t, "Bearer my-bearer-token", receivedHeaders.Get("Authorization"))
	assert.Equal(t, "custom-value", receivedHeaders.Get("X-Custom-Header"))
}

// TestRemoteWriter_StartClose 验证启动/关闭生命周期
func TestRemoteWriter_StartClose(t *testing.T) {
	log, _ := zap.NewDevelopment()
	cfg := DefaultRemoteWriteConfig()
	cfg.Endpoint = "http://localhost:9090/api/v1/write"
	cfg.PushInterval = 100 * time.Millisecond // 短间隔便于测试
	rw, err := NewRemoteWriter(cfg, log)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rw.Start(ctx)

	// 入队一些数据
	tss := []prompb.TimeSeries{
		{
			Labels:  []prompb.Label{{Name: "__name__", Value: "lifecycle_metric"}},
			Samples: []prompb.Sample{{Value: 1.0, Timestamp: time.Now().UnixMilli()}},
		},
	}
	rw.Enqueue(tss...)
	assert.GreaterOrEqual(t, rw.PendingQueueSize(), 1)

	// 关闭应触发 flush
	rw.Close()

	// 给一些时间让 flush 完成
	time.Sleep(200 * time.Millisecond)
}

// TestRemoteWriter_Stats 验证统计数据
func TestRemoteWriter_Stats(t *testing.T) {
	log, _ := zap.NewDevelopment()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := DefaultRemoteWriteConfig()
	cfg.Endpoint = server.URL + "/api/v1/write"
	cfg.MaxRetries = 0
	rw, err := NewRemoteWriter(cfg, log)
	require.NoError(t, err)
	defer rw.Close()

	ctx := context.Background()
	tss := []prompb.TimeSeries{
		{
			Labels:  []prompb.Label{{Name: "__name__", Value: "stats_test"}},
			Samples: []prompb.Sample{{Value: 1.0, Timestamp: time.Now().UnixMilli()}},
		},
	}

	// 成功一次
	err = rw.writeBatch(ctx, tss)
	require.NoError(t, err)

	// 用一个无效地址测试失败
	badCfg := cfg
	badCfg.Endpoint = "http://localhost:19999/nonexistent"
	badCfg.Timeout = 1 * time.Second
	rw2, _ := NewRemoteWriter(badCfg, log)
	defer rw2.Close()
	_ = rw2.writeBatch(ctx, tss)

	success, fail := rw.Stats()
	assert.Equal(t, int64(1), success)
	assert.Equal(t, int64(0), fail)

	success2, fail2 := rw2.Stats()
	assert.Equal(t, int64(0), success2)
	assert.GreaterOrEqual(t, fail2, int64(1))
}

// TestRemoteWriter_MarshalCompatibility 验证与标准 protobuf 库的兼容性
func TestRemoteWriter_MarshalCompatibility(t *testing.T) {
	tss := []prompb.TimeSeries{
		{
			Labels: []prompb.Label{
				{Name: "__name__", Value: "counter_total"},
				{Name: "status", Value: "200"},
			},
			Samples: []prompb.Sample{
				{Value: 42.0, Timestamp: 1700000000000},
				{Value: 100.0, Timestamp: 1700000001000},
			},
		},
		{
			Labels: []prompb.Label{
				{Name: "__name__", Value: "gauge_current"},
			},
			Samples: []prompb.Sample{
				{Value: 3.14, Timestamp: 1700000000000},
			},
		},
	}

	req := &prompb.WriteRequest{Timeseries: tss}

	// Marshal
	data, err := proto.Marshal(req)
	require.NoError(t, err)
	assert.NotEmpty(t, data)

	// Size
	size := req.Size()
	assert.Greater(t, size, 0)

	// Unmarshal
	var decoded prompb.WriteRequest
	err = proto.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, 2, len(decoded.Timeseries))
	assert.Equal(t, "counter_total", decoded.Timeseries[0].Labels[0].Value)
	assert.Equal(t, 2, len(decoded.Timeseries[0].Samples))
	assert.Equal(t, 42.0, decoded.Timeseries[0].Samples[0].Value)
	assert.Equal(t, int64(1700000000000), decoded.Timeseries[0].Samples[0].Timestamp)
	assert.Equal(t, 3.14, decoded.Timeseries[1].Samples[0].Value)

	fmt.Printf("WriteRequest size: %d bytes (protobuf), %d bytes (snappy compressed)\n",
		size, len(snappy.Encode(nil, data)))
}

// bufferSyncer 是一个简单的写入器，用于捕获 zap 日志输出
type bufferSyncer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *bufferSyncer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *bufferSyncer) Sync() error { return nil }

func (b *bufferSyncer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newBufferLogger 创建一个将日志写入 buffer 的 zap.Logger
func newBufferLogger() (*zap.Logger, *bufferSyncer) {
	syncer := &bufferSyncer{}
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

// TestRemoteWriter_RetryLogging_500Error 验证 500 错误时日志准确记录重试次数和最终失败原因
func TestRemoteWriter_RetryLogging_500Error(t *testing.T) {
	logger, syncer := newBufferLogger()

	// 创建返回 500 的测试服务器
	var requestCount int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requestCount, 1)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Internal Server Error"))
	}))
	defer server.Close()

	cfg := DefaultRemoteWriteConfig()
	cfg.Endpoint = server.URL + "/api/v1/write"
	cfg.MaxRetries = 3       // 总共 4 次尝试(1 初始 + 3 重试)
	cfg.Timeout = 2 * time.Second
	cfg.BatchSize = 500

	rw, err := NewRemoteWriter(cfg, logger)
	require.NoError(t, err)
	defer rw.Close()

	ctx := context.Background()
	tss := []prompb.TimeSeries{
		{
			Labels:  []prompb.Label{{Name: "__name__", Value: "retry_500_test"}},
			Samples: []prompb.Sample{{Value: 99.0, Timestamp: time.Now().UnixMilli()}},
		},
	}

	err = rw.writeBatch(ctx, tss)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "服务端返回 500")

	// 验证重试次数(4 次尝试)
	assert.Equal(t, int64(4), atomic.LoadInt64(&requestCount), "应该总共发送 4 次(1 初始 + 3 重试)")

	// 获取日志输出
	logOutput := syncer.String()

	// 验证关键日志内容
	assert.Contains(t, logOutput, "remote_write 重试", "应包含重试日志")
	assert.Contains(t, logOutput, "remote_write 服务端返回错误", "应包含 500 错误日志")
	assert.Contains(t, logOutput, "remote_write 全部重试失败", "应包含最终失败日志")
	assert.Contains(t, logOutput, `"attempt"`, "应包含 attempt 字段")
	assert.Contains(t, logOutput, `"status_code":500`, "应包含 500 状态码")
	assert.Contains(t, logOutput, `"timeseries_count":1`, "应包含 timeseries_count")
	assert.Contains(t, logOutput, `"elapsed"`, "应包含耗时")
	assert.Contains(t, logOutput, `"total_attempts":4`, "应包含总尝试次数")
	assert.Contains(t, logOutput, `"total_elapsed"`, "应包含总耗时")
	assert.Contains(t, logOutput, `服务端返回 500`, "应包含最终错误原因")

	// 验证重试日志中的 attempt 递增
	assert.Contains(t, logOutput, `"attempt":2`, "应记录第 2 次尝试")
	assert.Contains(t, logOutput, `"attempt":3`, "应记录第 3 次尝试")
	assert.Contains(t, logOutput, `"attempt":4`, "应记录第 4 次尝试")

	// 验证 backoff 时间
	assert.Contains(t, logOutput, `"backoff"`, "应包含退避时间")

	// 验证 Stats
	success, fail := rw.Stats()
	assert.Equal(t, int64(0), success)
	assert.Equal(t, int64(4), fail, "4 次尝试均应计入失败")

	t.Logf("500 错误日志验证通过: 重试 %d 次, 最终失败原因: %s",
		atomic.LoadInt64(&requestCount), err.Error())
}

// TestRemoteWriter_RetryLogging_NetworkTimeout 验证网络超时时日志准确记录重试次数和最终失败原因
func TestRemoteWriter_RetryLogging_NetworkTimeout(t *testing.T) {
	logger, syncer := newBufferLogger()

	// 不启动服务器，直接连接一个无效地址模拟网络超时
	cfg := DefaultRemoteWriteConfig()
	cfg.Endpoint = "http://127.0.0.1:19999/api/v1/write" // 未监听的端口
	cfg.MaxRetries = 2       // 总共 3 次尝试
	cfg.Timeout = 500 * time.Millisecond
	cfg.BatchSize = 100

	rw, err := NewRemoteWriter(cfg, logger)
	require.NoError(t, err)
	defer rw.Close()

	ctx := context.Background()
	tss := []prompb.TimeSeries{
		{
			Labels:  []prompb.Label{{Name: "__name__", Value: "timeout_test"}},
			Samples: []prompb.Sample{{Value: 42.0, Timestamp: time.Now().UnixMilli()}},
		},
	}

	startTime := time.Now()
	err = rw.writeBatch(ctx, tss)
	totalDuration := time.Since(startTime)

	assert.Error(t, err, "应返回连接错误")
	assert.Contains(t, err.Error(), "重试失败")

	logOutput := syncer.String()

	// 验证关键日志内容
	assert.Contains(t, logOutput, "remote_write 重试", "应包含重试日志")
	assert.Contains(t, logOutput, "remote_write 请求错误", "应包含网络错误日志")
	assert.Contains(t, logOutput, "remote_write 全部重试失败", "应包含最终失败日志")
	assert.Contains(t, logOutput, `"attempt"`, "应包含 attempt 字段")
	assert.Contains(t, logOutput, `"timeseries_count":1`, "应包含 timeseries_count")
	assert.Contains(t, logOutput, `"elapsed"`, "应包含每次尝试的耗时")
	assert.Contains(t, logOutput, `"total_attempts":3`, "应包含总尝试次数")
	assert.Contains(t, logOutput, `"total_elapsed"`, "应包含总耗时")
	assert.Contains(t, logOutput, `"error"`, "应包含具体错误")

	// 验证重试次数递增
	assert.Contains(t, logOutput, `"attempt":2`, "应记录第 2 次尝试")
	assert.Contains(t, logOutput, `"attempt":3`, "应记录第 3 次尝试")

	// 验证错误类型为连接超时/拒绝
	assert.True(t, strings.Contains(logOutput, "connection refused") ||
		strings.Contains(logOutput, "i/o timeout") ||
		strings.Contains(logOutput, "dial tcp"),
		"错误日志应包含网络错误类型")

	// 验证有合理的总耗时(应 >= backoff 总和)
	assert.True(t, totalDuration >= 100*time.Millisecond,
		"总耗时应至少包含退避时间")

	// 验证 Stats
	success, fail := rw.Stats()
	assert.Equal(t, int64(0), success)
	assert.Equal(t, int64(3), fail, "3 次尝试均应计入失败")

	t.Logf("网络超时日志验证通过: 耗时 %v, 错误: %s", totalDuration, err.Error())
}

// TestRemoteWriter_RetryLogging_4xxNoRetry 验证 4xx 客户端错误不会重试
func TestRemoteWriter_RetryLogging_4xxNoRetry(t *testing.T) {
	logger, syncer := newBufferLogger()

	var requestCount int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requestCount, 1)
		w.WriteHeader(http.StatusBadRequest) // 400
	}))
	defer server.Close()

	cfg := DefaultRemoteWriteConfig()
	cfg.Endpoint = server.URL + "/api/v1/write"
	cfg.MaxRetries = 3 // 即使设了重试，4xx 也不应该重试
	cfg.Timeout = 2 * time.Second

	rw, err := NewRemoteWriter(cfg, logger)
	require.NoError(t, err)
	defer rw.Close()

	ctx := context.Background()
	tss := []prompb.TimeSeries{
		{
			Labels:  []prompb.Label{{Name: "__name__", Value: "client_error_test"}},
			Samples: []prompb.Sample{{Value: 1.0, Timestamp: time.Now().UnixMilli()}},
		},
	}

	err = rw.writeBatch(ctx, tss)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "服务端返回 400")

	// 验证 4xx 不重试，只发送一次
	assert.Equal(t, int64(1), atomic.LoadInt64(&requestCount), "4xx 错误不应重试，只发 1 次")

	logOutput := syncer.String()
	assert.Contains(t, logOutput, "remote_write 客户端错误,不再重试", "应包含 4xx 不重试日志")
	assert.Contains(t, logOutput, `"status_code":400`, "应包含 400 状态码")
	assert.NotContains(t, logOutput, `"attempt":2`, "不应有第 2 次尝试")

	t.Logf("4xx 不重试验证通过: 仅发送 %d 次", atomic.LoadInt64(&requestCount))
}

// TestRemoteWriter_RetryLogging_SuccessPath 验证成功路径的日志完整性
func TestRemoteWriter_RetryLogging_SuccessPath(t *testing.T) {
	logger, syncer := newBufferLogger()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := DefaultRemoteWriteConfig()
	cfg.Endpoint = server.URL + "/api/v1/write"
	cfg.MaxRetries = 3
	cfg.BatchSize = 500

	rw, err := NewRemoteWriter(cfg, logger)
	require.NoError(t, err)
	defer rw.Close()

	ctx := context.Background()
	tss := []prompb.TimeSeries{
		{
			Labels: []prompb.Label{
				{Name: "__name__", Value: "success_metric"},
				{Name: "job", Value: "test"},
			},
			Samples: []prompb.Sample{
				{Value: 1.0, Timestamp: time.Now().UnixMilli()},
				{Value: 2.0, Timestamp: time.Now().UnixMilli()},
			},
		},
	}

	err = rw.writeBatch(ctx, tss)
	assert.NoError(t, err)

	logOutput := syncer.String()

	// 验证成功路径的关键日志
	assert.Contains(t, logOutput, "remote_write writeBatch 开始编码", "应包含编码开始日志")
	assert.Contains(t, logOutput, "remote_write 编码完成", "应包含编码完成日志")
	assert.Contains(t, logOutput, "remote_write HTTP 请求已构造", "应包含请求构造日志")
	assert.Contains(t, logOutput, "remote_write 发送成功", "应包含成功日志")
	assert.Contains(t, logOutput, `"timeseries_count":1`, "应包含 timeseries 数量")
	assert.Contains(t, logOutput, `"compressed_size"`, "应包含压缩后大小")
	assert.Contains(t, logOutput, `"elapsed"`, "应包含耗时")

	success, fail := rw.Stats()
	assert.Equal(t, int64(1), success)
	assert.Equal(t, int64(0), fail)

	t.Log("成功路径日志验证通过")
}

// TestRemoteWriter_RetryLogging_BatchSize 验证日志中批次大小准确反映
func TestRemoteWriter_RetryLogging_BatchSize(t *testing.T) {
	logger, syncer := newBufferLogger()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := DefaultRemoteWriteConfig()
	cfg.Endpoint = server.URL + "/api/v1/write"
	cfg.BatchSize = 1000

	rw, err := NewRemoteWriter(cfg, logger)
	require.NoError(t, err)
	defer rw.Close()

	ctx := context.Background()

	// 构造多个 TimeSeries
	batchSize := 5
	tss := make([]prompb.TimeSeries, batchSize)
	for i := 0; i < batchSize; i++ {
		tss[i] = prompb.TimeSeries{
			Labels:  []prompb.Label{{Name: "__name__", Value: fmt.Sprintf("metric_%d", i)}},
			Samples: []prompb.Sample{{Value: float64(i), Timestamp: time.Now().UnixMilli()}},
		}
	}

	err = rw.writeBatch(ctx, tss)
	assert.NoError(t, err)

	logOutput := syncer.String()

	// 验证日志准确记录了批次大小
	assert.Contains(t, logOutput, fmt.Sprintf(`"timeseries_count":%d`, batchSize),
		"应准确记录 timeseries 数量")
	assert.Contains(t, logOutput, fmt.Sprintf(`"compressed_size"`),
		"应包含压缩后大小")

	t.Logf("批次大小日志验证通过: %d 条数据", batchSize)
}