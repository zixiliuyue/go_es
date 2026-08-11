package server

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zixiliuyue/go_es/internal/search"
	"github.com/zixiliuyue/go_es/internal/storage"
	"go.uber.org/zap"
)

// ---------- W3C TraceContext 解析测试 ----------

func TestW3CTraceContext_Parse(t *testing.T) {
	tests := []struct {
		name    string
		header  string
		wantOK  bool
		wantTID string
		wantSID string
		wantSampled bool
	}{
		{
			name:    "valid sampled",
			header:  "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
			wantOK:  true,
			wantTID: "0af7651916cd43dd8448eb211c80319c",
			wantSID: "b7ad6b7169203331",
			wantSampled: true,
		},
		{
			name:    "valid not sampled",
			header:  "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-00",
			wantOK:  true,
			wantTID: "0af7651916cd43dd8448eb211c80319c",
			wantSID: "b7ad6b7169203331",
			wantSampled: false,
		},
		{
			name:   "invalid format - missing parts",
			header: "00-0af7651916cd43dd8448eb211c80319c",
			wantOK: false,
		},
		{
			name:   "invalid - bad traceid length",
			header: "00-0af7651916cd43dd8448eb211c80319-b7ad6b7169203331-01",
			wantOK: false,
		},
		{
			name:   "invalid - bad spanid length",
			header: "00-0af7651916cd43dd8448eb211c80319c-b7ad6b716920333-01",
			wantOK: false,
		},
		{
			name:   "empty",
			header: "",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			if tt.header != "" {
				h.Set("traceparent", tt.header)
			}
			tc, ok := parseW3CTraceContext(h)
			if ok != tt.wantOK {
				t.Fatalf("ok=%v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			gotTID := hex.EncodeToString(tc.TraceID[:])
			if gotTID != tt.wantTID {
				t.Errorf("TraceID=%s, want %s", gotTID, tt.wantTID)
			}
			gotSID := hex.EncodeToString(tc.SpanID[:])
			if gotSID != tt.wantSID {
				t.Errorf("SpanID=%s, want %s", gotSID, tt.wantSID)
			}
			if tc.Sampled != tt.wantSampled {
				t.Errorf("Sampled=%v, want %v", tc.Sampled, tt.wantSampled)
			}
			if !tc.Remote {
				t.Error("expected Remote=true")
			}
		})
	}
}

func TestW3CTraceContext_ParseWithState(t *testing.T) {
	h := http.Header{}
	h.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	h.Set("tracestate", "congo=t61rcWkgMzE,rojo=00f067aa0ba9")

	tc, ok := parseW3CTraceContext(h)
	if !ok {
		t.Fatal("expected valid context")
	}
	if tc.State != "congo=t61rcWkgMzE,rojo=00f067aa0ba9" {
		t.Errorf("State=%s", tc.State)
	}
}

func TestW3CTraceContext_Inject(t *testing.T) {
	tc := TraceContext{
		TraceID:  TraceID{0x0a, 0xf7, 0x65, 0x19, 0x16, 0xcd, 0x43, 0xdd, 0x84, 0x48, 0xeb, 0x21, 0x1c, 0x80, 0x31, 0x9c},
		SpanID:   SpanID{0xb7, 0xad, 0x6b, 0x71, 0x69, 0x20, 0x33, 0x31},
		Flags:    0x01,
		Sampled:  true,
		State:    "congo=t61rcWkgMzE",
	}

	h := http.Header{}
	injectW3CTraceContext(h, tc)

	tp := h.Get("traceparent")
	wantTP := "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	if tp != wantTP {
		t.Errorf("traceparent=%s, want %s", tp, wantTP)
	}

	ts := h.Get("tracestate")
	if ts != "congo=t61rcWkgMzE" {
		t.Errorf("tracestate=%s", ts)
	}
}

