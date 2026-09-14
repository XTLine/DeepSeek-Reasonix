package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"reasonix/internal/projectiondb"
	"reasonix/internal/provider"
)

func TestExternalHistoryColdOpenDefersBodiesBeforeModelReset(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions-v4")
	service, err := NewService("local", NewFilesystemPersistence(root))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := service.Create(t.Context(), CreateOptions{SessionID: "bounded-open"})
	if err != nil {
		t.Fatal(err)
	}
	oldPayload, err := json.Marshal(map[string]any{"message": provider.Message{ID: "old", Role: provider.RoleUser, Content: strings.Repeat("old", 40<<10)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Session().AppendBatch(t.Context(), "old", []Event{{Kind: "message/complete", Payload: oldPayload}}); err != nil {
		t.Fatal(err)
	}
	current := provider.Message{ID: "current", Role: provider.RoleUser, Content: "current workset"}
	currentPayload, err := json.Marshal(map[string]any{"messages": []provider.Message{current}, "reason": "bounded cold open"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Session().AppendBatch(t.Context(), "reset", []Event{{Kind: "model/context-replace", Payload: currentPayload}}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Session().Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	ref := runtime.Ref()
	if err := service.Close(t.Context(), ref); err != nil {
		t.Fatal(err)
	}

	log, err := os.Open(filepath.Join(root, ref.SessionID, "events.frames"))
	if err != nil {
		t.Fatal(err)
	}
	var historicalDigest string
	err = scanV4CommitFileRefs(t.Context(), log, 0, 1, contentStoreForSessionDir(filepath.Join(root, ref.SessionID)), nil, func(_ int64, commit Commit) bool {
		for _, event := range commit.Events {
			if event.Kind == "message/complete" && event.PayloadRef != nil {
				historicalDigest = event.PayloadRef.Digest
			}
		}
		return true
	})
	_ = log.Close()
	if err != nil || historicalDigest == "" {
		t.Fatalf("historical content reference = %q, %v", historicalDigest, err)
	}
	object := filepath.Join(root, ".content-v1", "objects", historicalDigest[:2], historicalDigest[2:4], historicalDigest)
	if err := os.Remove(object); err != nil {
		t.Fatal(err)
	}

	reopenedService, err := NewService("local", NewFilesystemPersistence(root))
	if err != nil {
		t.Fatal(err)
	}
	binding, err := reopenedService.Open(t.Context(), ref)
	if err != nil {
		t.Fatalf("cold open resolved retired history body: %v", err)
	}
	defer binding.Release(context.Background())
	model := binding.Runtime().Session().DeriveMessages()
	if len(model) != 1 || model[0].ID != current.ID || model[0].Content != current.Content {
		t.Fatalf("cold model projection = %+v", model)
	}
	if _, err := reopenedService.Query().HistoryPage(t.Context(), ref, "", 100); err == nil {
		t.Fatal("history query accepted a missing referenced body")
	}
}

func TestHistoryPageKeepsSnapshotAndAuthorizesReferencedContent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions-v4")
	service, err := NewService("local", NewFilesystemPersistence(root))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := service.Create(t.Context(), CreateOptions{SessionID: "paged"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(context.Background(), runtime.Ref()); err != nil {
			t.Error(err)
		}
	})
	appendMessage := func(id, content string) {
		payload, err := json.Marshal(map[string]any{"message": provider.Message{ID: id, Role: provider.RoleUser, Content: content}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runtime.Session().Append(t.Context(), Batch{OperationID: "message-" + id, Events: []Event{{Kind: "message/complete", Payload: payload}}}); err != nil {
			t.Fatal(err)
		}
		if _, err := runtime.Session().Flush(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	appendMessage("one", "first")
	appendMessage("two", strings.Repeat("large", 20000))
	appendMessage("three", "third")
	runtime.Session().mu.Lock()
	residentMessages := len(runtime.Session().projection.Messages)
	residentModel := len(runtime.Session().projection.ModelMessages)
	runtime.Session().mu.Unlock()
	if residentMessages != 0 || residentModel != 3 {
		t.Fatalf("service runtime retained durable UI bodies: messages=%d model=%d", residentMessages, residentModel)
	}
	ref := runtime.Ref()
	first, err := service.Query().HistoryPage(t.Context(), ref, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Messages) != 1 || first.Messages[0].MessageID != "three" || !first.HasMore || first.NextCursor == "" {
		t.Fatalf("first page = %+v", first)
	}
	appendMessage("four", "must not enter the fixed snapshot")
	second, err := service.Query().HistoryPage(t.Context(), ref, first.NextCursor, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Messages) != 2 || second.Messages[0].MessageID != "one" || second.Messages[1].MessageID != "two" {
		t.Fatalf("fixed snapshot second page = %+v", second)
	}
	large := second.Messages[1]
	if large.ContentRef == nil || len(large.Inline) != 0 {
		t.Fatalf("large message was not referenced: %+v", large)
	}
	chunk, err := service.Query().ReadContent(t.Context(), ref, *large.ContentRef, 0, min(64, large.ContentRef.Bytes))
	if err != nil {
		t.Fatal(err)
	}
	var decoded provider.Message
	full, err := service.Query().ReadContent(t.Context(), ref, *large.ContentRef, 0, large.ContentRef.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunk) == 0 || json.Unmarshal(full, &decoded) != nil || decoded.ID != "two" {
		t.Fatalf("resolved content prefix=%q id=%q", chunk, decoded.ID)
	}
	foreign := *large.ContentRef
	foreign.Digest = strings.Repeat("0", len(foreign.Digest))
	if _, err := service.Query().ReadContent(t.Context(), ref, foreign, 0, 1); err == nil {
		t.Fatal("content hash without a session reference was authorized")
	}
}

func TestSearchHistoryUsesStableSnapshotAndOpaqueQueryCursor(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions-v4")
	service, err := NewService("local", NewFilesystemPersistence(root))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := service.Create(t.Context(), CreateOptions{SessionID: "search"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(context.Background(), runtime.Ref()); err != nil {
			t.Error(err)
		}
	})
	appendMessage := func(id, content string) {
		payload, _ := json.Marshal(map[string]any{"message": provider.Message{ID: id, Role: provider.RoleUser, Content: content}})
		if _, err := runtime.Session().Append(t.Context(), Batch{OperationID: id, Events: []Event{{Kind: "message/complete", Payload: payload}}}); err != nil {
			t.Fatal(err)
		}
		if _, err := runtime.Session().Flush(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	appendMessage("one", "first needle")
	appendMessage("two", "second needle")
	appendMessage("three", "unrelated")
	first, err := service.Query().SearchHistory(t.Context(), runtime.Ref(), "needle", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Hits) != 1 || first.Hits[0].MessageID != "two" || !first.HasMore || first.NextCursor == "" {
		t.Fatalf("first search = %+v", first)
	}
	appendMessage("four", "new needle outside snapshot")
	second, err := service.Query().SearchHistory(t.Context(), runtime.Ref(), "needle", first.NextCursor, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Hits) != 1 || second.Hits[0].MessageID != "one" {
		t.Fatalf("second search = %+v", second)
	}
	if _, err := service.Query().SearchHistory(t.Context(), runtime.Ref(), "different", first.NextCursor, 10); err == nil {
		t.Fatal("search cursor was accepted for another query")
	}
}

func TestSearchHistoryCoversInlineFieldsAndReferencedBodies(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions-v4")
	service, err := NewService("local", NewFilesystemPersistence(root))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := service.Create(t.Context(), CreateOptions{SessionID: "search-storage"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(context.Background(), runtime.Ref()); err != nil {
			t.Error(err)
		}
	})
	messages := []provider.Message{
		{ID: "inline", Role: provider.RoleAssistant, RawContent: "raw-field-needle", ReasoningContent: "reasoning-field-needle"},
		{ID: "referenced", Role: provider.RoleUser, Content: strings.Repeat("large-body-", 7000) + "referenced-field-needle"},
	}
	for _, message := range messages {
		payload, marshalErr := json.Marshal(map[string]any{"message": message})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if _, err := runtime.Session().AppendBatch(t.Context(), "append-"+message.ID, []Event{{Kind: "message/complete", Payload: payload}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runtime.Session().Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	for query, want := range map[string]string{
		"raw-field-needle":        "inline",
		"reasoning-field-needle":  "inline",
		"referenced-field-needle": "referenced",
	} {
		page, err := service.Query().SearchHistory(t.Context(), runtime.Ref(), query, "", 10)
		if err != nil {
			t.Fatalf("search %q: %v", query, err)
		}
		if len(page.Hits) != 1 || page.Hits[0].MessageID != want {
			t.Fatalf("search %q = %+v, want %q", query, page.Hits, want)
		}
	}
}

func TestHistoryIndexHasSnapshotPositionIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.sqlite")
	handle, err := projectiondb.Open(t.Context(), projectiondb.OpenOptions{Path: path, Migrations: historyMigrations, RequireDisk: true, MaxOpenConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer handle.DB.Close()
	rows, err := handle.DB.QueryContext(context.Background(), `PRAGMA index_list(messages)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var sequence, unique, partial int
		var name, origin string
		if err := rows.Scan(&sequence, &name, &unique, &origin, &partial); err != nil {
			t.Fatal(err)
		}
		found = found || name == "messages_snapshot_position"
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("snapshot-position query index is missing")
	}
}
