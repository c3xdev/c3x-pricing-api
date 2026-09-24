package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/c3xdev/c3x-pricing-api/internal/config"
	"github.com/c3xdev/c3x-pricing-api/internal/db"
	c3xgql "github.com/c3xdev/c3x-pricing-api/internal/graphql"
)

// recordingDB is a fake products store: it records each filter and returns
// `rows` products (capped by the effective limit, like the real query),
// each carrying an on-demand and a reserved price.
type recordingDB struct {
	mu      sync.Mutex
	rows    int
	filters []db.ProductFilter
}

func (f *recordingDB) QueryProducts(_ context.Context, filter *db.ProductFilter) ([]db.Product, error) {
	f.mu.Lock()
	f.filters = append(f.filters, *filter)
	f.mu.Unlock()
	n := f.rows
	if l := db.EffectiveLimit(filter.Limit); n > l {
		n = l
	}
	out := make([]db.Product, n)
	for i := range out {
		out[i] = db.Product{
			ProductHash: fmt.Sprintf("h%d", i), SKU: "S", VendorName: "aws", Service: "svc",
			Prices: []db.Price{
				{PriceHash: "p1", PurchaseOption: "on_demand", Unit: "Hrs", USD: "0.192"},
				{PriceHash: "p2", PurchaseOption: "reserved", Unit: "Hrs", USD: "0.12"},
			},
		}
	}
	return out, nil
}

// prodConfig mirrors the production docker-compose settings.
func prodConfig() *config.Config {
	return &config.Config{
		MaxRequestBodyMB:            4,
		MaxBatchSize:                50,
		QueryTimeoutSecs:            30,
		MaxQueryDepth:               10,
		DisableIntrospection:        true,
		MaxProductsPerRequest:       1000,
		MaxProductQueriesPerRequest: 50,
		InflightWaitMillis:          10,
	}
}

func newLimitsServer(t *testing.T, fake *recordingDB, cfg *config.Config) *Server {
	t.Helper()
	schema, err := c3xgql.NewSchema(fake)
	if err != nil {
		t.Fatal(err)
	}
	return &Server{schema: schema, cfg: cfg}
}

func postGraphQL(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/graphql", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleGraphQL(w, req)
	return w
}

type gqlResp struct {
	Data struct {
		Products []struct {
			Prices []struct {
				USD  string `json:"USD"`
				Unit string `json:"unit"`
			} `json:"prices"`
		} `json:"products"`
		Typename string `json:"__typename"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// cliBodies are the exact request bodies the c3x CLI sends, captured from
// its query builder (internal/pricing/http.go buildQuery + jsonQuote) and
// from `c3x doctor`. Every one must keep working under the production
// limits.
var cliBodies = []struct {
	name       string
	body       string
	wantFilter func(t *testing.T, f db.ProductFilter)
	wantPrices int
}{
	{
		name: "global region, no family, no attrs, no price filter",
		body: `{"query":"{products(filter:{vendorName:\"aws\",service:\"AmazonCloudFront\"},limit:50){prices{USD unit}}}"}`,
		wantFilter: func(t *testing.T, f db.ProductFilter) {
			if f.Region != nil || f.ProductFamily != nil || len(f.AttributeFilters) != 0 {
				t.Errorf("unexpected filter fields: %+v", f)
			}
		},
		wantPrices: 2,
	},
	{
		name: "full shape: family, region, attributes, purchaseOption+unit",
		body: `{"query":"{products(filter:{vendorName:\"aws\",service:\"AmazonEC2\",productFamily:\"Compute Instance\",region:\"us-east-1\",attributeFilters:[{key:\"instanceType\",value:\"m5.xlarge\"},{key:\"operatingSystem\",value:\"Linux\"},{key:\"tenancy\",value:\"Shared\"}]},limit:50){prices(filter:{purchaseOption:\"on_demand\",unit:\"Hrs\"}){USD unit}}}"}`,
		wantFilter: func(t *testing.T, f db.ProductFilter) {
			if *f.ProductFamily != "Compute Instance" || *f.Region != "us-east-1" || len(f.AttributeFilters) != 3 ||
				f.AttributeFilters[0].Key != "instanceType" || *f.AttributeFilters[0].Value != "m5.xlarge" {
				t.Errorf("unexpected filter: %+v", f)
			}
		},
		wantPrices: 1,
	},
	{
		name:       "purchaseOption only",
		body:       `{"query":"{products(filter:{vendorName:\"azure\",service:\"Virtual Machines\",region:\"eastus\"},limit:50){prices(filter:{purchaseOption:\"Consumption\"}){USD unit}}}"}`,
		wantFilter: func(t *testing.T, f db.ProductFilter) {},
		wantPrices: 0, // fake prices are on_demand/reserved
	},
	{
		name:       "unit only",
		body:       `{"query":"{products(filter:{vendorName:\"gcp\",service:\"Cloud Storage\",productFamily:\"Storage\",region:\"us-central1\"},limit:50){prices(filter:{unit:\"GiBy.mo\"}){USD unit}}}"}`,
		wantFilter: func(t *testing.T, f db.ProductFilter) {},
		wantPrices: 0,
	},
	{
		name: "escaped attribute value",
		body: `{"query":"{products(filter:{vendorName:\"aws\",service:\"AmazonS3\",region:\"us-east-1\",attributeFilters:[{key:\"volumeType\",value:\"Quote\\\"d\\\\Back\"}]},limit:50){prices{USD unit}}}"}`,
		wantFilter: func(t *testing.T, f db.ProductFilter) {
			if got := *f.AttributeFilters[0].Value; got != `Quote"d\Back` {
				t.Errorf("attribute value = %q", got)
			}
		},
		wantPrices: 2,
	},
}