func TestW3CTraceContext_Roundtrip(t *testing.T) {
	original := TraceContext{
		TraceID:  generateTraceID(),
		SpanID:   generateSpanID(),
		Flags:    0x01,
		Sampled:  true,
		State:    "vendor1=value1",
	}

	h := http.Header{}
	injectW3CTraceContext(h, original)
	parsed, ok := parseW3CTraceContext(h)
	if !ok {
		t.Fatal("roundtrip parse failed")
	}
	if parsed.TraceID != original.TraceID {
		t.Error("TraceID mismatch")
	}
	if parsed.SpanID != original.SpanID {
		t.Error("SpanID mismatch")
	}
	if parsed.Flags != original.Flags {
		t.Error("Flags mismatch")
	}
	if parsed.State != original.State {
		t.Error("State mismatch")
	}
}

// ---------- B3 解析测试 ----------

func TestB3Context_MultiHeader(t *testing.T) {
	h := http.Header{}
	h.Set("X-B3-TraceId", "0af7651916cd43dd8448eb211c80319c")
	h.Set("X-B3-SpanId", "b7ad6b7169203331")
	h.Set("X-B3-Sampled", "1")
	h.Set("X-B3-ParentSpanId", "00f067aa0ba90001")

	tc, ok := parseB3Context(h)
	if !ok {
		t.Fatal("expected valid B3 context")
	}
	gotTID := hex.EncodeToString(tc.TraceID[:])
	if gotTID != "0af7651916cd43dd8448eb211c80319c" {
		t.Errorf("TraceID=%s", gotTID)
	}
	if !tc.Sampled {
		t.Error("expected sampled")
	}
	gotPSID := hex.EncodeToString(tc.ParentSpanID[:])
	if gotPSID != "00f067aa0ba90001" {
		t.Errorf("ParentSpanID=%s", gotPSID)
	}
}

func TestB3Context_SingleHeader(t *testing.T) {
	h := http.Header{}
	h.Set("b3", "0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-1-00f067aa0ba90001")

	tc, ok := parseB3Context(h)
	if !ok {
		t.Fatal("expected valid B3 context")
	}
	gotTID := hex.EncodeToString(tc.TraceID[:])
	if gotTID != "0af7651916cd43dd8448eb211c80319c" {
		t.Errorf("TraceID=%s", gotTID)
	}
	gotSID := hex.EncodeToString(tc.SpanID[:])
	if gotSID != "b7ad6b7169203331" {
		t.Errorf("SpanID=%s", gotSID)
	}
	if !tc.Sampled {
		t.Error("expected sampled")
	}
}

func TestB3Context_DebugMode(t *testing.T) {
	h := http.Header{}
	h.Set("b3", "0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-d")

	tc, ok := parseB3Context(h)
	if !ok {
		t.Fatal("expected valid B3 context")
	}
	if !tc.Sampled {
		t.Error("debug mode should set sampled")
	}
	if tc.Flags != 0x01 {
		t.Error("debug mode should set flags")
	}
}

func TestB3Context_Inject(t *testing.T) {
	tc := TraceContext{
		TraceID:  TraceID{0x0a, 0xf7, 0x65, 0x19, 0x16, 0xcd, 0x43, 0xdd, 0x84, 0x48, 0xeb, 0x21, 0x1c, 0x80, 0x31, 0x9c},
		SpanID:   SpanID{0xb7, 0xad, 0x6b, 0x71, 0x69, 0x20, 0x33, 0x31},
		Sampled:  true,
	}

	h := http.Header{}
	injectB3Context(h, tc)

	b3 := h.Get("b3")
	wantB3 := "0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-1"
	if b3 != wantB3 {
		t.Errorf("b3=%s, want %s", b3, wantB3)
	}

	if h.Get("X-B3-TraceId") != "0af7651916cd43dd8448eb211c80319c" {
		t.Error("X-B3-TraceId mismatch")
	}
	if h.Get("X-B3-SpanId") != "b7ad6b7169203331" {
		t.Error("X-B3-SpanId mismatch")
	}
	if h.Get("X-B3-Sampled") != "1" {
		t.Error("X-B3-Sampled mismatch")
	}
}

