package serve

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"reasonix/internal/contract/config"
)

type decisionService struct {
	mu    sync.Mutex
	paths []string
}

func (d *decisionService) record(r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.paths = append(d.paths, r.Method+" "+r.URL.Path)
}

func (d *decisionService) seen() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.paths...)
}

func decisionServer(t *testing.T, systemOne http.HandlerFunc) (*httptest.Server, *decisionService) {
	t.Helper()
	svc := &decisionService{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { svc.record(r); w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /v1/systemone", func(w http.ResponseWriter, r *http.Request) { svc.record(r); systemOne(w, r) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		svc.record(r)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"Not Found"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, svc
}

func checkSavedDecisionProvider(t *testing.T, baseURL string) map[string]any {
	t.Helper()
	t.Setenv("DECISION_API_KEY", "decision-key-0123456789")
	s := newProviderEditServer(t)
	edit := config.LoadForEdit(config.UserConfigPath())
	entry, ok := edit.Provider("existing")
	if !ok {
		t.Fatal("fixture provider is missing")
	}
	entry.Kind = "typesafe"
	entry.BaseURL = baseURL
	entry.Models = []string{"jev-latest"}
	entry.Default = "jev-latest"
	entry.APIKeyEnv = "DECISION_API_KEY"
	if err := edit.SaveTo(config.UserConfigPath()); err != nil {
		t.Fatal(err)
	}
	s.AllowProviderEdit()
	srv := httptest.NewServer(operatorHandler(s))
	defer srv.Close()
	return decodeCheck(t, postProvider(t, srv.URL, "/providers/check", `{"name":"existing"}`))
}

func TestCheckProviderVerifiesADecisionSourceThroughItsOwnContract(t *testing.T) {
	srv, svc := decisionServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"answers":{"probe":{"type":"noul","noul":0.5}}}`))
	})
	got := checkSavedDecisionProvider(t, srv.URL)
	if got["ok"] != true || got["kind"] != "typesafe" || got["matches"] != true {
		t.Fatalf("check = %v; want a passing typesafe check", got)
	}
	for _, p := range svc.seen() {
		if p != "POST /v1/systemone" {
			t.Fatalf("decision source was asked for %q; only its own endpoint is part of the contract (%v)", p, svc.seen())
		}
	}
	if len(svc.seen()) == 0 {
		t.Fatal("the decision endpoint was never asked")
	}
}

func TestCheckProviderNamesWhyADecisionSourceFailed(t *testing.T) {
	tests := []struct {
		name   string
		reply  http.HandlerFunc
		code   string
		status float64
	}{
		{"path missing", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }, "provider.probe.decision_path_not_found", 404},
		{"key refused", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }, "provider.probe.unauthorized", 401},
		{"not a decision answer", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`<html>hi</html>`)) }, "provider.probe.decision_not_compatible", 0},
		{"answer without verdict", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"answers":{}}`)) }, "provider.probe.decision_not_compatible", 0},
		{"upstream broken", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) }, "provider.probe.upstream_error", 502},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := decisionServer(t, tc.reply)
			got := checkSavedDecisionProvider(t, srv.URL)
			if got["ok"] == true || got["code"] != tc.code {
				t.Fatalf("check = %v; want code %s", got, tc.code)
			}
			if status, _ := got["httpStatus"].(float64); status != tc.status {
				t.Fatalf("httpStatus = %v, want %v", got["httpStatus"], tc.status)
			}
		})
	}
}

func TestCheckProviderDecisionSourceUnreachable(t *testing.T) {
	srv, _ := decisionServer(t, func(http.ResponseWriter, *http.Request) {})
	url := srv.URL
	srv.Close()
	got := checkSavedDecisionProvider(t, url)
	if got["ok"] == true || got["code"] != "provider.probe.unreachable" {
		t.Fatalf("check = %v; want provider.probe.unreachable", got)
	}
}

func decodeCheck(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	return got
}
