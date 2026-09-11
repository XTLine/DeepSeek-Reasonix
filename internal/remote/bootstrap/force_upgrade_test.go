package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"reasonix/internal/remote"
)

// TestForceUpgradePrefersUploadAndSkipsLocate: a same-platform force upgrade
// must skip the locate fast-path and the npm-first ladder, installing via the
// local binary upload instead.
func TestForceUpgradePrefersUploadAndSkipsLocate(t *testing.T) {
	skipOnWindows(t)
	root := t.TempDir()
	paths := pathsFor(root, root)
	localBin := filepath.Join(t.TempDir(), "reasonix")
	if err := os.WriteFile(localBin, []byte("fake-cli"), 0o755); err != nil {
		t.Fatal(err)
	}
	conn := newFakeConn(t, root, func(cmd string) (remote.ExecResult, error) {
		switch {
		case strings.Contains(cmd, "uname"):
			return ok("Linux x86_64\n")
		case strings.Contains(cmd, "command -v reasonix"):
			t.Errorf("force upgrade must skip the locate fast-path; ran: %s", cmd)
			return ok("")
		case strings.Contains(cmd, "npm i -g"):
			t.Errorf("npm must not run before exact-release sources; ran: %s", cmd)
			return ok("")
		case strings.Contains(cmd, "--version"):
			// LocateUploadedCommand probing the freshly uploaded binary.
			return ok(uploadedBinPath(root) + "\nreasonix v1.9.5\nportfile:yes\nsessionevents:yes\ndetachedheal:yes\ncaps:yes\n")
		case strings.Contains(cmd, "nohup"):
			_ = os.WriteFile(paths.PortFile, []byte("127.0.0.1:44321\n"), 0o600)
			return ok("54321\n")
		case strings.Contains(cmd, "kill -0"), strings.Contains(cmd, "ps -p"):
			return ok("1\n")
		default:
			return ok("")
		}
	})
	res, err := EnsureServe(context.Background(), conn, Options{
		Workspace:      "~",
		ForceUpgrade:   true,
		LocalBinary:    localBin,
		LocalGOOS:      runtime.GOOS,
		LocalGOARCH:    runtime.GOARCH,
		ProductVersion: "1.9.5",
		Clock:          time.Now,
	})
	if err != nil {
		t.Fatalf("EnsureServe: %v", err)
	}
	if res.Reused {
		t.Fatal("force upgrade must not report reuse")
	}
	if res.State.Version != "1.9.5" {
		t.Fatalf("state version = %q, want 1.9.5", res.State.Version)
	}
}

// TestForceUpgradeCrossPlatformFetchesRelease: with no same-platform binary,
// the official release download at ProductVersion runs before npm.
func TestForceUpgradeCrossPlatformFetchesRelease(t *testing.T) {
	skipOnWindows(t)
	root := t.TempDir()
	paths := pathsFor(root, root)
	var fetched string
	conn := newFakeConn(t, root, func(cmd string) (remote.ExecResult, error) {
		switch {
		case strings.Contains(cmd, "uname"):
			return ok("Linux x86_64\n")
		case strings.Contains(cmd, "npm i -g"):
			t.Errorf("npm must not run before the release download; ran: %s", cmd)
			return ok("")
		case strings.Contains(cmd, "--version"):
			return ok(uploadedBinPath(root) + "\nreasonix v1.9.5\nportfile:yes\nsessionevents:yes\ndetachedheal:yes\ncaps:yes\n")
		case strings.Contains(cmd, "nohup"):
			_ = os.WriteFile(paths.PortFile, []byte("127.0.0.1:44321\n"), 0o600)
			return ok("54321\n")
		case strings.Contains(cmd, "kill -0"), strings.Contains(cmd, "ps -p"):
			return ok("1\n")
		default:
			return ok("")
		}
	})
	_, err := EnsureServe(context.Background(), conn, Options{
		Workspace:      "~",
		ForceUpgrade:   true,
		LocalGOOS:      "windows", // cross-platform: the upload path is impossible
		ProductVersion: "1.9.5",
		FetchBinary: func(_ context.Context, version, goos, goarch string) ([]byte, error) {
			fetched = version + "/" + goos + "/" + goarch
			return []byte("fake-release-cli"), nil
		},
		Clock: time.Now,
	})
	if err != nil {
		t.Fatalf("EnsureServe: %v", err)
	}
	if fetched != "1.9.5/linux/amd64" {
		t.Fatalf("FetchBinary args = %q, want 1.9.5/linux/amd64", fetched)
	}
}

// TestForceUpgradeRejectsShortVersion: a ladder result below ProductVersion
// is an error, not a silent half-upgrade.
func TestForceUpgradeRejectsShortVersion(t *testing.T) {
	skipOnWindows(t)
	root := t.TempDir()
	conn := newFakeConn(t, root, func(cmd string) (remote.ExecResult, error) {
		switch {
		case strings.Contains(cmd, "uname"):
			return ok("Linux x86_64\n")
		case strings.Contains(cmd, "--version"):
			return ok(uploadedBinPath(root) + "\nreasonix v1.9.0\nportfile:yes\nsessionevents:yes\ndetachedheal:yes\ncaps:yes\n")
		default:
			return ok("")
		}
	})
	_, err := EnsureServe(context.Background(), conn, Options{
		Workspace:      "~",
		ForceUpgrade:   true,
		ProductVersion: "1.9.5",
		FetchBinary: func(_ context.Context, _ string, _, _ string) ([]byte, error) {
			return []byte("fake-release-cli"), nil
		},
		Clock: time.Now,
	})
	if err == nil || !strings.Contains(err.Error(), `desktop requires "1.9.5"`) {
		t.Fatalf("err = %v, want version-short error", err)
	}
}

// TestUpgradeTargetMet is pure and runs on every platform.
func TestUpgradeTargetMet(t *testing.T) {
	if !upgradeTargetMet("1.9.5", "1.9.5") || !upgradeTargetMet("2.0.0", "1.9.5") {
		t.Fatal("met targets must pass")
	}
	if upgradeTargetMet("1.9.0", "1.9.5") || upgradeTargetMet("", "1.9.5") {
		t.Fatal("short or empty versions must fail a real target")
	}
	if !upgradeTargetMet("", "dev") || !upgradeTargetMet("1.2.3", "dev") {
		t.Fatal("a dev target must not gate")
	}
}