func TestB3Context_InjectWithParent(t *testing.T) {
	tc := TraceContext{
		TraceID:      TraceID{0x0a, 0xf7, 0x65, 0x19, 0x16, 0xcd, 0x43, 0xdd, 0x84, 0x48, 0xeb, 0x21, 0x1c, 0x80, 0x31, 0x9c},
		SpanID:       SpanID{0xb7, 0xad, 0x6b, 0x71, 0x69, 0x20, 0x33, 0x31},
		ParentSpanID: SpanID{0x00, 0xf0, 0x67, 0xaa, 0x0b, 0xa9, 0x00, 0x01},
		Sampled:      true,
	}

	h := http.Header{}
	injectB3Context(h, tc)

	b3 := h.Get("b3")
	wantB3 := "0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-1-00f067aa0ba90001"
	if b3 != wantB3 {
		t.Errorf("b3=%s, want %s", b3, wantB3)
	}
}

// ---------- 优先 W3C 回退 B3 ----------

func TestTraceContextFromHeaders_W3CPriority(t *testing.T) {
	h := http.Header{}
	h.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	h.Set("b3", "wrong-traceid-wrongspanid-1")

	tc, ok := TraceContextFromHeaders(h)
	if !ok {
		t.Fatal("expected valid context")
	}
	gotTID := hex.EncodeToString(tc.TraceID[:])
	if gotTID != "0af7651916cd43dd8448eb211c80319c" {
		t.Errorf("should prefer W3C TraceID, got %s", gotTID)
	}
}

func TestTraceContextFromHeaders_B3Fallback(t *testing.T) {
	h := http.Header{}
	h.Set("X-B3-TraceId", "0af7651916cd43dd8448eb211c80319c")
	h.Set("X-B3-SpanId", "b7ad6b7169203331")

	tc, ok := TraceContextFromHeaders(h)
	if !ok {
		t.Fatal("expected valid B3 fallback")
	}
	gotTID := hex.EncodeToString(tc.TraceID[:])
	if gotTID != "0af7651916cd43dd8448eb211c80319c" {
		t.Errorf("TraceID=%s", gotTID)
	}
}

func TestTraceContextFromHeaders_Empty(t *testing.T) {
	h := http.Header{}
	_, ok := TraceContextFromHeaders(h)
	if ok {
		t.Error("should not find context in empty headers")
	}
}

// ---------- Propagation 注入策略 ----------

func TestTraceContextToHeaders(t *testing.T) {
	tc := TraceContext{
		TraceID:  generateTraceID(),
		SpanID:   generateSpanID(),
		Flags:    0x01,
		Sampled:  true,
	}

	tests := []struct {
		name           string
		propagation    string
		wantW3C        bool
		wantB3         bool
	}{
		{"tracecontext only", "tracecontext", true, false},
		{"b3 only", "b3", false, true},
		{"both", "both", true, true},
		{"default empty", "", true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			TraceContextToHeaders(h, tc, tt.propagation)

			hasW3C := h.Get("traceparent") != ""
			hasB3 := h.Get("b3") != ""

			if hasW3C != tt.wantW3C {
				t.Errorf("W3C present=%v, want %v", hasW3C, tt.wantW3C)
			}
			if hasB3 != tt.wantB3 {
				t.Errorf("B3 present=%v, want %v", hasB3, tt.wantB3)
			}
		})
	}
}

// ---------- TracerProvider + Tracer ----------

func TestTracerProvider_NewAndGetTracer(t *testing.T) {
	tp := NewTracerProvider(TracingConfig{
		Enabled:     true,
		ServiceName: "test-service",
		ServiceVer:  "1.0.0",
		Propagation: "both",
	})

	if !tp.IsEnabled() {
		t.Error("should be enabled")
	}

	tracer := tp.Tracer("http")
	if tracer == nil {
		t.Fatal("expected non-nil tracer")
	}

	// 再次获取应该返回同一实例
	tracer2 := tp.Tracer("http")
	if tracer != tracer2 {
		t.Error("expected same tracer instance")
	}
}

