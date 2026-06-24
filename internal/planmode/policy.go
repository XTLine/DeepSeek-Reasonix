package planmode

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Marker is the model-facing plan-mode instruction block. It rides in the user
// turn, not the system prompt or tool schema, so plan toggles preserve cache shape.
const Marker = "[Plan mode — planning only. You may research the codebase and web, ask clarifying questions with ask, and maintain planning state with todo_write. You must not write files, run unsafe shell commands, install capabilities, mutate memory, delegate to sub-agents or skills, control long-lived processes, or mark execution steps complete. Before planning, if a decision that is genuinely the user's — tech stack, an ambiguous requirement, scope, an irreversible choice — would materially shape the plan and you can't settle it from the codebase or a sensible default, use the ask tool to clarify it first; otherwise pick the obvious default and state the assumption in the plan instead of asking. Then present a LAYERED plan as your reply and stop. Structure the plan as a two-level markdown list so it becomes a layered task list: each PHASE is a top-level numbered list item (a coherent milestone, e.g. \"1. Add the config loader\"), and each phase's concrete, verifiable sub-steps are bullets indented beneath it (e.g. \"   - parse the TOML into Config\"). Use plain numbered list items for phases — do NOT write phases as markdown headings (##, ###) — so both levels parse. Keep phases few (about 2-6). The user will be asked to approve before any changes are made.]"

// Call is the plan-mode view of one tool invocation.
type Call struct {
	Name     string
	ReadOnly bool
	Args     json.RawMessage
}

// Decision reports whether plan mode refuses a call and why.
type Decision struct {
	Blocked bool
	Message string
}

// Policy is the single plan-mode stage policy.
type Policy struct {
	AllowedTools []string
}

var knownBlockedTools = map[string]bool{
	"write_file":     true,
	"edit_file":      true,
	"multi_edit":     true,
	"move_file":      true,
	"apply_patch":    true,
	"edit_notebook":  true,
	"range_delete":   true,
	"symbol_delete":  true,
	"complete_step":  true,
	"task":           true,
	"parallel_tasks": true,
	"run_skill":      true,
	"install_source": true,
	"install_skill":  true,
	"remember":       true,
	"forget":         true,
	"kill_shell":     true,
}

var alwaysAllowedTools = map[string]bool{
	"ask":        true,
	"todo_write": true,
}

var bashMetachars = []string{"&&", "||", ">>", "<<", "$(", "\x60", ";", "|", ">", "<", "&", "\n", "\r"}

var safeBashCommands = []string{
	"git status", "git diff", "git log", "git show",
	"git ls-files", "git grep", "git blame",
	"ls", "cat", "grep", "find", "head", "tail", "pwd",
	"echo", "wc", "which", "type", "uname", "hostname",
	"go version", "go list", "go doc", "go vet",
	"node -v", "npm list", "python --version",
}

var findWriteArgs = map[string]bool{
	"-delete":  true,
	"-exec":    true,
	"-execdir": true,
	"-ok":      true,
	"-okdir":   true,
	"-fprint":  true,
	"-fprintf": true,
	"-fls":     true,
}

var goWriteOrExecArgs = map[string]bool{
	"-fix":      true,
	"-mod":      true,
	"-modfile":  true,
	"-toolexec": true,
	"-vettool":  true,
}

// Decide applies the plan-mode stage gate before permission policy.
func (p Policy) Decide(call Call) Decision {
	name := strings.TrimSpace(call.Name)
	if name == "bash" {
		return decideBash(call.Args)
	}
	if knownBlockedTools[name] {
		return blockKnown(name)
	}
	if alwaysAllowedTools[name] {
		return Decision{}
	}
	if call.ReadOnly {
		return Decision{}
	}
	if p.allowed(name) {
		return Decision{}
	}
	return Decision{
		Blocked: true,
		Message: fmt.Sprintf("blocked: %q is a writer tool and plan mode is read-only. Keep exploring with read-only tools, then write your plan as your reply — the user will be asked to approve it before any changes are made.", name),
	}
}

