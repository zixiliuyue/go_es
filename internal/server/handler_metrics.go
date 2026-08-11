// handleMetrics 暴露 Prometheus 抓取端点
package server

import (
	"context"
	"net/http"
	"time"

	"github.com/golang/snappy"
	"github.com/gogo/protobuf/proto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/prometheus/prompb"
)

// handleMetrics GET /metrics
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	s.metrics.Collect(s)

	// 后台异步推送 metrics 到 exporters(不阻塞响应)
	if s.remoteWriter != nil || (s.otelExporter != nil && s.otelExporter.MetricsExporter() != nil) {
		go s.exportMetricsFromRegistry()
	}

	h := promhttp.HandlerFor(s.metrics.Registry(), promhttp.HandlerOpts{
		ErrorHandling: promhttp.ContinueOnError,
	})
	h.ServeHTTP(w, r)
}

// exportMetricsFromRegistry 从 Prometheus registry 采集指标并推送到 exporters
func (s *Server) exportMetricsFromRegistry() {
	if s == nil || s.metrics == nil {
		return
	}

	metricFamilies, err := s.metrics.Registry().Gather()
	if err != nil {
		return
	}

	var tss []prompb.TimeSeries
	for _, mf := range metricFamilies {
		for _, m := range mf.GetMetric() {
			labels := convertDTOToPrompbLabels(m.GetLabel())

			var samples []prompb.Sample
			ts := time.Now().UnixMilli()

			if m.Counter != nil {
				samples = append(samples, prompb.Sample{
					Value:     m.Counter.GetValue(),
					Timestamp: ts,
				})
			}
			if m.Gauge != nil {
				samples = append(samples, prompb.Sample{
					Value:     m.Gauge.GetValue(),
					Timestamp: ts,
				})
			}
			if m.Untyped != nil {
				samples = append(samples, prompb.Sample{
					Value:     m.Untyped.GetValue(),
					Timestamp: ts,
				})
			}
			if m.Histogram != nil {
				// 聚合为 sum 发送
				samples = append(samples, prompb.Sample{
					Value:     m.Histogram.GetSampleSum(),
					Timestamp: ts,
				})
			}
			if m.Summary != nil {
				samples = append(samples, prompb.Sample{
					Value:     m.Summary.GetSampleSum(),
					Timestamp: ts,
				})
			}

			if len(samples) > 0 {
				tss = append(tss, prompb.TimeSeries{
					Labels:  labels,
					Samples: samples,
				})
			}
		}
	}

	if len(tss) == 0 {
		return
	}

	// 推送到 remote_write
	if s.remoteWriter != nil {
		s.remoteWriter.Enqueue(tss...)
	}

	// 推送到 OTel metrics exporter
	if s.otelExporter != nil && s.otelExporter.MetricsExporter() != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = s.otelExporter.MetricsExporter().PushMetrics(ctx, tss)
		}()
	}
}

// convertDTOToPrompbLabels 将 DTO labels 转为 prompb labels
func convertDTOToPrompbLabels(labels []*dto.LabelPair) []prompb.Label {
	result := make([]prompb.Label, 0, len(labels))
	for _, l := range labels {
		result = append(result, prompb.Label{
			Name:  l.GetName(),
			Value: l.GetValue(),
		})
	}
	return result
}

// Ensure proto and snappy are used
var (
	_ proto.Message = (*prompb.WriteRequest)(nil)
	_ []byte        = snappy.Encode(nil, nil)
)