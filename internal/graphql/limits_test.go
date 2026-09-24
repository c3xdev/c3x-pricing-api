package graphql

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/c3xdev/c3x-pricing-api/internal/db"
	gql "github.com/graphql-go/graphql"
)

// fakeDB records every filter it is asked for and returns `rows` products
// (capped by the filter's effective limit, as the real query is).
type fakeDB struct {
	mu      sync.Mutex
	rows    int
	filters []db.ProductFilter
}

func (f *fakeDB) QueryProducts(_ context.Context, filter *db.ProductFilter) ([]db.Product, error) {
	f.mu.Lock()
	f.filters = append(f.filters, *filter)
	f.mu.Unlock()
	n := f.rows
	if l := db.EffectiveLimit(filter.Limit); n > l {
		n = l
	}
	out := make([]db.Product, n)
	for i := range out {
		out[i] = db.Product{ProductHash: fmt.Sprintf("h%d", i), SKU: "s", VendorName: "aws", Service: "svc",
			Prices: []db.Price{{PriceHash: "p", Unit: "Hrs", USD: "0.1"}}}
	}
	return out, nil
}

func run(t *testing.T, f *fakeDB, ctx context.Context, q string) *gql.Result {
	t.Helper()
	schema, err := NewSchema(f)
	if err != nil {
		t.Fatal(err)
	}
	return gql.Do(gql.Params{Schema: schema, RequestString: q, Context: ctx})
}

func productsLen(t *testing.T, r *gql.Result, field string) int {
	t.Helper()
	data, _ := r.Data.(map[string]interface{})
	list, _ := data[field].([]interface{})
	return len(list)
}

func TestProducts_RequiresVendorAndService(t *testing.T) {
	cases := map[string]string{
		"no filter fields": `{products(filter:{}){sku}}`,
		"vendor only":      `{products(filter:{vendorName:"aws"}){sku}}`,
		"service only":     `{products(filter:{service:"AmazonEC2"}){sku}}`,
		"empty vendor":     `{products(filter:{vendorName:"",service:"AmazonEC2"}){sku}}`,
		"empty service":    `{products(filter:{vendorName:"aws",service:""}){sku}}`,
		"sku only":         `{products(filter:{sku:"ABC"}){sku}}`,
	}
	for name, q := range cases {
		t.Run(name, func(t *testing.T) {
			f := &fakeDB{rows: 1}
			r := run(t, f, context.Background(), q)
			if len(r.Errors) == 0 || !strings.Contains(r.Errors[0].Message, "vendorName and service") {
				t.Fatalf("want filter-too-broad error, got %+v", r.Errors)
			}
			if len(f.filters) != 0 {
				t.Fatal("rejected query must not reach the database")
			}
		})
	}
}

func TestProducts_BudgetCapsProductsAcrossAliases(t *testing.T) {
	f := &fakeDB{rows: 5000}
	ctx := WithBudget(context.Background(), NewRequestBudget(120, 50))
	q := `{
	  a: products(filter:{vendorName:"aws",service:"s"},limit:50){sku}
	  b: products(filter:{vendorName:"aws",service:"s"},limit:50){sku}
	  c: products(filter:{vendorName:"aws",service:"s"},limit:50){sku}
	  d: products(filter:{vendorName:"aws",service:"s"},limit:50){sku}
	}`
	r := run(t, f, ctx, q)
	// Field execution order is the executor's business, so check the
	// multiset: two full aliases, one clamped to the 20 left, one refused.
	counts := map[int]int{}
	for _, a := range []string{"a", "b", "c", "d"} {
		counts[productsLen(t, r, a)]++
	}
	if counts[50] != 2 || counts[20] != 1 || counts[0] != 1 {
		t.Fatalf("per-alias product counts = %v, want two 50s, one 20, one 0", counts)
	}
	if len(r.Errors) != 1 || !strings.Contains(r.Errors[0].Message, "maximum number of products returned") {
		t.Fatalf("fourth alias must error once the budget is spent, got %+v", r.Errors)
	}
	if len(f.filters) != 3 {
		t.Fatalf("spent budget must not query the database: %d queries", len(f.filters))
	}
}

func TestProducts_BudgetRefundsUnusedRows(t *testing.T) {
	f := &fakeDB{rows: 1} // every lookup matches one product
	ctx := WithBudget(context.Background(), NewRequestBudget(100, 0))
	var b strings.Builder
	b.WriteString("{")
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&b, `q%d: products(filter:{vendorName:"aws",service:"s"},limit:50){sku} `, i)
	}
	b.WriteString("}")
	r := run(t, f, ctx, b.String())
	if len(r.Errors) != 0 {
		t.Fatalf("10 x 1 product fits a 100 budget once unused reservations are refunded: %+v", r.Errors)
	}
}

func TestProducts_BudgetCapsQueryCount(t *testing.T) {
	f := &fakeDB{rows: 0}
	ctx := WithBudget(context.Background(), NewRequestBudget(0, 3))
	var b strings.Builder
	b.WriteString("{")
	for i := 0; i < 5; i++ {
		fmt.Fprintf(&b, `q%d: products(filter:{vendorName:"aws",service:"s"}){sku} `, i)
	}
	b.WriteString("}")
	r := run(t, f, ctx, b.String())
	if len(f.filters) != 3 {
		t.Fatalf("want 3 database queries, got %d", len(f.filters))
	}
	if len(r.Errors) != 2 || !strings.Contains(r.Errors[0].Message, "maximum number of products queries") {
		t.Fatalf("want 2 query-cap errors, got %+v", r.Errors)
	}
}

func TestProducts_DefaultLimitClampedToBudget(t *testing.T) {
	f := &fakeDB{rows: 5000}
	ctx := WithBudget(context.Background(), NewRequestBudget(1000, 50))
	r := run(t, f, ctx, `{products(filter:{vendorName:"aws",service:"s"}){sku}}`)
	if len(r.Errors) != 0 {
		t.Fatal(r.Errors)
	}
	if got := f.filters[0].Limit; got != 1000 {
		t.Fatalf("unset limit must be clamped to the budget: limit=%d", got)
	}
	if productsLen(t, r, "products") != 1000 {
		t.Fatalf("got %d products", productsLen(t, r, "products"))
	}
}

func TestProducts_NoBudgetIsUnbounded(t *testing.T) {
	f := &fakeDB{rows: 3}
	r := run(t, f, context.Background(), `{products(filter:{vendorName:"aws",service:"s"},limit:50){sku}}`)
	if len(r.Errors) != 0 || productsLen(t, r, "products") != 3 || f.filters[0].Limit != 50 {
		t.Fatalf("errors=%v len=%d limit=%d", r.Errors, productsLen(t, r, "products"), f.filters[0].Limit)
	}
}
