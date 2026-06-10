package server

import (
	"bufio"
	"container/list"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/c3xdev/c3x-pricing-api/internal/config"
	"github.com/c3xdev/c3x-pricing-api/internal/db"
	c3xgql "github.com/c3xdev/c3x-pricing-api/internal/graphql"
	gql "github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/gqlerrors"
	"github.com/graphql-go/graphql/language/ast"
	"github.com/graphql-go/graphql/language/parser"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"golang.org/x/time/rate"
)

type Server struct {
	schema gql.Schema
	cfg    *config.Config
	db     *db.DB

	// H2: Per-IP token-bucket rate limiting with a bounded LRU of limiters.
	rateMu       sync.Mutex
	rateLimiters map[string]*list.Element // IP -> LRU element holding *ipLimiter
	rateLRU      *list.List               // front = most recently used

	// Trusted proxy CIDRs/IPs, parsed once at startup from TRUSTED_PROXIES env var.
	trustedProxies []*net.IPNet
	trustedIPs     []net.IP

	// stopCh is closed when the server shuts down, to signal background goroutines.
	stopCh chan struct{}
}

// ipLimiter holds the rate.Limiter for an IP plus its last-use timestamp for LRU eviction.
type ipLimiter struct {
	ip      string
	limiter *rate.Limiter
	lastUse time.Time
}

const maxRateLimiters = 10000

// validRequestID matches safe X-Request-ID values (T4: log injection prevention).
var validRequestID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type graphQLRequest struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables"`
}

func New(cfg *config.Config, database *db.DB) (*Server, error) {
	schema, err := c3xgql.NewSchema(database)
	if err != nil {
		return nil, fmt.Errorf("failed to create graphql schema: %w", err)
	}

	s := &Server{
		schema:       schema,
		cfg:          cfg,
		db:           database,
		rateLimiters: make(map[string]*list.Element),
		rateLRU:      list.New(),
		stopCh:       make(chan struct{}),
	}

	// Parse TRUSTED_PROXIES once at startup instead of per-request.
	if tp := os.Getenv("TRUSTED_PROXIES"); tp != "" {
		for _, entry := range strings.Split(tp, ",") {
			entry = strings.TrimSpace(entry)
			if _, network, err := net.ParseCIDR(entry); err == nil {
				s.trustedProxies = append(s.trustedProxies, network)
			} else if ip := net.ParseIP(entry); ip != nil {
				s.trustedIPs = append(s.trustedIPs, ip)
			} else {
				slog.Warn("invalid TRUSTED_PROXIES entry, skipping", slog.String("entry", entry)) //nolint:gosec // G706: entry is from env var, structured logging prevents injection
			}
		}
		slog.Info("parsed trusted proxies", "cidrs", len(s.trustedProxies), "ips", len(s.trustedIPs))
	}

	// Periodically evict idle limiters (older than 10 minutes) to release memory.
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.evictIdleLimiters(10 * time.Minute)
			case <-s.stopCh:
				return
			}
		}
	}()

	// Reap stale scrape runs (stuck in 'running' for >6 hours) every 15 minutes.
	go func() {
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if n, err := s.db.ReapStaleScrapeRuns(context.Background(), 6*time.Hour); err != nil {
					slog.Warn("failed to reap stale scrape runs", "error", err)
				} else if n > 0 {
					slog.Info("reaped stale scrape runs", "count", n)
				}
			case <-s.stopCh:
				return
			}
		}
	}()

	return s, nil
}

// evictIdleLimiters removes limiters whose lastUse is older than maxAge.
func (s *Server) evictIdleLimiters(maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge)
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	for e := s.rateLRU.Back(); e != nil; {
		il := e.Value.(*ipLimiter)
		if il.lastUse.After(cutoff) {
			return
		}
		prev := e.Prev()
		s.rateLRU.Remove(e)
		delete(s.rateLimiters, il.ip)
		e = prev
	}
}

// Stop signals background goroutines to exit. Call during shutdown.
func (s *Server) Stop() {
	close(s.stopCh)
}

