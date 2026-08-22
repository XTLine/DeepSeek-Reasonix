package main

import (
	"os"
	"testing"
	"time"

	"reasonix/internal/config"
)

// The switch-latency contract: OpenRemoteProjectTab must return as soon as the
// clicked row's identity is adopted — the serve-side /resume round trip (with
// its snapshot + full session load under the serve lock) runs in the background
// and the surface follows the connecting→ready state events.

func TestOpenRemoteProjectTabResumeReturnsBeforeServeRoundTrip(t *testing.T) {
	fs := newFakeServe(t, "s3cret", []serveSessionEntry{
		{Name: "s1", Path: "/remote/sessions/s1.jsonl", Title: "First", Turns: 1, Current: true},
		{Name: "s2", Path: "/remote/sessions/s2.jsonl", Title: "Second", Turns: 1},
	})
	kernel := &fakeRemoteKernel{
		statuses:    []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView:  RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL},
		ensureToken: "s3cret",
	}
	seedBridgeTestHost(t, "box")
	a := &App{remoteRuntime: kernel}
	cleanupRemoteTabPumps(t, a)
	meta := openReadyRemoteTab(t, a, RemoteTabOpenOptions{SessionName: "s1", SessionPath: "/remote/sessions/s1.jsonl"})

	fs.mu.Lock()
	fs.slowResume = 400 * time.Millisecond
	fs.mu.Unlock()

	start := time.Now()
	switched, err := a.OpenRemoteProjectTab("box", "~/app", RemoteTabOpenOptions{
		SessionName: "s2",
		SessionPath: "/remote/sessions/s2.jsonl",
		SessionTitle: "Second",
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("open blocked on the resume round trip: %v (serve delay 400ms)", elapsed)
	}
	if want := "box\x00~/app\x00s2"; switched.TopicID != want {
		t.Fatalf("returned meta TopicID = %q, want the adopted s2 identity %q", switched.TopicID, want)
	}
	waitForTabState(t, a, meta.ID, "ready")
	a.remoteTabMu.Lock()
	name, path := a.remoteTabs[meta.ID].sessionName, a.remoteTabs[meta.ID].currentSessionPath
	a.remoteTabMu.Unlock()
	if name != "s2" || path != "/remote/sessions/s2.jsonl" {
		t.Fatalf("post-resume identity = %q @ %q, want s2", name, path)
	}
	cleanupRemoteTabPumps(t, a)
}

// A slow resume that lands after a newer switch must not stomp the newer
// session's identity or re-emit ready out of order.
func TestOpenRemoteProjectTabLateResumeCannotStompNewerSwitch(t *testing.T) {
	fs := newFakeServe(t, "s3cret", []serveSessionEntry{
		{Name: "s1", Path: "/remote/sessions/s1.jsonl", Title: "First", Turns: 1, Current: true},
		{Name: "s2", Path: "/remote/sessions/s2.jsonl", Title: "Second", Turns: 1},
		{Name: "s3", Path: "/remote/sessions/s3.jsonl", Title: "Third", Turns: 1},
	})
	kernel := &fakeRemoteKernel{
		statuses:    []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView:  RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL},
		ensureToken: "s3cret",
	}
	seedBridgeTestHost(t, "box")
	a := &App{remoteRuntime: kernel}
	cleanupRemoteTabPumps(t, a)
	meta := openReadyRemoteTab(t, a, RemoteTabOpenOptions{SessionName: "s1", SessionPath: "/remote/sessions/s1.jsonl"})

	fs.mu.Lock()
	fs.slowResume = 300 * time.Millisecond
	fs.mu.Unlock()

	if _, err := a.OpenRemoteProjectTab("box", "~/app", RemoteTabOpenOptions{SessionName: "s2", SessionPath: "/remote/sessions/s2.jsonl"}); err != nil {
		t.Fatal(err)
	}
	// Immediately switch again: s3 wins; s2's resume is still in flight.
	if _, err := a.OpenRemoteProjectTab("box", "~/app", RemoteTabOpenOptions{SessionName: "s3", SessionPath: "/remote/sessions/s3.jsonl"}); err != nil {
		t.Fatal(err)
	}
	waitForTabState(t, a, meta.ID, "ready")
	// Let any unguarded late apply surface before asserting.
	time.Sleep(500 * time.Millisecond)
	a.remoteTabMu.Lock()
	name := a.remoteTabs[meta.ID].sessionName
	state := a.remoteTabs[meta.ID].state
	a.remoteTabMu.Unlock()
	if name != "s3" {
		t.Fatalf("late s2 resume stomped the newer switch: sessionName = %q, want s3", name)
	}
	if state != "ready" {
		t.Fatalf("tab state = %q, want ready", state)
	}
	cleanupRemoteTabPumps(t, a)
}

// Re-registering an already-pinned remote project must not rewrite the user
// config file: OpenRemoteProjectTab re-adds on every click, and the repeated
// disk write shows up as switch latency.
func TestAddRemoteProjectSkipsConfigRewriteWhenPinned(t *testing.T) {
	seedBridgeTestHost(t, "box")
	if _, err := addRemoteProjectForTest(t, "box", "~/app"); err != nil {
		t.Fatal(err)
	}
	path := config.UserConfigPath()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	statBefore, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)

	view, err := addRemoteProjectForTest(t, "box", "~/app")
	if err != nil {
		t.Fatal(err)
	}
	if !view.Merged {
		t.Fatalf("re-add did not merge into the existing pin: %+v", view)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	statAfter, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if statAfter.ModTime() != statBefore.ModTime() {
		t.Fatalf("config rewritten on re-add: mtime %v -> %v", statBefore.ModTime(), statAfter.ModTime())
	}
	if string(after) != string(before) {
		t.Fatalf("config content changed on re-add")
	}
}

func addRemoteProjectForTest(t *testing.T, hostID, workspace string) (RemoteProjectView, error) {
	t.Helper()
	a := &App{}
	return a.AddRemoteProject(hostID, workspace)
}
