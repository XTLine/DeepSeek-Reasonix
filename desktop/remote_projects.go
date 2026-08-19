package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"reasonix/internal/config"
)

// ── View structs mirrored in frontend/src/lib/types.ts ──

// RemoteTabRef marks a tab as remote and binds it to a host+workspace pair.
type RemoteTabRef struct {
	HostID    string `json:"hostId"`
	Workspace string `json:"workspace"`
}

type RemoteProjectView struct {
	HostID    string `json:"hostId"`
	Workspace string `json:"workspace"`
	Title     string `json:"title,omitempty"`
	Color     string `json:"color,omitempty"`
}

// RemoteTabOpenOptions mirrors the frontend opts bag: NewSession lands the
// tab in a fresh serve session; SessionName resumes a listed one.
type RemoteTabOpenOptions struct {
	NewSession  bool   `json:"newSession,omitempty"`
	SessionName string `json:"sessionName,omitempty"`
}

// RemoteTabStateView is the payload on the remote-tab:{id}:state channel.
// State: connecting | ready | reconnecting | serve_down | error.
type RemoteTabStateView struct {
	State string `json:"state"`
	Error string `json:"error,omitempty"`
}

// remoteTab is one open remote project tab.
type remoteTab struct {
	id          string
	ref         RemoteTabRef
	state       string
	err         string
	newSession  bool
	sessionName string
	hostLabel   string

	// Bridge fields, mutated under App.remoteTabMu. client keeps the serve
	// session cookie in its jar across reconnects; token is retained for a
	// re-handshake when that cookie expires; gen lets a superseded SSE pump
	// self-exit once a reconnect starts a newer one.
	client *http.Client
	base   string
	token  string
	gen    uint64
	cancel context.CancelFunc
}

// ── Registry CRUD (user config, same lock discipline as remote hosts) ──

func (a *App) ListRemoteProjects() ([]RemoteProjectView, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	out := make([]RemoteProjectView, 0, len(cfg.Remote.Projects))
	for _, p := range cfg.Remote.Projects {
		out = append(out, remoteProjectEntryToView(p))
	}
	return out, nil
}

func (a *App) AddRemoteProject(hostID, workspace string) (RemoteProjectView, error) {
	var view RemoteProjectView
	err := editUserConfig(func(c *config.Config) error {
		entry := config.RemoteProjectEntry{
			HostID:    strings.TrimSpace(hostID),
			Workspace: strings.TrimSpace(workspace),
		}
		if err := c.UpsertRemoteProject(entry); err != nil {
			return err
		}
		view = remoteProjectEntryToView(entry)
		return nil
	})
	if err != nil {
		return RemoteProjectView{}, err
	}
	return view, nil
}

func (a *App) RemoveRemoteProject(hostID, workspace string) error {
	return editUserConfig(func(c *config.Config) error {
		c.RemoveRemoteProject(strings.TrimSpace(hostID), strings.TrimSpace(workspace))
		return nil
	})
}

func remoteProjectEntryToView(p config.RemoteProjectEntry) RemoteProjectView {
	return RemoteProjectView{HostID: p.HostID, Workspace: p.Workspace, Title: p.Title}
}

// remoteProjectNodes lists pinned remote workspaces as project group shells
// for the tree snapshot. Read failures degrade to "no remote projects" at the
// caller — a broken config must not take the whole tree down.
func (a *App) remoteProjectNodes() ([]ProjectNode, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	out := make([]ProjectNode, 0, len(cfg.Remote.Projects))
	for _, p := range cfg.Remote.Projects {
		label := strings.TrimSpace(p.Title)
		if label == "" {
			label = remoteWorkspaceName(p.Workspace)
		}
		out = append(out, ProjectNode{
			Key:    "project_remote_" + p.HostID + "_" + p.Workspace,
			Kind:   "project",
			Label:  label,
			Root:   p.Workspace,
			Remote: &RemoteTabRef{HostID: p.HostID, Workspace: p.Workspace},
		})
	}
	return out, nil
}

// ── Remote project tabs ──

// OpenRemoteProjectTab registers the project (idempotent), opens an in-app
// tab for the remote workspace, and returns its meta immediately. The remote
// Serve bootstrap runs in the background: a first run downloads/installs the
// CLI and can take minutes, so the surface follows progress through
// remote-tab:{id}:state events instead of this promise.
func (a *App) OpenRemoteProjectTab(hostID, workspace string, opts RemoteTabOpenOptions) (TabMeta, error) {
	hostID = strings.TrimSpace(hostID)
	workspace = strings.TrimSpace(workspace)
	if hostID == "" || workspace == "" {
		return TabMeta{}, fmt.Errorf("remote project tab: host and workspace are required")
	}
	cfg, err := config.Load()
	if err != nil {
		return TabMeta{}, err
	}
	host, ok := cfg.RemoteHost(hostID)
	if !ok {
		return TabMeta{}, fmt.Errorf("remote host %q is not configured", hostID)
	}
	if _, err := a.AddRemoteProject(hostID, workspace); err != nil {
		return TabMeta{}, err
	}

	// Reuse a live tab for the same remote workspace: repeated clicks on the
	// tree group (or wizard finish followed by a group click) must not stack
	// tabs. A terminal-error tab is replaced by a fresh one below.
	a.remoteTabMu.Lock()
	var reuse *remoteTab
	for _, existing := range a.remoteTabs {
		if existing.ref.HostID == hostID && existing.ref.Workspace == workspace && existing.state != "error" {
			reuse = existing
			break
		}
	}
	a.remoteTabMu.Unlock()
	if reuse != nil {
		meta := remoteTabMeta(reuse, host.Name)
		a.activateRemoteTab(reuse.id, meta)
		return meta, nil
	}

	ref := RemoteTabRef{HostID: hostID, Workspace: workspace}
	tabID := newTabID()
	tab := &remoteTab{id: tabID, ref: ref, state: "connecting", newSession: opts.NewSession, sessionName: opts.SessionName, hostLabel: host.Name}
	a.remoteTabMu.Lock()
	if a.remoteTabs == nil {
		a.remoteTabs = map[string]*remoteTab{}
	}
	// Drop a terminal-error tab for the same ref so repeated retries cannot
	// grow the registry (no CloseRemoteTab binding exists yet).
	for id, existing := range a.remoteTabs {
		if existing.ref.HostID == hostID && existing.ref.Workspace == workspace && existing.state == "error" {
			delete(a.remoteTabs, id)
		}
	}
	a.remoteTabs[tabID] = tab
	a.remoteTabMu.Unlock()
	a.emitRemoteTabState(tabID, "connecting", "")

	a.goSafe("remoteTabServe", func() { a.bootstrapRemoteTab(tabID, hostID, workspace) })

	meta := remoteTabMeta(tab, host.Name)
	a.activateRemoteTab(tabID, meta)
	return meta, nil
}

