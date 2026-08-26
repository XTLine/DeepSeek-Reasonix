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
			kind, _, _, _ := probeRemoteTabFrame(string(frame))
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
	} else {
		tab.routing.rehydratingFrames = append(tab.routing.rehydratingFrames, append(json.RawMessage(nil), frame...))
	}
	return true
}
