package main

import (
	"strings"
)

// ListProjectTopics keeps authoritative topic-state failures visible to the
// Wails caller instead of converting a future-schema or unreadable database
// into an apparently empty sidebar page.
func (a *App) ListProjectTopics(req ProjectTopicPageRequest) (ProjectTopicPage, error) {
	// Remote project groups carry a qualified virtual root
	// ("remote-project:<host>:<workspace>") that is not a filesystem path.
	// Their sessions come from the serve-side catalog, so answer with an empty
	// page instead of building a topic-state scope (and legacy metadata paths,
	// whose colon is an invalid Windows path component) from that string.
	if strings.HasPrefix(strings.TrimSpace(req.WorkspaceRoot), "remote-project:") {
		return ProjectTopicPage{Items: []ProjectNode{}}, nil
	}
	if err := topicStateReadable(topicTitleRoot(req.Scope, req.WorkspaceRoot)); err != nil {
		return ProjectTopicPage{Items: []ProjectNode{}}, err
	}
	return a.listProjectTopics(req)
}
