package scraper

import (
	"context"

	"github.com/c3xdev/c3x-pricing-api/internal/db"
)

// ProductHandler is called with batches of products during scraping.
// Implementations should upsert the products to the database.
type ProductHandler func(ctx context.Context, products []db.Product) error

// Scraper defines the interface for cloud pricing data scrapers.
// ScrapeWithHandler streams products to the handler in batches instead of
// collecting everything in memory. This is essential for large services like
// EC2 which have millions of products across 100+ regions.
type Scraper interface {
	Name() string
	ScrapeWithHandler(ctx context.Context, handler ProductHandler) error
	// FailedServices returns the number of services that failed during the
	// last ScrapeWithHandler call. Used by the orchestrator to decide whether
	// stale product cleanup is safe.
	FailedServices() int64
}
