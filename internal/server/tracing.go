// 分布式追踪 (OpenTelemetry Trace Context Propagation)
//
// 本模块实现基于 OpenTelemetry 规范的分布式追踪上下文透传, 支持:
//   1. W3C TraceContext 传播 (traceparent / tracestate 头)
//   2. B3 传播 (b3 / X-B3-TraceId / X-B3-SpanId / X-B3-Sampled 头)
//   3. HTTP 入站 trace context 提取与出站注入
//   4. Span 生命周期管理 (create -> set attributes -> end)
//   5. 与现有日志系统集成 (trace_id / span_id 关联)
//
// Usage:
//   tp := NewTracerProvider("my-service", "0.0.1")
//   tracer := tp.Tracer("http")
//   span := tracer.Start(ctx, "HTTP GET /api/users")
//   defer span.End()
//
// 配置:
//   通过 TracingConfig 启用, 支持选择传播器类型 (tracecontext / b3 / both)
//   支持自定义采样策略与 exporter 注入

package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// TraceContextKey context 中存储 trace context 的 key
type traceCtxKey int

const (
	traceCtxKeyTraceID traceCtxKey = iota + 200
	traceCtxKeySpanID
	traceCtxKeySpan
)

// TracingConfig 追踪配置
type TracingConfig struct {
	Enabled     bool     `json:"enabled"`
	ServiceName string   `json:"service_name"`
	ServiceVer  string   `json:"service_version"`
	// Propagation: "tracecontext" (W3C only) / "b3" (Zipkin only) / "both" (default)
	Propagation string   `json:"propagation"`
	// SamplingRate 采样率 0.0-1.0, 默认 1.0 (全采样)
	SamplingRate float64 `json:"sampling_rate"`
}

// DefaultTracingConfig 默认追踪配置
func DefaultTracingConfig() TracingConfig {
	return TracingConfig{
		Enabled:      true,
		ServiceName:  "go_es",
		ServiceVer:   "0.0.1",
		Propagation:  "both",
		SamplingRate: 1.0,
	}
}

// TraceID W3C Trace ID (32 hex chars)
type TraceID [16]byte

// SpanID W3C Span ID (16 hex chars)
type SpanID [8]byte

// TraceFlags W3C Trace Flags (byte)
type TraceFlags byte

// TraceState W3C Trace State (optional, vendor extensions)
type TraceState string

// TraceContext 完整追踪上下文
type TraceContext struct {
	TraceID    TraceID
	SpanID     SpanID
	ParentSpanID SpanID
	Flags      TraceFlags
	State      TraceState
	// 采样决策
	Sampled    bool
	Remote     bool // 是否来自远程 (true = 从请求头解析)
}

// TraceContextFromHeaders 从 HTTP 请求头提取 TraceContext
// 优先 W3C tracecontext, 回退 B3
func TraceContextFromHeaders(headers http.Header) (TraceContext, bool) {
	// 1. 优先 W3C TraceContext
	if tc, ok := parseW3CTraceContext(headers); ok {
		return tc, true
	}
	// 2. 回退 B3
	if tc, ok := parseB3Context(headers); ok {
		return tc, true
	}
	return TraceContext{}, false
}

// TraceContextToHeaders 将 TraceContext 注入 HTTP 响应头
// 根据配置选择传播格式
func TraceContextToHeaders(h http.Header, tc TraceContext, propagation string) {
	switch propagation {
	case "tracecontext":
		injectW3CTraceContext(h, tc)
	case "b3":
		injectB3Context(h, tc)
	default: // "both" or empty
		injectW3CTraceContext(h, tc)
		injectB3Context(h, tc)
	}
}

// ---------- W3C TraceContext ----------

