package boot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"reasonix/internal/contract/event"
	"reasonix/internal/runtime/agent"
	"reasonix/internal/runtime/agent/testutil"
	"reasonix/internal/safety/sandbox"
	"reasonix/internal/session/control"
)

func TestEffectUnclassifiedCommandDoesNotVerifySourceChange(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	shell := sandbox.ResolveShell("bash", "", io.Discard)
	if shell.Kind != sandbox.ShellBash || shell.Path == "" {
		t.Skip("bash is unavailable")
	}
	if _, err := exec.LookPath(shell.Path); err != nil {
		t.Skip("resolved bash is not executable")
	}
	for _, tc := range []struct {
		name     string
		command  string
		declared string
		cited    bool
		failed   bool
		ready    bool
		before   bool
		rewrite  bool
	}{
		{name: "no command"},
		{name: "unrelated cleanup", command: "rm -f unused.tmp"},
		{name: "unrelated output", command: "printf 'unrelated command ran'"},
		{name: "failed command", command: "sh -c 'exit 7'", failed: true},
		{name: "uncited custom check", command: "sh check.sh"},
		{name: "declared custom check", command: "sh check.sh", declared: "sh check.sh", ready: true},
		{name: "cited custom check", command: "sh check.sh", cited: true, ready: true},
		{name: "different declared check", command: "sh check.sh", declared: "sh required.sh"},
		{name: "failed cited custom check", command: "sh failing.sh", failed: true, cited: true},
		{name: "recognized check", command: "go test ./...", ready: true},
		{name: "stale cited custom check", command: "sh check.sh", before: true, cited: true},
		{name: "edit after cited check", command: "sh check.sh", cited: true, rewrite: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateConfigHome(t)
			home, dir := robustTempDir(t), robustTempDir(t)
			t.Setenv("REASONIX_HOME", home)
			t.Chdir(dir)
			writeUserConfig(t, userModel+"\n[codegraph]\nenabled = false\n")
			registerBootTokenProfileTestProvider()
			initial := "package fixture\nconst Value = 1\n"
			if tc.before {
				initial = "package fixture\nconst Value = 2\n"
			}
			writeFile(t, dir, "main.go", initial)
			writeFile(t, dir, "check.sh", "grep -q 'const Value = 2' main.go && printf 'custom check passed'\n")
			writeFile(t, dir, "failing.sh", "exit 7\n")
			writeFile(t, dir, "go.mod", "module fixture\n\ngo 1.26\n")
			writeFile(t, dir, "main_test.go", "package fixture\nimport \"testing\"\nfunc TestValue(t *testing.T) { if Value != 2 { t.Fatal(Value) } }\n")
			if tc.declared != "" {
				writeFile(t, dir, "AGENTS.md", "## Reasonix host checks\n\n- verify: "+tc.declared+"\n")
			}
			approveWorkspace(t, dir)
			changed := "package fixture\nconst Value = 2\n"
			turns := []testutil.Turn{call("source", "write_file", `{"path":"main.go","content":"package fixture\nconst Value = 2\n"}`)}
			if tc.command != "" {
				args, err := json.Marshal(map[string]string{"command": tc.command})
				if err != nil {
					t.Fatal(err)
				}
				command := call("command", "bash", string(args))
				if tc.before {
					changed = "package fixture\nconst Value = 3\n"
					turns = []testutil.Turn{command, call("source", "write_file", `{"path":"main.go","content":"package fixture\nconst Value = 3\n"}`)}
				} else {
					turns = append(turns, command)
				}
			}
			if tc.cited {
				args, err := json.Marshal(map[string]any{
					"step": "change Value", "result": "Value is 2",
					"evidence": []map[string]string{{"kind": "verification", "summary": "checked Value", "command": tc.command}},
				})
				if err != nil {
					t.Fatal(err)
				}
				turns = append(turns, call("citation", "complete_step", string(args)))
			}
			if tc.rewrite {
				changed = "package fixture\nconst Value = 3\n"
				turns = append(turns, call("rewrite", "write_file", `{"path":"main.go","content":"package fixture\nconst Value = 3\n"}`))
			}
			sink := &noticeRecorder{}
			prov := testutil.NewMock("unclassified-verification", append(turns, testutil.Turn{Text: "done"})...)
			setBootTokenProfileTestProvider(t, prov)
			ctrl, err := Build(context.Background(), Options{
				Home: home, WorkspaceRoot: dir, AgentPreset: AgentPresetBalanced,
				Sink: sink, HeadlessApprovalMode: control.ToolApprovalAuto, resolvedShell: &shell,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer ctrl.Close()
			ctrl.SetFreshSessionPath(filepath.Join(home, "sessions", "verification.jsonl"))
			err = ctrl.Run(context.Background(), "change Value to 2")
			body, readErr := os.ReadFile(filepath.Join(dir, "main.go"))
			if readErr != nil || string(body) != changed {
				t.Fatalf("source edit = %q, %v", body, readErr)
			}
			results := toolResults(prov.Requests())
			if tc.command != "" {
				result := results["command"]
				t.Logf("bash %q returned: %s", tc.command, result)
				sink.mu.Lock()
				var ran *event.Tool
				for _, e := range sink.events {
					if e.Kind == event.ToolResult && e.Tool.ID == "command" {
						ran = &e.Tool
					}
				}
				sink.mu.Unlock()
				if ran == nil || ran.StartedAt == 0 || ran.RefusalCode != "" || (ran.Err != "") != tc.failed {
					t.Fatalf("command execution = %+v, want executed with failed=%v", ran, tc.failed)
				}
			}
			if tc.ready {
				if err != nil {
					t.Fatalf("Run = %v, want verified completion", err)
				}
				return
			}
			var unready *agent.FinalReadinessError
			if !errors.As(err, &unready) || !slices.Contains(unready.Missing, "verification") {
				t.Fatalf("Run = %v, want verification debt after a source edit and %q", err, tc.command)
			}
			if !strings.Contains(unready.Reason, "main.go") {
				t.Errorf("verification reason does not identify the source change: %q", unready.Reason)
			}
		})
	}
}
