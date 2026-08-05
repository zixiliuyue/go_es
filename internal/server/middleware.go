// 中间件与公共响应工具
package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
)

// withMiddleware 包装 mux,加日志、recover、错误格式
func (s *Server) withMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// go-elasticsearch v8 客户端在建立连接时会做"产品嗅探":
		// 期望服务端在响应中带 X-Elastic-Product: Elasticsearch 头
		// 我们用自研服务端冒充 ES 时,需要补上此头才能让 SDK 客户端接受
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		start := time.Now()
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error("panic in handler",
					zap.Any("panic", rec),
					zap.String("path", r.URL.Path))
				writeError(w, http.StatusInternalServerError,
					"server_error", "internal server error", "")
			}
		}()
		s.logger.Debug("request",
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path))
		ww := &statusWriter{ResponseWriter: w, status: 200}
		h.ServeHTTP(ww, r)
		s.logger.Debug("response",
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path),
			zap.Int("status", ww.status),
			zap.Duration("cost", time.Since(start)))
	})
}

// statusWriter 包装 ResponseWriter 以便记录状态码
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// 错误响应格式对齐 ES: {"error": {"type": ..., "reason": ..., "root_cause": [...]}, "status": ...}

type esError struct {
	Type      string `json:"type"`
	Reason    string `json:"reason"`
	RootCause []struct {
		Type   string `json:"type"`
		Reason string `json:"reason"`
	} `json:"root_cause,omitempty"`
}

type esErrorBody struct {
	Error  esError `json:"error"`
	Status int     `json:"status"`
}

// writeError 写出 ES 风格错误响应
// status: HTTP 状态码
// errType: error.type
// reason: error.reason
// rootCause: 可选根因描述
func writeError(w http.ResponseWriter, status int, errType, reason, rootCause string) {
	body := esErrorBody{
		Status: status,
		Error: esError{
			Type:   errType,
			Reason: reason,
		},
	}
	if rootCause != "" {
		body.Error.RootCause = []struct {
			Type   string `json:"type"`
			Reason string `json:"reason"`
		}{{Type: errType, Reason: rootCause}}
	}
	writeJSON(w, status, body)
}

// writeJSON 统一 JSON 响应
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// decodeJSON 解码 JSON 请求体到 out
func decodeJSON(r *http.Request, out interface{}) error {
	if r.Body == nil {
		return nil
	}
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	return dec.Decode(out)
}

// splitPath 把 "/a/b/c" 拆成 ["a","b","c"]
func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}
