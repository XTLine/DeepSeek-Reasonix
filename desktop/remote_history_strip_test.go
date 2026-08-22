package main

import (
	"strings"
	"testing"
)

// The desktop-side leak defense: RemoteTabSnapshot must strip host-injected
// transient blocks (<reasoning-language> and friends) from user rows of the
// serve /history payload itself, so the surface never depends on the remote
// serve running the latest strip fix.

func TestRemoteTabSnapshotStripsTransientUserBlocks(t *testing.T) {
	fs := newFakeServe(t, "s3cret", []serveSessionEntry{
		{Name: "s1", Path: "/remote/sessions/s1.jsonl", Current: true},
	})
	fs.mu.Lock()
	fs.historyBody = `[` +
		`{"role":"user","content":"<reasoning-language>必须使用简体中文书写全部可见思考/推理文本:从第一个字开始就用中文</reasoning-language>\n\n3的英文是什么"},` +
		`{"role":"assistant","content":"three <b>bold</b> stays"}]`
	fs.mu.Unlock()
	kernel := &fakeRemoteKernel{
		statuses:    []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView:  RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL},
		ensureToken: "s3cret",
	}
	seedBridgeTestHost(t, "box")
	a := &App{remoteRuntime: kernel}
	cleanupRemoteTabPumps(t, a)
	meta := openReadyRemoteTab(t, a, RemoteTabOpenOptions{SessionName: "s1", SessionPath: "/remote/sessions/s1.jsonl"})

	snap, err := a.RemoteTabSnapshot(meta.ID, RemoteTabSnapshotOptions{Members: []string{"/history", "/status"}})
	if err != nil {
		t.Fatal(err)
	}
	body := string(snap.History)
	if strings.Contains(body, "<reasoning-language>") {
		t.Fatalf("history still leaks the transient block: %.200s", body)
	}
	if !strings.Contains(body, "3的英文是什么") {
		t.Fatalf("stripped user row lost the user-authored text: %.200s", body)
	}
	if !strings.Contains(body, "three <b>bold</b> stays") {
		t.Fatalf("non-user row must pass through untouched: %.200s", body)
	}
	cleanupRemoteTabPumps(t, a)
}

// The 304 reuse path must hand back the sanitized body too: the cache stores
// the post-strip bytes, never the raw serve payload.
func TestRemoteTabSnapshotCacheStoresSanitizedHistory(t *testing.T) {
	fs := newFakeServe(t, "s3cret", []serveSessionEntry{
		{Name: "s1", Path: "/remote/sessions/s1.jsonl", Current: true},
	})
	fs.mu.Lock()
	fs.historyBody = `[{"role":"user","content":"<response-language>zh</response-language>\n\nhello"}]`
	fs.mu.Unlock()
	kernel := &fakeRemoteKernel{
		statuses:    []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView:  RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL},
		ensureToken: "s3cret",
	}
	seedBridgeTestHost(t, "box")
	a := &App{remoteRuntime: kernel}
	cleanupRemoteTabPumps(t, a)
	meta := openReadyRemoteTab(t, a, RemoteTabOpenOptions{SessionName: "s1", SessionPath: "/remote/sessions/s1.jsonl"})

	for i := 0; i < 2; i++ {
		snap, err := a.RemoteTabSnapshot(meta.ID, RemoteTabSnapshotOptions{Members: []string{"/history"}})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(snap.History), "<response-language>") {
			t.Fatalf("pass %d leaked the transient block from cache: %.200s", i, string(snap.History))
		}
	}
	a.remoteTabMu.Lock()
	for _, entry := range a.remoteTabs[meta.ID].snapshotCache {
		if entry.body != nil && strings.Contains(string(entry.body), "<response-language>") {
			t.Fatal("snapshot cache stored the raw (unsanitized) history body")
		}
	}
	a.remoteTabMu.Unlock()
	cleanupRemoteTabPumps(t, a)
}