func (s *Server) Start() error {
	// Register Go + process collectors on our dedicated registry so /metrics
	// exposes runtime stats in addition to the custom HTTP/scrape metrics.
	metricsRegistry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", instrument("graphql",
		requestIDMiddleware(recoverMiddleware(s.rateLimitMiddleware(s.authMiddleware(s.handleGraphQL))))))
	mux.HandleFunc("/healthz", instrument("healthz", s.handleLiveness))
	mux.HandleFunc("/readyz", instrument("readyz", s.handleReadiness))
	mux.HandleFunc("/health", instrument("readyz", s.handleReadiness)) // backward compat
	mux.HandleFunc("/status", instrument("status", s.handleStatus))
	mux.HandleFunc("/catalog", instrument("catalog", handleCatalog))

	// Serve /metrics on a separate admin port if configured, otherwise on the main mux.
	if s.cfg.MetricsPort != "" && s.cfg.MetricsPort != "0" {
		adminMux := http.NewServeMux()
		adminMux.Handle("/metrics", promhttp.HandlerFor(metricsRegistry, promhttp.HandlerOpts{
			Registry:          metricsRegistry,
			EnableOpenMetrics: true,
		}))
		go func() {
			adminSrv := &http.Server{Addr: ":" + s.cfg.MetricsPort, Handler: adminMux, ReadHeaderTimeout: 10 * time.Second}
			slog.Info("admin/metrics server starting", "addr", adminSrv.Addr)
			if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("admin server failed", "error", err)
			}
		}()
	} else {
		mux.Handle("/metrics", promhttp.HandlerFor(metricsRegistry, promhttp.HandlerOpts{
			Registry:          metricsRegistry,
			EnableOpenMetrics: true,
		}))
	}

	// Wrap the mux with security headers, CORS, gzip, and OpenTelemetry HTTP
	// instrumentation. Order matters: OTel must wrap everything so spans start
	// at the boundary; gzip sits inside it so the trace covers compression
	// cost; security+CORS headers must be set before gzip buffers the body.
	var handler http.Handler = mux
	handler = gzipMiddleware(handler)
	handler = securityHeadersMiddleware(handler)
	if s.cfg.CORSOrigins != "" {
		if strings.Contains(s.cfg.CORSOrigins, "*") {
			slog.Warn("CORS_ALLOWED_ORIGINS contains wildcard '*'; all origins will be echoed")
		}
		handler = corsMiddleware(handler, s.cfg.CORSOrigins)
	}
	handler = otelhttp.NewHandler(handler, "c3x-pricing-api",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			// Bound span-name cardinality to the mux's logical routes.
			return r.Method + " " + handlerLabel(r.URL.Path)
		}),
	)

	addr := ":" + s.cfg.Port
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Graceful shutdown
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-done
		slog.Info("server shutting down")
		s.Stop() // L1: Stop background goroutines (rate limiter ticker)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	slog.Info("server starting", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// handleLiveness returns 200 unconditionally. Kubernetes uses this to know
// the process is alive; it should never fail unless the process is deadlocked.
func (s *Server) handleLiveness(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// handleStatus returns the current scrape status and product counts per vendor.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	type vendorInfo struct {
		Status      string `json:"status"`
		Products    int64  `json:"products"`
		LastScraped string `json:"lastScraped,omitempty"`
	}

	vendors := map[string]*vendorInfo{
		"aws":   {Status: "never"},
		"azure": {Status: "never"},
		"gcp":   {Status: "never"},
	}

	// Use scrape_runs product counts instead of COUNT(*) on the products table
	// (which takes 5+ seconds on 5M rows). The scrape_runs table records
	// the product count at completion time.
	rows, err := s.db.Pool.Query(ctx, `
		SELECT DISTINCT ON (vendor) vendor, products
		FROM scrape_runs WHERE status = 'success'
		ORDER BY vendor, id DESC`)
	if err == nil {
		for rows.Next() {
			var vendor string
			var count int
			if rows.Scan(&vendor, &count) == nil {
				if v, ok := vendors[vendor]; ok {
					v.Products = int64(count)
					v.Status = "ready"
					// Feed the Prometheus gauge whenever /status is
					// polled — monitoring hits this endpoint anyway,
					// so the gauge tracks reality without a dedicated
					// background refresher.
					SetProductsTotal(vendor, int64(count))
				}
			}
		}
		rows.Close()
	}

	// Get latest scrape run status per vendor
	rows2, err := s.db.Pool.Query(ctx, `
		SELECT DISTINCT ON (vendor) vendor, status, finished_at
		FROM scrape_runs ORDER BY vendor, id DESC`)
	if err == nil {
		defer rows2.Close()
		for rows2.Next() {
			var vendor, status string
			var finishedAt *time.Time
			if rows2.Scan(&vendor, &status, &finishedAt) == nil {
				if v, ok := vendors[vendor]; ok {
					if status == "running" {
						v.Status = "scraping"
					} else if finishedAt != nil {
						v.LastScraped = finishedAt.Format(time.RFC3339)
						if status == "success" {
							SetScrapeLastSuccess(vendor, *finishedAt)
						}
						if time.Since(*finishedAt) > 48*time.Hour {
							v.Status = "stale"
						}
					}
				}
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"vendors": vendors})
}

