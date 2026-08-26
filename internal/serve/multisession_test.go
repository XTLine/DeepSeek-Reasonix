package serve

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"reasonix/internal/agent"
	"reasonix/internal/billing"
	"reasonix/internal/boot"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/event"
)

type replayStubController struct{ *control.Controller }

func (replayStubController) SessionPath() string { return "/sessions/a.jsonl" }
func (replayStubController) ReplayPendingPromptsWith(factory func() event.Sink) {
	factory().Emit(event.Event{Kind: event.ApprovalRequest})
}

type closeProbeController struct {
	*control.Controller
	closed atomic.Bool
}

func (c *closeProbeController) Close() {
	c.closed.Store(true)
	c.Controller.Close()
}

func TestServerCloseClosesPublishedForegroundReplacement(t *testing.T) {
	bc := NewBroadcaster()
	first := &closeProbeController{Controller: control.New(control.Options{Sink: bc})}
	replacement := &closeProbeController{Controller: control.New(control.Options{Sink: bc})}
	server := New(first, bc, config.ServeConfig{})
	if !server.publishControllerSwap(first, replacement, replacement.SessionPath()) {
		t.Fatal("replacement publication failed")
	}
	server.Close()
	if !replacement.closed.Load() {
		t.Fatal("server shutdown left the published foreground controller open")
	}
}

func TestBusyNewRejectsUntaggedLegacyController(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.jsonl")
	bc := NewBroadcaster()
	ctrl := control.New(control.Options{Runner: blockingRunner{}, Sink: bc, SessionDir: dir, SessionPath: path})
	server := New(ctrl, bc, config.ServeConfig{})
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	defer ctrl.Close()
	ctrl.Submit("keep running")
	waitRunning(t, ctrl)
	resp, err := http.Post(httpServer.URL+"/new", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("untagged busy /new = %d, want 409: %s", resp.StatusCode, body)
	}
	if server.ctl() != control.SessionAPI(ctrl) || !ctrl.Running() {
		t.Fatal("legacy controller was detached without a session tag")
	}
	ctrl.Cancel()
	waitNotRunning(t, ctrl)
}

func TestReplayPendingPromptsBroadcastTagsFrames(t *testing.T) {
	bc := NewBroadcaster()
	ctrl := control.New(control.Options{Sink: bc})
	server := New(replayStubController{ctrl}, bc, config.ServeConfig{})
	all, stop := bc.SubscribeAll()
	defer stop()
	server.replayPendingPromptsBroadcast()
	select {
	case frame := <-all:
		if !strings.Contains(string(frame), `"sessionPath":"/sessions/a.jsonl"`) {
			t.Fatalf("replayed frame is untagged: %s", frame)
		}
	default:
		t.Fatal("pending prompt was not replayed")
	}
}

func TestBuildTaggedInheritsWorkspacePlacement(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "sessions")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bc := NewBroadcaster()
	ctrl := control.New(control.Options{Sink: bc, SessionDir: sessionDir, WorkspaceRoot: root})
	server := New(ctrl, bc, config.ServeConfig{})
	var got boot.Options
	server.buildControllerWithOptions = func(_ context.Context, _ string, opts boot.Options) (*control.Controller, error) {
		got = opts
		return control.New(control.Options{Sink: opts.Sink, SessionDir: opts.SessionDir, WorkspaceRoot: opts.WorkspaceRoot}), nil
	}
	built, _, err := server.buildTagged(context.Background(), "provider/model", false)
	if err != nil {
		t.Fatal(err)
	}
	defer built.Close()
	if got.SessionDir != sessionDir || got.WorkspaceRoot != root {
		t.Fatalf("build placement = (%q, %q), want (%q, %q)", got.SessionDir, got.WorkspaceRoot, sessionDir, root)
	}
}

type balanceProbeController struct {
	*control.Controller
	calls atomic.Int32
}

func (c *balanceProbeController) Balance(context.Context) (*billing.Balance, error) {
	c.calls.Add(1)
	return &billing.Balance{Available: true}, nil
}

func (c *balanceProbeController) RuntimeStatus() control.RuntimeStatus {
	return control.RuntimeStatus{Running: true, PendingPrompt: true, Cancellable: true}
}