// parseW3CTraceContext 解析 W3C traceparent 头
// 格式: traceparent: 00-{traceId}-{spanId}-{traceFlags}
func parseW3CTraceContext(headers http.Header) (TraceContext, bool) {
	tp := headers.Get("traceparent")
	if tp == "" {
		return TraceContext{}, false
	}

	parts := strings.Split(tp, "-")
	if len(parts) != 4 {
		return TraceContext{}, false
	}

	var tc TraceContext

	// Version
	if len(parts[0]) != 2 {
		return TraceContext{}, false
	}

	// TraceID (32 hex chars = 16 bytes)
	if len(parts[1]) != 32 {
		return TraceContext{}, false
	}
	traceBytes, err := hex.DecodeString(parts[1])
	if err != nil || len(traceBytes) != 16 {
		return TraceContext{}, false
	}
	copy(tc.TraceID[:], traceBytes)

	// SpanID (16 hex chars = 8 bytes)
	if len(parts[2]) != 16 {
		return TraceContext{}, false
	}
	spanBytes, err := hex.DecodeString(parts[2])
	if err != nil || len(spanBytes) != 8 {
		return TraceContext{}, false
	}
	copy(tc.SpanID[:], spanBytes)

	// TraceFlags (2 hex chars)
	if len(parts[3]) != 2 {
		return TraceContext{}, false
	}
	flags, err := hex.DecodeString(parts[3])
	if err != nil || len(flags) != 1 {
		return TraceContext{}, false
	}
	tc.Flags = TraceFlags(flags[0])

	// TraceState (optional)
	if ts := headers.Get("tracestate"); ts != "" {
		tc.State = TraceState(ts)
	}

	tc.Sampled = (tc.Flags & 0x01) != 0
	tc.Remote = true

	return tc, true
}

// injectW3CTraceContext 注入 W3C traceparent / tracestate 头
func injectW3CTraceContext(h http.Header, tc TraceContext) {
	traceIDHex := hex.EncodeToString(tc.TraceID[:])
	spanIDHex := hex.EncodeToString(tc.SpanID[:])
	flagsHex := fmt.Sprintf("%02x", tc.Flags)

	h.Set("traceparent", fmt.Sprintf("00-%s-%s-%s", traceIDHex, spanIDHex, flagsHex))
	if tc.State != "" {
		h.Set("tracestate", string(tc.State))
	}
}

// ---------- B3 (Zipkin) ----------

// parseB3Context 解析 B3 头
// 支持两种格式:
//   多头: X-B3-TraceId, X-B3-SpanId, X-B3-Sampled
//   单头: b3 = {TraceId}-{SpanId}-{Sampled}-{ParentSpanId}
func parseB3Context(headers http.Header) (TraceContext, bool) {
	var tc TraceContext

	// 尝试单头 b3
	if b3 := headers.Get("b3"); b3 != "" {
		parts := strings.Split(b3, "-")
		if len(parts) >= 2 {
			if traceID, ok := parseB3TraceID(parts[0]); ok {
				tc.TraceID = traceID
			} else {
				return TraceContext{}, false
			}
			if spanID, ok := parseB3SpanID(parts[1]); ok {
				tc.SpanID = spanID
			} else {
				return TraceContext{}, false
			}
			if len(parts) >= 3 {
				tc.Sampled = parts[2] == "1" || parts[2] == "d"
				if parts[2] == "d" {
					tc.Flags = 0x01
				}
			}
			if len(parts) >= 4 {
				if spanID, ok := parseB3SpanID(parts[3]); ok {
					tc.ParentSpanID = spanID
				}
			}
			tc.Remote = true
			return tc, true
		}
	}

	// 多头格式
	traceIDStr := headers.Get("X-B3-TraceId")
	spanIDStr := headers.Get("X-B3-SpanId")

	if traceIDStr == "" || spanIDStr == "" {
		return TraceContext{}, false
	}

	traceID, ok := parseB3TraceID(traceIDStr)
	if !ok {
		return TraceContext{}, false
	}
	spanID, ok := parseB3SpanID(spanIDStr)
	if !ok {
		return TraceContext{}, false
	}

	tc.TraceID = traceID
	tc.SpanID = spanID

	if sampled := headers.Get("X-B3-Sampled"); sampled == "1" {
		tc.Sampled = true
		tc.Flags = 0x01
	}

	if parentSpanID := headers.Get("X-B3-ParentSpanId"); parentSpanID != "" {
		if spanID, ok := parseB3SpanID(parentSpanID); ok {
			tc.ParentSpanID = spanID
		}
	}

	tc.Remote = true
	return tc, true
}

