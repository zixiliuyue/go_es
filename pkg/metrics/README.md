# Prometheus Metrics Support

This package provides optional Prometheus metrics integration for go_es.

## Usage

```go
package main

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/zixiliuyue/go_es/pkg/metrics"
	"net/http"
)

func main() {
	// Create metrics
	m := metrics.NewMetrics("go_es")
	m.Register(prometheus.DefaultRegisterer)

	// Expose metrics
	http.Handle("/metrics", promhttp.Handler())
	http.ListenAndServe(":9090", nil)
}
```

## Notes

- If you don't use Prometheus, this package doesn't add any dependency to your project because prometheus is imported only inside this package.
- Use `metrics.NoopMetrics` when you don't enable metrics.
