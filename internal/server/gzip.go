package server

import (
	"bufio"
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// gzipMinSize is the smallest response we bother compressing. Below this,
// the overhead of setting up gzip outweighs the bandwidth savings.
const gzipMinSize = 1024

var gzipWriterPool = sync.Pool{
	New: func() interface{} {
		// Level 5 delivers ~95% of the compression of level 6 at ~2x throughput.
		// For JSON API responses this is the right tradeoff.
		w, _ := gzip.NewWriterLevel(io.Discard, 5)
		return w
	},
}

// gzipMiddleware transparently gzip-compresses responses when the client sends
// Accept-Encoding: gzip, the response is not already encoded, and the buffered
// body meets gzipMinSize. It preserves streaming semantics via http.Flusher
// and connection hijacking via http.Hijacker.
func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !clientAcceptsGzip(r) {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponseWriter{ResponseWriter: w, minSize: gzipMinSize}
		defer gw.Close()
		next.ServeHTTP(gw, r)
	})
}

func clientAcceptsGzip(r *http.Request) bool {
	ae := r.Header.Get("Accept-Encoding")
	if ae == "" {
		return false
	}
	for _, part := range strings.Split(ae, ",") {
		if strings.EqualFold(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]), "gzip") {
			return true
		}
	}
	return false
}

// gzipResponseWriter buffers writes until it has seen enough bytes to decide
// whether to compress. It also respects an upstream Content-Encoding header
// (passing pre-compressed responses through untouched).
type gzipResponseWriter struct {
	http.ResponseWriter

	minSize      int
	status       int
	wroteHeader  bool
	buf          []byte
	passThrough  bool // true → do not compress (too small, or pre-encoded)
	gz           *gzip.Writer
}

func (g *gzipResponseWriter) WriteHeader(status int) {
	g.status = status
	// Do not call the underlying WriteHeader yet. We may still decide to
	// compress and need to mutate headers. Flush header on first body write.
}

func (g *gzipResponseWriter) Write(p []byte) (int, error) {
	// Skip compression for responses that must not have a body (RFC 9110).
	if !g.passThrough && g.gz == nil && (g.status == http.StatusNoContent || g.status == http.StatusNotModified || (g.status >= 100 && g.status < 200)) {
		g.passThrough = true
	}
	if g.passThrough {
		if !g.wroteHeader {
			g.writeHeaderNow()
		}
		return g.ResponseWriter.Write(p)
	}
	if g.gz != nil {
		return g.gz.Write(p)
	}

	// Still buffering. If the upstream handler set a Content-Encoding (e.g.
	// already compressed) or an explicit small Content-Length, bail out.
	if g.ResponseWriter.Header().Get("Content-Encoding") != "" {
		g.passThrough = true
		g.writeHeaderNow()
		if len(g.buf) > 0 {
			if _, err := g.ResponseWriter.Write(g.buf); err != nil {
				return 0, err
			}
			g.buf = nil
		}
		return g.ResponseWriter.Write(p)
	}
	if cl := g.ResponseWriter.Header().Get("Content-Length"); cl != "" {
		if n, err := strconv.Atoi(cl); err == nil && n < g.minSize {
			g.passThrough = true
			g.writeHeaderNow()
			if len(g.buf) > 0 {
				if _, err := g.ResponseWriter.Write(g.buf); err != nil {
					return 0, err
				}
				g.buf = nil
			}
			return g.ResponseWriter.Write(p)
		}
	}

	g.buf = append(g.buf, p...)
	if len(g.buf) < g.minSize {
		return len(p), nil
	}

	// Threshold crossed, start gzip.
	g.startGzip()
	n, err := g.gz.Write(g.buf)
	_ = n
	g.buf = nil
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func (g *gzipResponseWriter) startGzip() {
	h := g.ResponseWriter.Header()
	h.Set("Content-Encoding", "gzip")
	h.Del("Content-Length") // unknown after compression
	h.Add("Vary", "Accept-Encoding")
	g.writeHeaderNow()

	gz := gzipWriterPool.Get().(*gzip.Writer)
	gz.Reset(g.ResponseWriter)
	g.gz = gz
}

func (g *gzipResponseWriter) writeHeaderNow() {
	if g.wroteHeader {
		return
	}
	g.wroteHeader = true
	status := g.status
	if status == 0 {
		status = http.StatusOK
	}
	g.ResponseWriter.WriteHeader(status)
}

// Close flushes any buffered body. Called by the middleware via defer.
func (g *gzipResponseWriter) Close() error {
	if g.gz != nil {
		err := g.gz.Close()
		gzipWriterPool.Put(g.gz)
		g.gz = nil
		return err
	}
	// Body was smaller than minSize, or no body at all.
	g.passThrough = true
	if !g.wroteHeader {
		g.writeHeaderNow()
	}
	if len(g.buf) > 0 {
		_, err := g.ResponseWriter.Write(g.buf)
		g.buf = nil
		return err
	}
	return nil
}

// Flush implements http.Flusher. When compressing, we flush the gzip writer
// first so partial data reaches the wire in order.
func (g *gzipResponseWriter) Flush() {
	if g.gz != nil {
		_ = g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack implements http.Hijacker for upgrades (WebSockets etc.). We must
// disable compression for a hijacked connection.
func (g *gzipResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := g.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	g.passThrough = true
	return h.Hijack()
}
