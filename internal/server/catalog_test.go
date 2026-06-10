package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCatalogEndpointServesBundle(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	handleCatalog(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/catalog", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var bundle struct {
		SchemaVersion string `json:"schema_version"`
		Hash          string `json:"hash"`
		Count         int    `json:"count"`
		Entries       []struct {
			Provider string `json:"provider"`
			Kind     string `json:"kind"`
			TOML     string `json:"toml"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &bundle); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if bundle.Count < 300 || len(bundle.Entries) != bundle.Count {
		t.Errorf("count = %d entries = %d", bundle.Count, len(bundle.Entries))
	}
	if bundle.SchemaVersion != "1" || bundle.Hash == "" {
		t.Errorf("schema=%q hash=%q", bundle.SchemaVersion, bundle.Hash)
	}
	if etag := rec.Header().Get("ETag"); etag != `"`+bundle.Hash+`"` {
		t.Errorf("ETag %q != hash %q", etag, bundle.Hash)
	}
}

func TestCatalogEndpoint304OnMatch(t *testing.T) {
	t.Parallel()
	first := httptest.NewRecorder()
	handleCatalog(first, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/catalog", nil))
	etag := first.Header().Get("ETag")

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/catalog", nil)
	req.Header.Set("If-None-Match", etag)
	rec := httptest.NewRecorder()
	handleCatalog(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Errorf("status = %d, want 304", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Error("304 must have empty body")
	}
}
