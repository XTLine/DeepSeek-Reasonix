package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemotePrefsFailedSaveDoesNotPublishCache(t *testing.T) {
	root := t.TempDir()
	blocked := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blocked, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REASONIX_STATE_HOME", blocked)

	if err := setRemoteSessionTitleOverride("box", "~/app", "session", "unsaved"); err == nil {
		t.Fatal("setRemoteSessionTitleOverride unexpectedly succeeded")
	}
	if got := remoteSessionTitleOverride("box", "~/app", "session"); got != "" {
		t.Fatalf("failed save published cached title %q", got)
	}
}
