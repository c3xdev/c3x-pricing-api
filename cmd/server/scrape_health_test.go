package main

import (
	"strings"
	"testing"
)

// TestEmptyScrapeErrorFlagsEmptyRuns locks the rule that let a dead GCP
// key sit unnoticed for a month: a scrape that ingests nothing is a
// failure, no matter how quietly the individual services failed.
func TestEmptyScrapeErrorFlagsEmptyRuns(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		products       int
		failedServices int64
		wantErr        bool
		wantContains   string
	}{
		{
			// The real GCP outage: an invalid key makes every service
			// fail, each one is logged and swallowed, so the scraper
			// returns no error and ingests nothing.
			name: "no products, every service failed", products: 0, failedServices: 42,
			wantErr: true, wantContains: "42 service(s) failed",
		},
		{
			name: "no products, no service errors", products: 0, failedServices: 0,
			wantErr: true, wantContains: "0 products",
		},
		{
			name: "products ingested", products: 2422585, failedServices: 0,
			wantErr: false,
		},
		{
			// Partial data is still a success: one flaky service must not
			// fail an otherwise good run.
			name: "partial data with some failures", products: 1000, failedServices: 3,
			wantErr: false,
		},
		{
			name: "a single product is enough", products: 1, failedServices: 0,
			wantErr: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := emptyScrapeError(tc.products, tc.failedServices)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("emptyScrapeError(%d, %d) = nil, want an error",
						tc.products, tc.failedServices)
				}
				if !strings.Contains(err.Error(), tc.wantContains) {
					t.Errorf("error %q does not mention %q", err, tc.wantContains)
				}
				if !strings.Contains(err.Error(), "credentials") {
					t.Errorf("error %q should point at the likely cause", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("emptyScrapeError(%d, %d) = %v, want nil",
					tc.products, tc.failedServices, err)
			}
		})
	}
}
