package planmode

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecideAllowsReadOnlyResearchAndBlocksKnownWriters(t *testing.T) {
	p := Policy{}

	allowed := p.Decide(Call{Name: "read_file", ReadOnly: true})
	if allowed.Blocked {
		t.Fatalf("read-only research tool blocked: %s", allowed.Message)
	}

	blocked := p.Decide(Call{Name: "write_file", ReadOnly: false})
	if !blocked.Blocked {
		t.Fatal("write_file should be blocked in plan mode")
	}
	if !strings.Contains(blocked.Message, "not available in plan mode") {
		t.Fatalf("blocked message = %q, want plan-mode availability explanation", blocked.Message)
	}
}

func TestDecideDoesNotLetOverridesReopenKnownBlockedTools(t *testing.T) {
	p := Policy{AllowedTools: []string{"write_file"}}

	decision := p.Decide(Call{Name: "write_file", ReadOnly: false})
	if !decision.Blocked {
		t.Fatal("plan_mode_allowed_tools must not allow known blocked writer tools")
	}
	if got := p.IgnoredAllowedTools(); len(got) != 1 || got[0] != "write_file" {
		t.Fatalf("IgnoredAllowedTools() = %v, want [write_file]", got)
	}
}

func TestDecideBlocksSubagentStyleToolsWithCategoryMessage(t *testing.T) {
	for _, name := range []string{"explore", "research", "review", "security_review", "security-review"} {
		t.Run(name, func(t *testing.T) {
			decision := (Policy{}).Decide(Call{Name: name, ReadOnly: false})
			if !decision.Blocked {
				t.Fatalf("%s should be blocked in plan mode", name)
			}
			if !strings.Contains(decision.Message, "not available in plan mode") {
				t.Fatalf("blocked message = %q, want category message", decision.Message)
			}
		})
	}
}

func TestDecideAllowsReadOnlyTaskDelegation(t *testing.T) {
	decision := (Policy{}).Decide(Call{Name: "read_only_task", ReadOnly: true})
	if decision.Blocked {
		t.Fatalf("read_only_task should be allowed in plan mode: %s", decision.Message)
	}
}

func TestDecideStillValidatesBashArgumentsWhenOverridden(t *testing.T) {
	p := Policy{AllowedTools: []string{"bash"}}
	args, err := json.Marshal(map[string]any{"command": "rm -rf /"})
	if err != nil {
		t.Fatal(err)
	}

	decision := p.Decide(Call{Name: "bash", ReadOnly: false, Args: args})
	if !decision.Blocked {
		t.Fatal("bash override must not bypass plan-mode bash safety checks")
	}
}

func TestDecideBlocksBashProcessControlArguments(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{
			name: "background execution",
			args: map[string]any{"command": "git status", "run_in_background": true},
			want: "background execution",
		},
		{
			name: "process preservation",
			args: map[string]any{"command": "git status", "preserve_background_processes": true},
			want: "process preservation",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args, err := json.Marshal(tt.args)
			if err != nil {
				t.Fatal(err)
			}

			decision := (Policy{}).Decide(Call{Name: "bash", ReadOnly: false, Args: args})
			if !decision.Blocked {
				t.Fatalf("bash args %v should be blocked in plan mode", tt.args)
			}
			if !strings.Contains(decision.Message, tt.want) {
				t.Fatalf("blocked message = %q, want to mention %q", decision.Message, tt.want)
			}
		})
	}
}