func TestStatusRuntimeQuerySkipsBalance(t *testing.T) {
	bc := NewBroadcaster()
	ctrl := &balanceProbeController{Controller: control.New(control.Options{Sink: bc})}
	srv := httptest.NewServer(New(ctrl, bc, config.ServeConfig{}).Handler())
	defer srv.Close()

	full, err := http.Get(srv.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, full.Body)
	full.Body.Close()
	before := ctrl.calls.Load()
	if before == 0 {
		t.Fatal("full status did not fetch balance")
	}
	lite, err := http.Get(srv.URL + "/status?runtime=1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(lite.Body)
	lite.Body.Close()
	if ctrl.calls.Load() != before {
		t.Fatal("runtime status fetched balance")
	}
	for _, want := range []string{`"running":true`, `"pendingPrompt":true`, `"cancellable":true`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("runtime status missing %s: %s", want, body)
		}
	}
}

func TestDetachedRecoveryMovesRegistryKey(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.jsonl")
	recoveryPath := filepath.Join(dir, "old-recovery.jsonl")
	ctrl := control.New(control.Options{SessionDir: dir, SessionPath: oldPath})
	server := New(ctrl, NewBroadcaster(), config.ServeConfig{})
	detached := &detachedSession{path: oldPath, ctrl: ctrl}
	server.detached[oldPath] = detached
	if err := server.moveDetachedRecovery(ctrl, recoveryPath); err != nil {
		t.Fatal(err)
	}
	canonical := agent.CanonicalSessionPath(recoveryPath)
	if got := server.detached[canonical]; got != detached || detached.path != canonical {
		t.Fatalf("recovery registry = %+v path=%q", got, detached.path)
	}
	if server.detached[oldPath] != nil {
		t.Fatal("old detached registry key was retained")
	}
}

func TestBusyResumeDetachesAndReattachesRunningController(t *testing.T) {
	dir := t.TempDir()
	aPath := filepath.Join(dir, "a.jsonl")
	bPath := filepath.Join(dir, "b.jsonl")
	saveServeTestSession(t, aPath)
	saveServeTestSession(t, bPath)

	bc := NewBroadcaster()
	tag := NewSessionTagSink(bc)
	tag.SetPath(aPath)
	ctrlA := control.New(control.Options{Runner: blockingRunner{}, Sink: tag, SessionDir: dir, SessionPath: aPath, Label: "test"})
	server := New(ctrlA, bc, config.ServeConfig{})
	server.RegisterSessionTag(ctrlA, tag)
	leases := control.NewSessionLeaseKeeper()
	defer leases.Release()
	if err := leases.Rebind(aPath); err != nil {
		t.Fatal(err)
	}
	if err := server.SetSessionLeases(leases); err != nil {
		t.Fatal(err)
	}
	server.buildControllerWithOptions = func(_ context.Context, _ string, opts boot.Options) (*control.Controller, error) {
		return control.New(control.Options{Runner: blockingRunner{}, Sink: opts.Sink, SessionDir: opts.SessionDir, WorkspaceRoot: opts.WorkspaceRoot, Label: "test"}), nil
	}
	srv := httptest.NewServer(server.Handler())
	defer srv.Close()
	defer server.CloseBackground()

	ctrlA.Submit("keep running")
	waitRunning(t, ctrlA)
	postResume := func(path string) {
		payload, _ := json.Marshal(map[string]string{"path": path})
		resp, err := http.Post(srv.URL+"/resume", "application/json", strings.NewReader(string(payload)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("resume %q = %d: %s", path, resp.StatusCode, body)
		}
	}

	postResume(bPath)
	wantB, err := filepath.EvalSymlinks(bPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := filepath.Clean(server.ctl().SessionPath()); got != filepath.Clean(wantB) {
		t.Fatalf("foreground session = %q, want b", got)
	}
	if !ctrlA.Running() {
		t.Fatal("switched-away session stopped instead of running in background")
	}
	postResume(aPath)
	if server.ctl() != control.SessionAPI(ctrlA) {
		t.Fatal("reattach did not restore the original controller")
	}
	if !ctrlA.Running() {
		t.Fatal("running turn was lost during reattach")
	}
	ctrlA.Cancel()
	waitNotRunning(t, ctrlA)
}
