package control

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"reasonix/internal/base/testenv"
)

func TestWorkbenchBackslashTokenRoundTrip(t *testing.T) {
	for _, path := range []string{`literal\name.txt`, `before\ space.txt`, `repeat\\name.txt`, `repeat\\ space.txt`, "before\\\ttab.txt", `trailing\`, `C:\My Project\file.txt`, `C:\Folder\ file.txt`, `\\server\share\team notes.txt`} {
		t.Run(path, func(t *testing.T) {
			escaped := EscapeRefPath(path)
			if got := parseRefTokens("@" + escaped); !reflect.DeepEqual(got, []string{path}) {
				t.Fatalf("parsed %q, want %q", got, path)
			}
			if got := UnescapeRefPath(escaped); got != path {
				t.Fatalf("unescaped %q, want %q", got, path)
			}
			doubled := EscapeRefPath(strings.ReplaceAll(path, `\`, `\\`))
			if reflect.DeepEqual(parseRefTokens("@"+doubled), []string{path}) {
				t.Fatal("doubling backslashes unexpectedly preserved the filename")
			}
		})
	}
}

func TestWorkbenchBackslashScopedResolution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("literal backslashes are POSIX filename characters")
	}
	root := testenv.TempDir(t)
	c := &Controller{controllerDeps: controllerDeps{workspaceRoot: root}}
	for _, path := range []string{`literal\name.txt`, `before\ space.txt`, `repeat\\name.txt`, `repeat\\ space.txt`, "before\\\ttab.txt", `trailing\`, `folder\/`} {
		t.Run(path, func(t *testing.T) {
			file, want := filepath.Join(root, path), "selected file"
			if strings.HasSuffix(path, "/") {
				if err := os.Mkdir(file, 0o755); err != nil {
					t.Fatal(err)
				}
				file, want = filepath.Join(file, "child.txt"), "child.txt"
			}
			if err := os.WriteFile(file, []byte("selected file"), 0o644); err != nil {
				t.Fatal(err)
			}
			token := "@" + EscapeRefPath(path)
			refs := c.detectRefsMode(token, true)
			if len(refs) != 1 || refs[0].kind != refFile || refs[0].path != strings.TrimSuffix(path, "/") {
				t.Fatalf("references = %+v, want %q", refs, path)
			}
			block, errs := c.ResolveScopedRefs(t.Context(), token)
			if len(errs) != 0 || !strings.Contains(block, want) {
				t.Fatalf("resolved %q with errors %v", block, errs)
			}
		})
	}
}

func TestWorkbenchTrailingBackslashConsumesProse(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("literal backslashes are POSIX filename characters")
	}
	root := testenv.TempDir(t)
	for _, name := range []string{`trailing\`, "trailing inspect"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c := &Controller{controllerDeps: controllerDeps{workspaceRoot: root}}
	line := "@" + EscapeRefPath(`trailing\`) + " inspect"
	refs := c.detectRefsMode(line, true)
	if len(refs) != 1 || refs[0].path != "trailing inspect" {
		t.Fatalf("reference boundary ambiguity did not reproduce: %+v", refs)
	}
	block, errs := c.ResolveScopedRefs(t.Context(), line)
	if len(errs) != 0 || !strings.Contains(block, "trailing inspect") || strings.Contains(block, `trailing\`) {
		t.Fatalf("reference followed by prose resolved %q, errors %v", block, errs)
	}
}
