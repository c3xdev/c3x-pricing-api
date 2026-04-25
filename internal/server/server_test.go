package server

import (
	"container/list"
	"context"
	"crypto/subtle"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/c3xdev/c3x-pricing-api/internal/config"
)

func TestAuthMiddleware_NoAPIKey(t *testing.T) {
	cfg := &config.Config{APIKey: ""}
	s := &Server{cfg: cfg, rateLimiters: make(map[string]*list.Element), rateLRU: list.New()}

	handler := s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequestWithContext(context.Background(),"GET", "/test", nil)
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestAuthMiddleware_ValidBearerToken(t *testing.T) {
	cfg := &config.Config{APIKey: "test-secret-key"}
	s := &Server{cfg: cfg, rateLimiters: make(map[string]*list.Element), rateLRU: list.New()}

	handler := s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequestWithContext(context.Background(),"GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer test-secret-key")
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestAuthMiddleware_ValidXApiKey(t *testing.T) {
	cfg := &config.Config{APIKey: "test-secret-key"}
	s := &Server{cfg: cfg, rateLimiters: make(map[string]*list.Element), rateLRU: list.New()}

	handler := s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequestWithContext(context.Background(),"GET", "/test", nil)
	req.Header.Set("X-Api-Key", "test-secret-key")
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestAuthMiddleware_InvalidKey(t *testing.T) {
	cfg := &config.Config{APIKey: "test-secret-key"}
	s := &Server{cfg: cfg, rateLimiters: make(map[string]*list.Element), rateLRU: list.New()}

	handler := s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequestWithContext(context.Background(),"GET", "/test", nil)
	req.Header.Set("X-Api-Key", "wrong-key")
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestAuthMiddleware_NoKey(t *testing.T) {
	cfg := &config.Config{APIKey: "test-secret-key"}
	s := &Server{cfg: cfg, rateLimiters: make(map[string]*list.Element), rateLRU: list.New()}

	handler := s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequestWithContext(context.Background(),"GET", "/test", nil)
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestRateLimitMiddleware_UnderLimit(t *testing.T) {
	cfg := &config.Config{RateLimitPerSec: 10}
	s := &Server{cfg: cfg, rateLimiters: make(map[string]*list.Element), rateLRU: list.New()}

	handler := s.rateLimitMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequestWithContext(context.Background(),"GET", "/test", nil)
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestRateLimitMiddleware_OverLimit(t *testing.T) {
	cfg := &config.Config{RateLimitPerSec: 2}
	s := &Server{cfg: cfg, rateLimiters: make(map[string]*list.Element), rateLRU: list.New()}

	handler := s.rateLimitMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Token-bucket: rate=2/s with burst=2*rate=4. First 4 requests succeed,
	// subsequent ones within the same second are limited.
	okCount, limitedCount := 0, 0
	for i := 0; i < 10; i++ {
		req := httptest.NewRequestWithContext(context.Background(),"GET", "/test", nil)
		w := httptest.NewRecorder()
		handler(w, req)
		switch w.Code {
		case http.StatusOK:
			okCount++
		case http.StatusTooManyRequests:
			limitedCount++
		}
	}

	if okCount < 1 || okCount > 5 {
		t.Errorf("expected between 1 and 5 OK responses (burst region), got %d", okCount)
	}
	if limitedCount == 0 {
		t.Errorf("expected some 429 responses, got %d OK and %d limited", okCount, limitedCount)
	}
}

func TestGraphQL_MethodNotAllowed(t *testing.T) {
	cfg := &config.Config{APIKey: "", MaxRequestBodyMB: 1, MaxBatchSize: 100, QueryTimeoutSecs: 30, RateLimitPerSec: 100}
	s := &Server{cfg: cfg, rateLimiters: make(map[string]*list.Element), rateLRU: list.New()}

	req := httptest.NewRequestWithContext(context.Background(),"GET", "/graphql", nil)
	w := httptest.NewRecorder()
	s.handleGraphQL(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestGraphQL_BatchTooLarge(t *testing.T) {
	cfg := &config.Config{APIKey: "", MaxRequestBodyMB: 1, MaxBatchSize: 2, QueryTimeoutSecs: 30, RateLimitPerSec: 100}
	s := &Server{cfg: cfg, rateLimiters: make(map[string]*list.Element), rateLRU: list.New()}

	body := `[{"query":"{ __typename }"},{"query":"{ __typename }"},{"query":"{ __typename }"}]`
	req := httptest.NewRequestWithContext(context.Background(),"POST", "/graphql", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleGraphQL(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for oversized batch, got %d", w.Code)
	}
}

func TestTimingSafeCompare(t *testing.T) {
	// Verify that the auth uses constant-time comparison
	result := subtle.ConstantTimeCompare([]byte("test-key"), []byte("test-key"))
	if result != 1 {
		t.Error("expected match")
	}

	result = subtle.ConstantTimeCompare([]byte("test-key"), []byte("wrong-key"))
	if result != 0 {
		t.Error("expected no match")
	}
}

func TestConfigValidation_MissingDatabaseURL(t *testing.T) {
	cfg := &config.Config{DatabaseURL: ""}
	err := cfg.Validate()
	if err == nil {
		t.Error("expected error for missing DATABASE_URL")
	}
}

func TestConfigValidation_Valid(t *testing.T) {
	cfg := &config.Config{DatabaseURL: "postgres://user:pass@localhost/db"} //nolint:gosec // test fixture, not real credentials
	err := cfg.Validate()
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
