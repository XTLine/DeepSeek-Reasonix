package main

import (
	"fmt"
	"strings"
)

// beginRemoteTabResume publishes the clicked identity synchronously, then
// performs Serve's potentially slow resume outside the Wails binding path.
func (a *App) beginRemoteTabResume(tabID, name, sessionPath, sessionTitle string) error {
	a.remoteTabMu.Lock()
	tab := a.remoteTabs[tabID]
	if tab == nil {
		a.remoteTabMu.Unlock()
		return fmt.Errorf("remote tab %q closed while switching sessions", tabID)
	}
	tab.routing.resumeGen++
	resumeGen := tab.routing.resumeGen
	previous := snapshotRemoteTabResumeIdentity(tab)
	tab.session.newSession, tab.session.reset = false, false
	tab.session.name = strings.TrimSpace(name)
	if path := strings.TrimSpace(sessionPath); path != "" {
		prepareRemoteTabResumeRouteLocked(tab, path, false)
	}
	if title := strings.TrimSpace(sessionTitle); title != "" {
		tab.topicTitle = title
	}
	meta := remoteTabMetaLocked(tab)
	a.remoteTabMu.Unlock()
	a.emitRemoteEvent("remote-tab:updated", meta)
	a.goSafe("remoteTabResume", func() {
		a.resumeRemoteTabSessionPathGeneration(tabID, resumeGen, name, sessionPath, sessionTitle, &previous)
	})
	return nil
}