// injectB3Context 注入 B3 头
func injectB3Context(h http.Header, tc TraceContext) {
	traceIDHex := hex.EncodeToString(tc.TraceID[:])
	spanIDHex := hex.EncodeToString(tc.SpanID[:])

	sampled := "0"
	if tc.Sampled {
		sampled = "1"
	}

	b3 := fmt.Sprintf("%s-%s-%s", traceIDHex, spanIDHex, sampled)
	if tc.ParentSpanID != [8]byte{} {
		b3 += "-" + hex.EncodeToString(tc.ParentSpanID[:])
	}
	h.Set("b3", b3)

	// 同时设置多头格式 (最大兼容性)
	h.Set("X-B3-TraceId", traceIDHex)
	h.Set("X-B3-SpanId", spanIDHex)
	h.Set("X-B3-Sampled", sampled)
}

// parseB3TraceID 解析 B3 Trace ID (16 或 32 hex chars)
func parseB3TraceID(s string) (TraceID, bool) {
	s = strings.TrimSpace(s)
	if len(s) == 16 {
		// 短格式: 补 0 到 32 chars
		s = "0000000000000000" + s
	}
	if len(s) != 32 {
		return TraceID{}, false
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return TraceID{}, false
	}
	var tid TraceID
	copy(tid[:], b)
	return tid, true
}

// parseB3SpanID 解析 B3 Span ID (16 hex chars)
func parseB3SpanID(s string) (SpanID, bool) {
	s = strings.TrimSpace(s)
	if len(s) != 16 {
		return SpanID{}, false
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return SpanID{}, false
	}
	var sid SpanID
	copy(sid[:], b)
	return sid, true
}

// ---------- ID 生成 ----------

// generateTraceID 生成随机 128-bit Trace ID
func generateTraceID() TraceID {
	var tid TraceID
	_, _ = rand.Read(tid[:])
	return tid
}

// generateSpanID 生成随机 64-bit Span ID
func generateSpanID() SpanID {
	var sid SpanID
	_, _ = rand.Read(sid[:])
	return sid
}

// ---------- Span 生命周期 ----------

// SpanStatus Span 状态
type SpanStatus string

const (
	SpanStatusOK          SpanStatus = "OK"
	SpanStatusError       SpanStatus = "ERROR"
	SpanStatusNotFound    SpanStatus = "NOT_FOUND"
	SpanStatusInvalidArg  SpanStatus = "INVALID_ARGUMENT"
	SpanStatusUnavailable SpanStatus = "UNAVAILABLE"
)

// SpanKind Span 类型
type SpanKind int

const (
	SpanKindInternal SpanKind = iota
	SpanKindServer
	SpanKindClient
	SpanKindProducer
	SpanKindConsumer
)

// String 返回 SpanKind 的字符串表示
func (k SpanKind) String() string {
	switch k {
	case SpanKindInternal:
		return "internal"
	case SpanKindServer:
		return "server"
	case SpanKindClient:
		return "client"
	case SpanKindProducer:
		return "producer"
	case SpanKindConsumer:
		return "consumer"
	default:
		return "unknown"
	}
}

// Span 追踪 Span (简化实现, 遵循 OpenTelemetry 规范)
type Span struct {
	traceID    TraceID
	spanID     SpanID
	parentID   SpanID
	name       string
	kind       SpanKind
	status     SpanStatus
	startTime  time.Time
	endTime    time.Time
	attributes map[string]string
	events     []SpanEvent
	ended      bool
	mu         sync.Mutex
}