func TestTracerProvider_DefaultConfig(t *testing.T) {
	tp := NewTracerProvider(TracingConfig{})
	if tp.Config().ServiceName == "" {
		t.Error("should fill default service name")
	}
	if tp.Config().Propagation == "" {
		t.Error("should fill default propagation")
	}
}

func TestTracerProvider_UpdateConfig(t *testing.T) {
	tp := NewTracerProvider(TracingConfig{
		Enabled:     true,
		ServiceName: "test",
		Propagation: "both",
	})

	tracer := tp.Tracer("http")
	tracer.StartSpan(context.Background(), "span1")

	tp.UpdateConfig(TracingConfig{
		Enabled:     false,
		ServiceName: "updated",
		Propagation: "tracecontext",
	})

	if tp.Config().Enabled {
		t.Error("should be disabled after update")
	}

	// tracer 配置也应更新
	if tracer.config.Propagation != "tracecontext" {
		t.Error("tracer config should be updated")
	}
}

// ---------- Span 创建与生命周期 ----------

func TestSpan_Lifecycle(t *testing.T) {
	tp := NewTracerProvider(TracingConfig{Enabled: true})
	tracer := tp.Tracer("test")

	ctx := context.Background()
	ctx, span := tracer.StartSpan(ctx, "test-operation",
		WithSpanAttribute("key1", "value1"),
	)

	if span == nil {
		t.Fatal("expected non-nil span")
	}

	traceID, spanID := TraceInfoFromContext(ctx)
	if traceID == "" || spanID == "" {
		t.Error("expected trace info in context")
	}

	if !strings.Contains(traceID, "") || len(traceID) != 32 {
		t.Errorf("TraceID length should be 32, got %d: %s", len(traceID), traceID)
	}
	if len(spanID) != 16 {
		t.Errorf("SpanID length should be 16, got %d: %s", len(spanID), spanID)
	}

	got, _ := span.GetAttribute("key1")
	if got != "value1" {
		t.Error("attribute not set")
	}

	span.SetAttribute("key2", "value2")
	got2, _ := span.GetAttribute("key2")
	if got2 != "value2" {
		t.Error("attribute not set via SetAttribute")
	}

	span.AddEvent("my-event", map[string]string{"msg": "hello"})
	span.SetStatus(SpanStatusError)

	span.End()

	if span.Duration() <= 0 {
		t.Error("duration should be positive")
	}

	ts := span.TraceSpan()
	if ts.TraceID != traceID {
		t.Error("TraceSpan.TraceID mismatch")
	}
	if ts.Name != "test-operation" {
		t.Error("TraceSpan.Name mismatch")
	}
	if ts.Status != "ERROR" {
		t.Error("TraceSpan.Status should be ERROR")
	}
}

func TestSpan_NilSafety(t *testing.T) {
	var span *Span
	span.SetAttribute("k", "v") // should not panic
	span.SetStatus(SpanStatusOK)
	span.AddEvent("e", nil)
	span.End()
	_ = span.TraceIDString()
	_ = span.SpanIDString()
	_ = span.TraceSpan()
	_ = span.Duration()
}

// ---------- 远程 TraceContext 继承 ----------