// handleReadiness checks that the server can serve traffic (DB reachable).
func (s *Server) handleReadiness(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	// Check database connectivity with a 2-second timeout
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := s.db.PingCtx(ctx); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"unhealthy","error":"database connection failed"}`))
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) handleGraphQL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
		return
	}

	ct := r.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return
	}

	// Limit request body size
	maxBytes := int64(s.cfg.MaxRequestBodyMB) * 1024 * 1024
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Failed to read request body")
		return
	}

	// Detect batch vs single request per the GraphQL-over-HTTP spec.
	// Single: {"query":"..."} → respond with {...}
	// Batch:  [{"query":"..."},...] → respond with [{...},...]
	var batch []graphQLRequest
	isBatch := len(body) > 0 && body[0] == '['
	if isBatch {
		if err := json.Unmarshal(body, &batch); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", "Invalid JSON array of queries.")
			return
		}
	} else {
		var single graphQLRequest
		if err := json.Unmarshal(body, &single); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", "Invalid JSON. Send a JSON object or array of queries.")
			return
		}
		batch = []graphQLRequest{single}
	}

	// Limit batch size
	if len(batch) > s.cfg.MaxBatchSize {
		writeError(w, http.StatusBadRequest, "batch_too_large", fmt.Sprintf("Batch size %d exceeds maximum of %d", len(batch), s.cfg.MaxBatchSize))
		return
	}

	// Execute with timeout. M8: This context propagates through gql.Do → resolver →
	// DB query (via QueryProducts' ctx parameter), so cancellation of the timeout
	// will cancel in-flight PostgreSQL queries as well.
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.cfg.QueryTimeoutSecs)*time.Second)
	defer cancel()

	results := make([]interface{}, len(batch))
	for i, req := range batch {
		select {
		case <-ctx.Done():
			// Fill remaining results with timeout errors and return partial results.
			for j := i; j < len(batch); j++ {
				results[j] = &gql.Result{
					Errors: []gqlerrors.FormattedError{
						{Message: "query timeout exceeded"},
					},
				}
			}
			w.Header().Set("Content-Type", "application/json")
			encodeGraphQLResponse(w, results, isBatch)
			return
		default:
		}

		// Reject excessively large queries to limit GraphQL depth/complexity abuse
		if len(req.Query) > 10000 {
			results[i] = &gql.Result{Errors: []gqlerrors.FormattedError{{Message: "Query too large"}}}
			continue
		}

		// Parse once, reuse AST for both depth and introspection checks.
		// Previously this parsed the query 3 times (depth, introspection, gql.Do).
		doc, parseErr := parser.Parse(parser.ParseParams{Source: req.Query})
		if parseErr == nil {
			depth := computeQueryDepthFromAST(doc)
			if depth > s.cfg.MaxQueryDepth {
				results[i] = &gql.Result{Errors: []gqlerrors.FormattedError{{Message: fmt.Sprintf("Query depth %d exceeds maximum of %d", depth, s.cfg.MaxQueryDepth)}}}
				continue
			}

			if s.cfg.DisableIntrospection && astContainsIntrospection(doc) {
				results[i] = &gql.Result{Errors: []gqlerrors.FormattedError{{Message: "Introspection is disabled"}}}
				continue
			}
		}

		result := gql.Do(gql.Params{
			Schema:         s.schema,
			RequestString:  req.Query,
			VariableValues: req.Variables,
			Context:        ctx,
		})
		results[i] = result
		// Outcome counter for alerting: error / empty / ok.
		switch {
		case len(result.Errors) > 0:
			RecordGraphQLQuery("error")
		case graphQLResultIsEmpty(result):
			RecordGraphQLQuery("empty")
		default:
			RecordGraphQLQuery("ok")
		}
	}

	w.Header().Set("Content-Type", "application/json")
	encodeGraphQLResponse(w, results, isBatch)
}

// graphQLResultIsEmpty reports whether a successful result carried an
// empty `products` list — the strongest signal that a c3x-go catalog
// filter shape doesn't match anything in the database. Tracking these
// separately from errors lets operators alert on "the CLI is asking
// for products we don't have" without log-diving.
func graphQLResultIsEmpty(result *gql.Result) bool {
	data, ok := result.Data.(map[string]interface{})
	if !ok {
		return false
	}
	products, ok := data["products"]
	if !ok {
		return false
	}
	list, ok := products.([]interface{})
	return ok && len(list) == 0
}

// encodeGraphQLResponse writes the GraphQL response, returning an object for
// single requests and an array for batch requests per the GraphQL-over-HTTP spec.
func encodeGraphQLResponse(w http.ResponseWriter, results []interface{}, isBatch bool) {
	if isBatch {
		_ = json.NewEncoder(w).Encode(results)
	} else {
		_ = json.NewEncoder(w).Encode(results[0])
	}
}

func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.APIKey == "" {
			next(w, r)
			return
		}

		apiKey := ""

		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			apiKey = strings.TrimPrefix(auth, "Bearer ")
		}

		if apiKey == "" {
			apiKey = r.Header.Get("X-Api-Key")
		}

		// Hash both keys with SHA-256 before comparing to avoid leaking key length
		providedHash := sha256.Sum256([]byte(apiKey))
		expectedHash := sha256.Sum256([]byte(s.cfg.APIKey))
		if subtle.ConstantTimeCompare(providedHash[:], expectedHash[:]) != 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"Invalid API key","error_code":"invalid_api_key"}`))
			return
		}

		next(w, r)
	}
}

