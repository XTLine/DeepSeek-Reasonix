//go:build windows

package fileops

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsSnapshotUsesVolumeFileIndexAndChangeTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "version.txt")
	if err := os.WriteFile(path, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	target, before := DiskSnapshot(path, info)
	if !strings.Contains(target.Key, "volume=") || !strings.Contains(target.Key, "fileindex=") {
		t.Fatalf("target lacks native identity: %q", target.Key)
	}
	if err := os.WriteFile(path, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	_, after := DiskSnapshot(path, afterInfo)
	if before == after {
		t.Fatal("same-size replacement did not advance the native version")
	}
}

func TestWindowsHandleSnapshotUsesTheSourceHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "handle-version.txt")
	if err := os.WriteFile(path, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	target, version := DiskHandleSnapshot(path, file, info)
	if !strings.Contains(target.Key, "volume=") || !strings.Contains(target.Key, "fileindex=") {
		t.Fatalf("handle target lacks native identity: %q", target.Key)
	}
	if version == "" {
		t.Fatal("handle snapshot returned an empty version")
	}
}

func TestWindowsSnapshotDetectsSecurityDescriptorChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "acl-version.txt")
	if err := os.WriteFile(path, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	_, before := DiskSnapshot(path, info)

	// os.Chmod does not represent NTFS ACL changes. Disabling inheritance
	// changes the real security descriptor while preserving file contents.
	cmd := exec.Command("icacls.exe", path, "/inheritance:d")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("change ACL inheritance: %v: %s", err, output)
	}
	t.Cleanup(func() { _ = exec.Command("icacls.exe", path, "/inheritance:e").Run() })
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	_, after := DiskSnapshot(path, afterInfo)
	if before == after {
		t.Fatal("security descriptor change did not advance the native version")
	}
}
