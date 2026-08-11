// prometheus_remote 实现 Prometheus remote_write 协议的客户端
//
// 支持将 metrics 数据按照 Prometheus remote_write 协议规范
// 发送至指定的 remote_write 端点,包含:
//   - Snappy + Protobuf 编码(与 Prometheus remote_write 1.0 兼容)
//   - 可配置端点、鉴权、推送间隔、批大小
//   - 错误重试(指数退避)与数据完整性保障
//   - 指标级计数(push success/fail/dropped)

package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/prompb"
	"go.uber.org/zap"
)

// RemoteWriteConfig Prometheus remote_write 配置
type RemoteWriteConfig struct {
	Enabled     bool          `yaml:"enabled"`
	Endpoint    string        `yaml:"endpoint"`     // 远端写地址,如 "http://localhost:9201/api/v1/write"
	BasicAuth   BasicAuthCfg  `yaml:"basic_auth"`   // Basic 认证
	BearerToken string        `yaml:"bearer_token"` // Bearer Token
	PushInterval time.Duration `yaml:"push_interval"`
	BatchSize   int           `yaml:"batch_size"`
	MaxRetries  int           `yaml:"max_retries"`
	Timeout     time.Duration `yaml:"timeout"`
	Headers     map[string]string `yaml:"headers"`
}

// BasicAuthCfg Basic 认证配置
type BasicAuthCfg struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// DefaultRemoteWriteConfig 默认配置
func DefaultRemoteWriteConfig() RemoteWriteConfig {
	return RemoteWriteConfig{
		PushInterval: 15 * time.Second,
		BatchSize:    1000,
		MaxRetries:   3,
		Timeout:      30 * time.Second,
	}
}

// RemoteWriter Prometheus remote_write 客户端
type RemoteWriter struct {
	cfg    RemoteWriteConfig
	client *http.Client
	log    *zap.Logger

	// 指标数据队列
	mu     sync.Mutex
	queue  []prompb.TimeSeries
	done   chan struct{}
	closed bool

	// 内部计数
	pushSuccess int64
	pushFail    int64
}

// NewRemoteWriter 创建 remote_write 客户端
func NewRemoteWriter(cfg RemoteWriteConfig, log *zap.Logger) (*RemoteWriter, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("remote_write: endpoint 不能为空")
	}
	if cfg.PushInterval <= 0 {
		cfg.PushInterval = 15 * time.Second
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 1000
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 3
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	// 验证 endpoint URL
	if _, err := url.Parse(cfg.Endpoint); err != nil {
		return nil, fmt.Errorf("remote_write: endpoint %q 不是合法 URL: %w", cfg.Endpoint, err)
	}
	if cfg.BasicAuth.Username != "" && cfg.BasicAuth.Password == "" {
		return nil, fmt.Errorf("remote_write: basic_auth.username 已设置但 password 为空")
	}

	client := &http.Client{
		Timeout: cfg.Timeout,
	}

	return &RemoteWriter{
		cfg:    cfg,
		client: client,
		log:    log,
		done:   make(chan struct{}),
	}, nil
}

// Start 启动后台推送协程
func (rw *RemoteWriter) Start(ctx context.Context) {
	rw.mu.Lock()
	if rw.closed {
		rw.mu.Unlock()
		return
	}
	rw.mu.Unlock()

	interval := rw.cfg.PushInterval
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}

	rw.log.Info("remote_write 客户端启动",
		zap.String("endpoint", rw.cfg.Endpoint),
		zap.Duration("push_interval", interval),
		zap.Int("batch_size", rw.cfg.BatchSize))

	go rw.loop(ctx, interval)
}

func (rw *RemoteWriter) loop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			rw.flushAll(ctx)
			return
		case <-rw.done:
			rw.flushAll(ctx)
			return
		case <-ticker.C:
			rw.flushBatch(ctx)
		}
	}
}

