package main

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRemoteTabAllSessionEventsRouteOnlyCurrentFrames(t *testing.T) {
	fs := newFakeServe(t, "s3cret", []serveSessionEntry{
		{Name: "current", Path: "/sessions/current.jsonl", Current: true},
		{Name: "background", Path: "/sessions/background.jsonl"},
	})
	fs.mu.Lock()
	fs.eventFrames = []string{
		`{"kind":"turn_started","sessionPath":"/sessions/background.jsonl"}`,
		`{"kind":"turn_started","sessionPath":"/sessions/current.jsonl"}`,
		`{"kind":"ready","sessionPath":"/sessions/current.jsonl"}`,
	}
	fs.mu.Unlock()
	kernel := &fakeRemoteKernel{
		statuses:    []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView:  RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL},
		ensureToken: "s3cret",
	}
	seedBridgeTestHost(t, "box")
	log := &eventLog{}
	a := &App{remoteRuntime: kernel, remoteEventHook: log.add}
	cleanupRemoteTabPumps(t, a)
	meta := openReadyRemoteTab(t, a, RemoteTabOpenOptions{SessionName: "current", SessionPath: "/sessions/current.jsonl", SessionTitle: "Current"})
	events := log.recorded()
	for _, event := range events {
		if strings.HasPrefix(event, "remote-tab:"+meta.ID+":event") && strings.Contains(event, "/sessions/background.jsonl") {
			t.Fatalf("background frame leaked to foreground reducer: %v", events)
		}
	}
	if !slices.ContainsFunc(events, func(event string) bool {
		return strings.HasPrefix(event, "remote-tab:"+meta.ID+":event") && strings.Contains(event, "/sessions/current.jsonl")
	}) {
		t.Fatalf("current-session frame was not forwarded: %v", events)
	}
	a.remoteTabMu.Lock()
	backgroundRunning := a.remoteTabs[meta.ID].routing.running["/sessions/background.jsonl"]
	a.remoteTabMu.Unlock()
	if !backgroundRunning {
		t.Fatal("background running state was not retained for the project tree")
	}
	sessions, err := a.RemoteProjectSessions("box", "~/app")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(sessions, func(session RemoteSessionView) bool {
		return session.Path == "/sessions/background.jsonl" && session.Running
	}) {
		t.Fatalf("background session is not marked running: %+v", sessions)
	}
}

func remoteSessionTestClient(t *testing.T, fs *fakeServe) (*http.Client, context.Context) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	if err := serveHandshake(ctx, client, fs.server.URL, "s3cret"); err != nil {
		t.Fatal(err)
	}
	return client, ctx
}

func TestEnterRemoteSessionPathSkipsSessionCatalog(t *testing.T) {
	fs := newFakeServe(t, "s3cret", nil)
	client, ctx := remoteSessionTestClient(t, fs)
	target, err := enterRemoteSessionTarget(ctx, client, fs.server.URL, RemoteTabOpenOptions{SessionName: "known", SessionPath: "/remote/sessions/known.jsonl", SessionTitle: "Known"})
	if err != nil {
		t.Fatal(err)
	}
	if target.Path != "/remote/sessions/known.jsonl" || target.Title != "Known" {
		t.Fatalf("target = %+v", target)
	}
	for _, call := range fs.recorded() {
		if strings.HasPrefix(call, "GET /sessions") {
			t.Fatalf("explicit path unnecessarily fetched the session catalog: %v", fs.recorded())
		}
	}
}

func TestEnterRemoteSessionUnknownName(t *testing.T) {
	fs := newFakeServe(t, "s3cret", []serveSessionEntry{{Name: "s1", Path: "/x.jsonl"}})
	client, ctx := remoteSessionTestClient(t, fs)
	err := enterRemoteSession(ctx, client, fs.server.URL, RemoteTabOpenOptions{SessionName: "missing"})
	if err == nil || !strings.Contains(err.Error(), `"missing" not found`) {
		t.Fatalf("err = %v, want unknown session error", err)
	}
}
