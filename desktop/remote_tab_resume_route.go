package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

type remoteTabProvisionalResume struct {
	targetPath      string
	previousPath    string
	previousPending map[string]json.RawMessage
	previousRuntime remoteTabRuntimeState
	active          bool
}

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

func (a *App) beginRemoteTabProvisionalResume(tabID string, tab *remoteTab, client *http.Client, gen uint64, targetPath string) remoteTabProvisionalResume {
	route := remoteTabProvisionalResume{targetPath: strings.TrimSpace(targetPath)}
	a.remoteTabMu.Lock()
	defer a.remoteTabMu.Unlock()
	current := a.remoteTabs[tabID]
	if current != tab || current.client != client || current.gen != gen || current.state != "ready" {
		return route
	}
	route.previousPath = current.routing.currentPath
	if route.targetPath == route.previousPath {
		return route
	}
	route.previousPending = cloneRemotePendingEvents(current.pendingEvents)
	route.previousRuntime = current.runtime
	route.active = true
	current.routing.currentPath = route.targetPath
	current.routing.rehydratingPath = route.targetPath
	current.routing.revision++
	current.pendingEvents = nil
	current.runtime.revision++
	current.runtime.running = current.routing.running[route.targetPath]
	current.runtime.turnStartedAt = 0
	current.runtime.pendingPrompt = false
	current.runtime.cancelRequested = false
	current.runtime.cancellable = current.runtime.running || current.runtime.backgroundJobs > 0
	return route
}

func (a *App) rollbackRemoteTabProvisionalResume(tabID string, tab *remoteTab, client *http.Client, gen uint64, route remoteTabProvisionalResume) {
	if !route.active {
		return
	}
	a.remoteTabMu.Lock()
	defer a.remoteTabMu.Unlock()
	current := a.remoteTabs[tabID]
	if current != tab || current.client != client || current.gen != gen ||
		current.routing.currentPath != route.targetPath || current.routing.rehydratingPath != route.targetPath {
		return
	}
	current.routing.currentPath = route.previousPath
	current.routing.rehydratingPath = ""
	current.routing.revision++
	current.pendingEvents = route.previousPending
	restoredRuntime := route.previousRuntime
	restoredRuntime.revision = max(current.runtime.revision, route.previousRuntime.revision) + 1
	current.runtime = restoredRuntime
}

func (a *App) commitRemoteTabResume(tabID string, tab *remoteTab, client *http.Client, gen uint64, route remoteTabProvisionalResume, target serveSessionEntry, title string) (TabMeta, bool) {
	a.remoteTabMu.Lock()
	defer a.remoteTabMu.Unlock()
	current := a.remoteTabs[tabID]
	if current != tab || current.client != client || current.gen != gen || current.state != "ready" ||
		route.active && current.routing.rehydratingPath != route.targetPath {
		return TabMeta{}, false
	}
	current.topicTitle = title
	current.session.reset = false
	current.session.newSession = false
	current.session.name = strings.TrimSpace(target.Name)
	current.session.path = target.Path
	current.routing.currentPath = target.Path
	// Close the provisional routing epoch so a listing that began while
	// /resume was in flight cannot publish its pre-switch snapshot afterward.
	current.routing.revision++
	current.runtime.revision++
	current.runtime.running = current.runtime.running || target.Running || current.routing.running[target.Path]
	current.runtime.cancellable = current.runtime.cancellable || current.runtime.running
	return remoteTabMetaLocked(current), true
}

func (a *App) publishRemoteTabResumeReady(tabID string, tab *remoteTab, client *http.Client, gen uint64, route remoteTabProvisionalResume) {
	if !a.transitionRemoteTabState(tabID, gen, "ready", "ready", "") {
		return
	}
	a.remoteTabMu.Lock()
	defer a.remoteTabMu.Unlock()
	current := a.remoteTabs[tabID]
	if current == tab && current.client == client && current.gen == gen && current.routing.rehydratingPath == route.targetPath {
		current.routing.rehydratingPath = ""
	}
}

func cloneRemotePendingEvents(src map[string]json.RawMessage) map[string]json.RawMessage {
	if src == nil {
		return nil
	}
	dst := make(map[string]json.RawMessage, len(src))
	for key, frame := range src {
		dst[key] = append(json.RawMessage(nil), frame...)
	}
	return dst
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
	return true
}
