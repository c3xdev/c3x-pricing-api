package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGzipMiddleware_CompressesLargeBody(t *testing.T) {
	big := strings.Repeat("cloud-pricing-payload-", 200) // ~4.4 KB

	handler := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(big))
	}))

	req := httptest.NewRequest(http.MethodGet, "/graphql", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if got := rr.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := rr.Header().Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length should be stripped, got %q", got)
	}
	if !strings.Contains(rr.Header().Get("Vary"), "Accept-Encoding") {
		t.Fatalf("Vary header missing Accept-Encoding: %q", rr.Header().Get("Vary"))
	}

	gr, err := gzip.NewReader(rr.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	got, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("read gz: %v", err)
	}
	if string(got) != big {
		t.Fatalf("decompressed body mismatch (len got=%d want=%d)", len(got), len(big))
	}
}

func TestGzipMiddleware_SkipsSmallBody(t *testing.T) {
	small := []byte(`{"ok":true}`)
	handler := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(small)
	}))

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if got := rr.Header().Get("Content-Encoding"); got == "gzip" {
		t.Fatal("small body should not be compressed")
	}
	if !bytes.Equal(rr.Body.Bytes(), small) {
		t.Fatalf("body mismatch: got %q want %q", rr.Body.Bytes(), small)
	}
}

func TestGzipMiddleware_NoAcceptEncoding(t *testing.T) {
	body := strings.Repeat("x", 4000)
	handler := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	}))

	req := httptest.NewRequest(http.MethodGet, "/graphql", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if got := rr.Header().Get("Content-Encoding"); got == "gzip" {
		t.Fatal("no Accept-Encoding gzip → must not compress")
	}
	if rr.Body.String() != body {
		t.Fatal("body was modified when no gzip negotiated")
	}
}

func TestGzipMiddleware_DoesNotDoubleCompress(t *testing.T) {
	// Upstream handler already set Content-Encoding (e.g., pre-compressed asset).
	payload := []byte("\x1f\x8b\x08pretending-to-be-gzip-already")
	handler := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(payload)
	}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if !bytes.Equal(rr.Body.Bytes(), payload) {
		t.Fatalf("body was re-encoded; got %q want %q", rr.Body.Bytes(), payload)
	}
}