func TestSpan_FromRemoteContext(t *testing.T) {
	tp := NewTracerProvider(TracingConfig{Enabled: true})
	tracer := tp.Tracer("http")

	remoteTC := TraceContext{
		TraceID:  TraceID{0x0a, 0xf7, 0x65, 0x19, 0x16, 0xcd, 0x43, 0xdd, 0x84, 0x48, 0xeb, 0x21, 0x1c, 0x80, 0x31, 0x9c},
		SpanID:   SpanID{0xb7, 0xad, 0x6b, 0x71, 0x69, 0x20, 0x33, 0x31},
		Flags:    0x01,
		Sampled:  true,
		Remote:   true,
	}

	ctx := context.Background()
	ctx, span := tracer.StartSpan(ctx, "child-span", WithSpanRemoteContext(remoteTC))
	if span == nil {
		t.Fatal("expected non-nil span")
	}

	// 子 Span 应继承父 TraceID
	traceIDStr := span.TraceIDString()
	if traceIDStr != "0af7651916cd43dd8448eb211c80319c" {
		t.Errorf("child TraceID=%s, should inherit parent's", traceIDStr)
	}

	// 子 Span 应有自己的 SpanID (不同)
	spanIDStr := span.SpanIDString()
	if spanIDStr == "b7ad6b7169203331" {
		t.Error("child SpanID should be different from parent's")
	}
}

// ---------- TraceContext 辅助 ----------

func TestNewTraceContext(t *testing.T) {
	tc := NewTraceContext()
	if len(hex.EncodeToString(tc.TraceID[:])) != 32 {
		t.Error("TraceID should be 32 hex chars")
	}
	if len(hex.EncodeToString(tc.SpanID[:])) != 16 {
		t.Error("SpanID should be 16 hex chars")
	}
	if !tc.Sampled {
		t.Error("should be sampled by default")
	}
	if tc.Remote {
		t.Error("new context should not be remote")
	}
}

func TestNewRootSpanContext(t *testing.T) {
	ctx, tc := NewRootSpanContext(context.Background())
	if tc.TraceID == (TraceID{}) {
		t.Error("expected non-zero TraceID")
	}

	traceID, spanID := TraceInfoFromContext(ctx)
	if traceID == "" || spanID == "" {
		t.Error("expected trace info in context")
	}
}

func TestSpanFromTraceContext(t *testing.T) {
	tc := TraceContext{
		TraceID:  generateTraceID(),
		SpanID:   generateSpanID(),
		ParentSpanID: generateSpanID(),
	}

	span := SpanFromTraceContext(tc)
	if span.traceID != tc.TraceID {
		t.Error("TraceID mismatch")
	}
	if span.spanID != tc.SpanID {
		t.Error("SpanID mismatch")
	}
	if span.parentID != tc.ParentSpanID {
		t.Error("ParentSpanID mismatch")
	}
}

// ---------- TraceContext 透传中间件 ----------

func TestTraceMiddleware_W3CPropagation(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	tp := NewTracerProvider(TracingConfig{
		Enabled:     true,
		ServiceName: "test-service",
		Propagation: "tracecontext",
	})

	g := &guards{tracerProvider: tp, logger: logger}
	handler := g.middlewareTrace(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceID, spanID := TraceInfoFromContext(r.Context())
		if traceID == "" || spanID == "" {
			t.Error("expected trace info in context")
		}
		w.WriteHeader(http.StatusOK)
	}))

	// 模拟入站请求带 W3C traceparent
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	// 验证响应头包含 traceparent
	tpHeader := rec.Header().Get("traceparent")
	if tpHeader == "" {
		t.Fatal("expected traceparent in response headers")
	}
	if !strings.HasPrefix(tpHeader, "00-") {
		t.Error("traceparent should start with version prefix")
	}

	// 验证包含 B3 头 (tracecontext 模式下不应有 b3 头)
	b3Header := rec.Header().Get("b3")
	if b3Header != "" {
		t.Error("b3 header should not be set in tracecontext-only mode")
	}
}

