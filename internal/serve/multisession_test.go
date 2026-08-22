package serve

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reasonix/internal/billing"
	"reasonix/internal/boot"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/event"
)

// replayStubController stubs the pending-prompt replay to observe what
// replayPendingPromptsBroadcast pushes onto the stream.
type replayStubController struct {
	*control.Controller
}

func (replayStubController) SessionPath() string { return "/sessions/a.jsonl" }

func (replayStubController) ReplayPendingPromptsWith(sinkFn func() event.Sink) {
	sink := sinkFn()
	sink.Emit(event.Event{Kind: event.ApprovalRequest})
}

// TestReplayPendingPromptsBroadcastTagsFrames pins the switch-back card
// recovery: pending prompts re-emitted after a resume carry the current
// session tag so the desktop's long-lived pump routes them to the frontend.
func TestReplayPendingPromptsBroadcastTagsFrames(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "a.jsonl")
	saveServeTestSession(t, active)

	bc := NewBroadcaster()
	ctrl := control.New(control.Options{Sink: bc, SessionDir: dir, SessionPath: active})
	server := New(replayStubController{ctrl}, bc, config.ServeConfig{})

	all, cancel := bc.Subscribe(true)
	defer cancel()
	server.replayPendingPromptsBroadcast()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case data := <-all:
			if strings.Contains(string(data), `"sessionPath":"/sessions/a.jsonl"`) {
				return // tagged pending prompt reached the all-sessions stream
			}
		case <-deadline:
			t.Fatal("tagged pending prompt not broadcast")
		}
	}
}

// healTestController wraps a real controller as a background session that is
// mid-turn on a stale credential channel: it reports running until Cancel
// flips it idle, exactly like a turn retrying a dead reverse-tunnel port.
type healTestController struct {
	*control.Controller
	canceled atomic.Bool
}

func (h *healTestController) RuntimeStatus() control.RuntimeStatus {
	if h.canceled.Load() {
		return control.RuntimeStatus{}
	}
	return control.RuntimeStatus{Running: true}
}

func (h *healTestController) Cancel() {
	h.canceled.Store(true)
	h.Controller.Cancel()
}

// TestProvidersReloadRetiresStaleDetached reproduces the multi-session
// credential heal end to end: a busy background controller built before a
// reverse-port drift keeps dialing the dead old port. POST /providers/reload
// (what the desktop calls after healing the config) must cancel that doomed
// background turn so the close-on-idle watcher retires the controller — no
// session may re-attach later to a stale provider.
func TestProvidersReloadRetiresStaleDetached(t *testing.T) {
	dir := t.TempDir()
	aPath := filepath.Join(dir, "a.jsonl")
	bPath := filepath.Join(dir, "b.jsonl")
	saveServeTestSession(t, aPath)
	saveServeTestSession(t, bPath)

	bc := NewBroadcaster()
	ctrlA := control.New(control.Options{Sink: bc, SessionDir: dir, SessionPath: aPath})
	server := New(ctrlA, bc, config.ServeConfig{})
	leases := control.NewSessionLeaseKeeper()
	defer leases.Release()
	if err := leases.Rebind(aPath); err != nil {
		t.Fatal(err)
	}
	if err := leases.BindControllerAuthority(ctrlA); err != nil {
		t.Fatal(err)
	}
	server.SetSessionLeases(leases)
	server.buildController = func(_ context.Context, _ string, _ boot.Options) (*control.Controller, error) {
		return control.New(control.Options{Sink: bc, SessionDir: dir}), nil
	}

	// A busy background controller over session b, holding b's lease — the
	// pre-drift state of a session switched away from mid-turn.
	stale := &healTestController{Controller: control.New(control.Options{Sink: bc, SessionDir: dir, SessionPath: bPath})}
	staleKeeper := control.NewSessionLeaseKeeper()
	if err := staleKeeper.Rebind(bPath); err != nil {
		t.Fatal(err)
	}
	server.registerDetached(stale, staleKeeper, nil)

	srv := httptest.NewServer(server.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/providers/reload", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("providers/reload status = %d (body %s)", resp.StatusCode, body)
	}
	if !stale.canceled.Load() {
		t.Fatal("provider heal must cancel the doomed background controller (stale reverse-tunnel port)")
	}
	// The watcher retires it once idle; its session's lease frees up again.
	server.WaitForDetachedIdle()
	if _, err := agent.TryAcquireSessionLease(bPath); err != nil {
		t.Fatalf("background lease not released after heal retire: %v", err)
	}
}

