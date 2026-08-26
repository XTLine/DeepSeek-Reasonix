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
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/billing"
	"reasonix/internal/boot"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/provider"
)

type replayStubController struct{ *control.Controller }

func (replayStubController) SessionPath() string { return "/sessions/a.jsonl" }
func (replayStubController) ReplayPendingPromptsWith(factory func() event.Sink) {
	factory().Emit(event.Event{Kind: event.ApprovalRequest})
}

type replayRaceController struct {
	control.SessionAPI
	path     string
	onPath   func()
	replayed chan string
}

func (c *replayRaceController) SessionPath() string {
	if c.onPath != nil {
		c.onPath()
	}
	return c.path
}

func (c *replayRaceController) ReplayPendingPromptsWith(factory func() event.Sink) {
	c.replayed <- c.path
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

func TestEventsReplayUsesControllerCapturedWithPath(t *testing.T) {
	bc := NewBroadcaster()
	baseA := control.New(control.Options{Sink: bc})
	baseB := control.New(control.Options{Sink: bc})
	replayed := make(chan string, 2)
	a := &replayRaceController{SessionAPI: baseA, path: "/sessions/a.jsonl", replayed: replayed}
	b := &replayRaceController{SessionAPI: baseB, path: "/sessions/b.jsonl", replayed: replayed}
	server := New(a, bc, config.ServeConfig{})
	a.onPath = func() {
		a.onPath = nil
		if !server.publishControllerSwap(a, b, b.path) {
			t.Error("controller swap did not occur during path capture")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/events", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { server.events(rec, req); close(done) }()
	select {
	case path := <-replayed:
		if path != a.path {
			t.Fatalf("replayed controller path = %q, want captured %q", path, a.path)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("events handler did not replay pending prompts")
	}
	cancel()
	<-done
}

func TestSlashNewRefreshesControllerTagAndForegroundRoute(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "current.jsonl")
	saveServeTestSession(t, path)
	bc := NewBroadcaster()
	tag := NewSessionTagSink(bc)
	tag.SetPath(path)
	loaded, err := agent.LoadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	exec := agent.New(nil, nil, loaded, agent.Options{}, tag)
	ctrl := control.New(control.Options{Executor: exec, Sink: tag, SessionDir: dir, SessionPath: path, Label: "test"})
	server := New(ctrl, bc, config.ServeConfig{})
	server.RegisterSessionTag(ctrl, tag)
	leases := control.NewSessionLeaseKeeper()
	defer leases.Release()
	if err := leases.Rebind(path); err != nil {
		t.Fatal(err)
	}
	if err := server.SetSessionLeases(leases); err != nil {
		t.Fatal(err)
	}
	all, stop := bc.SubscribeAll()
	defer stop()
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	resp, err := http.Post(httpServer.URL+"/submit", "application/json", strings.NewReader(`{"input":"/new"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("slash /new submit status = %d, want 204", resp.StatusCode)
	}
	select {
	case frame := <-all:
		deadline := time.After(2 * time.Second)
		for !strings.Contains(string(frame), `"kind":"notice"`) || !strings.Contains(string(frame), `"text":"new session"`) {
			select {
			case frame = <-all:
			case <-deadline:
				t.Fatal("slash /new did not publish its completion notice")
			}
		}
		newPath := agent.CanonicalSessionPath(ctrl.SessionPath())
		if newPath == agent.CanonicalSessionPath(path) || tag.Path() != newPath || bc.CurrentSession() != newPath {
			t.Fatalf("slash /new routing = controller %q tag %q broadcaster %q frame=%s", newPath, tag.Path(), bc.CurrentSession(), frame)
		}
		if !strings.Contains(string(frame), `"sessionPath":"`+newPath+`"`) {
			t.Fatalf("slash /new notice kept the old session tag: %s", frame)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("slash /new emitted no event")
	}
}

type backgroundJobOnlyController struct{ *control.Controller }

func (c *backgroundJobOnlyController) RuntimeStatus() control.RuntimeStatus {
	return control.RuntimeStatus{BackgroundJobs: 1, Cancellable: true}
}

type detachedProviderHealProbe struct {
	*control.Controller
	closed atomic.Bool
}

func (c *detachedProviderHealProbe) RuntimeStatus() control.RuntimeStatus {
	return control.RuntimeStatus{BackgroundJobs: 1, Cancellable: true}
}

func (c *detachedProviderHealProbe) Close() {
	c.closed.Store(true)
	c.Controller.Close()
}

func TestProviderHealSynchronouslyRetiresDetachedControllers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "background.jsonl")
	ctrl := &detachedProviderHealProbe{Controller: control.New(control.Options{SessionPath: path})}
	server := New(control.New(control.Options{}), NewBroadcaster(), config.ServeConfig{})
	tag := NewSessionTagSink(server.bc)
	tag.SetPath(path)
	if _, err := server.registerDetached(ctrl, nil, tag); err != nil {
		t.Fatal(err)
	}
	server.retireDetachedForProviderHeal()
	if !ctrl.closed.Load() {
		t.Fatal("provider heal returned before the detached controller closed")
	}
	if server.detachedBusy(path) {
		t.Fatal("provider heal left the detached controller reattachable")
	}
}

func TestSessionsReportsForegroundBackgroundJobsAsRunning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.jsonl")
	saveServeTestSession(t, path)
	ctrl := &backgroundJobOnlyController{Controller: control.New(control.Options{SessionDir: dir, SessionPath: path})}
	server := New(ctrl, NewBroadcaster(), config.ServeConfig{})
	rec := httptest.NewRecorder()
	server.sessions(rec, httptest.NewRequest(http.MethodGet, "/sessions", nil))
	var rows []sessionListEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].Current || !rows[0].Running {
		t.Fatalf("foreground background-job session = %+v, want current and running", rows)
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

func TestRegisterDetachedRevalidatesPathAtPublication(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.jsonl")
	newPath := filepath.Join(dir, "recovery.jsonl")
	saveServeTestSession(t, oldPath)
	saveServeTestSession(t, newPath)
	ctrl := control.New(control.Options{Runner: blockingRunner{}, SessionDir: dir, SessionPath: oldPath})
	server := New(ctrl, NewBroadcaster(), config.ServeConfig{})
	tag := NewSessionTagSink(server.bc)
	server.RegisterSessionTag(ctrl, tag)
	started, release := make(chan struct{}), make(chan struct{})
	registerDetachedHookForTest = func() { close(started); <-release }
	t.Cleanup(func() { registerDetachedHookForTest = nil })
	result := make(chan *detachedSession, 1)
	go func() { detached, _ := server.registerDetached(ctrl, nil, tag); result <- detached }()
	<-started
	loaded, err := agent.LoadSession(newPath)
	if err != nil {
		t.Fatal(err)
	}
	ctrl.Resume(loaded, newPath)
	ctrl.Submit("keep running")
	waitRunning(t, ctrl)
	close(release)
	detached := <-result
	canonical := agent.CanonicalSessionPath(newPath)
	server.detachedMu.Lock()
	registered := server.detached[canonical]
	server.detachedMu.Unlock()
	if detached == nil || detached.path != canonical || registered != detached {
		t.Fatalf("detached publication path = %q entry=%v, want %q", detached.path, registered == detached, canonical)
	}
	ctrl.Cancel()
	waitNotRunning(t, ctrl)
	server.CloseBackground()
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

func TestDetachedRecoveryKeepsServeRoutingWrapper(t *testing.T) {
	dir := t.TempDir()
	aPath := filepath.Join(dir, "a.jsonl")
	bPath := filepath.Join(dir, "b.jsonl")
	saveServeTestSession(t, aPath)
	saveServeTestSession(t, bPath)
	loaded, err := agent.LoadSession(aPath)
	if err != nil {
		t.Fatal(err)
	}
	bc := NewBroadcaster()
	tag := NewSessionTagSink(bc)
	tag.SetPath(aPath)
	exec := agent.New(nil, nil, loaded, agent.Options{}, tag)
	ctrlA := control.New(control.Options{Runner: blockingRunner{}, Executor: exec, Sink: tag, SessionDir: dir, SessionPath: aPath, Label: "test"})
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
		return control.New(control.Options{Sink: opts.Sink, SessionDir: opts.SessionDir, Label: "test"}), nil
	}
	ctrlA.Submit("keep running")
	waitRunning(t, ctrlA)
	if err := server.busyDetach(context.Background(), ctrlA, bPath, func(next *control.Controller) error {
		session, loadErr := agent.LoadSession(bPath)
		if loadErr == nil {
			next.Resume(session, bPath)
		}
		return loadErr
	}); err != nil {
		t.Fatal(err)
	}
	disk, err := agent.LoadSession(aPath)
	if err != nil {
		t.Fatal(err)
	}
	disk.Add(provider.Message{Role: provider.RoleUser, Content: "disk diverged"})
	if err := disk.Save(aPath); err != nil {
		t.Fatal(err)
	}
	ctrlA.Executor().Session().Add(provider.Message{Role: provider.RoleUser, Content: "local diverged"})
	if err := ctrlA.Snapshot(); err != nil {
		t.Fatal(err)
	}
	recoveryPath := agent.CanonicalSessionPath(ctrlA.SessionPath())
	if recoveryPath == agent.CanonicalSessionPath(aPath) {
		t.Fatal("detached controller did not move to a recovery transcript")
	}
	server.detachedMu.Lock()
	detached := server.detached[recoveryPath]
	oldEntry := server.detached[agent.CanonicalSessionPath(aPath)]
	server.detachedMu.Unlock()
	if detached == nil || detached.ctrl != control.SessionAPI(ctrlA) || oldEntry != nil || tag.Path() != recoveryPath {
		t.Fatalf("detached recovery routing = entry %v old %v tag %q want %q", detached != nil, oldEntry != nil, tag.Path(), recoveryPath)
	}
	ctrlA.Cancel()
	waitNotRunning(t, ctrlA)
	server.CloseBackground()
}
