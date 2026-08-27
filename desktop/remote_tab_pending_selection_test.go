package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRemoteTabReconnectDefersCachedSessionSelectionUntilReady(t *testing.T) {
	const oldPath = "/sessions/old.jsonl"
	const targetPath = "/sessions/target.jsonl"
	fs := newFakeServe(t, "s3cret", []serveSessionEntry{
		{Name: "old", Path: oldPath, Title: "Old", Current: true},
		{Name: "target", Path: targetPath, Title: "Target"},
	})
	kernel := &fakeRemoteKernel{
		statuses:   []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView: RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL}, ensureToken: "s3cret",
	}
	seedBridgeTestHost(t, "box")
	a := &App{remoteRuntime: kernel}
	cleanupRemoteTabPumps(t, a)
	meta := openReadyRemoteTab(t, a, RemoteTabOpenOptions{SessionName: "old", SessionPath: oldPath})

	started := make(chan string, 2)
	fs.mu.Lock()
	fs.resumeStarted = started
	fs.mu.Unlock()
	a.remoteTabsHostStatus("box", "reconnecting", "")
	waitForTabState(t, a, meta.ID, "reconnecting")
	if _, err := a.OpenRemoteProjectTab("box", "~/app", RemoteTabOpenOptions{
		SessionName: "target", SessionPath: targetPath, SessionTitle: "Target",
	}); err != nil {
		t.Fatal(err)
	}

	a.remoteTabMu.Lock()
	tab := a.remoteTabs[meta.ID]
	state, sessionPath, route := tab.state, tab.session.path, tab.routing.currentPath
	pending := tab.pendingSelection
	a.remoteTabMu.Unlock()
	if state != "reconnecting" || sessionPath != oldPath || route != oldPath || pending == nil || pending.path != targetPath {
		t.Fatalf("deferred state/session/route/pending = %q/%q/%q/%+v", state, sessionPath, route, pending)
	}
	select {
	case path := <-started:
		t.Fatalf("selection resumed %q before the tab was ready", path)
	default:
	}

	a.remoteTabsHostStatus("box", "connected", "")
	select {
	case path := <-started:
		if path != targetPath {
			t.Fatalf("ready resume path = %q, want %q", path, targetPath)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("deferred selection was not resumed after reconnect")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		a.remoteTabMu.Lock()
		state, sessionPath, route = tab.state, tab.session.path, tab.routing.currentPath
		pending = tab.pendingSelection
		a.remoteTabMu.Unlock()
		if state == "ready" && sessionPath == targetPath && route == targetPath && pending == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("final state/session/route/pending = %q/%q/%q/%+v", state, sessionPath, route, pending)
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case path := <-started:
		t.Fatalf("deferred selection resumed more than once; extra path %q", path)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestRemoteTabDeferredResumeRejectsSupersededSelectionRevision(t *testing.T) {
	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{
			StatusCode: http.StatusNoContent, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader("")), Request: req,
		}, nil
	})}
	tab := &remoteTab{
		id: "remote-1", state: "ready", client: client, base: "http://127.0.0.1:43210", gen: 7,
		selectionRevision: 2,
		routing:           remoteTabSessionRouting{currentPath: "/sessions/newer.jsonl", running: map[string]bool{}},
	}
	a := &App{remoteTabs: map[string]*remoteTab{tab.id: tab}}
	a.resumeRemoteTabSessionPathForSelection(tab.id, "stale", "/sessions/stale.jsonl", "Stale", 1)
	if requests != 0 {
		t.Fatalf("superseded deferred selection sent %d Serve requests", requests)
	}
}

