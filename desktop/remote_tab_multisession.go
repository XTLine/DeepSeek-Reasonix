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
}

// enterRemoteSession is the compatibility wrapper used by bridge tests.
func enterRemoteSession(ctx context.Context, client *http.Client, base string, opts RemoteTabOpenOptions) error {
	_, err := enterRemoteSessionTarget(ctx, client, base, opts)
	return err
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
			changed = !tab.routing.running[sessionPath]
			tab.routing.running[sessionPath] = true
		case "turn_done":
			changed = tab.routing.running[sessionPath]
			tab.routing.running[sessionPath] = false
		}
	}
	foreground := sessionPath == "" || tab.routing.currentPath != "" && sessionPath == tab.routing.currentPath
	backgroundChanged := changed && !foreground
	meta := remoteTabMetaLocked(tab)
	a.remoteTabMu.Unlock()
	if backgroundChanged {
		a.emitRemoteEvent("remote-tab:updated", meta)
	}
	return foreground
}
