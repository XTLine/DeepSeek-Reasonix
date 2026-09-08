package boot

import (
	"context"
	"path/filepath"
	"testing"

	"reasonix/internal/control"
)

// TestRebindRestoresSessionPostureFromSidecar proves a controller binding an
// existing session path (process restart, serve resume, remote takeover)
// serves that session's persisted composer posture instead of boot defaults:
// the sidecar a previous session wrote is authoritative when no in-memory
// posture survives.
func TestRebindRestoresSessionPostureFromSidecar(t *testing.T) {
	isolateConfigHome(t)
	first, err := BuildRuntime(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(first.Controller.Close)

	path := filepath.Join(t.TempDir(), "session.jsonl")
	first.Controller.SetFreshSessionPath(path)
	first.Controller.SetToolApprovalMode(control.ToolApprovalYolo)
	if got := first.Controller.ToolApprovalMode(); got != control.ToolApprovalYolo {
		t.Fatalf("session controller mode = %q, want yolo", got)
	}

	// A second controller stands in for a fresh process binding the same
	// session path; it boots with the default ask posture.
	rebound, err := BuildRuntime(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rebound.Controller.Close)
	if got := rebound.Controller.ToolApprovalMode(); got != control.ToolApprovalAsk {
		t.Fatalf("fresh controller mode = %q, want ask", got)
	}
	rebound.Controller.SetSessionPath(path)
	if got := rebound.Controller.ToolApprovalMode(); got != control.ToolApprovalYolo {
		t.Fatalf("rebound controller mode = %q, want yolo from the session sidecar", got)
	}
}
