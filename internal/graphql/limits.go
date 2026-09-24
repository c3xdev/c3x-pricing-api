package graphql

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/c3xdev/c3x-pricing-api/internal/db"
)

// ProductQuerier is the database surface the schema needs. *db.DB
// satisfies it; tests substitute a fake.
type ProductQuerier interface {
	QueryProducts(ctx context.Context, filter *db.ProductFilter) ([]db.Product, error)
}

// ErrFilterTooBroad rejects a products query without both vendorName and
// service. Every lookup the c3x CLI sends carries both (its query builder
// always writes them), and without them a query is a scan across vendors
// or a whole vendor, which is the expensive shape this guard exists for.
var ErrFilterTooBroad = errors.New("products filter must set both vendorName and service")

// validateProductFilter enforces the minimum selectivity of a products query.
func validateProductFilter(f *db.ProductFilter) error {
	if f.VendorName == nil || *f.VendorName == "" || f.Service == nil || *f.Service == "" {
		return ErrFilterTooBroad
	}
	return nil
}

// RequestBudget bounds the database work one HTTP request can cause,
// summed over every `products` field in the request: aliases inside one
// document and every item of a batch. The server creates one per request
// and attaches it with WithBudget; with none attached, resolvers are
// unbounded (as in unit tests).
type RequestBudget struct {
	mu           sync.Mutex
	productsLeft int
	queriesLeft  int
}

// NewRequestBudget returns a budget allowing at most maxQueries products
// fields returning at most maxProducts products in total. Non-positive
// values disable the corresponding cap.
func NewRequestBudget(maxProducts, maxQueries int) *RequestBudget {
	// Internally -1 means unlimited and 0 means spent.
	if maxProducts <= 0 {
		maxProducts = -1
	}
	if maxQueries <= 0 {
		maxQueries = -1
	}
	return &RequestBudget{productsLeft: maxProducts, queriesLeft: maxQueries}
}

type budgetKey struct{}

// WithBudget attaches b to ctx for the resolvers to draw on.
func WithBudget(ctx context.Context, b *RequestBudget) context.Context {
	return context.WithValue(ctx, budgetKey{}, b)
}

func budgetFrom(ctx context.Context) *RequestBudget {
	b, _ := ctx.Value(budgetKey{}).(*RequestBudget)
	return b
}

// reserve claims one query and up to `limit` products. It returns the
// limit to apply (clamped to what is left) or an error once a cap is
// spent. The caller must settle the reservation with refund.
func (b *RequestBudget) reserve(limit int) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.queriesLeft == 0 {
		return 0, fmt.Errorf("request exceeds the maximum number of products queries")
	}
	if b.productsLeft == 0 {
		return 0, fmt.Errorf("request exceeds the maximum number of products returned")
	}
	if b.queriesLeft > 0 {
		b.queriesLeft--
	}
	if b.productsLeft > 0 {
		if limit > b.productsLeft {
			limit = b.productsLeft
		}
		b.productsLeft -= limit
	}
	return limit, nil
}

// refund returns the unused part of a reservation.
func (b *RequestBudget) refund(reserved, used int) {
	if used >= reserved {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.productsLeft >= 0 {
		b.productsLeft += reserved - used
	}
}
