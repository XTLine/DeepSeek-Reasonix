package serve

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"reasonix/internal/agent"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/store"
)

func postureTestServer(t *testing.T, dir, aPath string) (*httptest.Server, *control.Controller) {
	t.Helper()
	bc := NewBroadcaster()
	exec := agent.New(nil, nil, agent.NewSession("sys"), agent.Options{}, bc)
	ctrl := control.New(control.Options{Executor: exec, Sink: bc, SessionDir: dir, SessionPath: aPath})
	server := New(ctrl, bc, config.ServeConfig{})
	leases := control.NewSessionLeaseKeeper()
	t.Cleanup(leases.Release)
	if err := leases.Rebind(aPath); err != nil {
		t.Fatal(err)
	}
	if err := server.SetSessionLeases(leases); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.Handler())
	t.Cleanup(srv.Close)
	return srv, ctrl
}

func postureMode(t *testing.T, srv *httptest.Server, mode string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"mode": mode})
	resp, err := http.Post(srv.URL+"/tool-approval-mode", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	respBody, _ := readAll(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /tool-approval-mode status = %d body %q", resp.StatusCode, respBody)
	}
}

func currentServeMode(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	resp, err := http.Get(srv.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var status map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	mode, _ := status["toolApprovalMode"].(string)
	return mode
}

func resumeServe(t *testing.T, srv *httptest.Server, path string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"path": path})
	resp, err := http.Post(srv.URL+"/resume", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	respBody, _ := readAll(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /resume status = %d body %q", resp.StatusCode, respBody)
	}
}

func assertPostureSidecar(t *testing.T, path, wantMode string) {
	t.Helper()
	raw, err := os.ReadFile(store.SessionPostureState(path))
	if err != nil {
		t.Fatalf("read posture sidecar of %s: %v", path, err)
	}
	var file struct {
		ToolApprovalMode string `json:"toolApprovalMode"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("decode posture sidecar of %s: %v", path, err)
	}
	if file.ToolApprovalMode != wantMode {
		t.Fatalf("posture sidecar of %s mode = %q, want %q (raw %s)", path, file.ToolApprovalMode, wantMode, raw)
	}
}

// TestServeSessionSwitchRestoresEachSessionOwnPosture reproduces the remote
// issue: switching serve sessions must not leak one session's yolo grant onto
// another, and coming back to a session that recorded yolo must restore it.
func TestServeSessionSwitchRestoresEachSessionOwnPosture(t *testing.T) {
	dir := t.TempDir()
	aPath := filepath.Join(dir, "a.jsonl")
	bPath := filepath.Join(dir, "b.jsonl")
	saveServeTestSession(t, aPath)
	saveServeTestSession(t, bPath)
	srv, _ := postureTestServer(t, dir, aPath)

	postureMode(t, srv, control.ToolApprovalYolo)
	assertPostureSidecar(t, aPath, control.ToolApprovalYolo)
	if got := currentServeMode(t, srv); got != control.ToolApprovalYolo {
		t.Fatalf("mode after yolo = %q", got)
	}

	// Session b never recorded a posture; it inherits the running posture
	// without writing (a sidecar appears only once b's own axes change).
	resumeServe(t, srv, bPath)
	if got := currentServeMode(t, srv); got != control.ToolApprovalYolo {
		t.Fatalf("mode on unrecorded session b = %q, want inherited yolo", got)
	}
	if _, err := os.Stat(store.SessionPostureState(bPath)); !os.IsNotExist(err) {
		t.Fatal("resume onto b wrote a sidecar before b changed anything")
	}

	// b records its own posture; switching back to a must restore a's yolo
	// instead of inheriting b's ask.
	postureMode(t, srv, control.ToolApprovalAsk)
	assertPostureSidecar(t, bPath, control.ToolApprovalAsk)
	resumeServe(t, srv, aPath)
	if got := currentServeMode(t, srv); got != control.ToolApprovalYolo {
		t.Fatalf("mode back on a = %q, want its recorded yolo", got)
	}
	resumeServe(t, srv, bPath)
	if got := currentServeMode(t, srv); got != control.ToolApprovalAsk {
		t.Fatalf("mode back on b = %q, want its recorded ask", got)
	}
}

// TestServeModeSwitchPersistsAcrossSessionRebind guards the controller hooks:
// an explicit mode change writes the bound session's sidecar, and a fresh
// controller binding the same path (process restart) restores it.
func TestServeModeSwitchPersistsAcrossSessionRebind(t *testing.T) {
	dir := t.TempDir()
	aPath := filepath.Join(dir, "a.jsonl")
	saveServeTestSession(t, aPath)
	srv, _ := postureTestServer(t, dir, aPath)

	postureMode(t, srv, control.ToolApprovalYolo)
	assertPostureSidecar(t, aPath, control.ToolApprovalYolo)

	// A second controller binding the same session restores yolo.
	bc := NewBroadcaster()
	exec := agent.New(nil, nil, agent.NewSession("sys"), agent.Options{}, bc)
	ctrl2 := control.New(control.Options{Executor: exec, Sink: bc, SessionDir: dir, SessionPath: aPath})
	defer ctrl2.Close()
	if got := ctrl2.ToolApprovalMode(); got != control.ToolApprovalYolo {
		t.Fatalf("rebound controller mode = %q, want yolo from sidecar", got)
	}
}
