package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
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

func TestRemoteResumeTransportFailureReconcilesCommittedTarget(t *testing.T) {
	const oldPath = "/sessions/old.jsonl"
	const targetPath = "/sessions/target.jsonl"
	seedBridgeTestHost(t, "box")
	postCalls := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case req.Method == http.MethodPost && req.URL.Path == "/resume":
			postCalls++
			// Model a response lost after Serve has already committed targetPath.
			return nil, io.ErrUnexpectedEOF
		case req.Method == http.MethodGet && req.URL.Path == "/sessions":
			body := `[{"name":"target","path":"/sessions/target.jsonl","title":"Target","current":true}]`
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
		default:
			return nil, errors.New("unexpected request")
		}
	})}
	tab := &remoteTab{
		id: "remote-1", ref: RemoteTabRef{HostID: "box", Workspace: "~/app"}, state: "ready",
		client: client, base: "http://127.0.0.1:43210", gen: 7,
		topicTitle: "Old",
		session:    remoteTabSessionState{name: "old", path: oldPath},
		routing:    remoteTabSessionRouting{currentPath: oldPath, running: map[string]bool{}},
	}
	a := &App{remoteTabs: map[string]*remoteTab{tab.id: tab}}
	a.resumeRemoteTabSessionPath(tab.id, "target", targetPath, "Target")
	a.remoteTabMu.Lock()
	state, path, sessionPath, title := tab.state, tab.routing.currentPath, tab.session.path, tab.topicTitle
	rehydrating := tab.routing.rehydratingPath
	a.remoteTabMu.Unlock()
	if postCalls != 1 || state != "ready" || path != targetPath || sessionPath != targetPath || title != "Target" || rehydrating != "" {
		t.Fatalf("reconciled resume calls/state/route/session/title/rehydrating = %d/%q/%q/%q/%q/%q", postCalls, state, path, sessionPath, title, rehydrating)
	}
}

func TestRemoteResumeReplayStopsAfterLaterSessionAdoption(t *testing.T) {
	const targetPath = "/sessions/target.jsonl"
	const laterPath = "/sessions/later.jsonl"
	client := &http.Client{}
	tab := &remoteTab{
		id: "remote-1", state: "ready", client: client, gen: 7,
		session: remoteTabSessionState{name: "target", path: targetPath},
		routing: remoteTabSessionRouting{
			currentPath: targetPath, rehydratingPath: targetPath, running: map[string]bool{},
			rehydratingFrames: []json.RawMessage{
				json.RawMessage(`{"kind":"text","text":"target-first","sessionPath":"/sessions/target.jsonl"}`),
				json.RawMessage(`{"kind":"text","text":"target-second","sessionPath":"/sessions/target.jsonl"}`),
			},
		},
	}
	log := &eventLog{}
	adopted := false
	a := &App{remoteTabs: map[string]*remoteTab{tab.id: tab}}
	a.remoteEventHook = func(name string, payload any) {
		log.add(name, payload)
		if !adopted && name == "remote-tab:"+tab.id+":event" {
			data, _ := json.Marshal(payload)
			if strings.Contains(string(data), "target-first") {
				adopted = true
				a.adoptRemoteTabFrameCurrent(tab.id, tab.gen, laterPath, true)
			}
		}
	}
	a.publishRemoteTabResumeReady(tab.id, tab, client, tab.gen, targetPath)
	events := strings.Join(log.recorded(), "\n")
	if !strings.Contains(events, "target-first") || strings.Contains(events, "target-second") {
		t.Fatalf("replay crossed later adoption barrier: %s", events)
	}
	a.remoteTabMu.Lock()
	path, rehydrating := tab.routing.currentPath, tab.routing.rehydratingPath
	a.remoteTabMu.Unlock()
	if path != laterPath || rehydrating != "" {
		t.Fatalf("later route/rehydration = %q/%q, want %q/empty", path, rehydrating, laterPath)
	}
}

func TestExternalSessionAdoptionResetsAndSeedsForegroundRuntime(t *testing.T) {
	const oldPath = "/sessions/old.jsonl"
	const targetPath = "/sessions/target.jsonl"
	tab := &remoteTab{
		routing: remoteTabSessionRouting{currentPath: oldPath, running: map[string]bool{targetPath: true}},
		session: remoteTabSessionState{name: "old", path: oldPath},
		pendingEvents: map[string]json.RawMessage{
			"approval_request:old": json.RawMessage(`{"kind":"approval_request"}`),
		},
		runtime: remoteTabRuntimeState{
			running: true, turnStartedAt: 99, backgroundJobs: 3,
			pendingPrompt: true, cancelRequested: true, cancellable: true,
		},
	}
	if !adoptRemoteTabSessionPathLocked(tab, targetPath) {
		t.Fatal("target session was not adopted")
	}
	if len(tab.pendingEvents) != 0 || !tab.runtime.running || tab.runtime.turnStartedAt != 0 ||
		tab.runtime.backgroundJobs != 0 || tab.runtime.pendingPrompt || tab.runtime.cancelRequested || !tab.runtime.cancellable {
		t.Fatalf("adopted runtime retained old controller state: %+v pending=%d", tab.runtime, len(tab.pendingEvents))
	}
}