// IgnoredAllowedTools names configured overrides that plan mode will not honor.
func (p Policy) IgnoredAllowedTools() []string {
	var out []string
	seen := map[string]bool{}
	for _, name := range p.AllowedTools {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		if name == "bash" || knownBlockedTools[name] {
			out = append(out, name)
			seen[name] = true
		}
	}
	sort.Strings(out)
	return out
}

func (p Policy) allowed(name string) bool {
	for _, allowed := range p.AllowedTools {
		if strings.TrimSpace(allowed) == name {
			return true
		}
	}
	return false
}

func blockKnown(name string) Decision {
	if name == "complete_step" {
		return Decision{
			Blocked: true,
			Message: "blocked: complete_step is only available after plan approval. While planning, keep task state with todo_write and present the plan for user approval.",
		}
	}
	return Decision{
		Blocked: true,
		Message: fmt.Sprintf("blocked: %q is not available in plan mode. Keep exploring with read-only tools — the user will be asked to approve the plan before any changes are made.", name),
	}
}

func decideBash(args json.RawMessage) Decision {
	var p struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(args, &p); err != nil || strings.TrimSpace(p.Command) == "" {
		return Decision{
			Blocked: true,
			Message: "blocked: bash command in plan mode must include a valid read-only command.",
		}
	}
	cmd := strings.TrimSpace(p.Command)
	lower := strings.ToLower(cmd)

	for _, mc := range bashMetachars {
		if strings.Contains(lower, mc) {
			return Decision{
				Blocked: true,
				Message: fmt.Sprintf("blocked: bash command in plan mode must not contain shell operators (%q). Use separate calls for chained commands.", mc),
			}
		}
	}

	for _, safe := range safeBashCommands {
		if !bashMatchesSafePrefix(lower, safe) {
			continue
		}
		if arg := unsafeSafeCommandArg(cmd, safe); arg != "" {
			return Decision{
				Blocked: true,
				Message: fmt.Sprintf("blocked: bash command in plan mode uses a write-capable argument (%q). Use a read-only command while planning.", arg),
			}
		}
		return Decision{}
	}

	return Decision{
		Blocked: true,
		Message: fmt.Sprintf("blocked: bash commands in plan mode must be read-only. %q is not in the safe command list. Use read-only tools for exploration, then exit plan mode to run this command.", cmd),
	}
}

func bashMatchesSafePrefix(lower, safe string) bool {
	if !strings.HasPrefix(lower, safe) {
		return false
	}
	if len(lower) == len(safe) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(lower[len(safe):])
	return unicode.IsSpace(r)
}

func unsafeSafeCommandArg(cmd, safe string) string {
	fields := strings.Fields(cmd)
	base := strings.Fields(safe)
	if len(fields) <= len(base) {
		return ""
	}
	args := fields[len(base):]
	lowerArgs := make([]string, len(args))
	for i, arg := range args {
		lowerArgs[i] = strings.ToLower(arg)
	}
	if strings.HasPrefix(safe, "git ") {
		for _, arg := range lowerArgs {
			if arg == "--output" || strings.HasPrefix(arg, "--output=") || arg == "--ext-diff" {
				return arg
			}
		}
	}
	switch safe {
	case "git grep":
		for i, arg := range args {
			lowerArg := lowerArgs[i]
			if arg == "-O" || strings.HasPrefix(arg, "-O") || strings.HasPrefix(lowerArg, "--open-files-in-pager") {
				return arg
			}
		}
	case "find":
		for _, arg := range lowerArgs {
			if findWriteArgs[arg] {
				return arg
			}
		}
	case "go list", "go vet":
		for _, arg := range lowerArgs {
			if goWriteOrExecArgs[arg] || strings.HasPrefix(arg, "-mod=mod") || strings.HasPrefix(arg, "-modfile=") || strings.HasPrefix(arg, "-toolexec=") || strings.HasPrefix(arg, "-vettool=") {
				return arg
			}
		}
	}
	return ""
}
