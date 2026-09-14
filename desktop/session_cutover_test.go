package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"reasonix/internal/boot"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/provider"
	"reasonix/internal/session"
)

func TestDesktopHistorySliceUsesCanonicalDurableIndex(t *testing.T) {
	isolateDesktopUserDirs(t)
	model, _ := configureSwitchableDefaultModels(t)
	app := NewApp()
	app.ctx = context.Background()
	root := t.TempDir()
	dir := desktopSessionDir(root)
	service := app.desktopSessionService(dir)
	runtime, err := service.Create(t.Context(), session.CreateOptions{SessionID: "canonical-history"})
	if err != nil {
		t.Fatal(err)
	}
	appendSessionTestMessage(t, runtime, "history-user", provider.Message{ID: "history-user", Role: provider.RoleUser, Origin: provider.MessageOriginUser, Content: "durable user turn"})

	ctrl, err := app.buildTabControllerBoot(app.ctx, boot.Options{Model: model, WorkspaceRoot: root, SessionDir: dir, Sink: event.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ctrl.(control.IdentityLifecycle).OpenSession(t.Context(), runtime.Ref()); err != nil {
		t.Fatal(err)
	}
	large := "durable assistant " + strings.Repeat("x", historyInlineRefThreshold+1024)
	// Append after the controller's agent projection was created. The legacy
	// live-history path cannot see this message; the canonical query can.
	appendSessionTestMessage(t, runtime, "history-assistant", provider.Message{ID: "history-assistant", Role: provider.RoleAssistant, Content: large})
	tab := &WorkspaceTab{ID: "canonical-history-tab", Scope: "project", WorkspaceRoot: root, SessionID: runtime.Ref().SessionID, Ready: true, Ctrl: ctrl, sink: &tabEventSink{tabID: "canonical-history-tab", app: app}, disabledMCP: map[string]ServerView{}}
	app.tabs = map[string]*WorkspaceTab{tab.ID: tab}
	app.tabOrder = []string{tab.ID}
	app.activeTabID = tab.ID
	t.Cleanup(func() { ctrl.Close() })

	page := app.HistorySliceForTab(tab.ID, HistorySliceRequest{Turns: 12, Entries: 120, Bytes: 512 << 10})
	if page.Error != "" || page.Source != "canonical-index" {
		t.Fatalf("canonical history page = source %q error %q", page.Source, page.Error)
	}
	if page.TotalTurns != 1 || len(page.Entries) != 2 {
		t.Fatalf("canonical history shape = turns %d entries %d, want 1/2", page.TotalTurns, len(page.Entries))
	}
	assistant := page.Entries[1]
	if len(assistant.Refs) != 1 || assistant.Refs[0].Field != "content" {
		t.Fatalf("large canonical message refs = %+v", assistant.Refs)
	}
	chunk := app.HistoryContentForTab(tab.ID, assistant.Refs[0], 0)
	if chunk.Stale || !chunk.Done || chunk.Data != large {
		t.Fatalf("canonical expanded content = stale:%v done:%v bytes:%d, want %d", chunk.Stale, chunk.Done, len(chunk.Data), len(large))
	}

	appendSessionTestMessage(t, runtime, "history-next", provider.Message{ID: "history-next", Role: provider.RoleUser, Origin: provider.MessageOriginUser, Content: "new turn"})
	if stale := app.HistoryContentForTab(tab.ID, assistant.Refs[0], 0); !stale.Stale {
		t.Fatal("content ref from older durable snapshot must become stale after append")
	}
}

func appendSessionTestMessage(t *testing.T, runtime *session.Runtime, operationID string, message provider.Message) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"message": message})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Session().AppendBatch(t.Context(), operationID, []session.Event{{Kind: "message/complete", Payload: payload}}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Session().Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func appendSessionTestModel(t *testing.T, runtime *session.Runtime, operationID, modelRef string) {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"modelRef": modelRef})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Session().AppendBatch(t.Context(), operationID, []session.Event{{Kind: "session/config", Payload: payload}}); err != nil {
		t.Fatal(err)
	}
}

