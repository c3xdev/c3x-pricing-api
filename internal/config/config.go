package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Port              string
	DatabaseURL       string
	APIKey            string
	GCPAPIKey         string
	ScrapeConcurrency int
	// ScrapeConcurrencyAWS/Azure/GCP optionally override ScrapeConcurrency on a
	// per-vendor basis. Useful because vendor APIs have very different fan-out
	// economics (AWS bulk JSON vs Azure paged REST vs GCP SKU list). 0 means
	// "inherit the global ScrapeConcurrency".
	ScrapeConcurrencyAWS   int
	ScrapeConcurrencyAzure int
	ScrapeConcurrencyGCP   int
	LogLevel          string
	MaxRequestBodyMB  int
	MaxBatchSize      int
	QueryTimeoutSecs  int
	RateLimitPerSec   int
	// H9: maximum GraphQL query selection depth. Guards against deeply nested
	// abuse queries (e.g. 100-deep aliased chains). Override via MAX_QUERY_DEPTH.
	MaxQueryDepth int
	// H9: when true, reject queries containing GraphQL introspection fields
	// (__schema, __type). Useful to reduce attack surface in production.
	DisableIntrospection bool
	// CNYUSDRate is the CNY-to-USD exchange rate used for AWS China region pricing.
	// Default 6.2069 (approximate rate as of 2024). Override via CNY_USD_RATE env var.
	CNYUSDRate float64
	// CORSOrigins is a comma-separated list of allowed CORS origins.
	// If empty, CORS headers are not set. Override via CORS_ALLOWED_ORIGINS.
	CORSOrigins string
	// DBMaxConns sets the maximum number of connections in the pgx pool.
	// Default 0 means pgxpool's own default (max(4, numCPU)).
	DBMaxConns int
	// DBMinConns sets the minimum number of connections in the pgx pool.
	DBMinConns int
	// Env is the deployment environment (e.g. "development", "production").
	Env string
	// MetricsPort serves /metrics on a separate port for security isolation.
	// Empty or "0" = serve on the main port (backward compatible).
	MetricsPort string
}

func Load() *Config {
	return &Config{
		Port:                 getEnv("PORT", "4000"),
		DatabaseURL:          getEnv("DATABASE_URL", ""),
		APIKey:               getEnv("API_KEY", ""),
		GCPAPIKey:            getEnv("GCP_API_KEY", ""),
		ScrapeConcurrency:      getEnvInt("SCRAPE_CONCURRENCY", 4),
		ScrapeConcurrencyAWS:   getEnvInt("SCRAPE_CONCURRENCY_AWS", 0),
		ScrapeConcurrencyAzure: getEnvInt("SCRAPE_CONCURRENCY_AZURE", 8),
		ScrapeConcurrencyGCP:   getEnvInt("SCRAPE_CONCURRENCY_GCP", 0),
		LogLevel:             getEnv("LOG_LEVEL", "info"),
		MaxRequestBodyMB:     getEnvInt("MAX_REQUEST_BODY_MB", 4),
		MaxBatchSize:         getEnvInt("MAX_BATCH_SIZE", 100),
		QueryTimeoutSecs:     getEnvInt("QUERY_TIMEOUT_SECS", 30),
		RateLimitPerSec:      getEnvInt("RATE_LIMIT_PER_SEC", 100),
		MaxQueryDepth:        getEnvInt("MAX_QUERY_DEPTH", 10),
		DisableIntrospection: getEnvBool("DISABLE_INTROSPECTION", false),
		CNYUSDRate:           getEnvFloat("CNY_USD_RATE", 6.2069),
		CORSOrigins:         getEnv("CORS_ALLOWED_ORIGINS", ""),
		DBMaxConns:           getEnvInt("DB_MAX_CONNS", 0),
		DBMinConns:           getEnvInt("DB_MIN_CONNS", 0),
		Env:                  getEnv("ENV", "development"),
		MetricsPort:          getEnv("METRICS_PORT", ""),
	}
}

// Validate checks that required configuration values are set.
func (c *Config) Validate() error {
	if c.DatabaseURL == "" {
		return errMissingConfig("DATABASE_URL")
	}
	// O13: Prevent insecure SSL mode in production. Parse the URL properly so
	// substrings inside passwords or other fields cannot produce false positives
	// and URL-encoded / uppercase variants cannot bypass the check.
	if strings.EqualFold(c.Env, "production") {
		sslmode, err := extractSSLMode(c.DatabaseURL)
		if err != nil {
			return fmt.Errorf("DATABASE_URL: %w", err)
		}
		if sslmode == "disable" || sslmode == "allow" || sslmode == "prefer" {
			return fmt.Errorf("DATABASE_URL: sslmode=%s is forbidden in production; use require, verify-ca, or verify-full", sslmode)
		}
	}
	return nil
}

// extractSSLMode returns the effective libpq sslmode for a connection string,
// handling both URL form (postgres://…?sslmode=require) and keyword/value form
// (host=… sslmode=require). Returns "" when sslmode is unset.
func extractSSLMode(connStr string) (string, error) {
	s := strings.TrimSpace(connStr)
	if strings.HasPrefix(s, "postgres://") || strings.HasPrefix(s, "postgresql://") {
		u, err := url.Parse(s)
		if err != nil {
			return "", fmt.Errorf("invalid URL: %w", err)
		}
		return strings.ToLower(u.Query().Get("sslmode")), nil
	}
	// Keyword/value form: space-separated key=value pairs.
	for _, kv := range strings.Fields(s) {
		if eq := strings.IndexByte(kv, '='); eq > 0 {
			if strings.EqualFold(kv[:eq], "sslmode") {
				return strings.ToLower(kv[eq+1:]), nil
			}
		}
	}
	return "", nil
}

type configError struct {
	field string
}

func errMissingConfig(field string) error {
	return &configError{field: field}
}


// ConcurrencyForVendor returns the effective scrape concurrency for a vendor.
// It honors per-vendor overrides (SCRAPE_CONCURRENCY_{AWS,AZURE,GCP}) and falls
// back to the global ScrapeConcurrency. Unknown vendors also fall back.
func (c *Config) ConcurrencyForVendor(vendor string) int {
	switch strings.ToLower(vendor) {
	case "aws":
		if c.ScrapeConcurrencyAWS > 0 {
			return c.ScrapeConcurrencyAWS
		}
	case "azure":
		if c.ScrapeConcurrencyAzure > 0 {
			return c.ScrapeConcurrencyAzure
		}
	case "gcp":
		if c.ScrapeConcurrencyGCP > 0 {
			return c.ScrapeConcurrencyGCP
		}
	}
	return c.ScrapeConcurrency
}
func (e *configError) Error() string {
	return "required environment variable " + e.field + " is not set"
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return fallback
}

func getEnvFloat(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return fallback
}

func getEnvBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}
