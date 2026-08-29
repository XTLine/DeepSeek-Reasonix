package main

import "testing"

// A remote project group's qualified root ("remote-project:<host>:<workspace>")
// is not a filesystem path. Listing must answer with an empty page instead of
// building a topic-state scope from it — on Windows the colon is an invalid
// path component and the legacy metadata read failed with a user-visible
// "topic metadata is unavailable (io)" toast right after the connect wizard.
func TestListProjectTopicsAnswersRemoteRootWithEmptyPage(t *testing.T) {
	isolateDesktopUserDirs(t)
	app := NewApp()
	page, err := app.ListProjectTopics(ProjectTopicPageRequest{
		Scope:         "project",
		WorkspaceRoot: "remote-project:gpu-box:/home/dev/app",
		Limit:         50,
	})
	if err != nil {
		t.Fatalf("remote root listing failed: %v", err)
	}
	if page.Items == nil || len(page.Items) != 0 {
		t.Fatalf("remote root page items = %#v, want an empty page", page.Items)
	}
}
