package serve

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"reasonix/internal/agent"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/eventwire"
)

func TestSessionTransitionPublishesRouteOnlyAfterControllerCommit(t *testing.T) {
	bc := NewBroadcaster()
	tag := newSessionTagSink(bc)
	dir := t.TempDir()
	initialPath := filepath.Join(dir, "old.jsonl")
	sess := agent.NewSession("sys")
	exec := agent.New(nil, nil, sess, agent.Options{}, tag)
	ctrl := control.New(control.Options{Runner: exec, Executor: exec, Sink: tag, SessionDir: dir, SessionPath: initialPath})
	oldPath := agent.CanonicalSessionPath(ctrl.SessionPath())
	tag.SetPath(oldPath)
	bc.SetCurrentSession(oldPath)

	srv := New(ctrl, bc, config.ServeConfig{})
	srv.RegisterSessionTag(ctrl, tag)
	handler := srv.sessionTransitionHandler(ctrl, nil)
	ctrl.SetOnSessionTransition(func(info control.SessionTransitionInfo) error {
		if err := handler(info); err != nil {
			return err
		}
		// This models output produced after transition preparation but before
		// ClearSession swaps its executor and session path.
		tag.Emit(event.Event{Kind: event.Notice, Text: "pre-commit hook output"})
		return nil
	})

	all, stop := bc.SubscribeAll()
	defer stop()
	if err := ctrl.ClearSession(); err != nil {
		t.Fatal(err)
	}
	newPath := agent.CanonicalSessionPath(ctrl.SessionPath())
	if newPath == oldPath {
		t.Fatal("clear did not rotate the session path")
	}
	tag.Emit(event.Event{Kind: event.Notice, Text: "post-commit output"})

	var before, after eventwire.Event
	if err := json.Unmarshal(<-all, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(<-all, &after); err != nil {
		t.Fatal(err)
	}
	if before.SessionPath != oldPath || !before.SessionCurrent {
		t.Fatalf("pre-commit frame route = %q current=%v, want old foreground %q", before.SessionPath, before.SessionCurrent, oldPath)
	}
	if after.SessionPath != newPath || !after.SessionCurrent {
		t.Fatalf("post-commit frame route = %q current=%v, want new foreground %q", after.SessionPath, after.SessionCurrent, newPath)
	}
}
