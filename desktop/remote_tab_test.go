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

// newFakeServe builds a stand-in for the workspace Serve's HTTP surface. The
// mux is wrapped in a token-mode gate that mirrors the real authGate's
// contract: POST /auth/token is matched on the EXACT path (a "//auth/token"
// double slash — what naive base+path joins produce from EnsureServer's
// trailing-slash LocalURL — is denied with 401 before routing), and every
// other path requires the session cookie the bootstrap installs. A bare mux
// cannot catch this: it 301-redirects unclean paths and Go's client follows
// preserving POST, so a double-slash request would silently succeed here
// while the real Serve rejects it.
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
		// The serve abandons the current session on /new: no file, not listed.
		for i := range fs.sessions {
			fs.sessions[i].Current = false
		}
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
		for i := range fs.sessions {
			fs.sessions[i].Current = fs.sessions[i].Path == body.Path
		}
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
	snapshot("/models", `{"current":"remote/chat","label":"chat","models":[{"ref":"remote/chat","provider":"remote","model":"chat","active":true}]}`)
	snapshot("/status", `{"state":"ready"}`)
	snapshot("/branches", `{"branches":[]}`)
	snapshot("/skills", `[]`)
	gate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/token" {
			mux.ServeHTTP(w, r)
			return
		}
		if c, err := r.Cookie("reasonix_token"); err == nil && c.Value == fs.token {
			mux.ServeHTTP(w, r)
			return
		}
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	})
	fs.server = httptest.NewServer(gate)
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

func (l *eventLog) recorded() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.events))
	copy(out, l.events)
	return out
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