// SpanEvent Span 事件 (关键日志点)
type SpanEvent struct {
	Name      string
	Timestamp time.Time
	Attrs     map[string]string
}

// TraceSpan 对外暴露的 Span 信息 (用于日志关联)
type TraceSpan struct {
	TraceID string
	SpanID  string
	Name    string
	Status  string
}

// SpanFromContext 从 context 中获取 Span
func SpanFromContext(ctx context.Context) *Span {
	if v, ok := ctx.Value(traceCtxKeySpan).(*Span); ok {
		return v
	}
	return nil
}

// TraceInfoFromContext 从 context 中获取 trace 信息 (用于日志)
func TraceInfoFromContext(ctx context.Context) (traceID, spanID string) {
	if v := ctx.Value(traceCtxKeyTraceID); v != nil {
		traceID = v.(string)
	}
	if v := ctx.Value(traceCtxKeySpanID); v != nil {
		spanID = v.(string)
	}
	return
}

// ---------- Tracer ----------

// Tracer 简化的 OpenTelemetry Tracer
type Tracer struct {
	serviceName string
	serviceVer  string
	config      TracingConfig
}

// TracerProvider Tracer 提供者 (管理全局 tracer)
type TracerProvider struct {
	tracers  map[string]*Tracer
	mu       sync.RWMutex
	config   TracingConfig
	exporter SpanExporter
}

// SpanExporter Span 导出接口
type SpanExporter interface {
	ExportTracingSpan(span *Span)
}

// SetSpanExporter 设置 span 导出器
func (tp *TracerProvider) SetSpanExporter(exp SpanExporter) {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	tp.exporter = exp
}

// GetSpanExporter 获取 span 导出器
func (tp *TracerProvider) GetSpanExporter() SpanExporter {
	tp.mu.RLock()
	defer tp.mu.RUnlock()
	return tp.exporter
}

// NewTracerProvider 创建 TracerProvider
func NewTracerProvider(cfg TracingConfig) *TracerProvider {
	if cfg.ServiceName == "" {
		cfg = DefaultTracingConfig()
	}
	if cfg.Propagation == "" {
		cfg.Propagation = "both"
	}
	if cfg.SamplingRate == 0 {
		cfg.SamplingRate = 1.0
	}
	return &TracerProvider{
		tracers: make(map[string]*Tracer),
		config:  cfg,
	}
}

// Tracer 获取或创建 Tracer
func (tp *TracerProvider) Tracer(name string) *Tracer {
	tp.mu.RLock()
	if t, ok := tp.tracers[name]; ok {
		tp.mu.RUnlock()
		return t
	}
	tp.mu.RUnlock()

	tp.mu.Lock()
	defer tp.mu.Unlock()
	if t, ok := tp.tracers[name]; ok {
		return t
	}
	t := &Tracer{
		serviceName: tp.config.ServiceName,
		serviceVer:  tp.config.ServiceVer,
		config:      tp.config,
	}
	tp.tracers[name] = t
	return t
}

// IsEnabled 是否启用追踪
func (tp *TracerProvider) IsEnabled() bool {
	return tp.config.Enabled
}

// Config 获取配置
func (tp *TracerProvider) Config() TracingConfig {
	return tp.config
}

// UpdateConfig 热更新配置
func (tp *TracerProvider) UpdateConfig(cfg TracingConfig) {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	tp.config = cfg
	for _, t := range tp.tracers {
		t.config = cfg
	}
}

