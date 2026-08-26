package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestRemoteTabServeDownSavedSessionClearsPendingBeforeDelayedMarker(t *testing.T) {
	const oldPath = "/sessions/old.jsonl"
	const savedPath = "/sessions/saved.jsonl"
	feed := make(chan string, 1)
	fs := newFakeServe(t, "s3cret", []serveSessionEntry{
		{Name: "old", Path: oldPath, Title: "Old", Current: true},
		{Name: "saved", Path: savedPath, Title: "Saved"},
	})
	kernel := &fakeRemoteKernel{
		statuses:   []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView: RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL}, ensureToken: "s3cret",
	}
	seedBridgeTestHost(t, "box")
	log := &eventLog{}
	a := &App{remoteRuntime: kernel, remoteEventHook: log.add}
	cleanupRemoteTabPumps(t, a)
	meta := openReadyRemoteTab(t, a, RemoteTabOpenOptions{SessionName: "old", SessionPath: oldPath})

	a.remoteTabMu.Lock()
	tab := a.remoteTabs[meta.ID]
	if tab.cancel != nil {
		tab.cancel()
	}
	tab.gen++
	tab.cancel, tab.client, tab.base, tab.token = nil, nil, "", ""
	tab.state = "serve_down"
	tab.pendingEvents = map[string]json.RawMessage{
		"approval_request:old": json.RawMessage(`{"kind":"approval_request","approval":{"id":"old"}}`),
	}
	tab.runtime = remoteTabRuntimeState{pendingPrompt: true, cancellable: true}
	a.remoteTabMu.Unlock()
	fs.mu.Lock()
	fs.eventFeed = feed
	fs.mu.Unlock()

	if _, err := a.OpenRemoteProjectTab("box", "~/app", RemoteTabOpenOptions{SessionName: "saved", SessionPath: savedPath, SessionTitle: "Saved"}); err != nil {
		t.Fatal(err)
	}
	waitForTabState(t, a, meta.ID, "ready")
	a.remoteTabMu.Lock()
	path, pending, prompt := tab.routing.currentPath, len(tab.pendingEvents), tab.runtime.pendingPrompt
	a.remoteTabMu.Unlock()
	if path != savedPath || pending != 0 || prompt {
		t.Fatalf("saved attach route/pending/prompt = %q/%d/%v, want %q/0/false", path, pending, prompt, savedPath)
	}

	eventPrefix := "remote-tab:" + meta.ID + ":event"
	before := log.count(eventPrefix)
	feed <- `{"kind":"session_changed","sessionPath":"/sessions/saved.jsonl","sessionCurrent":true}`
	waitForRemoteEventCount(t, log, eventPrefix, before+1)
	a.remoteTabMu.Lock()
	pending, prompt = len(tab.pendingEvents), tab.runtime.pendingPrompt
	a.remoteTabMu.Unlock()
	if pending != 0 || prompt {
		t.Fatalf("delayed saved-session marker restored stale prompt: pending=%d prompt=%v", pending, prompt)
	}
}

func TestExternalResumeRefreshesAdoptedSessionTitle(t *testing.T) {
	const firstPath = "/sessions/first.jsonl"
	const nextPath = "/sessions/next.jsonl"
	feed := make(chan string, 1)
	fs := newFakeServe(t, "s3cret", []serveSessionEntry{{Name: "first", Path: firstPath, Title: "First title", Current: true}})
	kernel := &fakeRemoteKernel{
		statuses:   []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView: RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL}, ensureToken: "s3cret",
	}
	seedBridgeTestHost(t, "box")
	log := &eventLog{}
	a := &App{remoteRuntime: kernel, remoteEventHook: log.add}
	cleanupRemoteTabPumps(t, a)
	fs.mu.Lock()
	fs.eventFeed = feed
	fs.mu.Unlock()
	meta := openReadyRemoteTab(t, a, RemoteTabOpenOptions{SessionName: "first", SessionPath: firstPath})
	fs.mu.Lock()
	fs.sessions = []serveSessionEntry{{Name: "next", Path: nextPath, Title: "Next title", Current: true}}
	fs.mu.Unlock()

	eventPrefix := "remote-tab:" + meta.ID + ":event"
	before := log.count(eventPrefix)
	feed <- `{"kind":"session_changed","sessionPath":"/sessions/next.jsonl","sessionCurrent":true}`
	waitForRemoteEventCount(t, log, eventPrefix, before+1)
	deadline := time.Now().Add(2 * time.Second)
	for {
		a.remoteTabMu.Lock()
		title, path := a.remoteTabs[meta.ID].topicTitle, a.remoteTabs[meta.ID].routing.currentPath
		a.remoteTabMu.Unlock()
		if title == "Next title" && path == nextPath {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("externally adopted title/path = %q/%q, want Next title/%q", title, path, nextPath)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRemoteAttachRetiringSessionConflictKeepsCurrentReady(t *testing.T) {
	const currentPath = "/sessions/current.jsonl"
	fs := newFakeServe(t, "s3cret", []serveSessionEntry{
		{Name: "current", Path: currentPath, Title: "Current title", Current: true},
		{Name: "retiring", Path: "/sessions/retiring.jsonl", Title: "Retiring title"},
	})
	fs.mu.Lock()
	fs.failEnter = "session is finishing background teardown; retry shortly"
	fs.mu.Unlock()
	kernel := &fakeRemoteKernel{
		statuses:   []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView: RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL}, ensureToken: "s3cret",
	}
	seedBridgeTestHost(t, "box")
	a := &App{remoteRuntime: kernel}
	cleanupRemoteTabPumps(t, a)
	meta := openReadyRemoteTab(t, a, RemoteTabOpenOptions{SessionName: "retiring", SessionPath: "/sessions/retiring.jsonl"})
	a.remoteTabMu.Lock()
	tab := a.remoteTabs[meta.ID]
	state, path, title := tab.state, tab.routing.currentPath, tab.topicTitle
	a.remoteTabMu.Unlock()
	if state != "ready" || path != currentPath || title != "Current title" {
		t.Fatalf("soft attach state/path/title = %q/%q/%q, want ready/%q/Current title", state, path, title, currentPath)
	}
}