func TestRemoteTabResumeRequeuesSelectionWhenReadinessDrops(t *testing.T) {
	const oldPath = "/sessions/old.jsonl"
	const targetPath = "/sessions/target.jsonl"
	started := make(chan string, 1)
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost && req.URL.Path == "/resume" {
			started <- targetPath
			return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"running":false}`)), Request: req}, nil
	})}
	previous := &remoteTabOpenSelection{
		session: remoteTabSessionState{name: "old", path: oldPath}, topicTitle: "Old",
		currentPath: oldPath, revision: 1,
	}
	tab := &remoteTab{
		id: "remote-1", state: "reconnecting", gen: 7, selectionRevision: 1,
		topicTitle: "Target", session: remoteTabSessionState{name: "target", path: targetPath},
		routing: remoteTabSessionRouting{currentPath: targetPath, running: map[string]bool{}},
	}
	a := &App{remoteTabs: map[string]*remoteTab{tab.id: tab}}
	if deferred := a.resumeRemoteTabSessionPathForOpenSelection(tab.id, "target", targetPath, "Target", 1, previous); !deferred {
		t.Fatal("readiness loss did not preserve the committed selection")
	}
	if tab.pendingSelection == nil || !tab.pendingSelection.identityCommitted || tab.pendingSelection.previous != previous {
		t.Fatalf("queued selection = %+v, want committed selection with original rollback", tab.pendingSelection)
	}
	select {
	case <-started:
		t.Fatal("resume ran while the tab was reconnecting")
	default:
	}

	a.remoteTabMu.Lock()
	tab.state, tab.client, tab.base = "ready", client, "http://127.0.0.1:43210"
	a.remoteTabMu.Unlock()
	a.applyPendingRemoteTabOpenSelection(tab.id)
	select {
	case path := <-started:
		if path != targetPath {
			t.Fatalf("resumed path = %q, want %q", path, targetPath)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued selection was not resumed after readiness returned")
	}
}

func TestRemoteTabNewSessionSupersedesDeferredCachedSelection(t *testing.T) {
	const oldPath = "/sessions/old.jsonl"
	const targetPath = "/sessions/target.jsonl"
	const freshPath = "/sessions/fresh.jsonl"
	fs := newFakeServe(t, "s3cret", []serveSessionEntry{
		{Name: "old", Path: oldPath, Title: "Old", Current: true},
		{Name: "target", Path: targetPath, Title: "Target"},
	})
	kernel := &fakeRemoteKernel{
		statuses:   []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView: RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL}, ensureToken: "s3cret",
	}
	seedBridgeTestHost(t, "box")
	a := &App{remoteRuntime: kernel}
	cleanupRemoteTabPumps(t, a)
	meta := openReadyRemoteTab(t, a, RemoteTabOpenOptions{SessionName: "old", SessionPath: oldPath})

	resumeStarted := make(chan string, 1)
	newStarted := make(chan struct{}, 1)
	fs.mu.Lock()
	fs.resumeStarted, fs.newStarted, fs.newSessionPath = resumeStarted, newStarted, freshPath
	fs.mu.Unlock()
	a.remoteTabsHostStatus("box", "reconnecting", "")
	waitForTabState(t, a, meta.ID, "reconnecting")
	if _, err := a.OpenRemoteProjectTab("box", "~/app", RemoteTabOpenOptions{SessionName: "target", SessionPath: targetPath}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.OpenRemoteProjectTab("box", "~/app", RemoteTabOpenOptions{NewSession: true}); err != nil {
		t.Fatal(err)
	}
	a.remoteTabMu.Lock()
	tab := a.remoteTabs[meta.ID]
	pending := tab.pendingSelection
	a.remoteTabMu.Unlock()
	if pending == nil || !pending.newSession || pending.path != "" {
		t.Fatalf("pending selection = %+v, want newest New Session intent", pending)
	}

	a.remoteTabsHostStatus("box", "connected", "")
	select {
	case <-newStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("deferred New Session did not rotate after reconnect")
	}
	select {
	case path := <-resumeStarted:
		t.Fatalf("superseded cached session resumed %q", path)
	default:
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		a.remoteTabMu.Lock()
		state, path, reset, currentPending := tab.state, tab.routing.currentPath, tab.session.reset, tab.pendingSelection
		a.remoteTabMu.Unlock()
		if state == "ready" && path == freshPath && reset && currentPending == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("final state/path/reset/pending = %q/%q/%v/%+v", state, path, reset, currentPending)
		}
		time.Sleep(time.Millisecond)
	}
}
