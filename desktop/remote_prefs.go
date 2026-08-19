package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"reasonix/internal/config"
	"reasonix/internal/fileutil"
)

var remotePrefsMu sync.Mutex

// remotePrefs is desktop-only remote UI state, stored beside the other desktop
// JSON prefs (desktop-workspaces.json, desktop-tabs.json). All fields are
// optional so an older file decodes cleanly.
type remotePrefs struct {
	LastHostID          string            `json:"lastHostId,omitempty"`
	LastWorkspaceByHost map[string]string `json:"lastWorkspaceByHost,omitempty"`
	ExplorerTab         string            `json:"explorerTab,omitempty"`
	// SessionTitles holds desktop-owned display titles for remote sessions,
	// mirroring how local topic titles live on the desktop rather than in
	// the agent: key hostID\x00workspace\x00session-name.
	SessionTitles map[string]string `json:"sessionTitles,omitempty"`
	// PinnedSessions lists pinned remote sessions by the same key, ordered.
	PinnedSessions []string `json:"pinnedSessions,omitempty"`
	// CredentialProxySecret is the random root of the per-host virtual tokens
	// used by local-proxy credential mode. Rotating it revokes every token.
	CredentialProxySecret string `json:"credentialProxySecret,omitempty"`
}

func remotePrefsPath() string {
	dir := config.MemoryUserDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "desktop-remote.json")
}

func loadRemotePrefs() remotePrefs {
	var p remotePrefs
	path := remotePrefsPath()
	if path == "" {
		return p
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return p
	}
	_ = json.Unmarshal(data, &p)
	if p.LastWorkspaceByHost == nil {
		p.LastWorkspaceByHost = map[string]string{}
	}
	if p.SessionTitles == nil {
		p.SessionTitles = map[string]string{}
	}
	return p
}

func remoteSessionTitleKey(hostID, workspace, name string) string {
	return hostID + "\x00" + workspace + "\x00" + name
}

func remoteSessionTitleOverride(hostID, workspace, name string) string {
	remotePrefsMu.Lock()
	defer remotePrefsMu.Unlock()
	return loadRemotePrefs().SessionTitles[remoteSessionTitleKey(hostID, workspace, name)]
}

func remoteSessionPinned(hostID, workspace, name string) bool {
	remotePrefsMu.Lock()
	defer remotePrefsMu.Unlock()
	for _, key := range loadRemotePrefs().PinnedSessions {
		if key == remoteSessionTitleKey(hostID, workspace, name) {
			return true
		}
	}
	return false
}

func setRemoteSessionPinned(hostID, workspace, name string, pinned bool) {
	remotePrefsMu.Lock()
	defer remotePrefsMu.Unlock()
	p := loadRemotePrefs()
	key := remoteSessionTitleKey(hostID, workspace, name)
	next := p.PinnedSessions[:0:0]
	for _, existing := range p.PinnedSessions {
		if existing != key || pinned {
			next = append(next, existing)
		}
	}
	if pinned && !remoteSessionPinnedLocked(p, key) {
		next = append(next, key)
	}
	p.PinnedSessions = next
	saveRemotePrefs(p)
}

func remoteSessionPinnedLocked(p remotePrefs, key string) bool {
	for _, existing := range p.PinnedSessions {
		if existing == key {
			return true
		}
	}
	return false
}

func setRemoteSessionTitleOverride(hostID, workspace, name, title string) {
	remotePrefsMu.Lock()
	defer remotePrefsMu.Unlock()
	p := loadRemotePrefs()
	key := remoteSessionTitleKey(hostID, workspace, name)
	if strings.TrimSpace(title) == "" {
		delete(p.SessionTitles, key)
	} else {
		p.SessionTitles[key] = strings.TrimSpace(title)
	}
	saveRemotePrefs(p)
}

func saveRemotePrefs(p remotePrefs) error {
	path := remotePrefsPath()
	if path == "" {
		return fmt.Errorf("remote prefs: no user dir")
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("remote prefs: encode: %w", err)
	}
	return fileutil.AtomicWriteFile(path, data, 0o600)
}

func (a *App) saveLastRemoteWorkspace(hostID, workspace string) {
	remotePrefsMu.Lock()
	defer remotePrefsMu.Unlock()
	p := loadRemotePrefs()
	if p.LastWorkspaceByHost == nil {
		p.LastWorkspaceByHost = map[string]string{}
	}
	p.LastHostID = hostID
	p.LastWorkspaceByHost[hostID] = workspace
	saveRemotePrefs(p)
}

// RemoteLastWorkspace returns the last opened workspace for hostID (bound so
// the frontend can prefill the server card).
func (a *App) RemoteLastWorkspace(hostID string) string {
	remotePrefsMu.Lock()
	defer remotePrefsMu.Unlock()
	return loadRemotePrefs().LastWorkspaceByHost[hostID]
}
