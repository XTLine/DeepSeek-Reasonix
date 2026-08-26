package main

import "testing"

func TestRemoteTabTitleRefreshRejectsDifferentServeCurrent(t *testing.T) {
	fs := newFakeServe(t, "s3cret", []serveSessionEntry{
		{Name: "a", Path: "/a.jsonl", Title: "Title A", Current: true},
		{Name: "b", Path: "/b.jsonl", Title: "Title B"},
	})
	kernel := &fakeRemoteKernel{
		statuses:   []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView: RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL}, ensureToken: "s3cret",
	}
	seedBridgeTestHost(t, "box")
	a := &App{remoteRuntime: kernel}
	cleanupRemoteTabPumps(t, a)
	meta := openReadyRemoteTab(t, a, RemoteTabOpenOptions{SessionName: "a", SessionPath: "/a.jsonl", SessionTitle: "Selected A"})
	fs.mu.Lock()
	fs.sessions[0].Current = false
	fs.sessions[1].Current = true
	fs.mu.Unlock()

	a.refreshRemoteTabTitle(meta.ID)

	a.remoteTabMu.Lock()
	tab := a.remoteTabs[meta.ID]
	name, sessionPath, route, title := tab.session.name, tab.session.path, tab.routing.currentPath, tab.topicTitle
	a.remoteTabMu.Unlock()
	if name != "a" || sessionPath != "/a.jsonl" || route != "/a.jsonl" || title != "Selected A" {
		t.Fatalf("different Serve current replaced title-refresh target: %q/%q/%q/%q", name, sessionPath, route, title)
	}
}
