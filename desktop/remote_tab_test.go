package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"reasonix/internal/config"
)

// fakeServe is a minimal Serve stand-in for bridge tests: token handshake,
// session enter, an SSE feed that emits two frames then holds, and recorded
// command endpoints for the proxy bindings.
type fakeServe struct {
	t      *testing.T
	token  string
	server *httptest.Server

	mu          sync.Mutex
	newCalled   int
	resumePath  string
	cookieOnNew bool
	sessions    []serveSessionEntry
	calls       []string // "METHOD /path body" per command request
	failNext    string   // non-empty ⇒ next command endpoint replies 409 with this text
	failHistory bool     // /history replies 500 when set
	eventsConns int      // /events connections opened
}

func (fs *fakeServe) eventsCount() int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.eventsConns
}

func (fs *fakeServe) recorded() []string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	out := make([]string, len(fs.calls))
	copy(out, fs.calls)
	return out
}

func (fs *fakeServe) record(method, path, body string) {
	fs.mu.Lock()
	fs.calls = append(fs.calls, method+" "+path+" "+body)
	fs.mu.Unlock()
}

func newFakeServe(t *testing.T, token string, sessions []serveSessionEntry) *fakeServe {
	t.Helper()
	fs := &fakeServe{t: t, token: token, sessions: sessions}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /auth/token", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token != fs.token {
			http.Error(w, "denied", http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "reasonix_token", Value: fs.token, Path: "/", HttpOnly: true})
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /new", func(w http.ResponseWriter, r *http.Request) {
		fs.mu.Lock()
		fs.newCalled++
		_, cookieErr := r.Cookie("reasonix_token")
		fs.cookieOnNew = cookieErr == nil
		fs.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /resume", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Path == "" {
			http.Error(w, "missing path", http.StatusBadRequest)
			return
		}
		fs.mu.Lock()
		fs.resumePath = body.Path
		fs.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /sessions", func(w http.ResponseWriter, r *http.Request) {
		fs.record(r.Method, "/sessions", "")
		writeTestJSON(w, fs.sessions)
	})
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		fs.mu.Lock()
		fs.eventsConns++
		fs.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flusher", http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", `{"kind":"session_start"}`)
		fmt.Fprintf(w, "data: %s\n\n", `{"kind":"ready"}`)
		flusher.Flush()
		<-r.Context().Done()
	})
	command := func(path string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			data, _ := io.ReadAll(io.LimitReader(r.Body, 4<<10))
			fs.record(r.Method, path, string(data))
			fs.mu.Lock()
			fail := fs.failNext
			fs.failNext = ""
			fs.mu.Unlock()
			if fail != "" {
				http.Error(w, fail, http.StatusConflict)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}
	}
	for _, path := range []string{"/submit", "/cancel", "/approve", "/answer", "/rewind", "/goal", "/tool-approval-mode", "/delete-session", "/model", "/effort", "/plan", "/compact", "/fork", "/summarize", "/forget"} {
		mux.HandleFunc("POST "+path, command(path))
	}
	snapshot := func(path, payload string) {
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			fs.record(r.Method, path, "")
			if path == "/history" {
				fs.mu.Lock()
				fail := fs.failHistory
				fs.mu.Unlock()
				if fail {
					http.Error(w, "gone", http.StatusInternalServerError)
					return
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(payload))
		})
	}
	snapshot("/history", `[{"role":"user","content":"hi"}]`)
	snapshot("/context", `{"used":10}`)
	snapshot("/todos", `[]`)
	snapshot("/checkpoints", `[{"turn":1}]`)
	snapshot("/models", `["m1"]`)
	snapshot("/status", `{"state":"ready"}`)
	snapshot("/branches", `{"branches":[]}`)
	snapshot("/skills", `[]`)
	fs.server = httptest.NewServer(mux)
	t.Cleanup(fs.server.Close)
	return fs
}

func (fs *fakeServe) snapshot() (newCalled int, resumePath string, cookieOnNew bool) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.newCalled, fs.resumePath, fs.cookieOnNew
}

func writeTestJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func seedBridgeTestHost(t *testing.T, hostID string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	t.Setenv("HOME", home)
	if err := editUserConfig(func(c *config.Config) error {
		return c.UpsertRemoteHost(config.RemoteHostEntry{Name: hostID, Host: "127.0.0.1", Port: 22, User: "dev"})
	}); err != nil {
		t.Fatal(err)
	}
}

// eventLog records every emitRemoteEvent call from any goroutine.
type eventLog struct {
	mu     sync.Mutex
	events []string // "name payload"
}

func (l *eventLog) add(name string, payload any) {
	text, _ := json.Marshal(payload)
	l.mu.Lock()
	l.events = append(l.events, name+" "+string(text))
	l.mu.Unlock()
}

func (l *eventLog) count(prefix string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, e := range l.events {
		if strings.HasPrefix(e, prefix) {
			n++
		}
	}
	return n
}

func waitForTabState(t *testing.T, a *App, tabID, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		a.remoteTabMu.Lock()
		tab := a.remoteTabs[tabID]
		state := ""
		if tab != nil {
			state = tab.state
		}
		a.remoteTabMu.Unlock()
		if state == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("remote tab %s state = %q, want %q", tabID, state, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// cleanupRemoteTabPumps cancels every open tab's SSE pump when the test
// ends. Without this the long-lived /events connection keeps the httptest
// server's Close waiting forever.
func cleanupRemoteTabPumps(t *testing.T, a *App) {
	t.Helper()
	t.Cleanup(func() {
		a.remoteTabMu.Lock()
		for _, tab := range a.remoteTabs {
			if tab.cancel != nil {
				tab.cancel()
			}
		}
		a.remoteTabMu.Unlock()
	})
}

// TestRemoteTabBridgeEntersNewSessionAndStreams pins the happy path: open →
// handshake → pump subscribed → POST /new → ready, with frames forwarded on
// the tab's event channel and the session cookie riding the jar.
func TestRemoteTabBridgeEntersNewSessionAndStreams(t *testing.T) {
	fs := newFakeServe(t, "s3cret", nil)
	kernel := &fakeRemoteKernel{
		statuses:    []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView:  RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL},
		ensureToken: "s3cret",
	}
	seedBridgeTestHost(t, "box")
	log := &eventLog{}
	a := &App{remoteRuntime: kernel, remoteEventHook: log.add}
	cleanupRemoteTabPumps(t, a)

	meta, err := a.OpenRemoteProjectTab("box", "~/app", RemoteTabOpenOptions{NewSession: true})
	if err != nil {
		t.Fatal(err)
	}
	waitForTabState(t, a, meta.ID, "ready")

	newCalled, _, cookieOnNew := fs.snapshot()
	if newCalled != 1 {
		t.Fatalf("POST /new called %d times, want 1", newCalled)
	}
	if !cookieOnNew {
		t.Fatal("POST /new carried no session cookie; handshake did not populate the jar")
	}
	if got := log.count("remote-tab:" + meta.ID + ":event"); got < 2 {
		t.Fatalf("pump forwarded %d frames, want ≥2 (events: %v)", got, log.events)
	}
	if log.count("remote-tab:"+meta.ID+":state") < 2 {
		t.Fatalf("expected connecting + ready state events, got %v", log.events)
	}

	// Cancelling the pump (close/reconnect) must exit silently: no error
	// state is emitted for a deliberate stop.
	a.remoteTabMu.Lock()
	cancel := a.remoteTabs[meta.ID].cancel
	a.remoteTabMu.Unlock()
	if cancel != nil {
		cancel()
	}
	time.Sleep(100 * time.Millisecond)
	a.remoteTabMu.Lock()
	state := a.remoteTabs[meta.ID].state
	a.remoteTabMu.Unlock()
	if state != "ready" {
		t.Fatalf("state after pump cancel = %q, want ready (silent exit)", state)
	}
}

// TestRemoteTabBridgeHandshakeFailureSurfacesError pins that a rejected
// token lands the tab in error instead of a phantom ready shell.
func TestRemoteTabBridgeHandshakeFailureSurfacesError(t *testing.T) {
	fs := newFakeServe(t, "s3cret", nil)
	kernel := &fakeRemoteKernel{
		statuses:    []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView:  RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL},
		ensureToken: "wrong-token",
	}
	seedBridgeTestHost(t, "box")
	a := &App{remoteRuntime: kernel}
	cleanupRemoteTabPumps(t, a)

	meta, err := a.OpenRemoteProjectTab("box", "~/app", RemoteTabOpenOptions{NewSession: true})
	if err != nil {
		t.Fatal(err)
	}
	waitForTabState(t, a, meta.ID, "error")
	a.remoteTabMu.Lock()
	tabErr := a.remoteTabs[meta.ID].err
	a.remoteTabMu.Unlock()
	if !strings.Contains(tabErr, "401") {
		t.Fatalf("tab error = %q, want handshake 401", tabErr)
	}
	if _, _, cookieOnNew := fs.snapshot(); cookieOnNew {
		t.Fatal("POST /new must not be reached after a failed handshake")
	}
}

