package main

import (
	"testing"
	"time"
)

func waitForCatalogTopic(t *testing.T, app *App, scope, workspaceRoot, topicID string) []ProjectNode {
	t.Helper()
	app.startSessionCatalog()
	t.Cleanup(func() { app.stopSessionCatalog(time.Second) })
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		nodes := app.ListProjectTree()
		for _, folder := range nodes {
			if scope == "project" && (!sameProjectRoot(folder.Root, workspaceRoot) || folder.Kind != "project") {
				continue
			}
			if scope != "project" && folder.Kind != "global_folder" {
				continue
			}
			for _, topic := range folder.Children {
				if topic.TopicID == topicID {
					return nodes
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("catalog topic %q did not become visible", topicID)
	return nil
}

func waitForCatalogTreeCondition(t *testing.T, app *App, description string, matches func([]ProjectNode) bool) []ProjectNode {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var nodes []ProjectNode
	for time.Now().Before(deadline) {
		nodes = app.ListProjectTree()
		if matches(nodes) {
			return nodes
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("catalog did not reach %s: %#v", description, nodes)
	return nil
}
