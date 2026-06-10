package server

// /catalog serves the resource knowledge base: every TOML definition
// as one JSON bundle. The c3x CLI fetches this on estimate runs (and
// caches against the ETag), making the API — not the CLI binary —
// the source of truth for which resources are supported and how
// they price. Supports conditional requests so the common case (a
// warm client) is a 304 with no body.

import (
	"log/slog"
	"net/http"

	"github.com/c3xdev/c3x-pricing-api/catalog"
)

func handleCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, hash, _, err := catalog.BundleJSON()
	if err != nil {
		slog.Error("catalog bundle build failed", "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	etag := `"` + hash + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", "application/json")
	// Clients may cache for an hour without revalidating; the ETag
	// makes longer-lived caches cheap to revalidate after that.
	w.Header().Set("Cache-Control", "public, max-age=3600")
	if match := r.Header.Get("If-None-Match"); match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(body)
}
