package config

import (
	"strings"
	"testing"
)

func TestValidate_SSLModeProductionGuard(t *testing.T) {
	tests := []struct {
		name        string
		env         string
		databaseURL string
		wantErr     bool
	}{
		{"dev allows disable", "development", "postgres://u:p@h/db?sslmode=disable", false},
		{"prod rejects disable", "production", "postgres://u:p@h/db?sslmode=disable", true},
		{"prod rejects uppercase", "production", "postgres://u:p@h/db?sslmode=DISABLE", true},
		{"prod rejects allow", "production", "postgres://u:p@h/db?sslmode=allow", true},
		{"prod rejects prefer", "production", "postgres://u:p@h/db?sslmode=prefer", true},
		{"prod accepts require", "production", "postgres://u:p@h/db?sslmode=require", false},
		{"prod accepts verify-full", "production", "postgres://u:p@h/db?sslmode=verify-full", false},
		{"prod ignores sslmode inside password", "production", "postgres://u:sslmode%3Ddisable@h/db?sslmode=require", false},
		{"prod rejects keyword form disable", "production", "host=h user=u password=p dbname=db sslmode=disable", true},
		{"prod accepts keyword form require", "production", "host=h user=u password=p dbname=db sslmode=require", false},
		{"prod env comparison is case-insensitive", "PRODUCTION", "postgres://u:p@h/db?sslmode=disable", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{DatabaseURL: tc.databaseURL, Env: tc.env}
			err := c.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() err=%v, wantErr=%v", err, tc.wantErr)
			}
			if tc.wantErr && err != nil && !strings.Contains(err.Error(), "sslmode") {
				t.Fatalf("expected sslmode error, got: %v", err)
			}
		})
	}
}

func TestValidate_MissingDatabaseURL(t *testing.T) {
	c := &Config{}
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for missing DATABASE_URL")
	}
}

func TestLoad_IntrospectionDefaultFollowsEnv(t *testing.T) {
	t.Setenv("DISABLE_INTROSPECTION", "")
	t.Setenv("ENV", "production")
	if !Load().DisableIntrospection {
		t.Fatal("introspection must default to disabled in production")
	}
	t.Setenv("ENV", "development")
	if Load().DisableIntrospection {
		t.Fatal("introspection must default to enabled outside production")
	}
	t.Setenv("ENV", "production")
	t.Setenv("DISABLE_INTROSPECTION", "false")
	if Load().DisableIntrospection {
		t.Fatal("DISABLE_INTROSPECTION=false must override the production default")
	}
}

func TestLoad_MetricsAddr(t *testing.T) {
	tests := []struct {
		name, addr, port, want string
	}{
		{"default is localhost only", "", "", DefaultMetricsAddr},
		{"explicit addr", "0.0.0.0:9100", "", "0.0.0.0:9100"},
		{"addr wins over legacy port", "127.0.0.1:9100", "9200", "127.0.0.1:9100"},
		{"legacy port", "", "9200", ":9200"},
		{"legacy port 0 disables", "", "0", "off"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("METRICS_ADDR", tc.addr)
			t.Setenv("METRICS_PORT", tc.port)
			if got := Load().MetricsAddr; got != tc.want {
				t.Fatalf("MetricsAddr=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestLoad_RequestLimitDefaults(t *testing.T) {
	for _, k := range []string{"MAX_BATCH_SIZE", "MAX_PRODUCTS_PER_REQUEST",
		"MAX_PRODUCT_QUERIES_PER_REQUEST", "MAX_INFLIGHT_REQUESTS", "INFLIGHT_WAIT_MS"} {
		t.Setenv(k, "")
	}
	c := Load()
	// The c3x CLI sends one products query per request with limit:50 and
	// never batches; every default must stay comfortably above that.
	if c.MaxBatchSize != 50 {
		t.Errorf("MaxBatchSize=%d, want 50", c.MaxBatchSize)
	}
	if c.MaxProductsPerRequest != 1000 {
		t.Errorf("MaxProductsPerRequest=%d, want 1000", c.MaxProductsPerRequest)
	}
	if c.MaxProductQueriesPerRequest != 50 {
		t.Errorf("MaxProductQueriesPerRequest=%d, want 50", c.MaxProductQueriesPerRequest)
	}
	if c.MaxInflightRequests != 0 || c.InflightWaitMillis != 250 {
		t.Errorf("inflight defaults = %d/%dms, want 0 (auto)/250ms", c.MaxInflightRequests, c.InflightWaitMillis)
	}
}
