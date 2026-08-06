// 中间件与公共响应工具
//
// 历史: 早期版本的 withMiddleware/recover/trace 逻辑已迁出到 guards.go,
// 这里只保留通用响应工具(writeError / writeJSON / decodeJSON / splitPath)
// 与 statusWriter 包装器(给业务 handler 写非 200 状态码用).
package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

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

// 抑制 time 包未用警告(早期版本在 withMiddleware 用了 time.Now, 现已迁出)
var _ = time.Now
