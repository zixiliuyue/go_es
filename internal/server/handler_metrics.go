// handleMetrics 暴露 Prometheus 抓取端点
package server

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// handleMetrics GET /metrics
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	// 抓取前先收集 gouge 统计
	s.metrics.Collect(s)
	h := promhttp.HandlerFor(s.metrics.Registry(), promhttp.HandlerOpts{
		// 抓取端点本身的错误是 5xx, 否则 200
		ErrorHandling: promhttp.ContinueOnError,
	})
	h.ServeHTTP(w, r)
}
