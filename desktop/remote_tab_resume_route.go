package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

func probeRemoteTabFrame(frame string) (kind, path string, current, reset bool) {
	var probe struct {
		Kind           string `json:"kind"`
		SessionPath    string `json:"sessionPath"`
		SessionCurrent bool   `json:"sessionCurrent"`
		SessionReset   bool   `json:"sessionReset"`
	}
	kind = "?"
	if json.Unmarshal([]byte(frame), &probe) == nil && probe.Kind != "" {
		kind = probe.Kind
	}
	return kind, strings.TrimSpace(probe.SessionPath), probe.SessionCurrent, probe.SessionReset
}

// reconcileRemoteTabResumeRoute installs the route Serve reports after an
// ambiguous transport failure. The common unchanged case restores the exact
// preflight snapshot; an externally changed route drops controller-local state
// and publishes the authoritative identity behind a new ready barrier.
func (a *App) reconcileRemoteTabResumeRoute(tabID string, resumeGen, connectionGen uint64, previous *remoteTabResumeIdentity, authoritative serveSessionEntry, resumeErr error) {
	authoritative.Path = strings.TrimSpace(authoritative.Path)
	if previous != nil && authoritative.Path == previous.routePath {
		a.rollbackRemoteTabResume(tabID, resumeGen, previous)
		a.transitionRemoteTabState(tabID, connectionGen, "ready", "ready", resumeErr.Error())
		return
	}
	a.remoteTabMu.Lock()
	tab := a.remoteTabs[tabID]
	if tab == nil || tab.gen != connectionGen || tab.routing.resumeGen != resumeGen || tab.state != "ready" {
		a.remoteTabMu.Unlock()
		return
	}
	if !adoptRemoteTabSessionPathLocked(tab, authoritative.Path) {
		tab.routing.rehydratingPath = ""
		tab.routing.rehydratingFrames = nil
	}
	tab.session.name = strings.TrimSpace(authoritative.Name)
	tab.session.path = authoritative.Path
	tab.session.newSession = false
	tab.session.reset = false
	tab.runtime.running = authoritative.Running || tab.routing.running[authoritative.Path]
	tab.runtime.cancellable = tab.runtime.running
	title := strings.TrimSpace(authoritative.Title)
	if title == "" {
		title = strings.TrimSpace(authoritative.Name)
	}
	if title == "" {
		title = remoteWorkspaceName(tab.ref.Workspace)
	}
	tab.topicTitle = title
	meta := remoteTabMetaLocked(tab)
	a.remoteTabMu.Unlock()
	a.emitRemoteEvent("remote-tab:updated", meta)
	a.saveTabsFromRemote()
	a.transitionRemoteTabState(tabID, connectionGen, "ready", "ready", resumeErr.Error())
}

func (a *App) publishRemoteTabResumeReady(tabID string, tab *remoteTab, client *http.Client, gen uint64, targetPath string) {
	if !a.transitionRemoteTabState(tabID, gen, "ready", "ready", "") {
		return
	}
	for {
		a.remoteTabMu.Lock()
		current := a.remoteTabs[tabID]
		if current != tab || current.client != client || current.gen != gen || current.routing.rehydratingPath != targetPath {
			a.remoteTabMu.Unlock()
			return
		}
		frames := current.routing.rehydratingFrames
		current.routing.rehydratingFrames = nil
		if len(frames) == 0 {
			// Clearing the path under the same lock that producers use closes the
			// replay/live race: a later frame either joined this drain or observes
			// the committed live route after every older frame was published.
			current.routing.rehydratingPath = ""
			a.remoteTabMu.Unlock()
			return
		}
		a.remoteTabMu.Unlock()
		for _, frame := range frames {
			kind, path, _, _ := probeRemoteTabFrame(string(frame))
			a.remoteTabMu.Lock()
			current = a.remoteTabs[tabID]
			valid := current == tab && current.client == client && current.gen == gen &&
				current.routing.currentPath == targetPath &&
				current.routing.rehydratingPath == targetPath &&
				(path == "" || path == targetPath)
			a.remoteTabMu.Unlock()
			if !valid {
				return
			}
			a.publishRemoteTabFrame(tabID, gen, kind, frame)
		}
	}
}

func remotePendingEventKey(kind string, frame json.RawMessage) string {
	var probe struct {
		Approval *struct {
			ID string `json:"id"`
		} `json:"approval"`
		Ask *struct {
			ID string `json:"id"`
		} `json:"ask"`
	}
	_ = json.Unmarshal(frame, &probe)
	id := ""
	if probe.Approval != nil {
		id = probe.Approval.ID
	} else if probe.Ask != nil {
		id = probe.Ask.ID
	}
	return kind + ":" + strings.TrimSpace(id)
}

func (a *App) bufferRemoteTabResumeFrame(tabID string, gen uint64, sessionPath, kind string, frame json.RawMessage) bool {
	sessionPath = strings.TrimSpace(sessionPath)
	if sessionPath == "" {
		return false
	}
	key := ""
	switch kind {
	case "approval_request", "ask_request":
		key = remotePendingEventKey(kind, frame)
	case "extension_surface":
		if remotePendingExtensionForm(frame) {
			key = remotePendingExtensionFormKey
		}
	}
	a.remoteTabMu.Lock()
	defer a.remoteTabMu.Unlock()
	tab := a.remoteTabs[tabID]
	if tab == nil || tab.gen != gen || tab.routing.rehydratingPath != sessionPath {
		return false
	}
	if key != "" {
		tab.runtime.revision++
		if tab.pendingEvents == nil {
			tab.pendingEvents = make(map[string]json.RawMessage)
		}
		tab.pendingEvents[key] = append(json.RawMessage(nil), frame...)
		tab.runtime.pendingPrompt = true
		tab.runtime.cancellable = true
	}
	// Actionable frames must also cross the same fenced handoff as ordinary
	// output. Snapshot hydration deduplicates prompt IDs against live-buffered
	// events, while this replay closes the window after snapshot capture.
	tab.routing.rehydratingFrames = append(tab.routing.rehydratingFrames, append(json.RawMessage(nil), frame...))
	return true
}