// StartSpan 创建新 Span
func (t *Tracer) StartSpan(ctx context.Context, name string, opts ...SpanStartOption) (context.Context, *Span) {
	if !t.config.Enabled || !t.shouldSample() {
		return ctx, nil
	}

	span := &Span{
		name:       name,
		kind:       SpanKindServer,
		status:     SpanStatusOK,
		startTime:  time.Now(),
		attributes: make(map[string]string),
	}

	// 尝试从 context 提取远程 TraceContext
	if tc := traceContextFromContext(ctx); tc != nil {
		span.traceID = tc.TraceID
		span.parentID = tc.SpanID
		// 采样决策继承
		if tc.Sampled {
			span.status = SpanStatusOK
		}
	} else {
		span.traceID = generateTraceID()
	}
	span.spanID = generateSpanID()

	// 应用选项
	for _, opt := range opts {
		opt(span)
	}

	// 将 span 信息注入 context
	traceIDHex := hex.EncodeToString(span.traceID[:])
	spanIDHex := hex.EncodeToString(span.spanID[:])
	ctx = context.WithValue(ctx, traceCtxKeyTraceID, traceIDHex)
	ctx = context.WithValue(ctx, traceCtxKeySpanID, spanIDHex)
	ctx = context.WithValue(ctx, traceCtxKeySpan, span)

	return ctx, span
}

// shouldSample 是否应该采样
func (t *Tracer) shouldSample() bool {
	if t.config.SamplingRate >= 1.0 {
		return true
	}
	if t.config.SamplingRate <= 0 {
		return false
	}
	// 简化: 使用时间戳的随机位作为采样判断
	return float64(time.Now().UnixNano()%1000)/1000.0 < t.config.SamplingRate
}

// ---------- Span 选项 ----------

// SpanStartOption Span 启动选项
type SpanStartOption func(*Span)

// WithSpanKind 设置 Span 类型
func WithSpanKind(kind SpanKind) SpanStartOption {
	return func(s *Span) {
		s.kind = kind
	}
}

// WithSpanAttribute 添加 Span 属性
func WithSpanAttribute(key, value string) SpanStartOption {
	return func(s *Span) {
		s.attributes[key] = value
	}
}

// WithSpanAttributes 批量添加 Span 属性
func WithSpanAttributes(attrs map[string]string) SpanStartOption {
	return func(s *Span) {
		for k, v := range attrs {
			s.attributes[k] = v
		}
	}
}

// WithSpanRemoteContext 设置远程 TraceContext (从请求头解析)
func WithSpanRemoteContext(tc TraceContext) SpanStartOption {
	return func(s *Span) {
		if tc.Remote {
			s.traceID = tc.TraceID
			s.parentID = tc.SpanID
		}
	}
}

// WithSpanStatus 设置 Span 状态
func WithSpanStatus(status SpanStatus) SpanStartOption {
	return func(s *Span) {
		s.status = status
	}
}

// ---------- Span 方法 ----------

// SetAttribute 设置 Span 属性
func (s *Span) SetAttribute(key, value string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attributes[key] = value
}

// SetStatus 设置 Span 状态
func (s *Span) SetStatus(status SpanStatus) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
}

// AddEvent 添加 Span 事件
func (s *Span) AddEvent(name string, attrs map[string]string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, SpanEvent{
		Name:      name,
		Timestamp: time.Now(),
		Attrs:     attrs,
	})
}

// End 结束 Span
func (s *Span) End() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	s.ended = true
	s.endTime = time.Now()

	// 如果设置了导出器, 导出 span
	if tp := tracerProviderFromSpan(s); tp != nil {
		if exp := tp.GetSpanExporter(); exp != nil {
			go exp.ExportTracingSpan(s)
		}
	}
}

// tracerProviderFromSpan 简化: 通过全局变量获取 TracerProvider
// 实际集成由 Server.initExporters 中 tp.SetSpanExporter 完成
var globalTracerProvider *TracerProvider

// SetGlobalTracerProvider 设置全局 TracerProvider
func SetGlobalTracerProvider(tp *TracerProvider) {
	globalTracerProvider = tp
}

func tracerProviderFromSpan(s *Span) *TracerProvider {
	return globalTracerProvider
}

// TraceIDString 获取 Trace ID 的十六进制字符串
func (s *Span) TraceIDString() string {
	if s == nil {
		return ""
	}
	return hex.EncodeToString(s.traceID[:])
}