// TestRemoteTabBridgeToleratesTrailingSlashBase pins the production LocalURL
// shape: EnsureServer reports "http://127.0.0.1:port/" with a trailing slash.
// Naive base+"/auth/token" concatenation used to hit "//auth/token", which the
// serve auth gate rejects with 401 before routing the endpoint — the handshake
// died on every wizard-completed connect.
func TestRemoteTabBridgeToleratesTrailingSlashBase(t *testing.T) {
	fs := newFakeServe(t, "s3cret", nil)
	kernel := &fakeRemoteKernel{
		statuses:    []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView:  RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL + "/"},
		ensureToken: "s3cret",
	}
	seedBridgeTestHost(t, "box")
	a := &App{remoteRuntime: kernel}
	cleanupRemoteTabPumps(t, a)

	meta, err := a.OpenRemoteProjectTab("box", "~/app", RemoteTabOpenOptions{NewSession: true})
	if err != nil {
		t.Fatal(err)
	}
	waitForTabState(t, a, meta.ID, "ready")
	newCalled, _, cookieOnNew := fs.snapshot()
	if newCalled != 1 || !cookieOnNew {
		t.Fatalf("handshake with trailing-slash base failed: /new=%d cookie=%v", newCalled, cookieOnNew)
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

// TestModelsForTabRemoteUsesDesktopCatalog: a remote tab's model switcher
// lists the desktop provider catalog. Current is the desktop-owned tab model,
// not the remote serve GET /models payload.
func TestModelsForTabRemoteUsesDesktopCatalog(t *testing.T) {
	isolateDesktopUserDirs(t)
	setDesktopTestCredential(t, "DEEPSEEK_API_KEY", "sk-test")
	cfg := config.Default()
	cfg.DefaultModel = "deepseek/deepseek-v4-flash"
	cfg.Desktop.ProviderAccess = []string{"deepseek"}
	cfg.Providers = append(cfg.Providers, config.ProviderEntry{
		Name: "deepseek", Kind: "anthropic", BaseURL: "https://api.deepseek.com/anthropic",
		Models: []string{"deepseek-v4-flash", "deepseek-v4-pro"}, Default: "deepseek-v4-flash", APIKeyEnv: "DEEPSEEK_API_KEY",
	})
	if err := cfg.UpsertRemoteHost(config.RemoteHostEntry{Name: "box", Host: "127.0.0.1", Port: 22, User: "dev", CredentialMode: "local-proxy"}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SaveTo(config.UserConfigPath()); err != nil {
		t.Fatalf("save config: %v", err)
	}

	fs := newFakeServe(t, "s3cret", nil)
	kernel := &fakeRemoteKernel{
		statuses:    []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView:  RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL},
		ensureToken: "s3cret",
	}
	a := &App{remoteRuntime: kernel}
	cleanupRemoteTabPumps(t, a)
	meta := openReadyRemoteTab(t, a, RemoteTabOpenOptions{NewSession: true})

	got := a.ModelsForTab(meta.ID)
	refs := map[string]bool{}
	current := ""
	for _, m := range got {
		refs[m.Ref] = true
		if m.Current {
			current = m.Ref
		}
	}
	if !refs["deepseek/deepseek-v4-flash"] || !refs["deepseek/deepseek-v4-pro"] {
		t.Fatalf("ModelsForTab(%s) = %+v, want desktop deepseek catalog", meta.ID, got)
	}
	if refs["remote/chat"] {
		t.Fatalf("ModelsForTab leaked the serve catalog: %+v", got)
	}
	if current != "deepseek/deepseek-v4-flash" {
		t.Fatalf("current = %q, want desktop default_model", current)
	}
}

// TestSetModelForTabRemoteOwnsModelOnDesktop: picking a model on a remote
// tab stores it on the desktop tab and does not POST /model to the serve.
func TestSetModelForTabRemoteOwnsModelOnDesktop(t *testing.T) {
	isolateDesktopUserDirs(t)
	setDesktopTestCredential(t, "DEEPSEEK_API_KEY", "sk-test")
	cfg := config.Default()
	cfg.DefaultModel = "deepseek/deepseek-v4-flash"
	cfg.Desktop.ProviderAccess = []string{"deepseek"}
	cfg.Providers = append(cfg.Providers, config.ProviderEntry{
		Name: "deepseek", Kind: "anthropic", BaseURL: "https://api.deepseek.com/anthropic",
		Models: []string{"deepseek-v4-flash", "deepseek-v4-pro"}, Default: "deepseek-v4-flash", APIKeyEnv: "DEEPSEEK_API_KEY",
	})
	if err := cfg.UpsertRemoteHost(config.RemoteHostEntry{Name: "box", Host: "127.0.0.1", Port: 22, User: "dev", CredentialMode: "local-proxy"}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SaveTo(config.UserConfigPath()); err != nil {
		t.Fatalf("save config: %v", err)
	}

	fs := newFakeServe(t, "s3cret", nil)
	kernel := &fakeRemoteKernel{
		statuses:    []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView:  RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL},
		ensureToken: "s3cret",
	}
	a := &App{remoteRuntime: kernel}
	cleanupRemoteTabPumps(t, a)
	meta := openReadyRemoteTab(t, a, RemoteTabOpenOptions{NewSession: true})

	if err := a.SetModelForTab(meta.ID, "deepseek/deepseek-v4-pro"); err != nil {
		t.Fatalf("SetModelForTab: %v", err)
	}
	for _, c := range fs.recorded() {
		if strings.HasPrefix(c, "POST /model") {
			t.Fatalf("serve saw %v, desktop-owned switch must not POST /model", fs.recorded())
		}
	}
	var current string
	for _, m := range a.ModelsForTab(meta.ID) {
		if m.Current {
			current = m.Ref
		}
	}
	if current != "deepseek/deepseek-v4-pro" {
		t.Fatalf("current = %q, want deepseek/deepseek-v4-pro", current)
	}
}

// TestModelsForTabRemoteCredentialHostOffersServeCatalog: when the host keeps
// its keys on the remote, the picker lists the serve's own catalog, not the
// desktop's.
func TestModelsForTabRemoteCredentialHostOffersServeCatalog(t *testing.T) {
	isolateDesktopUserDirs(t)
	cfg := config.Default()
	if err := cfg.UpsertRemoteHost(config.RemoteHostEntry{Name: "box", Host: "127.0.0.1", Port: 22, User: "dev"}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SaveTo(config.UserConfigPath()); err != nil {
		t.Fatalf("save config: %v", err)
	}
	fs := newFakeServe(t, "s3cret", nil)
	kernel := &fakeRemoteKernel{
		statuses:    []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView:  RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL},
		ensureToken: "s3cret",
	}
	a := &App{remoteRuntime: kernel}
	cleanupRemoteTabPumps(t, a)
	meta := openReadyRemoteTab(t, a, RemoteTabOpenOptions{NewSession: true})

	got := a.ModelsForTab(meta.ID)
	if len(got) != 1 || got[0].Ref != "remote/chat" || !got[0].Current {
		t.Fatalf("ModelsForTab = %+v, want the serve catalog with remote/chat current", got)
	}
}

// TestSetModelForTabRemoteCredentialPostsServeModel: remote-credential hosts
// switch through the serve's per-session endpoint.
func TestSetModelForTabRemoteCredentialPostsServeModel(t *testing.T) {
	isolateDesktopUserDirs(t)
	cfg := config.Default()
	if err := cfg.UpsertRemoteHost(config.RemoteHostEntry{Name: "box", Host: "127.0.0.1", Port: 22, User: "dev"}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SaveTo(config.UserConfigPath()); err != nil {
		t.Fatalf("save config: %v", err)
	}
	fs := newFakeServe(t, "s3cret", nil)
	kernel := &fakeRemoteKernel{
		statuses:    []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView:  RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL},
		ensureToken: "s3cret",
	}
	a := &App{remoteRuntime: kernel}
	cleanupRemoteTabPumps(t, a)
	meta := openReadyRemoteTab(t, a, RemoteTabOpenOptions{NewSession: true})

	if err := a.SetModelForTab(meta.ID, "remote/chat"); err != nil {
		t.Fatalf("SetModelForTab: %v", err)
	}
	posted := false
	for _, c := range fs.recorded() {
		if strings.HasPrefix(c, "POST /model ") && strings.Contains(c, `"ref":"remote/chat"`) {
			posted = true
		}
	}
	if !posted {
		t.Fatalf("serve never saw POST /model with the ref: %v", fs.recorded())
	}
	a.remoteTabMu.Lock()
	model := ""
	if tab := a.remoteTabs[meta.ID]; tab != nil {
		model = tab.model
	}
	a.remoteTabMu.Unlock()
	if model != "remote/chat" {
		t.Fatalf("tab.model = %q, want remote/chat", model)
	}
}

// TestSetRemoteTabModelFailureKeepsPreviousModel: a local-proxy switch that
// fails at the credential-proxy step must leave the tab's previous model
// intact instead of half-committing the new one.
func TestSetRemoteTabModelFailureKeepsPreviousModel(t *testing.T) {
	isolateDesktopUserDirs(t)
	setDesktopTestCredential(t, "DEEPSEEK_API_KEY", "sk-test")
	cfg := config.Default()
	cfg.DefaultModel = "deepseek/deepseek-v4-flash"
	cfg.Desktop.ProviderAccess = []string{"deepseek"}
	cfg.Providers = append(cfg.Providers, config.ProviderEntry{
		Name: "deepseek", Kind: "anthropic", BaseURL: "https://api.deepseek.com/anthropic",
		Models: []string{"deepseek-v4-flash", "deepseek-v4-pro"}, Default: "deepseek-v4-flash", APIKeyEnv: "DEEPSEEK_API_KEY",
	})
	if err := cfg.UpsertRemoteHost(config.RemoteHostEntry{Name: "box", Host: "127.0.0.1", Port: 22, User: "dev", CredentialMode: "local-proxy"}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SaveTo(config.UserConfigPath()); err != nil {
		t.Fatalf("save config: %v", err)
	}
	fs := newFakeServe(t, "s3cret", nil)
	kernel := &fakeRemoteKernel{
		statuses:    []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView:  RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL},
		ensureToken: "s3cret",
	}
	a := &App{remoteRuntime: kernel}
	cleanupRemoteTabPumps(t, a)
	meta := openReadyRemoteTab(t, a, RemoteTabOpenOptions{NewSession: true})

	// Kill the local key after the tab seeded its default: config.Load()
	// re-pins credentials-file values into the environment, so the stored
	// entry itself must be cleared. The proxy step of the switch must fail,
	// and the tab must keep the seeded model.
	if _, err := config.SetCredential("DEEPSEEK_API_KEY", ""); err != nil {
		t.Fatalf("clear credential: %v", err)
	}
	t.Setenv("DEEPSEEK_API_KEY", "")
	if err := a.SetModelForTab(meta.ID, "deepseek/deepseek-v4-pro"); err == nil {
		t.Fatal("SetModelForTab must fail without the local key")
	}
	a.remoteTabMu.Lock()
	model := ""
	if tab := a.remoteTabs[meta.ID]; tab != nil {
		model = tab.model
	}
	a.remoteTabMu.Unlock()
	if model != "deepseek/deepseek-v4-flash" {
		t.Fatalf("tab.model = %q, want the untouched previous model", model)
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

// TestRemoteProjectSessionsWithoutOpenTab pins the read-only one-shot path:
// listing sessions for a workspace with no live tab reuses the registry's
// ready registration, handshakes, and maps entries to the frontend view —
// without ever ensuring a serve.
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
	if kernel.ensureCalls != 0 {
		t.Fatalf("listing woke the serve: %d EnsureServer calls", kernel.ensureCalls)
	}
}

// TestRemoteProjectSessionsNeverWakesServe: a query path must never
// cold-start a serve — no ready registration means an error, and EnsureServer
// must not even be attempted (the old behavior here starved tab bootstraps
// on the per-host serve lock).
func TestRemoteProjectSessionsNeverWakesServe(t *testing.T) {
	kernel := &fakeRemoteKernel{
		statuses: []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		// No ready registration: ServeSnapshot reports nothing.
	}
	seedBridgeTestHost(t, "box")
	a := &App{remoteRuntime: kernel}

	if _, err := a.RemoteProjectSessions("box", "~/app"); err == nil {
		t.Fatal("listing must report the serve as not running")
	}
	if kernel.ensureCalls != 0 {
		t.Fatalf("listing woke the serve: %d EnsureServer calls", kernel.ensureCalls)
	}
}

// TestResolveOverlappingWorkspace pins the merge rules for overlapping pins:
// exact match wins, then the nearest ancestor, then the shallowest
// descendant; disjoint paths never merge.
func TestResolveOverlappingWorkspace(t *testing.T) {
	entries := []config.RemoteProjectEntry{
		{HostID: "box", Workspace: "/srv/app"},
		{HostID: "box", Workspace: "/srv/app/sub"},
		{HostID: "other", Workspace: "/srv/app"},
	}
	for _, tc := range []struct {
		ws   string
		want string
		ok   bool
	}{
		{ws: "/srv/app", want: "/srv/app", ok: true},             // exact
		{ws: "/srv/app/", want: "/srv/app", ok: true},            // trailing slash normalizes to exact
		{ws: "/srv/app/sub/deep", want: "/srv/app/sub", ok: true}, // nearest ancestor
		{ws: "/srv", want: "/srv/app", ok: true},                  // ancestor request merges into shallowest descendant
		{ws: "/srv/other", want: "", ok: false},                   // sibling never merges
		{ws: "", want: "", ok: false},                             // empty never merges
	} {
		got, ok := resolveOverlappingWorkspace(entries, "box", tc.ws)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("resolveOverlappingWorkspace(%q) = (%q, %v), want (%q, %v)", tc.ws, got, ok, tc.want, tc.ok)
		}
	}
	// Host scoping: the same path on another host must not capture the merge.
	if got, ok := resolveOverlappingWorkspace(entries, "other", "/srv/app/sub/x"); !ok || got != "/srv/app" {
		t.Fatalf("cross-host overlap merged: (%q, %v)", got, ok)
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

// TestRemoteTabTitleAdoptsServeSession pins the title pipeline: the serve's
// LLM-generated title for the current session replaces the workspace-name
// default and reaches the chrome through the tab-opened channel.
func TestRemoteTabTitleAdoptsServeSession(t *testing.T) {
	fs := newFakeServe(t, "s3cret", []serveSessionEntry{
		{Name: "s1", Path: "/x.jsonl", Title: "Fix the login bug", Turns: 1, Current: true},
	})
	kernel := &fakeRemoteKernel{
		statuses:    []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView:  RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL},
		ensureToken: "s3cret",
	}
	seedBridgeTestHost(t, "box")
	log := &eventLog{}
	a := &App{remoteRuntime: kernel, remoteEventHook: log.add}
	cleanupRemoteTabPumps(t, a)
	// Resume the seeded session so it stays current; /new would abandon it.
	meta := openReadyRemoteTab(t, a, RemoteTabOpenOptions{SessionName: "s1"})
	log.mu.Lock()
	log.events = nil
	log.mu.Unlock()

	a.refreshRemoteTabTitle(meta.ID)

	a.remoteTabMu.Lock()
	title := a.remoteTabs[meta.ID].topicTitle
	a.remoteTabMu.Unlock()
	if title != "Fix the login bug" {
		t.Fatalf("topicTitle = %q, want the serve title", title)
	}
	found := false
	for _, e := range log.recorded() {
		if strings.HasPrefix(e, "remote-tab:opened ") && strings.Contains(e, meta.ID) && strings.Contains(e, "Fix the login bug") {
			found = true
		}
	}
	if !found {
		t.Fatalf("title refresh not pushed to the chrome: %v", log.recorded())
	}
	for _, tab := range a.ListTabs() {
		if tab.ID == meta.ID && tab.TopicTitle != "Fix the login bug" {
			t.Fatalf("ListTabs title = %q", tab.TopicTitle)
		}
	}
}

// TestRemoteTabNewSessionResetsServeSession: a NewSession open on an
// existing tab POSTs /new (the old session stays in the history list) and
// re-emits ready so the frontend re-syncs its snapshot.
func TestRemoteTabNewSessionResetsServeSession(t *testing.T) {
	fs := newFakeServe(t, "s3cret", []serveSessionEntry{
		{Name: "s1", Path: "/remote/sessions/s1.jsonl", Title: "Serve title", Turns: 1, Current: true},
	})
	kernel := &fakeRemoteKernel{
		statuses:    []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView:  RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL},
		ensureToken: "s3cret",
	}
	seedBridgeTestHost(t, "box")
	log := &eventLog{}
	a := &App{remoteRuntime: kernel, remoteEventHook: log.add}
	cleanupRemoteTabPumps(t, a)
	meta := openReadyRemoteTab(t, a, RemoteTabOpenOptions{NewSession: true})

	// The bootstrap already entered a fresh session; the listing carries the
	// desktop-view blank (the serve abandoned s1 and lists no current row).
	sessions, err := a.RemoteProjectSessions("box", "~/app")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) == 0 || sessions[0].Name != "" || !sessions[0].Current || sessions[0].Title != "新的会话" {
		t.Fatalf("sessions = %+v, want the synthetic blank leading the listing", sessions)
	}

	// A further new-session open reuses the blank: no extra POST /new — the
	// same contract as the local reusable-blank tab.
	newBefore, _, _ := fs.snapshot()
	if _, err := a.OpenRemoteProjectTab("box", "~/app", RemoteTabOpenOptions{NewSession: true}); err != nil {
		t.Fatal(err)
	}
	if newAfter, _, _ := fs.snapshot(); newAfter != newBefore {
		t.Fatalf("POST /new called %d times after reuse, want %d", newAfter, newBefore)
	}

	// Resuming a listed session clears the blank and restores it as current.
	if _, err := a.OpenRemoteProjectTab("box", "~/app", RemoteTabOpenOptions{SessionName: "s1"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		sessions, err = a.RemoteProjectSessions("box", "~/app")
		if err != nil {
			t.Fatal(err)
		}
		if len(sessions) > 0 && sessions[0].Name == "s1" && sessions[0].Current {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("resume did not restore s1 as current: %+v", sessions)
		}
		time.Sleep(10 * time.Millisecond)
	}
	a.remoteTabMu.Lock()
	reset := a.remoteTabs[meta.ID].sessionReset
	title := a.remoteTabs[meta.ID].topicTitle
	a.remoteTabMu.Unlock()
	if reset {
		t.Fatal("sessionReset must clear after a resume")
	}
	if title != "Serve title" {
		t.Fatalf("topicTitle after resume = %q, want the serve title", title)
	}
}

// TestRenameRemoteProjectSession pins the desktop-owned title chain: the
// override wins in the session listing, and a live tab holding that session
// adopts the new title immediately; clearing falls back to the serve title.
func TestRenameRemoteProjectSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	t.Setenv("HOME", home)
	if err := editUserConfig(func(c *config.Config) error {
		return c.UpsertRemoteHost(config.RemoteHostEntry{Name: "box", Host: "127.0.0.1", Port: 22, User: "dev"})
	}); err != nil {
		t.Fatal(err)
	}
	fs := newFakeServe(t, "s3cret", []serveSessionEntry{
		{Name: "s1", Path: "/x.jsonl", Title: "Serve title", Turns: 1, Current: true, MtimeMilli: 1700000000000},
	})
	kernel := &fakeRemoteKernel{
		statuses:    []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView:  RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL},
		ensureToken: "s3cret",
	}
	log := &eventLog{}
	a := &App{remoteRuntime: kernel, remoteEventHook: log.add}
	cleanupRemoteTabPumps(t, a)
	meta := openReadyRemoteTab(t, a, RemoteTabOpenOptions{SessionName: "s1"})

	sessions, err := a.RemoteProjectSessions("box", "~/app")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].LastActivityAt != 1700000000000 || sessions[0].Title != "Serve title" {
		t.Fatalf("sessions = %+v, want serve title + mtime passthrough", sessions)
	}

	if err := a.RenameRemoteProjectSession("box", "~/app", "s1", "我的新标题"); err != nil {
		t.Fatal(err)
	}
	sessions, err = a.RemoteProjectSessions("box", "~/app")
	if err != nil {
		t.Fatal(err)
	}
	if sessions[0].Title != "我的新标题" {
		t.Fatalf("override title = %q", sessions[0].Title)
	}
	a.remoteTabMu.Lock()
	title := a.remoteTabs[meta.ID].topicTitle
	a.remoteTabMu.Unlock()
	if title != "我的新标题" {
		t.Fatalf("live tab title = %q, want the override", title)
	}

	if err := a.RenameRemoteProjectSession("box", "~/app", "s1", ""); err != nil {
		t.Fatal(err)
	}
	sessions, _ = a.RemoteProjectSessions("box", "~/app")
	if sessions[0].Title != "Serve title" {
		t.Fatalf("cleared override title = %q, want the serve title", sessions[0].Title)
	}
}