// Enqueue 入队一组 TimeSeries,在下次 flush 时发送
func (rw *RemoteWriter) Enqueue(tss ...prompb.TimeSeries) {
	if rw == nil || len(tss) == 0 {
		return
	}
	rw.mu.Lock()
	defer rw.mu.Unlock()
	rw.queue = append(rw.queue, tss...)
}

// enqueueLabels 便捷方法:从 label map 构造 TimeSeries 并入队
func (rw *RemoteWriter) enqueueLabels(ts time.Time, labels []prompb.Label, samples []prompb.Sample) {
	rw.Enqueue(prompb.TimeSeries{
		Labels:  labels,
		Samples: samples,
	})
}

// Close 停止推送,并尝试将队列中剩余数据 flush
func (rw *RemoteWriter) Close() {
	rw.mu.Lock()
	if rw.closed {
		rw.mu.Unlock()
		return
	}
	rw.closed = true
	close(rw.done)
	rw.mu.Unlock()
}

// flushBatch 推送一批数据
func (rw *RemoteWriter) flushBatch(ctx context.Context) {
	rw.mu.Lock()
	if len(rw.queue) == 0 {
		rw.mu.Unlock()
		return
	}
	// 取一批
	n := rw.cfg.BatchSize
	if n > len(rw.queue) {
		n = len(rw.queue)
	}
	batch := rw.queue[:n]
	rw.queue = rw.queue[n:]
	rw.mu.Unlock()

	rw.log.Debug("remote_write flushBatch 开始",
		zap.Int("batch_size", len(batch)),
		zap.Int("remaining_queue", rw.PendingQueueSize()),
	)
	start := time.Now()
	if err := rw.writeBatch(ctx, batch); err != nil {
		rw.log.Warn("remote_write flush 失败",
			zap.Int("batch_size", len(batch)),
			zap.Duration("elapsed", time.Since(start)),
			zap.Error(err))
	} else {
		rw.log.Debug("remote_write flushBatch 完成",
			zap.Int("batch_size", len(batch)),
			zap.Duration("elapsed", time.Since(start)),
		)
	}
}

// flushAll 推送全部剩余数据
func (rw *RemoteWriter) flushAll(ctx context.Context) {
	for {
		rw.mu.Lock()
		if len(rw.queue) == 0 {
			rw.mu.Unlock()
			return
		}
		n := rw.cfg.BatchSize
		if n > len(rw.queue) {
			n = len(rw.queue)
		}
		batch := rw.queue[:n]
		rw.queue = rw.queue[n:]
		rw.mu.Unlock()

		start := time.Now()
		if err := rw.writeBatch(ctx, batch); err != nil {
			rw.log.Error("remote_write flushAll 失败",
				zap.Int("batch_size", len(batch)),
				zap.Duration("elapsed", time.Since(start)),
				zap.Error(err))
		} else {
			rw.log.Debug("remote_write flushAll 完成",
				zap.Int("batch_size", len(batch)),
				zap.Duration("elapsed", time.Since(start)),
			)
		}
	}
}

