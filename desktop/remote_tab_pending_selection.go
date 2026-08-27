package main

import (
	"strings"
)

// remoteTabPendingOpenSelection retains a cached-session click while the
// existing shell is still attaching. The local identity must not advance
// until Serve can accept the matching /resume request.
type remoteTabPendingOpenSelection struct {
	name     string
	path     string
	title    string
	revision uint64
	deferred bool
}

func newRemoteTabPendingOpenSelection(opts RemoteTabOpenOptions) *remoteTabPendingOpenSelection {
	return &remoteTabPendingOpenSelection{
		name: strings.TrimSpace(opts.SessionName), path: strings.TrimSpace(opts.SessionPath),
		title: strings.TrimSpace(opts.SessionTitle),
	}
}

// applyPendingRemoteTabOpenSelection commits only the newest deferred click
// once the shell reaches ready, then uses the normal resume/rollback path.
func (a *App) applyPendingRemoteTabOpenSelection(tabID string) {
	a.tabSelectionMu.Lock()
	defer a.tabSelectionMu.Unlock()
	a.remoteTabMu.Lock()
	tab := a.remoteTabs[tabID]
	a.remoteTabMu.Unlock()
	if tab == nil {
		return
	}
	tab.routeEventMu.Lock()
	a.remoteTabMu.Lock()
	current := a.remoteTabs[tabID]
	if current != tab || current.state != "ready" || current.pendingSelection == nil {
		a.remoteTabMu.Unlock()
		tab.routeEventMu.Unlock()
		return
	}
	selection := current.pendingSelection
	current.pendingSelection = nil
	if current.selectionRevision != selection.revision {
		a.remoteTabMu.Unlock()
		tab.routeEventMu.Unlock()
		return
	}
	previous := &remoteTabOpenSelection{
		session: current.session, topicTitle: current.topicTitle,
		currentPath: current.routing.currentPath,
		pending:     cloneRemotePendingEvents(current.pendingEvents), runtime: current.runtime,
		revision: selection.revision,
	}
	current.session.newSession = false
	current.session.name = selection.name
	current.session.path = selection.path
	if selection.title != "" {
		current.topicTitle = selection.title
	}
	if selection.path != "" {
		commitRemoteTabAttachRoute(current, selection.path, false)
	}
	current.err = ""
	meta := remoteTabMetaLocked(current)
	a.remoteTabMu.Unlock()
	tab.routeEventMu.Unlock()

	a.emitRemoteEvent("remote-tab:updated", meta)
	a.saveTabsFromRemote()
	a.resumeRemoteTabOpenAsync(tabID, selection.name, selection.path, selection.title, previous)
}
