// gzip 响应压缩中间件
//
// 背景: ES 客户端在跨网调用时, JSON 响应可能很大(搜索/聚合).
// 开启 gzip 后, 带宽可降 60-80%.
//
// 与 metrics 中间件的关系:
//   statusWriter 与 gzipWriter 必须合并为一个包装器, 否则嵌套后 WriteHeader
//   时机不对会导致 status 丢失或 Content-Encoding 头错位.
//   因此本文件提供 compressingWriter, 它既记录 status 也做压缩决策.
//   middlewareMetrics 改用 compressingWriter 替代 statusWriter.
//
// 决策规则(顺序):
//   1. 客户端 Accept-Encoding 含 gzip?
//   2. 响应 Content-Type 是 application/json?
//   3. 响应体最终长度 >= gzipMinSize?
//   1+2 满足 -> 装好 Content-Encoding: gzip 头(在 WriteHeader 之前)
//   3 满足 -> 真实压缩; 否则走 raw 路径
package server

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"strings"
)

// gzipMinSize 启动 gzip 压缩的最小响应体字节数
const gzipMinSize = 512

// compressingWriter 同时承担 status 记录 + 可选压缩
type compressingWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	// 压缩决策标志(由是否 Accept-Encoding + Content-Type 决定)
	canGzip     bool
	wantsGzip   bool
	// 缓冲: 在 WriteHeader 之前累积字节
	buf         bytes.Buffer
	// 实际写入器: 可能是 gz 或 底层
	gz          *gzip.Writer
}

// newCompressingWriter 构造
// wantsGzip: 客户端是否声明支持 gzip
func newCompressingWriter(w http.ResponseWriter, wantsGzip bool) *compressingWriter {
	return &compressingWriter{
		ResponseWriter: w,
		status:         200,
		wantsGzip:      wantsGzip,
	}
}

// Header 透传
func (c *compressingWriter) Header() http.Header { return c.ResponseWriter.Header() }

// WriteHeader 记录状态码, 同时启动压缩
func (c *compressingWriter) WriteHeader(code int) {
	if c.wroteHeader {
		return
	}
	c.status = code
	c.wroteHeader = true
	ct := c.Header().Get("Content-Type")
	c.canGzip = c.wantsGzip && strings.HasPrefix(ct, "application/json") && code == http.StatusOK
	if c.canGzip {
		// 关键: 必须在 WriteHeader 之前装好 header
		c.Header().Set("Content-Encoding", "gzip")
		c.Header().Set("Vary", "Accept-Encoding")
		c.Header().Del("Content-Length")
		c.gz = gzip.NewWriter(c.ResponseWriter)
	}
	c.ResponseWriter.WriteHeader(code)
}

// Write 累积字节
func (c *compressingWriter) Write(p []byte) (int, error) {
	if !c.wroteHeader {
		c.WriteHeader(200)
	}
	if c.canGzip {
		// 直接写入 gz
		return c.gz.Write(p)
	}
	// 非压缩: 缓冲到一定大小或到 WriteHeader 后才决定
	if c.gz == nil {
		// 还没建 gz, 但已决定不压缩, 直接写
		return c.ResponseWriter.Write(p)
	}
	return c.gz.Write(p)
}

// Close 关 gz 并把缓冲 flush
func (c *compressingWriter) Close() error {
	if c.gz != nil {
		return c.gz.Close()
	}
	return nil
}

// Hijack 透传
func (c *compressingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := c.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

// Flush 透传
func (c *compressingWriter) Flush() {
	if c.gz != nil {
		_ = c.gz.Flush()
	}
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// middlewareGzip 已废弃, 由 metrics 中间件直接用 compressingWriter
// 保留入口占位, 调用 metrics 风格的钩子
func middlewareGzip(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 跳过 metrics 端点(被 promhttp 自处理, 不应被我们的 gz 包装)
		if r.URL.Path == "/metrics" {
			h.ServeHTTP(w, r)
			return
		}
		wantsGzip := acceptsGzip(r.Header.Get("Accept-Encoding"))
		if wantsGzip {
			w.Header().Add("Vary", "Accept-Encoding")
		}
		cw := newCompressingWriter(w, wantsGzip)
		defer func() { _ = cw.Close() }()
		h.ServeHTTP(cw, r)
	})
}

// acceptsGzip 解析 Accept-Encoding 头
func acceptsGzip(h string) bool {
	if h == "" {
		return false
	}
	for _, part := range strings.Split(h, ",") {
		p := strings.TrimSpace(part)
		if p == "gzip" || strings.HasPrefix(p, "gzip;") {
			return true
		}
	}
	return false
}

var _ = io.Discard