func TestCLIQueryShapes_WorkUnderProductionLimits(t *testing.T) {
	for _, tc := range cliBodies {
		t.Run(tc.name, func(t *testing.T) {
			fake := &recordingDB{rows: 50}
			s := newLimitsServer(t, fake, prodConfig())
			w := postGraphQL(t, s, tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			var r gqlResp
			if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
				t.Fatal(err)
			}
			if len(r.Errors) != 0 {
				t.Fatalf("errors: %+v", r.Errors)
			}
			if len(r.Data.Products) != 50 {
				t.Fatalf("got %d products, want all 50 the CLI asked for", len(r.Data.Products))
			}
			if got := len(r.Data.Products[0].Prices); got != tc.wantPrices {
				t.Fatalf("got %d prices, want %d", got, tc.wantPrices)
			}
			if len(fake.filters) != 1 || fake.filters[0].Limit != 50 {
				t.Fatalf("want one DB query with limit 50, got %+v", fake.filters)
			}
			f := fake.filters[0]
			if f.VendorName == nil || f.Service == nil {
				t.Fatal("vendorName/service not passed through")
			}
			tc.wantFilter(t, f)
		})
	}
}

func TestCLIDoctorProbe_WorksWithIntrospectionDisabled(t *testing.T) {
	s := newLimitsServer(t, &recordingDB{}, prodConfig())
	w := postGraphQL(t, s, `{"query":"{ __typename }"}`)
	var r gqlResp
	_ = json.Unmarshal(w.Body.Bytes(), &r)
	if w.Code != http.StatusOK || len(r.Errors) != 0 || r.Data.Typename != "Query" {
		t.Fatalf("doctor probe: status=%d body=%s", w.Code, w.Body)
	}
}

func TestIntrospection_BlockedInProductionConfig(t *testing.T) {
	s := newLimitsServer(t, &recordingDB{}, prodConfig())
	w := postGraphQL(t, s, `{"query":"{ __schema { types { name } } }"}`)
	if !strings.Contains(w.Body.String(), "Introspection is disabled") {
		t.Fatalf("introspection not blocked: %s", w.Body)
	}
	cfg := prodConfig()
	cfg.DisableIntrospection = false
	s = newLimitsServer(t, &recordingDB{}, cfg)
	w = postGraphQL(t, s, `{"query":"{ __schema { types { name } } }"}`)
	if strings.Contains(w.Body.String(), "Introspection is disabled") || !strings.Contains(w.Body.String(), "ProductFilter") {
		t.Fatalf("introspection should work when enabled: %s", w.Body)
	}
}

func TestProductsWithoutVendorAndService_Rejected(t *testing.T) {
	fake := &recordingDB{rows: 5}
	s := newLimitsServer(t, fake, prodConfig())
	w := postGraphQL(t, s, `{"query":"{ products(filter: {vendorName: \"azure\"}, limit: 1) { productHash } }"}`)
	if !strings.Contains(w.Body.String(), "vendorName and service") || len(fake.filters) != 0 {
		t.Fatalf("broad query not rejected: %s", w.Body)
	}
}