// TestRemoteSessionPinnedOrderingAndProjectTitle pins the desktop-owned
// row pin (pinned-first listing) and the registry-backed project rename.
func TestRemoteSessionPinnedOrderingAndProjectTitle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	t.Setenv("HOME", home)
	if err := editUserConfig(func(c *config.Config) error {
		return c.UpsertRemoteHost(config.RemoteHostEntry{Name: "box", Host: "127.0.0.1", Port: 22, User: "dev"})
	}); err != nil {
		t.Fatal(err)
	}
	fs := newFakeServe(t, "s3cret", []serveSessionEntry{
		{Name: "a", Path: "/a.jsonl", Title: "First", Current: false, MtimeMilli: 1},
		{Name: "b", Path: "/b.jsonl", Title: "Second", Current: true, MtimeMilli: 2},
	})
	kernel := &fakeRemoteKernel{
		statuses:    []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView:  RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL},
		ensureToken: "s3cret",
	}
	a := &App{remoteRuntime: kernel}
	cleanupRemoteTabPumps(t, a)

	if err := a.SetRemoteSessionPinned("box", "~/app", "a", true); err != nil {
		t.Fatal(err)
	}
	sessions, err := a.RemoteProjectSessions("box", "~/app")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 || sessions[0].Name != "a" || !sessions[0].Pinned || sessions[1].Pinned {
		t.Fatalf("sessions = %+v, want pinned a first", sessions)
	}

	meta, err := a.OpenRemoteProjectTab("box", "~/app", RemoteTabOpenOptions{NewSession: true})
	if err != nil {
		t.Fatal(err)
	}
	_ = meta
	if err := a.SetRemoteProjectTitle("box", "~/app", "云端演示"); err != nil {
		t.Fatal(err)
	}
	for _, node := range a.ListTabs() {
		_ = node
	}
	found := false
	for _, node := range a.GetProjectTreeSnapshot().Projects {
		if node.Remote != nil && node.Remote.HostID == "box" {
			found = true
			if node.Label != "云端演示" {
				t.Fatalf("group label = %q, want the renamed title", node.Label)
			}
		}
	}
	if !found {
		t.Fatal("remote group missing from the snapshot")
	}
}