// TestRemoteTabBridgeResumeResolvesSessionPath pins the name→path
// resolution: a SessionName open reads GET /sessions and POSTs /resume with
// the entry's path, never the bare name.
func TestRemoteTabBridgeResumeResolvesSessionPath(t *testing.T) {
	fs := newFakeServe(t, "s3cret", []serveSessionEntry{
		{Name: "s1", Path: "/remote/sessions/s1.jsonl", Title: "First", Turns: 2},
	})
	kernel := &fakeRemoteKernel{
		statuses:    []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView:  RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL},
		ensureToken: "s3cret",
	}
	seedBridgeTestHost(t, "box")
	a := &App{remoteRuntime: kernel}
	cleanupRemoteTabPumps(t, a)
	meta, err := a.OpenRemoteProjectTab("box", "~/app", RemoteTabOpenOptions{SessionName: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	waitForTabState(t, a, meta.ID, "ready")

	newCalled, resumePath, _ := fs.snapshot()
	if newCalled != 0 {
		t.Fatalf("POST /new called %d times, want 0 for a resume open", newCalled)
	}
	if resumePath != "/remote/sessions/s1.jsonl" {
		t.Fatalf("POST /resume path = %q, want the /sessions entry path", resumePath)
	}
}

// TestEnterRemoteSessionUnknownName fails fast on an unlisted session name.
func TestEnterRemoteSessionUnknownName(t *testing.T) {
	fs := newFakeServe(t, "s3cret", []serveSessionEntry{{Name: "s1", Path: "/x.jsonl"}})
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := serveHandshake(ctx, client, fs.server.URL, "s3cret"); err != nil {
		t.Fatal(err)
	}
	err = enterRemoteSession(ctx, client, fs.server.URL, RemoteTabOpenOptions{SessionName: "missing"})
	if err == nil || !strings.Contains(err.Error(), `"missing" not found`) {
		t.Fatalf("err = %v, want unknown session error", err)
	}
}

// openReadyRemoteTab opens a tab against the fake serve and waits for ready.
func openReadyRemoteTab(t *testing.T, a *App, opts RemoteTabOpenOptions) TabMeta {
	t.Helper()
	meta, err := a.OpenRemoteProjectTab("box", "~/app", opts)
	if err != nil {
		t.Fatal(err)
	}
	waitForTabState(t, a, meta.ID, "ready")
	return meta
}

// TestRemoteTabCommandsForwardedToServe pins that every command binding
// reaches the right serve endpoint with the mapped body.
func TestRemoteTabCommandsForwardedToServe(t *testing.T) {
	fs := newFakeServe(t, "s3cret", nil)
	kernel := &fakeRemoteKernel{
		statuses:    []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView:  RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL},
		ensureToken: "s3cret",
	}
	seedBridgeTestHost(t, "box")
	a := &App{remoteRuntime: kernel}
	cleanupRemoteTabPumps(t, a)
	meta := openReadyRemoteTab(t, a, RemoteTabOpenOptions{NewSession: true})

	steps := []struct {
		name string
		call func() error
		want string
	}{
		{"submit", func() error { return a.SubmitRemoteTab(meta.ID, "hello") }, `POST /submit {"input":"hello"}`},
		{"cancel", func() error { return a.CancelRemoteTab(meta.ID) }, "POST /cancel {}"},
		{"approve", func() error { return a.ApproveRemoteTab(meta.ID, "call-1", "allow") }, `POST /approve {"allow":true,"id":"call-1"}`},
		{"approve-deny", func() error { return a.ApproveRemoteTab(meta.ID, "call-2", "deny") }, `POST /approve {"allow":false,"id":"call-2"}`},
		{"answer", func() error { return a.AnswerRemoteTab(meta.ID, "ask-1", "yes") }, `POST /answer {"answers":[{"QuestionID":"ask-1","Selected":["yes"]}],"id":"ask-1"}`},
		{"rewind", func() error { return a.RewindRemoteTab(meta.ID, "3") }, `POST /rewind {"scope":"both","turn":3}`},
		{"approval-mode", func() error { return a.SetRemoteTabToolApprovalMode(meta.ID, "auto") }, `POST /tool-approval-mode {"mode":"auto"}`},
		{"goal", func() error { return a.SetRemoteTabGoal(meta.ID, "ship it") }, `POST /goal {"goal":"ship it"}`},
		{"model", func() error { return a.SetRemoteTabModel(meta.ID, "deepseek/chat") }, `POST /model {"ref":"deepseek/chat"}`},
		{"effort", func() error { return a.SetRemoteTabEffort(meta.ID, "high") }, `POST /effort {"level":"high"}`},
		{"plan-on", func() error { return a.SetRemoteTabPlanMode(meta.ID, true) }, `POST /plan {"on":true}`},
		{"compact", func() error { return a.CompactRemoteTab(meta.ID) }, "POST /compact {}"},
		{"fork", func() error { return a.ForkRemoteTab(meta.ID, 2, "try-auth") }, `POST /fork {"name":"try-auth","turn":2}`},
		{"summarize", func() error { return a.SummarizeRemoteTab(meta.ID, 4, "upto") }, `POST /summarize {"mode":"upto","turn":4}`},
		{"forget", func() error { return a.ForgetRemoteTab(meta.ID, "api-key") }, `POST /forget {"name":"api-key"}`},
	}
	for _, step := range steps {
		if err := step.call(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
	}
	calls := fs.recorded()
	for _, step := range steps {
		found := false
		for _, c := range calls {
			if c == step.want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%s: serve saw %v, want %q", step.name, calls, step.want)
		}
	}
	if _, err := a.RemoteTabBranches(meta.ID); err != nil {
		t.Fatalf("branches: %v", err)
	}
	if _, err := a.RemoteTabSkills(meta.ID); err != nil {
		t.Fatalf("skills: %v", err)
	}
	foundBranches, foundSkills := false, false
	for _, c := range fs.recorded() {
		if c == "GET /branches " {
			foundBranches = true
		}
		if c == "GET /skills " {
			foundSkills = true
		}
	}
	if !foundBranches || !foundSkills {
		t.Fatalf("branches/skills reads missing: %v", fs.recorded())
	}
}

// TestRemoteTabCommandRejectsUnknownOrUnreadyTab: an unknown tabID and a
// tab that has not finished bootstrap are errors, never silent no-ops.
func TestRemoteTabCommandRejectsUnknownOrUnreadyTab(t *testing.T) {
	a := &App{}
	if err := a.SubmitRemoteTab("missing", "hi"); err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("unknown tab err = %v, want not connected", err)
	}
	a.remoteTabMu.Lock()
	a.remoteTabs = map[string]*remoteTab{"booting": {id: "booting", state: "connecting"}}
	a.remoteTabMu.Unlock()
	if err := a.SubmitRemoteTab("booting", "hi"); err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("unready tab err = %v, want not connected", err)
	}
}

