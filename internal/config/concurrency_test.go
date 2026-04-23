package config

import (
	"testing"
)

func TestConcurrencyForVendor(t *testing.T) {
	cases := []struct {
		name   string
		cfg    Config
		vendor string
		want   int
	}{
		{
			name: "no override falls back to global",
			cfg:  Config{ScrapeConcurrency: 4},
			vendor: "aws", want: 4,
		},
		{
			name: "aws override wins",
			cfg:  Config{ScrapeConcurrency: 4, ScrapeConcurrencyAWS: 16},
			vendor: "aws", want: 16,
		},
		{
			name: "azure override wins",
			cfg:  Config{ScrapeConcurrency: 4, ScrapeConcurrencyAzure: 2},
			vendor: "azure", want: 2,
		},
		{
			name: "gcp override wins",
			cfg:  Config{ScrapeConcurrency: 4, ScrapeConcurrencyGCP: 8},
			vendor: "gcp", want: 8,
		},
		{
			name: "case insensitive vendor",
			cfg:  Config{ScrapeConcurrency: 4, ScrapeConcurrencyAWS: 16},
			vendor: "AWS", want: 16,
		},
		{
			name: "unknown vendor falls back",
			cfg:  Config{ScrapeConcurrency: 4, ScrapeConcurrencyAWS: 16},
			vendor: "oracle", want: 4,
		},
		{
			name: "zero override is ignored (inherits global)",
			cfg:  Config{ScrapeConcurrency: 4, ScrapeConcurrencyAWS: 0},
			vendor: "aws", want: 4,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.ConcurrencyForVendor(tc.vendor); got != tc.want {
				t.Fatalf("ConcurrencyForVendor(%q) = %d, want %d", tc.vendor, got, tc.want)
			}
		})
	}
}