// SpanIDString 获取 Span ID 的十六进制字符串
func (s *Span) SpanIDString() string {
	if s == nil {
		return ""
	}
	return hex.EncodeToString(s.spanID[:])
}

// TraceSpan 返回简化的 TraceSpan 信息 (用于日志)
func (s *Span) TraceSpan() TraceSpan {
	if s == nil {
		return TraceSpan{}
	}
	return TraceSpan{
		TraceID: s.TraceIDString(),
		SpanID:  s.SpanIDString(),
		Name:    s.name,
		Status:  string(s.status),
	}
}

// Duration Span 持续时间
func (s *Span) Duration() time.Duration {
	if s == nil || !s.ended {
		return 0
	}
	return s.endTime.Sub(s.startTime)
}

// GetAttribute 获取 Span 属性
func (s *Span) GetAttribute(key string) (string, bool) {
	if s == nil {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.attributes[key]
	return v, ok
}

// ---------- Context 辅助 ----------

// NewTraceContext 创建新的 TraceContext
func NewTraceContext() TraceContext {
	return TraceContext{
		TraceID: generateTraceID(),
		SpanID:  generateSpanID(),
		Flags:   TraceFlags(0x01), // sampled
		Sampled: true,
	}
}

// NewRootSpanContext 创建根 Span 的 Context
func NewRootSpanContext(ctx context.Context) (context.Context, TraceContext) {
	tc := NewTraceContext()
	// 注入到 context
	ctx = context.WithValue(ctx, traceCtxKeyTraceID, hex.EncodeToString(tc.TraceID[:]))
	ctx = context.WithValue(ctx, traceCtxKeySpanID, hex.EncodeToString(tc.SpanID[:]))
	return ctx, tc
}

// traceContextFromContext 从 context 中提取 TraceContext
func traceContextFromContext(ctx context.Context) *TraceContext {
	if v := ctx.Value(traceCtxKeyTraceID); v != nil {
		if v2 := ctx.Value(traceCtxKeySpanID); v2 != nil {
			tid, err := hex.DecodeString(v.(string))
			if err == nil && len(tid) == 16 {
				sid, err2 := hex.DecodeString(v2.(string))
				if err2 == nil && len(sid) == 8 {
					tc := &TraceContext{
						Sampled: true,
						Flags:   0x01,
					}
					copy(tc.TraceID[:], tid)
					copy(tc.SpanID[:], sid)
					return tc
				}
			}
		}
	}
	return nil
}

// SpanFromTraceContext 从 TraceContext 创建子 Span
func SpanFromTraceContext(tc TraceContext) Span {
	return Span{
		traceID:  tc.TraceID,
		spanID:   tc.SpanID,
		parentID: tc.ParentSpanID,
		status:   SpanStatusOK,
	}
}

// TraceContextToSpan 将 TraceContext 转换为 Span
func TraceContextToSpan(tc TraceContext) *Span {
	return &Span{
		traceID:  tc.TraceID,
		spanID:   tc.SpanID,
		parentID: tc.ParentSpanID,
		status:   SpanStatusOK,
		ended:    true, // 来自远程, 标记为已结束
		attributes: map[string]string{
			"remote": "true",
		},
	}
}

// ---------- HTTP 辅助 ----------

// PropagateTraceContext 从请求头提取并注入新的响应头 (用于服务间透传)
func PropagateTraceContext(r *http.Request) (TraceContext, bool) {
	return TraceContextFromHeaders(r.Header)
}

// InjectTraceContext 注入 TraceContext 到 HTTP 请求 (用于出站调用)
func InjectTraceContext(req *http.Request, tc TraceContext, propagation string) {
	TraceContextToHeaders(req.Header, tc, propagation)
}

// ExtractTraceContext 从 HTTP 请求头提取 TraceContext
func ExtractTraceContext(r *http.Request) (TraceContext, bool) {
	return TraceContextFromHeaders(r.Header)
}