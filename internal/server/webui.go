// 内置 Web UI
//
// 极简控制台, 用纯 HTML + 内联 JS(不引前端构建链).
// 静态资源通过 go:embed 编译进二进制, 部署只需一个文件.
//
// 路由:
//   GET /_ui/             -> 重定向到 /_ui/index.html
//   GET /_ui/index.html   -> 单页 UI
//
// 设计原则:
//   - 仅依赖 fetch API(浏览器内置), 不引 htmx/React 等
//   - 严格 escape 所有 JSON 嵌入(防 XSS)
//   - 在认证开启时仍可访问(白名单: /_ui 与 /metrics 一致)
package server

import (
	"embed"
	"io"
	"net/http"
)

//go:embed web/*
var webFS embed.FS

// handleUI GET /_ui/  或  /_ui/index.html
func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	data, err := webFS.ReadFile("web/index.html")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error(), "")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.Copy(w, byteReaderPool(data))
}

// handleAdminUI GET /_ui/admin.html 管理员控制台
func (s *Server) handleAdminUI(w http.ResponseWriter, r *http.Request) {
	data, err := webFS.ReadFile("web/admin.html")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error(), "")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.Copy(w, byteReaderPool(data))
}

// byteReaderPool 简单的 bytes.Reader 包装
func byteReaderPool(b []byte) io.Reader {
	return &byteReaderAt{data: b}
}

type byteReaderAt struct {
	data []byte
	off  int
}

func (b *byteReaderAt) Read(p []byte) (int, error) {
	if b.off >= len(b.data) {
		return 0, io.EOF
	}
	n := copy(p, b.data[b.off:])
	b.off += n
	return n, nil
}
