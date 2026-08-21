package main

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"strings"
	"sync/atomic"
	"time"

	"reasonix/internal/config"
)

// remoteTabModelSeq stamps every remote-tab model assignment; the credential
// proxy uses the stamps to resolve "most recently set" without relying on map
// iteration order.
var remoteTabModelSeq atomic.Uint64

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
	// Merged marks that an overlapping pin already existed and the returned
	// Workspace is that existing group's canonical path — no new pin was added.
	Merged bool `json:"merged,omitempty"`
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
	// topicTitle starts as the workspace name; the serve's LLM-generated
	// session title replaces it after a turn completes.
	topicTitle           string
	titleRefreshInFlight bool
	// sessionReset marks that the tab currently holds a fresh, contentless
	// session (POST /new landed, serve has not listed it yet): a further
	// NewSession open reuses this blank instead of resetting again — the
	// same contract as the local reusable-blank tab.
	sessionReset bool
	// model is the desktop-owned current model ref for this remote tab.
	// Chat requests still tunnel through the credential proxy; the serve
	// session is not rebuilt on switch. modelSeq orders concurrent writes so
	// route registration can pick the most recent deterministically.
	model    string
	modelSeq uint64

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
	hostID = strings.TrimSpace(hostID)
	workspace = strings.TrimSpace(workspace)
	var view RemoteProjectView
	err := editUserConfig(func(c *config.Config) error {
		// Overlapping pins on one host collapse into the existing group:
		// re-running the wizard on a nested directory must not multiply serve
		// processes and session lists over the same files. The returned view
		// carries the canonical (existing) workspace with Merged set.
		if merged, ok := resolveOverlappingWorkspace(c.Remote.Projects, hostID, workspace); ok {
			workspace = merged
			view = remoteProjectEntryToView(config.RemoteProjectEntry{HostID: hostID, Workspace: merged})
			view.Merged = true
			return nil
		}
		entry := config.RemoteProjectEntry{
			HostID:    hostID,
			Workspace: workspace,
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

// resolveOverlappingWorkspace finds the existing pin on the same host that the
// requested workspace should merge into: an exact match wins, then the
// nearest ancestor pin, then the shallowest descendant pin. Remote paths are
// POSIX; "~" and unresolvable relatives simply never overlap (safe default).
func resolveOverlappingWorkspace(existing []config.RemoteProjectEntry, hostID, workspace string) (string, bool) {
	target := cleanRemoteWorkspace(workspace)
	if target == "" {
		return "", false
	}
	ancestor, ancestorDepth := "", -1
	descendant, descendantDepth := "", 1<<30
	for _, p := range existing {
		if p.HostID != hostID {
			continue
		}
		cand := cleanRemoteWorkspace(p.Workspace)
		if cand == "" {
			continue
		}
		switch {
		case cand == target:
			return p.Workspace, true
		case isRemoteSubpath(cand, target): // existing pin is an ancestor of the request
			if d := pathDepth(cand); ancestor == "" || d > ancestorDepth {
				ancestor, ancestorDepth = p.Workspace, d
			}
		case isRemoteSubpath(target, cand): // existing pin is a descendant of the request
			if d := pathDepth(cand); descendant == "" || d < descendantDepth {
				descendant, descendantDepth = p.Workspace, d
			}
		}
	}
	if ancestor != "" {
		return ancestor, true
	}
	return descendant, descendant != ""
}

func cleanRemoteWorkspace(ws string) string {
	ws = strings.TrimSpace(ws)
	if ws == "" || ws == "~" {
		return ws
	}
	return path.Clean(strings.TrimRight(ws, "/"))
}

// isRemoteSubpath reports parent/child nesting between two cleaned POSIX
// paths; equal paths are deliberately not subpaths of each other.
func isRemoteSubpath(parent, child string) bool {
	return strings.HasPrefix(child, parent+"/")
}

func pathDepth(cleaned string) int {
	if cleaned == "" || cleaned == "/" {
		return 0
	}
	return strings.Count(cleaned, "/")
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
	// The pin registry collapses overlapping paths into the existing group:
	// whatever nested path the caller asked for, the tab must land on the
	// canonical workspace so tabs and serves stay one-per-group.
	proj, err := a.AddRemoteProject(hostID, workspace)
	if err != nil {
		return TabMeta{}, err
	}
	workspace = proj.Workspace

	// Reuse a live tab for the same remote workspace: repeated clicks on the
	// tree group (or wizard finish followed by a group click) must not stack
	// tabs. A terminal-error tab is replaced by a fresh one below. A restored
	// disconnected shell is revived in place: same id, reconnect bootstrap.
	a.remoteTabMu.Lock()
	var reuse *remoteTab
	for _, existing := range a.remoteTabs {
		if existing.ref.HostID == hostID && existing.ref.Workspace == workspace && existing.state != "error" {
			reuse = existing
			break
		}
	}
	revive := reuse != nil && reuse.state == "disconnected"
	if revive {
		reuse.state = "connecting"
	}
	a.remoteTabMu.Unlock()
	if reuse != nil {
		if revive {
			a.emitRemoteTabState(reuse.id, "connecting", "")
			a.goSafe("remoteTabServe", func() { a.bootstrapRemoteTab(reuse.id, hostID, workspace) })
		} else if name := strings.TrimSpace(opts.SessionName); name != "" {
			a.resumeRemoteTabSession(reuse.id, name)
		} else {
			a.remoteTabMu.Lock()
			blank := reuse.sessionReset
			a.remoteTabMu.Unlock()
			// Reuse the pending blank like EnsureBlankTab does locally; only
			// reset again once the current session earned content.
			if opts.NewSession && !blank {
				a.resetRemoteTabSession(reuse.id)
			}
		}
		meta := remoteTabMeta(reuse, host.Name)
		a.activateRemoteTab(reuse.id, meta)
		return meta, nil
	}

	ref := RemoteTabRef{HostID: hostID, Workspace: workspace}
	tabID := newTabID()
	tab := &remoteTab{id: tabID, ref: ref, state: "connecting", newSession: opts.NewSession, sessionName: opts.SessionName, hostLabel: host.Name, topicTitle: remoteWorkspaceName(workspace), model: resolveNewSessionModel(cfg)}
	tab.modelSeq = remoteTabModelSeq.Add(1)
	a.remoteTabMu.Lock()
	if a.remoteTabs == nil {
		a.remoteTabs = map[string]*remoteTab{}
	}
	// Drop a terminal-error tab for the same ref so repeated retries cannot
	// grow the registry.
	for id, existing := range a.remoteTabs {
		if existing.ref.HostID == hostID && existing.ref.Workspace == workspace && existing.state == "error" {
			delete(a.remoteTabs, id)
			a.remoteTabOrder = removeRemoteTabOrderID(a.remoteTabOrder, id)
		}
	}
	a.remoteTabs[tabID] = tab
	a.remoteTabOrder = append(a.remoteTabOrder, tabID)
	a.remoteTabMu.Unlock()
	a.emitRemoteTabState(tabID, "connecting", "")

	a.goSafe("remoteTabServe", func() { a.bootstrapRemoteTab(tabID, hostID, workspace) })

	meta := remoteTabMeta(tab, host.Name)
	a.activateRemoteTab(tabID, meta)
	// Persist after activation so the file records the highlighted remote id.
	a.saveTabsFromRemote()
	return meta, nil
}

// restoreRemoteTabShells rebuilds disconnected registry entries from the
// persisted tab file so remote tabs survive a restart. Shells never connect
// on their own: activating one (SetActiveTab) or opening its project
// bootstraps the reconnect, which lands in a fresh blank session.
func (a *App) restoreRemoteTabShells(f desktopTabsFile) {
	if len(f.RemoteTabs) == 0 {
		return
	}
	// Local ids are snapshotted under a.mu BEFORE taking remoteTabMu — the
	// save path locks in the a.mu → tabsSaveMu → remoteTabMu order, so this
	// function must never hold remoteTabMu while wanting a.mu.
	a.mu.RLock()
	localIDs := make(map[string]bool, len(a.tabs))
	for id := range a.tabs {
		localIDs[id] = true
	}
	a.mu.RUnlock()

	cfg, cfgErr := config.Load()
	a.remoteTabMu.Lock()
	if a.remoteTabs == nil {
		a.remoteTabs = map[string]*remoteTab{}
	}
	for _, entry := range f.RemoteTabs {
		id := strings.TrimSpace(entry.ID)
		hostID := strings.TrimSpace(entry.HostID)
		ws := strings.TrimSpace(entry.Workspace)
		if id == "" || hostID == "" || ws == "" || localIDs[id] || a.remoteTabs[id] != nil {
			continue
		}
		hostLabel := hostID
		if cfgErr == nil {
			if host, ok := cfg.RemoteHost(hostID); ok && strings.TrimSpace(host.Name) != "" {
				hostLabel = host.Name
			}
		}
		title := strings.TrimSpace(entry.TopicTitle)
		if title == "" {
			title = remoteWorkspaceName(ws)
		}
		restored := &remoteTab{
			id: id, ref: RemoteTabRef{HostID: hostID, Workspace: ws},
			state: "disconnected", newSession: true,
			hostLabel: hostLabel, topicTitle: title, model: strings.TrimSpace(entry.Model),
		}
		restored.modelSeq = remoteTabModelSeq.Add(1)
		a.remoteTabs[id] = restored
		a.remoteTabOrder = append(a.remoteTabOrder, id)
	}
	if f.ActiveTab != "" && a.remoteTabs[f.ActiveTab] != nil {
		a.remoteActiveTabID = f.ActiveTab
	}
	a.remoteTabMu.Unlock()
}

// removeRemoteTabOrderID drops one id from the remote strip order.
func removeRemoteTabOrderID(order []string, id string) []string {
	out := order[:0]
	for _, existing := range order {
		if existing != id {
			out = append(out, existing)
		}
	}
	return out
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
// create and reuse paths share it so both return identical metas. RemoteState
// seeds the surface before any state event arrives this run (restored shells).
func remoteTabMeta(tab *remoteTab, hostLabel string) TabMeta {
	label := hostLabel
	if strings.TrimSpace(tab.model) != "" {
		label = tab.model
	}
	return TabMeta{
		ID:            tab.id,
		Scope:         "project",
		WorkspaceRoot: tab.ref.Workspace,
		WorkspaceName: remoteWorkspaceName(tab.ref.Workspace),
		TopicTitle:    tab.topicTitle,
		Label:         label,
		Mode:          "normal",
		Active:        true,
		Cwd:           tab.ref.Workspace,
		Remote:        &tab.ref,
		RemoteState:   tab.state,
	}
}

// bootstrapRemoteTab drives one remote tab to a terminal state: ensure the
// SSH connection, ensure the remote Serve + loopback tunnel, then report
// ready (or the failure) on the tab's state channel.
func (a *App) bootstrapRemoteTab(tabID, hostID, workspace string) {
	// Idempotence guard: a concurrent reattach may have brought this tab to
	// ready while the open call was still in flight — bootstrapping again
	// would re-enter the session and stack a second pump.
	a.remoteTabMu.Lock()
	tabState := ""
	if tab := a.remoteTabs[tabID]; tab != nil {
		tabState = tab.state
	}
	a.remoteTabMu.Unlock()
	if tabState == "ready" {
		return
	}
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
	a.remoteTabMu.Lock()
	openTab.sessionReset = openTab.newSession
	if openTab.newSession {
		// A bootstrapped fresh session carries the localized default title,
		// same as the live-tab reset path.
		openTab.topicTitle = a.localizedDefaultTopicTitle()
	}
	a.remoteTabMu.Unlock()
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
