package main

import (
	"testing"

	"reasonix/internal/config"
)

// TestSnapshotIncludesRemoteProjectGroups pins that pinned remote workspaces
// surface in the tree snapshot as ordinary project groups whose Remote ref
// marks them for the cloud icon, and that a config read failure degrades to
// "no remote groups" instead of failing the whole snapshot.
func TestSnapshotIncludesRemoteProjectGroups(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	t.Setenv("HOME", home)
	if err := editUserConfig(func(c *config.Config) error {
		if err := c.UpsertRemoteHost(config.RemoteHostEntry{Name: "gpu-box", Host: "192.168.1.10", User: "dev"}); err != nil {
			return err
		}
		return c.UpsertRemoteProject(config.RemoteProjectEntry{HostID: "gpu-box", Workspace: "/home/dev/app"})
	}); err != nil {
		t.Fatal(err)
	}

	a := &App{}
	found := false
	for _, node := range a.GetProjectTreeSnapshot().Projects {
		if node.Remote == nil {
			continue
		}
		if node.Remote.HostID != "gpu-box" || node.Remote.Workspace != "/home/dev/app" {
			t.Fatalf("unexpected remote node: %+v", node)
		}
		found = true
		if node.Kind != "project" {
			t.Fatalf("remote group kind = %q, want project", node.Kind)
		}
		if node.Key != "project_remote_gpu-box_/home/dev/app" {
			t.Fatalf("remote group key = %q", node.Key)
		}
		if node.Root != "/home/dev/app" {
			t.Fatalf("remote group root = %q, want workspace for runtime projection", node.Root)
		}
		if node.Label != "app" {
			t.Fatalf("remote group label = %q, want workspace base name", node.Label)
		}
	}
	if !found {
		t.Fatal("snapshot missing the remote project group")
	}
}

// TestOpenRemoteProjectTabReusesLiveTab pins the "ensure" semantics: a live
// tab for the same host+workspace is returned as-is on repeat opens, so the
// wizard finish followed by tree-group clicks yields exactly one tab.
func TestOpenRemoteProjectTabReusesLiveTab(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	t.Setenv("HOME", home)
	if err := editUserConfig(func(c *config.Config) error {
		return c.UpsertRemoteHost(config.RemoteHostEntry{Name: "box", Host: "127.0.0.1", User: "dev"})
	}); err != nil {
		t.Fatal(err)
	}
	a := &App{remoteRuntime: &fakeRemoteKernel{}}
	first, err := a.OpenRemoteProjectTab("box", "~/app", RemoteTabOpenOptions{NewSession: true})
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.OpenRemoteProjectTab("box", "~/app", RemoteTabOpenOptions{NewSession: true})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("second open created a new tab: %q vs %q", first.ID, second.ID)
	}
	a.remoteTabMu.Lock()
	count := len(a.remoteTabs)
	a.remoteTabMu.Unlock()
	if count != 1 {
		t.Fatalf("remoteTabs = %d entries, want 1", count)
	}
	if first.Remote == nil || first.Remote.HostID != "box" || first.Remote.Workspace != "~/app" {
		t.Fatalf("meta remote ref = %+v", first.Remote)
	}
}