func batchBody(n int, item string) string {
	items := make([]string, n)
	for i := range items {
		items[i] = item
	}
	return "[" + strings.Join(items, ",") + "]"
}

func TestBatch_SizeCap(t *testing.T) {
	item := cliBodies[0].body
	s := newLimitsServer(t, &recordingDB{rows: 1}, prodConfig())
	if w := postGraphQL(t, s, batchBody(50, item)); w.Code != http.StatusOK {
		t.Fatalf("batch of 50 must pass: %d %s", w.Code, w.Body)
	}
	w := postGraphQL(t, s, batchBody(51, item))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "batch_too_large") {
		t.Fatalf("batch of 51 must be rejected: %d %s", w.Code, w.Body)
	}
}

func TestBatch_ProductBudgetSpansItems(t *testing.T) {
	fake := &recordingDB{rows: 50}
	s := newLimitsServer(t, fake, prodConfig())
	w := postGraphQL(t, s, batchBody(30, cliBodies[0].body))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var rs []gqlResp
	if err := json.Unmarshal(w.Body.Bytes(), &rs); err != nil {
		t.Fatal(err)
	}
	total, errored := 0, 0
	for _, r := range rs {
		total += len(r.Data.Products)
		if len(r.Errors) > 0 {
			errored++
		}
	}
	// 1000-product budget / 50 per item = 20 items served, 10 refused.
	if total != 1000 || errored != 10 {
		t.Fatalf("total=%d errored=%d, want 1000 / 10", total, errored)
	}
	if len(fake.filters) != 20 {
		t.Fatalf("refused items must not reach the DB: %d queries", len(fake.filters))
	}
}

func TestAliases_QueryCountCap(t *testing.T) {
	fake := &recordingDB{rows: 0}
	s := newLimitsServer(t, fake, prodConfig())
	var q bytes.Buffer
	q.WriteString("{")
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&q, `a%d: products(filter:{vendorName:"aws",service:"AmazonEC2"},limit:1){sku} `, i)
	}
	q.WriteString("}")
	body, _ := json.Marshal(map[string]string{"query": q.String()})
	w := postGraphQL(t, s, string(body))
	if len(fake.filters) != 50 {
		t.Fatalf("want 50 DB queries (the per-request cap), got %d", len(fake.filters))
	}
	if !strings.Contains(w.Body.String(), "maximum number of products queries") {
		t.Fatalf("missing cap error: %s", w.Body)
	}
}

func TestInflightLimit(t *testing.T) {
	cases := []struct{ configured, pool, want int }{
		{0, 0, 0},   // no pool info: unbounded
		{0, 4, 2},   // pgx default pool on 4 CPUs
		{0, 20, 18}, // production DB_MAX_CONNS
		{0, 2, 1},   // never below one
		{8, 20, 8},  // explicit wins
		{-1, 20, 0}, // explicitly disabled
	}
	for _, c := range cases {
		if got := inflightLimit(c.configured, c.pool); got != c.want {
			t.Errorf("inflightLimit(%d, %d) = %d, want %d", c.configured, c.pool, got, c.want)
		}
	}
}

func TestInflightMiddleware_503WhenSaturated(t *testing.T) {
	s := &Server{cfg: prodConfig(), inflight: make(chan struct{}, 1)}
	release := make(chan struct{})
	entered := make(chan struct{})
	h := s.inflightMiddleware(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	})

	go func() {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/graphql", nil)
		h(httptest.NewRecorder(), req)
	}()
	<-entered

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/graphql", nil)
	w := httptest.NewRecorder()
	h(w, req)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Fatalf("saturated: status=%d Retry-After=%q", w.Code, w.Header().Get("Retry-After"))
	}

	close(release)
	// The slot is freed once the first request finishes.
	done := make(chan int)
	go func() {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/graphql", nil)
		w := httptest.NewRecorder()
		h(w, req)
		done <- w.Code
	}()
	<-entered
	if code := <-done; code != http.StatusOK {
		t.Fatalf("after release: status=%d", code)
	}
}

func TestMetrics_NotOnPublicMux(t *testing.T) {
	s := &Server{cfg: prodConfig()}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	s.publicHandler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("public /metrics: status %d, want 404", w.Code)
	}

	req = httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil)
	w = httptest.NewRecorder()
	metricsHandler().ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "c3x_graphql_queries_total") {
		t.Fatalf("metrics listener: status %d", w.Code)
	}
}
