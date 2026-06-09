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

	// productsTotal exposes the row count of the products table,
	// partitioned by vendor. Lets operators alert on sudden drops
	// ("the scraper started returning 0 AWS products") which would
	// otherwise only surface when a c3x-go user notices empty
	// estimates.
	productsTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "c3x_products_total",
			Help: "Number of priced products currently in the database, by vendor.",
		},
		[]string{"vendor"},
	)

	// graphqlQueriesTotal tracks every GraphQL operation that
	// reached the resolver layer, partitioned by result. Cheap
	// alerting surface for "is the API still answering?".
	graphqlQueriesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "c3x_graphql_queries_total",
			Help: "GraphQL queries resolved, partitioned by outcome.",
		},
		[]string{"result"}, // "ok" | "empty" | "error"
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
		productsTotal,
		graphqlQueriesTotal,
	)
}

// SetProductsTotal updates the per-vendor product-count gauge.
// Called by a readiness background refresher so the value tracks
// the DB on a low-frequency cadence (avoiding a SELECT COUNT(*)
// on every scrape).
func SetProductsTotal(vendor string, count int64) {
	productsTotal.WithLabelValues(vendor).Set(float64(count))
}

// RecordGraphQLQuery is invoked by the GraphQL resolver after each
// operation. The result label has three values:
//
//	"ok"     — query returned products
//	"empty"  — query returned an empty result set (often a c3x-go
//	           filter shape that doesn't match the catalog yet)
//	"error"  — query returned an upstream error
func RecordGraphQLQuery(result string) {
	graphqlQueriesTotal.WithLabelValues(result).Inc()
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