func TestTraceMiddleware_B3Propagation(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	tp := NewTracerProvider(TracingConfig{
		Enabled:     true,
		ServiceName: "test-service",
		Propagation: "b3",
	})

	g := &guards{tracerProvider: tp, logger: logger}
	handler := g.middlewareTrace(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("b3", "0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-1")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	b3Header := rec.Header().Get("b3")
	if b3Header == "" {
		t.Fatal("expected b3 in response headers")
	}

	// 不应有 W3C traceparent
	if rec.Header().Get("traceparent") != "" {
		t.Error("traceparent should not be set in b3-only mode")
	}
}

func TestTraceMiddleware_BothPropagation(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	tp := NewTracerProvider(TracingConfig{
		Enabled:     true,
		ServiceName: "test-service",
		Propagation: "both",
	})

	g := &guards{tracerProvider: tp, logger: logger}
	handler := g.middlewareTrace(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	// 两者都应有
	if rec.Header().Get("traceparent") == "" {
		t.Error("expected traceparent in both mode")
	}
	if rec.Header().Get("b3") == "" {
		t.Error("expected b3 in both mode")
	}
}

func TestTraceMiddleware_Disabled(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	tp := NewTracerProvider(TracingConfig{
		Enabled:     false,
		ServiceName: "test-service",
		Propagation: "both",
	})

	g := &guards{tracerProvider: tp, logger: logger}
	called := false
	handler := g.middlewareTrace(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("handler should be called when tracing disabled")
	}
	// 无 trace 相关头
	if rec.Header().Get("traceparent") != "" {
		t.Error("no traceparent when disabled")
	}
	if rec.Header().Get("b3") != "" {
		t.Error("no b3 when disabled")
	}
}

func TestTraceMiddleware_NilProvider(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	g := &guards{tracerProvider: nil, logger: logger}
	called := false
	handler := g.middlewareTrace(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("handler should be called with nil provider")
	}
}

func TestTraceMiddleware_StatusCapture(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	tp := NewTracerProvider(TracingConfig{Enabled: true})
	g := &guards{tracerProvider: tp, logger: logger}

	handler := g.middlewareTrace(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 从 context 取 span
		span := SpanFromContext(r.Context())
		if span == nil {
			t.Fatal("expected span in context")
		}
		// 模拟业务处理
		w.WriteHeader(http.StatusInternalServerError)
	}))

	req := httptest.NewRequest("POST", "/api/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	// 验证状态码已被捕获
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status=%d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

// ---------- context 中 Span 继承远程 TraceContext ----------

func TestTraceMiddleware_InheritsRemoteTraceID(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	tp := NewTracerProvider(TracingConfig{Enabled: true})
	g := &guards{tracerProvider: tp, logger: logger}

	var gotTraceID string
	handler := g.middlewareTrace(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceID, _ := TraceInfoFromContext(r.Context())
		gotTraceID = traceID
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/api/data", nil)
	req.Header.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if gotTraceID != "0af7651916cd43dd8448eb211c80319c" {
		t.Errorf("expected inherited TraceID, got %s", gotTraceID)
	}
}

func TestTraceMiddleware_NewRootWhenNoRemote(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	tp := NewTracerProvider(TracingConfig{Enabled: true})
	g := &guards{tracerProvider: tp, logger: logger}

	var gotTraceID string
	handler := g.middlewareTrace(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceID, _ := TraceInfoFromContext(r.Context())
		gotTraceID = traceID
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/api/data", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if gotTraceID == "" {
		t.Error("should generate new root TraceID")
	}
	if len(gotTraceID) != 32 {
		t.Errorf("TraceID length=%d, want 32", len(gotTraceID))
	}
}

// ---------- Propagate/Inject 辅助函数 ----------

func TestPropagateTraceContext(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")

	tc, ok := PropagateTraceContext(req)
	if !ok {
		t.Fatal("expected found")
	}

	// 注入到新的 outbound 请求
	outReq := httptest.NewRequest("POST", "http://downstream/service", nil)
	InjectTraceContext(outReq, tc, "tracecontext")

	if outReq.Header.Get("traceparent") == "" {
		t.Error("expected traceparent in outbound request")
	}
}

// ---------- Sampling 逻辑 ----------

func TestTracer_Sampling(t *testing.T) {
	// 全采样
	tp1 := NewTracerProvider(TracingConfig{Enabled: true, SamplingRate: 1.0})
	tracer1 := tp1.Tracer("test")
	ctx, span := tracer1.StartSpan(context.Background(), "test")
	if span == nil {
		t.Error("should not be nil with 100% sampling")
	}
	span.End()
	_ = ctx

	// 零采样 (通过 UpdateConfig 显式设置)
	tp2 := NewTracerProvider(TracingConfig{Enabled: true})
	tp2.UpdateConfig(TracingConfig{Enabled: true, SamplingRate: 0})
	tracer2 := tp2.Tracer("test")
	_, span2 := tracer2.StartSpan(context.Background(), "test")
	if span2 != nil {
		t.Error("should be nil with 0% sampling")
	}
}

// ---------- 端到端: HTTP 请求完整追踪链 ----------

func TestTraceContext_EndToEnd(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	tp := NewTracerProvider(TracingConfig{
		Enabled:     true,
		ServiceName: "e2e-test",
		Propagation: "both",
	})

	g := &guards{tracerProvider: tp, logger: logger}

	// 模拟完整链路: 入站 -> 中间件 -> handler -> 出站
	var handlerTraceID, handlerSpanID string
	handler := g.middlewareTrace(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerTraceID, handlerSpanID = TraceInfoFromContext(r.Context())

		// 模拟出站调用, 将上下文透传到另一个请求
		outReq := httptest.NewRequest("GET", "http://other-service/api", nil)
		tc, ok := TraceContextFromHeaders(r.Header)
		if ok {
			InjectTraceContext(outReq, tc, "both")
		} else {
			// 使用当前 context 中的 trace 信息
			tc2 := TraceContext{
				Flags:   TraceFlags(0x01),
				Sampled: true,
			}
			tidBytes, _ := hex.DecodeString(handlerTraceID)
			sidBytes, _ := hex.DecodeString(handlerSpanID)
			copy(tc2.TraceID[:], tidBytes)
			copy(tc2.SpanID[:], sidBytes)
			InjectTraceContext(outReq, tc2, "both")
		}

		// 验证出站请求已带 trace context
		if outReq.Header.Get("traceparent") == "" {
			t.Error("outbound request should have traceparent")
		}
		if outReq.Header.Get("b3") == "" {
			t.Error("outbound request should have b3")
		}

		w.WriteHeader(http.StatusOK)
	}))

	// 入站请求带 W3C traceparent (模拟上游服务传入)
	req := httptest.NewRequest("GET", "/api/data", nil)
	req.Header.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	req.Header.Set("User-Agent", "test-agent")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	// 验证 handler 内的 trace 信息
	if handlerTraceID != "0af7651916cd43dd8448eb211c80319c" {
		t.Errorf("handler TraceID=%s, should inherit from upstream", handlerTraceID)
	}
	if handlerSpanID == "b7ad6b7169203331" {
		t.Error("handler SpanID should be different from parent (child span)")
	}

	// 验证响应头也带 trace context (供下游继续透传)
	if rec.Header().Get("traceparent") == "" {
		t.Error("response should have traceparent for downstream propagation")
	}
	if rec.Header().Get("b3") == "" {
		t.Error("response should have b3 for downstream propagation")
	}
}

// ---------- TracingConfig 默认值 ----------

func TestDefaultTracingConfig(t *testing.T) {
	cfg := DefaultTracingConfig()
	if !cfg.Enabled {
		t.Error("should be enabled by default")
	}
	if cfg.ServiceName == "" {
		t.Error("should have default service name")
	}
	if cfg.Propagation != "both" {
		t.Errorf("default propagation should be both, got %s", cfg.Propagation)
	}
	if cfg.SamplingRate != 1.0 {
		t.Error("default sampling rate should be 1.0")
	}
}

// ---------- Server 集成 ----------

func TestServer_TracingIntegration(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	store, err := storage.Open("")
	if err != nil {
		t.Skipf("skip: %v", err)
		return
	}
	defer store.Close()

	engine := search.New(store)

	s := NewWithOptions(store, engine, logger, ServerOptions{
		Tracing: TracingConfig{
			Enabled:     true,
			ServiceName: "integration-test",
			Propagation: "tracecontext",
		},
	})

	if s.TracerProvider() == nil {
		t.Fatal("expected TracerProvider")
	}
	if !s.TracerProvider().IsEnabled() {
		t.Error("should be enabled")
	}

	// 测试 HTTP handler 是否正确设置 trace context
	handler := s.Handler()
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	// 验证有 traceparent 响应头
	if rec.Header().Get("traceparent") == "" {
		t.Error("expected traceparent header in response")
	}
}

func TestServer_TracingDisabled(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	store, err := storage.Open("")
	if err != nil {
		t.Skipf("skip: %v", err)
		return
	}
	defer store.Close()

	engine := search.New(store)

	s := NewWithOptions(store, engine, logger, ServerOptions{
		Tracing: TracingConfig{
			Enabled:     false,
			ServiceName: "disabled-test",
		},
	})

	if s.TracerProvider() == nil {
		t.Fatal("expected TracerProvider even when disabled")
	}
	if s.TracerProvider().IsEnabled() {
		t.Error("should be disabled")
	}

	handler := s.Handler()
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Header().Get("traceparent") != "" {
		t.Error("no traceparent when disabled")
	}
}

// ---------- 边界条件 ----------

func TestTraceMiddleware_EmptyRequest(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	tp := NewTracerProvider(TracingConfig{Enabled: true})
	g := &guards{tracerProvider: tp, logger: logger}

	handler := g.middlewareTrace(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 即使是 / 路径, 也应有 trace context
		traceID, spanID := TraceInfoFromContext(r.Context())
		if traceID == "" || spanID == "" {
			t.Error("expected trace info even for root path")
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
}

func TestTraceMiddleware_MultipleRequests(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	tp := NewTracerProvider(TracingConfig{Enabled: true})
	g := &guards{tracerProvider: tp, logger: logger}

	var lastTraceID string
	handler := g.middlewareTrace(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceID, _ := TraceInfoFromContext(r.Context())
		lastTraceID = traceID
		w.WriteHeader(http.StatusOK)
	}))

	// 连续请求, 每个都应生成独立的 TraceID
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest("GET", "/api", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if lastTraceID == "" {
			t.Error("empty traceID on request", i)
		}
		if len(lastTraceID) != 32 {
			t.Errorf("request %d: TraceID length=%d", i, len(lastTraceID))
		}
	}
}

// ---------- TraceContext 从 context 提取 ----------

func TestTraceInfoFromContext_EmptyContext(t *testing.T) {
	traceID, spanID := TraceInfoFromContext(context.Background())
	if traceID != "" || spanID != "" {
		t.Error("empty context should return empty trace info")
	}
}

func TestTraceInfoFromContext_WithValues(t *testing.T) {
	ctx := context.Background()
	ctx = context.WithValue(ctx, traceCtxKeyTraceID, "abc123")
	ctx = context.WithValue(ctx, traceCtxKeySpanID, "def456")

	traceID, spanID := TraceInfoFromContext(ctx)
	if traceID != "abc123" {
		t.Errorf("traceID=%s", traceID)
	}
	if spanID != "def456" {
		t.Errorf("spanID=%s", spanID)
	}
}

// ---------- Time-related tests ----------

func TestSpan_DurationPrecision(t *testing.T) {
	tp := NewTracerProvider(TracingConfig{Enabled: true})
	tracer := tp.Tracer("test")

	_, span := tracer.StartSpan(context.Background(), "test")
	time.Sleep(10 * time.Millisecond)
	span.End()

	d := span.Duration()
	if d < 5*time.Millisecond {
		t.Errorf("duration too short: %v", d)
	}
}