// TestRemoteTabSnapshotMergesServeMembers: all six GETs merge in parallel;
// only /history is required — its failure errors, optional members degrade.
func TestRemoteTabSnapshotMergesServeMembers(t *testing.T) {
	fs := newFakeServe(t, "s3cret", nil)
	kernel := &fakeRemoteKernel{
		statuses:    []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView:  RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL},
		ensureToken: "s3cret",
	}
	seedBridgeTestHost(t, "box")
	a := &App{remoteRuntime: kernel}
	cleanupRemoteTabPumps(t, a)
	meta := openReadyRemoteTab(t, a, RemoteTabOpenOptions{NewSession: true})

	snap, err := a.RemoteTabSnapshot(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]json.RawMessage{
		"history": snap.History, "context": snap.Context, "todos": snap.Todos,
		"checkpoints": snap.Checkpoints, "models": snap.Models, "status": snap.Status,
	} {
		if len(raw) == 0 {
			t.Fatalf("snapshot member %s is empty", name)
		}
	}

	fs.mu.Lock()
	fs.failHistory = true
	fs.mu.Unlock()
	if _, err := a.RemoteTabSnapshot(meta.ID); err == nil {
		t.Fatal("snapshot with failing /history must error")
	}
}

// TestRemoteProjectSessionsWithoutOpenTab pins the one-shot path: listing
// sessions for a workspace with no live tab ensures the serve, handshakes,
// and maps entries to the frontend view.
func TestRemoteProjectSessionsWithoutOpenTab(t *testing.T) {
	fs := newFakeServe(t, "s3cret", []serveSessionEntry{
		{Name: "s1", Path: "/x.jsonl", Title: "First", Turns: 2},
	})
	kernel := &fakeRemoteKernel{
		statuses:    []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView:  RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL},
		ensureToken: "s3cret",
	}
	seedBridgeTestHost(t, "box")
	a := &App{remoteRuntime: kernel}

	sessions, err := a.RemoteProjectSessions("box", "~/app")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Name != "s1" || sessions[0].Title != "First" || sessions[0].Turns != 2 {
		t.Fatalf("sessions = %+v, want the mapped s1 entry", sessions)
	}
	found := false
	for _, c := range fs.recorded() {
		if c == "GET /sessions " {
			found = true
		}
	}
	if !found {
		t.Fatalf("GET /sessions not reached: %v", fs.recorded())
	}
}

