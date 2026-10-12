package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"reasonix/internal/base/testenv"
	"reasonix/internal/ext/plugin"
)

func TestWorkbenchReferenceTokenSpelling(t *testing.T) {
	for _, path := range []string{`name"`, `quoted-dir"/`, "space\tquote\"", `C:\My Project\file.txt`, `\\server\share\file.txt`} {
		if got := parseRefTokens("@" + EscapeRefPath(path)); !reflect.DeepEqual(got, []string{path}) {
			t.Errorf("reference for %q parsed as %q", path, got)
		}
	}
}

func TestWorkbenchReferenceRoutingPreservesQuotesAndShowsNamespaceCollisions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("colons and quotes are POSIX filename characters")
	}
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		result := map[string]any{}
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-03-26", "serverInfo": map[string]string{"name": "fixture", "version": "1"}}
		case "tools/list":
			result["tools"] = []any{}
		case "resources/read":
			reads.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	defer server.Close()
	host := plugin.NewHost()
	defer host.Close()
	if _, err := host.Add(t.Context(), plugin.Spec{Name: "github", Type: "http", URL: server.URL}); err != nil {
		t.Fatal(err)
	}
	root := testenv.TempDir(t)
	c := &Controller{controllerDeps: controllerDeps{workspaceRoot: root, mcp: newMcpManager(host, nil, t.Context(), 0)}}
	for _, tc := range []struct {
		path, token string
		folder      bool
	}{
		{"github:notes.md", "@./github:notes.md", false},
		{"src/github:notes.md", "@./src/github:notes.md", false},
		{"github:team notes.md", `@./github:team\ notes.md`, false},
		{"github:dir", "@./github:dir/", true},
		{"github:trail ", `@./github:trail\ /`, true},
		{`name"`, `@name"`, false},
		{`quoted-dir"`, `@quoted-dir"/`, true},
	} {
		t.Run(tc.path, func(t *testing.T) {
			path, want := filepath.Join(root, tc.path), "selected file"
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.folder {
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
				path, want = filepath.Join(path, "child.txt"), "child.txt"
			}
			if err := os.WriteFile(path, []byte("selected file"), 0o644); err != nil {
				t.Fatal(err)
			}
			refs := c.detectRefsMode(tc.token, true)
			if len(refs) != 1 || refs[0].kind != refFile || refs[0].path != tc.path {
				t.Fatalf("references = %+v, want workspace path %q", refs, tc.path)
			}
			block, errs := c.ResolveScopedRefs(t.Context(), tc.token)
			if len(errs) != 0 || !strings.Contains(block, want) {
				t.Fatalf("resolved %q with errors %v", block, errs)
			}
		})
	}
	if refs := c.detectRefsMode("@github:notes.md", true); len(refs) != 1 || refs[0].kind != refResource {
		t.Fatalf("unqualified collision did not reproduce: %+v", refs)
	}
	for _, name := range []string{"./github", "src/github", "./src/github"} {
		if _, err := host.Add(t.Context(), plugin.Spec{Name: name, Type: "http", URL: server.URL}); err != nil {
			t.Fatal(err)
		}
		if refs := c.detectRefsMode("@"+name+":notes.md", true); len(refs) != 1 || refs[0].kind != refResource {
			t.Fatalf("legacy server-name collision did not reproduce: %+v", refs)
		}
	}
	if reads.Load() != 0 {
		t.Fatalf("workspace references caused %d MCP reads", reads.Load())
	}
}
