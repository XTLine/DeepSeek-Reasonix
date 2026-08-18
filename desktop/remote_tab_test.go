package main

import (
	"context"
	"encoding/json"
	"fmt"
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
// session enter, and an SSE feed that emits two frames then holds.
type fakeServe struct {
	t      *testing.T
	token  string
	server *httptest.Server

	mu          sync.Mutex
	newCalled   int
	resumePath  string
	cookieOnNew bool
	sessions    []serveSessionEntry
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
	mux.HandleFunc("GET /sessions", func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, fs.sessions)
	})
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
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
