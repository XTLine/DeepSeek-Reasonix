package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type remoteTabSessionRouting struct {
	currentPath string
	running     map[string]bool
	revision    uint64
}

// enterRemoteSession is the compatibility wrapper used by bridge tests.
func enterRemoteSession(ctx context.Context, client *http.Client, base string, opts RemoteTabOpenOptions) error {
	_, err := enterRemoteSessionTarget(ctx, client, base, opts)
	return err
}

// preflightRemoteSessionTarget resolves the foreground identity without
// mutating Serve. Attach uses it to publish routing before the event pump can
// observe an immediate replay from a detached controller.
func preflightRemoteSessionTarget(ctx context.Context, client *http.Client, base string, opts RemoteTabOpenOptions) (serveSessionEntry, error) {
	name := strings.TrimSpace(opts.SessionName)
	if path := strings.TrimSpace(opts.SessionPath); path != "" {
		return serveSessionEntry{Name: name, Path: path, Title: strings.TrimSpace(opts.SessionTitle), Current: true}, nil
	}
	if name == "" {
		return serveCurrentSession(ctx, client, base)
	}
	sessions, err := serveSessions(ctx, client, base)
	if err != nil {
		return serveSessionEntry{}, err
	}
	for _, session := range sessions {
		if session.Name == name {
			session.Current = true
			return session, nil
		}
	}
	return serveSessionEntry{}, fmt.Errorf("remote session %q not found", name)
}

func enterRemoteSessionTarget(ctx context.Context, client *http.Client, base string, opts RemoteTabOpenOptions) (serveSessionEntry, error) {
	name := strings.TrimSpace(opts.SessionName)
	if opts.NewSession {
		path, err := servePostSessionPath(ctx, client, serveURL(base, "/new"), nil)
		if err != nil {
			return serveSessionEntry{}, err
		}
		return serveSessionEntry{Path: path, Current: true}, nil
	}
	if sessionPath := strings.TrimSpace(opts.SessionPath); sessionPath != "" {
		body, err := json.Marshal(map[string]string{"path": sessionPath})
		if err != nil {
			return serveSessionEntry{}, err
		}
		if err := servePost(ctx, client, serveURL(base, "/resume"), body); err != nil {
			return serveSessionEntry{}, err
		}
		return serveSessionEntry{Name: name, Path: sessionPath, Title: strings.TrimSpace(opts.SessionTitle), Current: true}, nil
	}
	// Focus-only attaches retain the current session; only explicit NewSession
	// may abandon it.
	if name == "" {
		current, _ := serveCurrentSession(ctx, client, base)
		return current, nil
	}
	sessions, err := serveSessions(ctx, client, base)
	if err != nil {
		return serveSessionEntry{}, err
	}
	for _, session := range sessions {
		if session.Name != name {
			continue
		}
		body, err := json.Marshal(map[string]string{"path": session.Path})
		if err != nil {
			return serveSessionEntry{}, err
		}
		if err := servePost(ctx, client, serveURL(base, "/resume"), body); err != nil {
			return serveSessionEntry{}, err
		}
		session.Current = true
		return session, nil
	}
	return serveSessionEntry{}, fmt.Errorf("remote session %q not found", name)
}

func serveCurrentSession(ctx context.Context, client *http.Client, base string) (serveSessionEntry, error) {
	sessions, err := serveSessions(ctx, client, base)
	if err != nil {
		return serveSessionEntry{}, err
	}
	for _, session := range sessions {
		if session.Current {
			return session, nil
		}
	}
	return serveSessionEntry{}, nil
}

// routeRemoteTabFrame tracks background runtime without leaking its frames
// into the foreground reducer. Untagged frames remain legacy-compatible.
func (a *App) routeRemoteTabFrame(tabID string, gen uint64, sessionPath, kind string) bool {
	a.remoteTabMu.Lock()
	tab := a.remoteTabs[tabID]
	if tab == nil || tab.gen != gen {
		a.remoteTabMu.Unlock()
		return false
	}
	if tab.routing.running == nil {
		tab.routing.running = map[string]bool{}
	}
	changed := false
	if sessionPath != "" {
		switch kind {
		case "turn_started":
			tab.routing.revision++
			changed = !tab.routing.running[sessionPath]
			tab.routing.running[sessionPath] = true
		case "turn_done":
			tab.routing.revision++
			changed = tab.routing.running[sessionPath]
			tab.routing.running[sessionPath] = false
		}
	}
	foreground := sessionPath == "" || tab.routing.currentPath != "" && sessionPath == tab.routing.currentPath
	_, knownBackground := tab.routing.running[sessionPath]
	// A detached turn can finish while background jobs remain. Its later notice
	// makes /sessions authoritative again, so refresh project-tree rows without
	// forwarding that background notice to the foreground reducer.
	backgroundChanged := !foreground && (changed || kind == "notice" && knownBackground)
	meta := remoteTabMetaLocked(tab)
	a.remoteTabMu.Unlock()
	if backgroundChanged {
		a.emitRemoteEvent("remote-tab:updated", meta)
	}
	return foreground
}

// remoteTabFramePathUnknown distinguishes a possible foreground recovery or
// slash-command rotation from a path already observed as background work.
func (a *App) remoteTabFramePathUnknown(tabID string, gen uint64, sessionPath string) bool {
	if sessionPath == "" {
		return false
	}
	a.remoteTabMu.Lock()
	defer a.remoteTabMu.Unlock()
	tab := a.remoteTabs[tabID]
	if tab == nil || tab.gen != gen || sessionPath == tab.routing.currentPath {
		return false
	}
	_, knownBackground := tab.routing.running[sessionPath]
	return !knownBackground
}

func (a *App) reconcileRemoteTabFramePath(tabID string, gen uint64, sessionPath string) bool {
	if _, err := a.RemoteTabStatus(tabID); err != nil {
		return false
	}
	a.remoteTabMu.Lock()
	tab := a.remoteTabs[tabID]
	if tab == nil || tab.gen != gen {
		a.remoteTabMu.Unlock()
		return false
	}
	if tab.routing.currentPath == sessionPath {
		a.remoteTabMu.Unlock()
		return true
	}
	// The authoritative status confirmed another foreground path. Remember
	// this tag as background so a lossy detached stream does not synchronously
	// fetch /status for every later token or notice from the same session.
	if tab.routing.running == nil {
		tab.routing.running = map[string]bool{}
	}
	var refresh *TabMeta
	if _, known := tab.routing.running[sessionPath]; !known {
		tab.routing.running[sessionPath] = false
		tab.routing.revision++
		meta := remoteTabMetaLocked(tab)
		refresh = &meta
	}
	a.remoteTabMu.Unlock()
	if refresh != nil {
		a.emitRemoteEvent("remote-tab:updated", *refresh)
	}
	return false
}

func (a *App) routeRemoteTabFrameReconciled(tabID string, gen uint64, sessionPath, kind string) bool {
	pathUnknown := a.remoteTabFramePathUnknown(tabID, gen, sessionPath)
	if a.routeRemoteTabFrame(tabID, gen, sessionPath, kind) {
		return true
	}
	return pathUnknown && a.reconcileRemoteTabFramePath(tabID, gen, sessionPath) &&
		a.routeRemoteTabFrame(tabID, gen, sessionPath, kind)
}