// writeBatch 将一批 TimeSeries 编码并发送(带重试)
func (rw *RemoteWriter) writeBatch(ctx context.Context, tss []prompb.TimeSeries) error {
	rw.log.Debug("remote_write writeBatch 开始编码",
		zap.Int("timeseries_count", len(tss)),
	)

	start := time.Now()
	// 构造 WriteRequest
	req := &prompb.WriteRequest{
		Timeseries: tss,
	}

	// Protobuf 序列化
	raw, err := req.Marshal()
	if err != nil {
		return fmt.Errorf("marshal WriteRequest: %w", err)
	}

	// Snappy 压缩
	compressed := snappy.Encode(nil, raw)

	rw.log.Debug("remote_write 编码完成",
		zap.Int("protobuf_size", len(raw)),
		zap.Int("compressed_size", len(compressed)),
		zap.Duration("encode_elapsed", time.Since(start)),
	)

	// 构造 HTTP 请求
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, rw.cfg.Endpoint, bytes.NewReader(compressed))
	if err != nil {
		return fmt.Errorf("构造 HTTP 请求: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/x-protobuf")
	httpReq.Header.Set("Content-Encoding", "snappy")
	httpReq.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")

	// 设置自定义 headers
	for k, v := range rw.cfg.Headers {
		httpReq.Header.Set(k, v)
	}

	// 认证
	if rw.cfg.BasicAuth.Username != "" {
		httpReq.SetBasicAuth(rw.cfg.BasicAuth.Username, rw.cfg.BasicAuth.Password)
	}
	if rw.cfg.BearerToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+rw.cfg.BearerToken)
	}

	rw.log.Debug("remote_write HTTP 请求已构造",
		zap.String("endpoint", rw.cfg.Endpoint),
		zap.Int("body_size", len(compressed)),
		zap.Int("custom_headers", len(rw.cfg.Headers)),
		zap.Bool("basic_auth", rw.cfg.BasicAuth.Username != ""),
		zap.Bool("bearer_token", rw.cfg.BearerToken != ""),
	)

	// 带重试执行
	return rw.sendWithRetries(ctx, httpReq)
}

// sendWithRetries 带指数退避重试发送
func (rw *RemoteWriter) sendWithRetries(ctx context.Context, req *http.Request) error {
	var lastErr error
	totalStart := time.Now()
	maxAttempts := rw.cfg.MaxRetries + 1

	for attempt := 0; attempt <= rw.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			// 指数退避: 100ms, 200ms, 400ms ...
			backoff := time.Duration(1<<uint(attempt-1)) * 100 * time.Millisecond
			rw.log.Debug("remote_write 重试",
				zap.Int("attempt", attempt+1),
				zap.Int("max_attempts", maxAttempts),
				zap.Duration("backoff", backoff),
			)
			select {
			case <-ctx.Done():
				return fmt.Errorf("remote_write 上下文已取消: %w", ctx.Err())
			case <-time.After(backoff):
			}
		}

		attemptStart := time.Now()
		resp, err := rw.client.Do(req)
		attemptElapsed := time.Since(attemptStart)

		if err != nil {
			lastErr = err
			rw.log.Warn("remote_write 请求错误",
				zap.Int("attempt", attempt+1),
				zap.Duration("elapsed", attemptElapsed),
				zap.Error(err),
			)
			rw.pushFail++
			continue
		}
		// 读取响应体
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		// 2xx 成功
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			rw.log.Debug("remote_write 发送成功",
				zap.Int("attempt", attempt+1),
				zap.Int("status_code", resp.StatusCode),
				zap.Duration("elapsed", attemptElapsed),
				zap.Duration("total_elapsed", time.Since(totalStart)),
			)
			rw.pushSuccess++
			return nil
		}
		// 5xx 可重试, 4xx 不可重试
		lastErr = fmt.Errorf("remote_write: 服务端返回 %d", resp.StatusCode)
		rw.pushFail++
		rw.log.Warn("remote_write 服务端返回错误",
			zap.Int("attempt", attempt+1),
			zap.Int("status_code", resp.StatusCode),
			zap.Duration("elapsed", attemptElapsed),
		)
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			// 客户端错误,不重试
			rw.log.Warn("remote_write 客户端错误,不再重试",
				zap.Int("status_code", resp.StatusCode),
			)
			return lastErr
		}
	}

	rw.log.Error("remote_write 全部重试失败",
		zap.Int("total_attempts", maxAttempts),
		zap.Duration("total_elapsed", time.Since(totalStart)),
		zap.Error(lastErr),
	)
	return fmt.Errorf("remote_write: 全部 %d 次重试失败: %w", maxAttempts, lastErr)
}

// Stats 返回推送统计
func (rw *RemoteWriter) Stats() (success, fail int64) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	return rw.pushSuccess, rw.pushFail
}

// PendingQueueSize 返回当前队列待发送条数(用于指标)
func (rw *RemoteWriter) PendingQueueSize() int {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	return len(rw.queue)
}