package server

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// HTTP server metrics. Labels are kept to a bounded, low-cardinality set:
//   - "handler": short logical handler name ("graphql", "healthz", "readyz", "metrics", "other")
//   - "method":  HTTP verb
//   - "status":  HTTP status code as string
//
// We do not label by full URL path because that would be unbounded.
var (
	httpRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total HTTP requests handled, partitioned by handler, method and status.",
		},
		[]string{"handler", "method", "status"},
	)

	httpRequestDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request latency distribution in seconds.",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 14), // 1ms … ~16s
		},
		[]string{"handler", "method", "status"},
	)

	httpResponseBytesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_response_bytes_total",
			Help: "Total bytes written to HTTP response bodies.",
		},
		[]string{"handler"},
	)

	// scrapeRunsLastSuccessSeconds is set by the readiness probe based on the
	// scrape_runs table and exposed for alerting on stale pricing data.
	scrapeRunsLastSuccessSeconds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "scrape_runs_last_success_seconds",
			Help: "Unix timestamp of the most recent successful scrape per vendor.",
		},
		[]string{"vendor"},
	)
)

// metricsRegistry is the single prometheus registry used by the server. We keep
// a dedicated one (instead of the global default) so tests stay hermetic.
var metricsRegistry = prometheus.NewRegistry()

func init() {
	metricsRegistry.MustRegister(
		httpRequestsTotal,
		httpRequestDurationSeconds,
		httpResponseBytesTotal,
		scrapeRunsLastSuccessSeconds,
	)
}

// recordHTTPMetrics is invoked at the end of each request.
func recordHTTPMetrics(handler, method string, status int, elapsed time.Duration, bytes int) {
	statusStr := strconv.Itoa(status)
	httpRequestsTotal.WithLabelValues(handler, method, statusStr).Inc()
	httpRequestDurationSeconds.WithLabelValues(handler, method, statusStr).Observe(elapsed.Seconds())
	if bytes > 0 {
		httpResponseBytesTotal.WithLabelValues(handler).Add(float64(bytes))
	}
}

// SetScrapeLastSuccess updates the per-vendor last-success gauge. Intended to
// be called from the scrape command (or by a readiness background refresher).
func SetScrapeLastSuccess(vendor string, ts time.Time) {
	if ts.IsZero() {
		return
	}
	scrapeRunsLastSuccessSeconds.WithLabelValues(vendor).Set(float64(ts.Unix()))
}