// TestBuildTaggedInheritsSessionDir pins the workspace placement fix: a
// replacement controller built for a busy switch (or model switch) must
// inherit the current controller's SessionDir and WorkspaceRoot — boot falls
// back to the GLOBAL session dir when opts.SessionDir is empty, which
// silently moved switched-to sessions into ~/.reasonix/sessions and flipped
// the /sessions listing to the wrong directory.
func TestBuildTaggedInheritsSessionDir(t *testing.T) {
	dir := t.TempDir()
	projDir := filepath.Join(dir, "projects", "demo", "sessions")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(projDir, "a.jsonl")
	saveServeTestSession(t, active)

	bc := NewBroadcaster()
	ctrl := control.New(control.Options{Sink: bc, SessionDir: projDir, WorkspaceRoot: dir})
	server := New(ctrl, bc, config.ServeConfig{})

	var gotOpts boot.Options
	server.buildController = func(_ context.Context, _ string, opts boot.Options) (*control.Controller, error) {
		gotOpts = opts
		return control.New(control.Options{Sink: bc, SessionDir: opts.SessionDir, WorkspaceRoot: opts.WorkspaceRoot}), nil
	}
	built, _, err := server.buildTagged(context.Background(), "some/model", false)
	if err != nil {
		t.Fatal(err)
	}
	if gotOpts.SessionDir != projDir {
		t.Fatalf("build opts SessionDir = %q, want the current controller's %q", gotOpts.SessionDir, projDir)
	}
	if gotOpts.WorkspaceRoot != dir {
		t.Fatalf("build opts WorkspaceRoot = %q, want %q", gotOpts.WorkspaceRoot, dir)
	}
	if built.SessionDir() != projDir {
		t.Fatalf("built controller SessionDir = %q, want %q", built.SessionDir(), projDir)
	}
}

// balanceProbeController counts Balance calls so we can prove the desktop
// runtime-status path does not pay for wallet fetches.
type balanceProbeController struct {
	*control.Controller
	balanceCalls atomic.Int32
}

func (c *balanceProbeController) Balance(ctx context.Context) (*billing.Balance, error) {
	c.balanceCalls.Add(1)
	return &billing.Balance{Available: true}, nil
}

func (c *balanceProbeController) RuntimeStatus() control.RuntimeStatus {
	return control.RuntimeStatus{Running: true, PendingPrompt: true, Cancellable: true}
}

// TestStatusRuntimeQuerySkipsBalance pins the remote hydrate/reconcile contract:
// GET /status?runtime=1 returns running-state fields without calling Balance,
// matching the local ListTabs/RuntimeStatus path that never mixes wallet I/O
// into session-surface refresh.
func TestStatusRuntimeQuerySkipsBalance(t *testing.T) {
	bc := NewBroadcaster()
	base := control.New(control.Options{Sink: bc})
	ctrl := &balanceProbeController{Controller: base}
	server := New(ctrl, bc, config.ServeConfig{})
	srv := httptest.NewServer(server.Handler())
	defer srv.Close()

	// Full status still may fetch balance.
	resp, err := http.Get(srv.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	fullBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("full /status = %d %s", resp.StatusCode, fullBody)
	}
	if ctrl.balanceCalls.Load() == 0 {
		t.Fatal("full /status should still call Balance")
	}
	before := ctrl.balanceCalls.Load()

	resp, err = http.Get(srv.URL + "/status?runtime=1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("runtime /status = %d %s", resp.StatusCode, body)
	}
	if ctrl.balanceCalls.Load() != before {
		t.Fatalf("runtime /status called Balance (%d -> %d)", before, ctrl.balanceCalls.Load())
	}
	if strings.Contains(string(body), `"balance"`) {
		t.Fatalf("runtime /status leaked balance payload: %s", body)
	}
	for _, want := range []string{`"running":true`, `"pendingPrompt":true`, `"cancellable":true`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("runtime /status missing %s in %s", want, body)
		}
	}
}
