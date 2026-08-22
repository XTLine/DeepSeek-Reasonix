package serve

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"
)

// gzipThreshold is the smallest response body worth compressing. Snapshot
// members — a long session's /history in particular — marshal to multi-KiB
// JSON that shrinks roughly 10x over the SSH tunnel; status lines and small
// errors stay plain.
const gzipThreshold = 1024

var gzipWriterPool = sync.Pool{New: func() any { return gzip.NewWriter(io.Discard) }}

// gzipMiddleware compresses JSON responses for clients that accept gzip (the
// desktop's Go HTTP stack and browsers decompress transparently). Bodies are
// buffered until they pass the threshold so small responses stay plain;
// /events (SSE) bypasses compression entirely — frames must hit the wire as
// they are produced.
func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/events" || !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipBufferedWriter{ResponseWriter: w}
		defer gw.close()
		next.ServeHTTP(gw, r)
	})
}

// gzipBufferedWriter buffers the response until the threshold, then swaps to
// a gzip stream. WriteHeader is deferred so headers (including the ETag the
// caching layer sets) are written once the encoding is known.
type gzipBufferedWriter struct {
	http.ResponseWriter
	buf     bytes.Buffer
	gz      *gzip.Writer
	started bool
	status  int
}

func (g *gzipBufferedWriter) WriteHeader(code int) {
	g.status = code
}

func (g *gzipBufferedWriter) Write(p []byte) (int, error) {
	if g.gz != nil {
		return g.gz.Write(p)
	}
	g.started = true
	g.buf.Write(p)
	if g.buf.Len() >= gzipThreshold {
		g.beginGzip()
	}
	return len(p), nil
}

func (g *gzipBufferedWriter) beginGzip() {
	h := g.ResponseWriter.Header()
	h.Set("Content-Encoding", "gzip")
	h.Add("Vary", "Accept-Encoding")
	h.Del("Content-Length")
	if g.status == 0 {
		g.status = http.StatusOK
	}
	g.ResponseWriter.WriteHeader(g.status)
	g.gz = gzipWriterPool.Get().(*gzip.Writer)
	g.gz.Reset(g.ResponseWriter)
	g.buf.WriteTo(g.gz)
	g.buf.Reset()
}

// Flush streams buffered/flushing content; for still-buffered small bodies it
// only forwards the flush (handlers that flush produce stream-style output
// and have already bypassed or will stay plain).
func (g *gzipBufferedWriter) Flush() {
	if g.gz != nil {
		_ = g.gz.Flush()
	}
	if flusher, ok := g.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (g *gzipBufferedWriter) close() {
	if g.gz != nil {
		_ = g.gz.Close()
		gzipWriterPool.Put(g.gz)
		g.gz = nil
		return
	}
	if g.status == 0 {
		g.status = http.StatusOK
	}
	g.ResponseWriter.WriteHeader(g.status)
	g.buf.WriteTo(g.ResponseWriter)
}