// activateRemoteTab highlights the tab in the strip and tells the frontend
// chrome to adopt it.
func (a *App) activateRemoteTab(tabID string, meta TabMeta) {
	a.remoteTabMu.Lock()
	a.remoteActiveTabID = tabID
	a.remoteTabMu.Unlock()
	a.emitRemoteEvent("remote-tab:opened", meta)
}

// remoteTabMeta builds the frontend-facing shape of one remote tab; the
// create and reuse paths share it so both return identical metas.
func remoteTabMeta(tab *remoteTab, hostLabel string) TabMeta {
	return TabMeta{
		ID:            tab.id,
		Scope:         "project",
		WorkspaceRoot: tab.ref.Workspace,
		WorkspaceName: remoteWorkspaceName(tab.ref.Workspace),
		TopicTitle:    remoteWorkspaceName(tab.ref.Workspace),
		Label:         hostLabel,
		Mode:          "normal",
		Active:        true,
		Cwd:           tab.ref.Workspace,
		Remote:        &tab.ref,
	}
}

// bootstrapRemoteTab drives one remote tab to a terminal state: ensure the
// SSH connection, ensure the remote Serve + loopback tunnel, then report
// ready (or the failure) on the tab's state channel.
func (a *App) bootstrapRemoteTab(tabID, hostID, workspace string) {
	rt, err := a.remoteRT()
	if err != nil {
		a.emitRemoteTabState(tabID, "error", err.Error())
		return
	}
	// Connect is idempotent: an already connecting/connected host returns
	// nil, a stopped generation is replaced with a fresh dial.
	if err := rt.Connect(hostID); err != nil {
		a.emitRemoteTabState(tabID, "error", err.Error())
		return
	}
	if err := waitForRemoteHost(rt, hostID, 60*time.Second); err != nil {
		a.emitRemoteTabState(tabID, "error", err.Error())
		return
	}
	ctx := a.bootContext()
	if ctx == nil {
		ctx = context.Background()
	}
	view, token, err := rt.EnsureServer(ctx, hostID, workspace)
	if err != nil {
		a.emitRemoteTabState(tabID, "error", err.Error())
		return
	}
	if view.State != "ready" || view.LocalURL == "" {
		msg := view.Error
		if msg == "" {
			msg = view.Message
		}
		if msg == "" {
			msg = "remote serve did not report a local URL"
		}
		a.emitRemoteTabState(tabID, "serve_down", msg)
		return
	}
	a.remoteTabMu.Lock()
	openTab := a.remoteTabs[tabID]
	a.remoteTabMu.Unlock()
	if openTab == nil {
		return // closed while the bootstrap was in flight
	}
	// ctx outlives the call: the pump derives from it, while the handshake
	// and session entry inside run under a bounded sub-context.
	opts := RemoteTabOpenOptions{NewSession: openTab.newSession, SessionName: openTab.sessionName}
	if err := a.attachRemoteTabServe(ctx, tabID, view.LocalURL, token, opts); err != nil {
		a.emitRemoteTabState(tabID, "error", err.Error())
		return
	}
	a.saveLastRemoteWorkspace(hostID, workspace)
	a.emitRemoteTabState(tabID, "ready", "")
}

// waitForRemoteHost polls the kernel until the host is usable. The frontend
// has waitForRemoteConnection over status events; this is the same contract
// server-side for cold OpenRemoteProjectTab calls.
func waitForRemoteHost(rt remoteKernel, hostID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		for _, s := range rt.Statuses() {
			if s.HostID != hostID {
				continue
			}
			switch s.State {
			case "connected", "degraded":
				return nil
			case "stopped":
				if s.Error != "" {
					return fmt.Errorf("remote host %q: %s", hostID, s.Error)
				}
				return fmt.Errorf("remote host %q stopped", hostID)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("remote host %q: connection timed out", hostID)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func (a *App) emitRemoteTabState(tabID, state, errMsg string) {
	a.remoteTabMu.Lock()
	if tab := a.remoteTabs[tabID]; tab != nil {
		tab.state = state
		tab.err = errMsg
	}
	a.remoteTabMu.Unlock()
	a.emitRemoteEvent(fmt.Sprintf("remote-tab:%s:state", tabID), RemoteTabStateView{State: state, Error: errMsg})
}

// remoteWorkspaceName is posix-safe (remote paths on a Windows host must not
// go through filepath).
func remoteWorkspaceName(ws string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(ws), "/")
	if trimmed == "" || trimmed == "~" {
		return "~"
	}
	if i := strings.LastIndex(trimmed, "/"); i >= 0 {
		return trimmed[i+1:]
	}
	return trimmed
}