// TestRemoteTabCommandSurfacesServeErrorBody: the serve's error text (the
// session-in-use close hint) rides through to the caller.
func TestRemoteTabCommandSurfacesServeErrorBody(t *testing.T) {
	fs := newFakeServe(t, "s3cret", nil)
	kernel := &fakeRemoteKernel{
		statuses:    []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView:  RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL},
		ensureToken: "s3cret",
	}
	seedBridgeTestHost(t, "box")
	a := &App{remoteRuntime: kernel}
	cleanupRemoteTabPumps(t, a)
	meta := openReadyRemoteTab(t, a, RemoteTabOpenOptions{NewSession: true})

	fs.mu.Lock()
	fs.failNext = "session in use; close the remote tab first"
	fs.mu.Unlock()
	err := a.SubmitRemoteTab(meta.ID, "hello")
	if err == nil || !strings.Contains(err.Error(), "close the remote tab first") {
		t.Fatalf("err = %v, want the serve error body surfaced", err)
	}
}

// TestCloseRemoteTabIsIdempotent: closing removes the registry entry, stops
// the pump, and a second close is a no-op.
func TestCloseRemoteTabIsIdempotent(t *testing.T) {
	fs := newFakeServe(t, "s3cret", nil)
	kernel := &fakeRemoteKernel{
		statuses:    []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView:  RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL},
		ensureToken: "s3cret",
	}
	seedBridgeTestHost(t, "box")
	a := &App{remoteRuntime: kernel}
	cleanupRemoteTabPumps(t, a)
	meta := openReadyRemoteTab(t, a, RemoteTabOpenOptions{NewSession: true})

	if err := a.CloseRemoteTab(meta.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.CloseRemoteTab(meta.ID); err != nil {
		t.Fatalf("second close: %v", err)
	}
	a.remoteTabMu.Lock()
	_, present := a.remoteTabs[meta.ID]
	a.remoteTabMu.Unlock()
	if present {
		t.Fatal("closed tab still in the registry")
	}
	if err := a.SubmitRemoteTab(meta.ID, "hi"); err == nil {
		t.Fatal("commands on a closed tab must fail")
	}
}

