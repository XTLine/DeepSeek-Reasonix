package cli

import (
	"errors"
	"strings"
	"testing"

	"reasonix/internal/agent"
)

func TestResumeConflictPinsOnlyOwnershipFailures(t *testing.T) {
	m := chatTUI{pendingTakeoverPath: "previous", pendingCommit: &[]string{}}
	m.recordResumeConflict("selected", agent.ErrSessionLeaseHeld)
	if m.pendingTakeoverPath != "selected" {
		t.Fatal("refused target was not retained")
	}
	err := errors.New("invalid session model")
	m.recordResumeConflict("broken", err)
	if m.pendingTakeoverPath != "" {
		t.Fatal("non-lease failure retained a stale takeover target")
	}
	if got := sessionLeaseHeldNotice(err); got != err.Error() || strings.Contains(got, "close") {
		t.Fatalf("misleading non-lease error: %s", got)
	}
}
