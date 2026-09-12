package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"
	"reasonix/internal/agent"
	"reasonix/internal/event"
)

func TestSessionPreviewKeepsSourceControllerDraftAndTargetUnchanged(t *testing.T) {
	source := newPeerTestTUI(t)
	m := newChatTUI(source.ctrl, "", make(chan event.Event, 10), 80)
	m.leases = source.leases
	m.input.SetValue("unsent source draft")
	path := filepath.Join(t.TempDir(), "view.jsonl")
	saveTestSession(t, path, "target history")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := agent.TryAcquireSessionLease(path)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Release()
	sourcePath := m.ctrl.SessionPath()
	m.openSessionPreview(path)
	if m.preview == nil {
		t.Fatal("preview did not open")
	}
	m.applySessionPreview(m.pendingTakeoverCmd().(cliPreviewLoadedMsg))
	if m.ctrl.SessionPath() != sourcePath || m.leases.HeldPath() == agent.CanonicalSessionPath(path) {
		t.Fatal("viewing rebound the target for writing")
	}
	if !m.hideComposer() {
		t.Fatal("preview left composer enabled")
	}
	ran := false
	m.startControllerTurn("hidden", "hidden", func() { ran = true })
	m.runSlashCommand("/clear")
	if ran || m.ctrl.SessionPath() != sourcePath {
		t.Fatal("preview admitted a mutation")
	}
	next, _ := m.handlePreviewKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = next.(chatTUI)
	if m.preview != nil || m.input.Value() != "unsent source draft" {
		t.Fatal("preview discarded draft or failed to close")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("viewing modified target history")
	}
}

func TestSessionPreviewRejectsStaleLoad(t *testing.T) {
	source := newPeerTestTUI(t)
	m := newChatTUI(source.ctrl, "", make(chan event.Event, 10), 80)
	m.leases = source.leases
	first := filepath.Join(t.TempDir(), "first.jsonl")
	second := filepath.Join(t.TempDir(), "second.jsonl")
	saveTestSession(t, first, "first")
	saveTestSession(t, second, "second")
	m.openSessionPreview(first)
	old := m.pendingTakeoverCmd().(cliPreviewLoadedMsg)
	m.openSessionPreview(second)
	m.applySessionPreview(old)
	if m.preview.path != second || !m.preview.loading {
		t.Fatal("old load replaced a newer preview")
	}
	m.closeSessionPreview()
}