func TestDesktopV3CatalogResumeRenameAndDeleteUseSessionIdentity(t *testing.T) {
	isolateDesktopUserDirs(t)
	model, targetModel := configureSwitchableDefaultModels(t)
	app := NewApp()
	app.ctx = context.Background()
	root := t.TempDir()
	dir := desktopSessionDir(root)
	service := app.desktopSessionService(dir)

	first, err := service.Create(t.Context(), session.CreateOptions{SessionID: "first-v3"})
	if err != nil {
		t.Fatal(err)
	}
	appendSessionTestModel(t, first, "first-model", model)
	appendSessionTestMessage(t, first, "first-message", provider.Message{ID: "user-first", Role: provider.RoleUser, Content: "first conversation"})
	second, err := service.Create(t.Context(), session.CreateOptions{SessionID: "second-v3"})
	if err != nil {
		t.Fatal(err)
	}
	appendSessionTestModel(t, second, "second-model", targetModel)
	appendSessionTestMessage(t, second, "second-message", provider.Message{ID: "user-second", Role: provider.RoleUser, Content: "second conversation"})

	ctrl, err := app.buildTabControllerBoot(app.ctx, boot.Options{Model: model, WorkspaceRoot: root, SessionDir: dir, Sink: event.Discard})
	if err != nil {
		t.Fatal(err)
	}
	identity := ctrl.(control.IdentityLifecycle)
	if _, err := identity.OpenSession(t.Context(), first.Ref()); err != nil {
		t.Fatal(err)
	}
	tab := &WorkspaceTab{ID: "v3-tab", Scope: "project", WorkspaceRoot: root, SessionID: first.Ref().SessionID, Ready: true, Ctrl: ctrl, sink: &tabEventSink{tabID: "v3-tab", app: app}, disabledMCP: map[string]ServerView{}}
	app.tabs[tab.ID] = tab
	app.tabOrder = []string{tab.ID}
	app.activeTabID = tab.ID
	app.newSessionRuntimeLocked(tab, sessionRuntimeKey(tab.currentSessionIdentity()))
	t.Cleanup(func() {
		if live := app.controllerForTab(tab); live != nil {
			live.Close()
		}
	})

	rows := app.ListSessions()
	if len(rows) != 2 || rows[0].SessionID == "" || rows[1].SessionID == "" {
		t.Fatalf("v3 catalog rows = %+v", rows)
	}
	if _, err := app.ResumeSessionForTab(tab.ID, sessionRoute(second.Ref().SessionID)); err != nil {
		t.Fatal(err)
	}
	if tab.SessionID != second.Ref().SessionID || tab.SessionPath != "" {
		t.Fatalf("resumed identity = id %q path %q", tab.SessionID, tab.SessionPath)
	}
	if tab.Ctrl == ctrl || tab.Ctrl.ModelRef() != targetModel {
		t.Fatalf("target model runtime = ctrl changed %v model %q, want true/%q", tab.Ctrl != ctrl, tab.Ctrl.ModelRef(), targetModel)
	}
	if got := tab.Ctrl.History(); len(got) != 1 || got[0].Content != "second conversation" {
		t.Fatalf("resumed history = %+v", got)
	}
	if err := app.RenameSession(sessionRoute(second.Ref().SessionID), "renamed v3"); err != nil {
		t.Fatal(err)
	}
	if snap, err := service.Query().Snapshot(t.Context(), second.Ref()); err != nil || snap.Projection.Title != "renamed v3" {
		t.Fatalf("renamed snapshot = %+v, %v", snap, err)
	}
	if err := app.DeleteSession(sessionRoute(second.Ref().SessionID)); err != nil {
		t.Fatal(err)
	}
	if tab.SessionID == second.Ref().SessionID || tab.SessionPath != "" {
		t.Fatalf("delete did not rotate to a fresh identity: %+v", tab)
	}
}

func TestDesktopV3ResumeModelBuildFailureKeepsSourceRuntime(t *testing.T) {
	isolateDesktopUserDirs(t)
	model, _ := configureSwitchableDefaultModels(t)
	app := NewApp()
	app.ctx = context.Background()
	root := t.TempDir()
	dir := desktopSessionDir(root)
	service := app.desktopSessionService(dir)

	source, err := service.Create(t.Context(), session.CreateOptions{SessionID: "resume-source"})
	if err != nil {
		t.Fatal(err)
	}
	appendSessionTestModel(t, source, "source-model", model)
	appendSessionTestMessage(t, source, "source-message", provider.Message{ID: "source-user", Role: provider.RoleUser, Content: "source remains"})
	target, err := service.Create(t.Context(), session.CreateOptions{SessionID: "resume-broken-target"})
	if err != nil {
		t.Fatal(err)
	}
	appendSessionTestModel(t, target, "target-model", "missing/model")
	appendSessionTestMessage(t, target, "target-message", provider.Message{ID: "target-user", Role: provider.RoleUser, Content: "must not publish"})

	ctrl, err := app.buildTabControllerBoot(app.ctx, boot.Options{Model: model, WorkspaceRoot: root, SessionDir: dir, Sink: event.Discard})
	if err != nil {
		t.Fatal(err)
	}
	identity := ctrl.(control.IdentityLifecycle)
	if _, err := identity.OpenSession(t.Context(), source.Ref()); err != nil {
		t.Fatal(err)
	}
	tab := &WorkspaceTab{ID: "source-tab", Scope: "project", WorkspaceRoot: root, SessionID: source.Ref().SessionID, Ready: true, Ctrl: ctrl, model: model, sink: &tabEventSink{tabID: "source-tab", app: app}, disabledMCP: map[string]ServerView{}}
	app.tabs[tab.ID] = tab
	app.tabOrder = []string{tab.ID}
	app.activeTabID = tab.ID
	app.newSessionRuntimeLocked(tab, sessionRuntimeKey(tab.currentSessionIdentity()))
	t.Cleanup(func() {
		if live := app.controllerForTab(tab); live != nil {
			live.Close()
		}
	})

	if _, err := app.ResumeSessionForTab(tab.ID, sessionRoute(target.Ref().SessionID)); err == nil {
		t.Fatal("resume with an unavailable target model unexpectedly succeeded")
	}
	if tab.Ctrl != ctrl || tab.SessionID != source.Ref().SessionID || tab.SessionPath != "" {
		t.Fatalf("failed resume changed source binding: ctrl=%v session=%q path=%q", tab.Ctrl == ctrl, tab.SessionID, tab.SessionPath)
	}
	if got := tab.Ctrl.History(); len(got) != 1 || got[0].Content != "source remains" {
		t.Fatalf("failed resume changed source history: %+v", got)
	}
}
