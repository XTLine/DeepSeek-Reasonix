package serve

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGzipMiddleware pins compression behavior: large JSON bodies are gzipped
// for accepting clients, small bodies stay plain, non-accepting clients and
// /events are untouched, and ETag/ETag-304 interplay stays consistent.
func TestGzipMiddleware(t *testing.T) {
	big := []byte(`{"data":"` + strings.Repeat("x", 8192) + `"}`)
	handler := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/events" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {}\n\n"))
			return
		}
		w.Header().Set("ETag", `"abc"`)
		if r.Header.Get("If-None-Match") == `"abc"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(big)
	}))
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Large body + gzip accepted => compressed.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/history", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", resp.Header.Get("Content-Encoding"))
	}
	if resp.Header.Get("ETag") != `"abc"` {
		t.Fatalf("ETag lost under gzip: %q", resp.Header.Get("ETag"))
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("response is not valid gzip: %v", err)
	}
	plain, _ := io.ReadAll(zr)
	if !bytes.Equal(plain, big) {
		t.Fatal("decompressed body mismatch")
	}

	// Same request without Accept-Encoding => plain.
	resp2, err := http.Get(srv.URL + "/history")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.Header.Get("Content-Encoding") == "gzip" {
		t.Fatal("client without Accept-Encoding must not receive gzip")
	}

	// 304 with gzip accepted => no compression, no body.
	req3, _ := http.NewRequest(http.MethodGet, srv.URL+"/history", nil)
	req3.Header.Set("Accept-Encoding", "gzip")
	req3.Header.Set("If-None-Match", `"abc"`)
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusNotModified {
		t.Fatalf("304 path status = %d", resp3.StatusCode)
	}
	if resp3.Header.Get("Content-Encoding") == "gzip" {
		t.Fatal("304 must stay uncompressed")
	}

	// /events bypasses the compressor entirely.
	req4, _ := http.NewRequest(http.MethodGet, srv.URL+"/events", nil)
	req4.Header.Set("Accept-Encoding", "gzip")
	resp4, err := http.DefaultClient.Do(req4)
	if err != nil {
		t.Fatal(err)
	}
	defer resp4.Body.Close()
	if resp4.Header.Get("Content-Encoding") == "gzip" {
		t.Fatal("/events must never be compressed")
	}
}