// TestRemoteTabFollowsHostReconnect pins the SSH-driven lifecycle: a
// transient drop suspends the pump and flags reconnecting, the regained
// connection re-attaches a fresh pump to the still-running serve, and a
// terminal failure parks the tab in error.
func TestRemoteTabFollowsHostReconnect(t *testing.T) {
	fs := newFakeServe(t, "s3cret", nil)
	kernel := &fakeRemoteKernel{
		statuses:    []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView:  RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL},
		ensureToken: "s3cret",
	}
	seedBridgeTestHost(t, "box")
	a := &App{remoteRuntime: kernel}
	cleanupRemoteTabPumps(t, a)
	meta := openReadyRemoteTab(t, a, RemoteTabOpenOptions{NewSession: true})
	firstConns := fs.eventsCount()

	a.remoteTabsHostStatus("box", "reconnecting", "")
	waitForTabState(t, a, meta.ID, "reconnecting")
	if err := a.SubmitRemoteTab(meta.ID, "hi"); err == nil {
		t.Fatal("commands during reconnecting must fail")
	}

	a.remoteTabsHostStatus("box", "connected", "")
	waitForTabState(t, a, meta.ID, "ready")
	if fs.eventsCount() <= firstConns {
		t.Fatalf("re-attach did not open a new event stream: %d then %d", firstConns, fs.eventsCount())
	}
	if err := a.SubmitRemoteTab(meta.ID, "back online"); err != nil {
		t.Fatalf("submit after reconnect: %v", err)
	}

	a.remoteTabsHostStatus("box", "stopped", "ssh: auth failed")
	waitForTabState(t, a, meta.ID, "error")
	a.remoteTabMu.Lock()
	tabErr := a.remoteTabs[meta.ID].err
	a.remoteTabMu.Unlock()
	if !strings.Contains(tabErr, "ssh: auth failed") {
		t.Fatalf("tab error = %q, want the host failure text", tabErr)
	}
}

// TestListTabsIncludesRemoteEntries pins the strip integration: open remote
// tabs appear in ListTabs, a highlighted remote tab deactivates the local
// entries, and SetActiveTab routes by registry membership.
func TestListTabsIncludesRemoteEntries(t *testing.T) {
	fs := newFakeServe(t, "s3cret", nil)
	kernel := &fakeRemoteKernel{
		statuses:    []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView:  RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL},
		ensureToken: "s3cret",
	}
	seedBridgeTestHost(t, "box")
	a := &App{remoteRuntime: kernel}
	cleanupRemoteTabPumps(t, a)
	meta := openReadyRemoteTab(t, a, RemoteTabOpenOptions{NewSession: true})

	tabs := a.ListTabs()
	var remote TabMeta
	found := false
	for _, tab := range tabs {
		if tab.ID == meta.ID {
			remote, found = tab, true
		}
	}
	if !found {
		t.Fatalf("remote tab missing from ListTabs: %+v", tabs)
	}
	if remote.Remote == nil || remote.Remote.HostID != "box" {
		t.Fatalf("remote meta ref = %+v", remote.Remote)
	}
	if !remote.Active {
		t.Fatal("freshly opened remote tab must carry the strip highlight")
	}

	if err := a.SetActiveTab(meta.ID); err != nil {
		t.Fatalf("SetActiveTab(remote): %v", err)
	}
	a.remoteTabMu.Lock()
	active := a.remoteActiveTabID
	a.remoteTabMu.Unlock()
	if active != meta.ID {
		t.Fatalf("remoteActiveTabID = %q, want %q", active, meta.ID)
	}
	if err := a.CloseTabWithPolicy(meta.ID, "keep_running"); err != nil {
		t.Fatalf("CloseTabWithPolicy(remote): %v", err)
	}
	a.remoteTabMu.Lock()
	_, present := a.remoteTabs[meta.ID]
	a.remoteTabMu.Unlock()
	if present {
		t.Fatal("CloseTabWithPolicy left the remote tab registered")
	}
}