// clientIP extracts the real client IP, respecting trusted proxy configuration.
// The trusted proxy CIDRs/IPs are parsed once at startup (in New()) to avoid
// per-request environment reads and CIDR parsing.
func (s *Server) clientIP(r *http.Request) string {
	ip := r.RemoteAddr
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		ip = host
	}
	if len(s.trustedProxies) == 0 && len(s.trustedIPs) == 0 {
		return ip
	}
	remoteIP := net.ParseIP(ip)
	trusted := false
	for _, network := range s.trustedProxies {
		if network.Contains(remoteIP) {
			trusted = true
			break
		}
	}
	if !trusted {
		for _, tip := range s.trustedIPs {
			if tip.Equal(remoteIP) {
				trusted = true
				break
			}
		}
	}
	if trusted {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			return strings.TrimSpace(strings.Split(fwd, ",")[0])
		}
	}
	return ip
}

func (s *Server) rateLimitMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.RateLimitPerSec <= 0 {
			next(w, r)
			return
		}

		ip := s.clientIP(r)

		limiter := s.getLimiter(ip)
		if !limiter.Allow() {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"Rate limit exceeded"}`))
			return
		}

		next(w, r)
	}
}

// getLimiter returns the per-IP token bucket limiter, creating or promoting it in the LRU.
func (s *Server) getLimiter(ip string) *rate.Limiter {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()

	if e, ok := s.rateLimiters[ip]; ok {
		il := e.Value.(*ipLimiter)
		il.lastUse = time.Now()
		s.rateLRU.MoveToFront(e)
		return il.limiter
	}

	// Evict the oldest entry when we exceed the cap.
	if s.rateLRU.Len() >= maxRateLimiters {
		if back := s.rateLRU.Back(); back != nil {
			old := back.Value.(*ipLimiter)
			s.rateLRU.Remove(back)
			delete(s.rateLimiters, old.ip)
		}
	}

	// Allow a small burst (2x the per-second rate) to accommodate legitimate bursts
	// without enabling a 2x window-boundary exploit.
	burst := s.cfg.RateLimitPerSec * 2
	if burst < 1 {
		burst = 1
	}
	il := &ipLimiter{
		ip:      ip,
		limiter: rate.NewLimiter(rate.Limit(s.cfg.RateLimitPerSec), burst),
		lastUse: time.Now(),
	}
	e := s.rateLRU.PushFront(il)
	s.rateLimiters[ip] = e
	return il.limiter
}

// computeQueryDepth parses the GraphQL query into an AST and returns the maximum
// selection-set nesting depth. It correctly handles named fragments (with cycle
// detection per T8) and inline fragments.
// computeQueryDepth is kept for backward compatibility with tests.
func computeQueryDepth(query string) (int, error) {
	doc, err := parser.Parse(parser.ParseParams{Source: query})
	if err != nil {
		return 0, err
	}
	return computeQueryDepthFromAST(doc), nil
}

// computeQueryDepthFromAST computes the maximum selection depth from a pre-parsed AST.
func computeQueryDepthFromAST(doc *ast.Document) int {
	fragments := make(map[string]*ast.FragmentDefinition)
	for _, def := range doc.Definitions {
		if frag, ok := def.(*ast.FragmentDefinition); ok {
			fragments[frag.Name.Value] = frag
		}
	}

	maxDepth := 0
	for _, def := range doc.Definitions {
		if op, ok := def.(*ast.OperationDefinition); ok {
			depth := selectionSetDepth(op.SelectionSet, fragments, make(map[string]bool))
			if depth > maxDepth {
				maxDepth = depth
			}
		}
	}
	return maxDepth
}

// selectionSetDepth recursively computes the depth of a selection set.
// visited tracks fragment names to prevent infinite recursion on cyclic fragments.
func selectionSetDepth(ss *ast.SelectionSet, fragments map[string]*ast.FragmentDefinition, visited map[string]bool) int {
	if ss == nil {
		return 0
	}
	maxDepth := 0
	for _, sel := range ss.Selections {
		var childDepth int
		switch s := sel.(type) {
		case *ast.Field:
			childDepth = 1 + selectionSetDepth(s.SelectionSet, fragments, visited)
		case *ast.InlineFragment:
			childDepth = selectionSetDepth(s.SelectionSet, fragments, visited)
		case *ast.FragmentSpread:
			name := s.Name.Value
			if visited[name] {
				continue // Cycle guard (T8)
			}
			if frag, ok := fragments[name]; ok {
				visited[name] = true
				childDepth = selectionSetDepth(frag.SelectionSet, fragments, visited)
				delete(visited, name) // Allow visiting from other paths
			}
		}
		if childDepth > maxDepth {
			maxDepth = childDepth
		}
	}
	return maxDepth
}

// astContainsIntrospection checks a pre-parsed AST for introspection fields.
func astContainsIntrospection(doc *ast.Document) bool {
	fragments := make(map[string]*ast.FragmentDefinition)
	for _, def := range doc.Definitions {
		if frag, ok := def.(*ast.FragmentDefinition); ok {
			fragments[frag.Name.Value] = frag
		}
	}

	for _, def := range doc.Definitions {
		if op, ok := def.(*ast.OperationDefinition); ok {
			if selectionSetHasIntrospection(op.SelectionSet, fragments, make(map[string]bool)) {
				return true
			}
		}
	}
	return false
}

// selectionSetHasIntrospection recursively checks whether any field in the
// selection set is a GraphQL introspection field.
func selectionSetHasIntrospection(ss *ast.SelectionSet, fragments map[string]*ast.FragmentDefinition, visited map[string]bool) bool {
	if ss == nil {
		return false
	}
	for _, sel := range ss.Selections {
		switch s := sel.(type) {
		case *ast.Field:
			// __typename is intentionally not blocked; it's a meta-field, not introspection.
			if s.Name.Value == "__schema" || s.Name.Value == "__type" {
				return true
			}
			if selectionSetHasIntrospection(s.SelectionSet, fragments, visited) {
				return true
			}
		case *ast.InlineFragment:
			if selectionSetHasIntrospection(s.SelectionSet, fragments, visited) {
				return true
			}
		case *ast.FragmentSpread:
			name := s.Name.Value
			if visited[name] {
				continue
			}
			if frag, ok := fragments[name]; ok {
				visited[name] = true
				if selectionSetHasIntrospection(frag.SelectionSet, fragments, visited) {
					return true
				}
				delete(visited, name)
			}
		}
	}
	return false
}

// responseTracker wraps http.ResponseWriter to track whether headers/body
// have already been written, so that panic recovery can avoid writing to
// an already-committed response.
type responseTracker struct {
	http.ResponseWriter
	headerWritten bool
}

func (rt *responseTracker) WriteHeader(code int) {
	rt.headerWritten = true
	rt.ResponseWriter.WriteHeader(code)
}

func (rt *responseTracker) Write(b []byte) (int, error) {
	rt.headerWritten = true
	return rt.ResponseWriter.Write(b)
}

func (rt *responseTracker) Flush() {
	if f, ok := rt.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (rt *responseTracker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := rt.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("underlying ResponseWriter does not support Hijack")
}

// recoverMiddleware catches panics in handlers and returns a 500 JSON error
// instead of crashing the process. If headers have already been written,
// only logging is performed. The client will see a truncated response.
func recoverMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rt := &responseTracker{ResponseWriter: w}
		defer func() {
			if rec := recover(); rec != nil {
				stack := debug.Stack()
				if len(stack) > 4096 {
					stack = stack[:4096]
				}
				slog.Error("panic recovered", "error", rec, "stack", string(stack))
				if !rt.headerWritten {
					rt.ResponseWriter.Header().Set("Content-Type", "application/json")
					rt.ResponseWriter.WriteHeader(http.StatusInternalServerError)
					_, _ = rt.ResponseWriter.Write([]byte(`{"error":"internal server error","error_code":"internal_error"}`))
				}
				// If headers already written, we can only log. The client will see a truncated response.
			}
		}()
		next(rt, r)
	}
}

// requestIDMiddleware assigns a unique request ID to each request (or reuses
// an incoming X-Request-ID header) and logs structured request metadata.
// T4: validates incoming X-Request-ID against a strict regex to prevent log injection.
// T5: handles rand.Read errors with a UnixNano fallback.
func requestIDMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get("X-Request-ID")
		if reqID == "" || !validRequestID.MatchString(reqID) {
			b := make([]byte, 8)
			if _, err := rand.Read(b); err != nil {
				reqID = fmt.Sprintf("%d", time.Now().UnixNano())
			} else {
				reqID = hex.EncodeToString(b)
			}
		}
		w.Header().Set("X-Request-ID", reqID)

		start := time.Now()
		rw := &statusWriter{ResponseWriter: w, status: 200}
		next(rw, r)

		// O15: Redact credential-bearing query parameters from access logs.
		sanitizedPath := redactSensitiveParams(r.URL.RequestURI())

		ip := r.RemoteAddr
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			ip = host
		}

		slog.Info("request", //nolint:gosec // G706: structured logging with sanitized path, no injection risk
			"method", r.Method,
			"path", sanitizedPath,
			"status", rw.status,
			"duration", time.Since(start).String(),
			"request_id", reqID,
			"ip", ip,
		)
	}
}

// redactSensitiveParams removes credential-related query parameters from URLs before logging.
func redactSensitiveParams(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := u.Query()
	for _, key := range []string{"api_key", "apikey", "token", "secret", "password"} {
		if q.Has(key) {
			q.Set(key, "[REDACTED]")
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Flush / Hijack passthrough so gzip, SSE, etc. keep working when wrapped.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

// handlerLabel returns a bounded, low-cardinality label for a URL path, for
// use in metrics and span names. Unknown paths collapse to "other".
func handlerLabel(path string) string {
	switch path {
	case "/graphql":
		return "graphql"
	case "/healthz":
		return "healthz"
	case "/readyz", "/health":
		return "readyz"
	case "/status":
		return "status"
	case "/metrics":
		return "metrics"
	default:
		return "other"
	}
}

// instrument wraps an http.HandlerFunc to record Prometheus metrics keyed by
// a stable handler label (not the raw URL path, to bound cardinality).
func instrument(label string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sw, ok := w.(*statusWriter)
		if !ok {
			sw = &statusWriter{ResponseWriter: w, status: http.StatusOK}
			w = sw
		}
		start := time.Now()
		next(w, r)
		recordHTTPMetrics(label, r.Method, sw.status, time.Since(start), sw.bytes)
	}
}

// writeError writes a structured JSON error response.
func writeError(w http.ResponseWriter, status int, code string, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":      message,
		"error_code": code,
	})
}

// securityHeadersMiddleware sets standard security response headers on every response.
func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'")
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// corsMiddleware handles CORS preflight and sets Access-Control headers.
// allowedOrigins is a comma-separated list of allowed origins.
func corsMiddleware(next http.Handler, allowedOrigins string) http.Handler {
	origins := make(map[string]bool)
	for _, o := range strings.Split(allowedOrigins, ",") {
		o = strings.TrimSpace(o)
		if o != "" {
			origins[o] = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// T2: Always set Vary: Origin so caches key on origin even for non-CORS requests.
		w.Header().Add("Vary", "Origin")

		origin := r.Header.Get("Origin")
		if origin != "" && (origins["*"] || origins[origin]) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Api-Key, X-Request-ID")
			w.Header().Set("Access-Control-Max-Age", "86400")
		}
		if r.Method == http.MethodOptions {
			// T2: Preflight responses should also vary on these headers.
			w.Header().Add("Vary", "Access-Control-Request-Method")
			w.Header().Add("Vary", "Access-Control-Request-Headers")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
