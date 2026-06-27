# Desktop GUI `/doctor` Command Design

**Date:** 2026-06-03
**Status:** Implemented
**Related:** PR #2631 (CLI `reasonix doctor` command)

## Summary

Add a `/doctor` slash command to the Desktop GUI that displays combined runtime diagnostics and static configuration info in the chat session, following the same pattern as existing read-only management commands (`/mcp`, `/skill`, `/model`).

## Background

PR #2631 introduced the `reasonix doctor` CLI command for static configuration diagnostics. However, Desktop GUI users had no way to quickly check runtime status (current turn state, MCP live connections, context usage) without switching to a terminal.

The `/doctor` command brings diagnostics into the GUI chat interface, combining:
- **Runtime state** (turn status, model, context, MCP, session stats)
- **Static config** (full `doctor` report: providers, plugins, LSP, sandbox, network, permissions)

## Goals

1. **Consistency** — Follow the exact pattern of `/mcp`, `/skill`, `/hooks` (backend `managementNotice` path, not frontend interception)
2. **Reuse existing code** — Leverage the `doctor` package from PR #2631, zero duplication
3. **Minimal changes** — Only touch control layer and i18n, no frontend JS/TS changes
4. **Discoverability** — Appear in slash autocomplete menu with description

## Architecture

```
User input: /doctor
    ↓
Frontend (App.tsx) does NOT intercept → Submit
    ↓
Backend: control.Submit() → managementNotice()
    ↓
Dispatch: case "/doctor" → c.doctorListText()
    ↓
Collect: runtime status + doctor.Collect()
    ↓
Emit: Notice event with formatted text
    ↓
Frontend renders as notice item in chat transcript
```

**Key decision:** `/doctor` follows the **same path as `/mcp` and `/skill`** (backend managementNotice), not the frontend-intercept path used by `/model <ref>`, `/clear`, `/theme`. This ensures:
- Consistent behavior across Desktop and serve/HTTP frontends
- No frontend JS/TS changes required
- Notice rendering uses existing styles

## Implementation

### Files Changed

**New files:**
- `internal/control/doctor.go` — `doctorListText()` method + formatters

**Modified files:**
- `internal/control/slash.go` — add `case "/doctor"` to `managementNotice()`
- `desktop/app.go` — add `/doctor` to `Commands()` builtin list
- `internal/i18n/i18n.go` — add `CmdDoctor` field
- `internal/i18n/messages_en.go` — `"show runtime diagnostics"`
- `internal/i18n/messages_zh.go` — `"显示运行时诊断"`
- `internal/i18n/messages_zh_tw.go` — `"顯示執行階段診斷"`

### Code Structure

#### `internal/control/doctor.go`

```go
func (c *Controller) doctorListText() string {
    var b strings.Builder

    // Runtime section
    b.WriteString("runtime\n")
    status := c.RuntimeStatus()
    fmt.Fprintf(&b, "  turn         %s\n", formatTurnStatus(status))
    fmt.Fprintf(&b, "  model        %s\n", c.label)
    used, window := c.ContextSnapshot()
    fmt.Fprintf(&b, "  context      %s / %s (%.1f%%)\n", ...)
    fmt.Fprintf(&b, "  mcp          %d connected, %d failed\n", ...)
    fmt.Fprintf(&b, "  session      %d messages, %s\n", ...)

    // Warnings (if any)
    if len(warnings) > 0 {
        fmt.Fprintf(&b, "  warnings     %s\n", ...)
    }

    // Doctor report (static config)
    b.WriteString("\n")
    cfg, _ := config.Load()
    doctorReport := doctor.Collect(doctor.Options{Config: cfg})
    b.WriteString(doctor.RenderText(doctorReport))

    return strings.TrimRight(b.String(), "\n")
}
```

**Helper functions:**
- `formatTokens(n int) string` — "2.4K" or "512"
- `formatBytes(n int64) string` — "128KB" or "1.5MB"

#### `internal/control/slash.go`

```go
func (c *Controller) managementNotice(trimmed string) bool {
    fields := strings.Fields(trimmed)
    if len(fields) == 0 {
        return false
    }
    switch fields[0] {
    case "/doctor":
        c.notice(c.doctorListText())
    case "/model":
        c.notice(c.modelListText())
    // ...
```

## Output Example

```
ℹ runtime
  turn         idle
  model        deepseek/deepseek-chat
  context      2.4K / 128K (1.9%)
  mcp          3 connected, 1 failed
  session      42 messages, 128KB
  warnings     MCP server 'foo' failed: connection timeout

reasonix  doctor
  system       linux/amd64
  cwd          ~/code/work/...
  config       ~/.reasonix/config.toml
  user config  ~/.reasonix/config.toml
  model        deepseek/deepseek-chat

providers
  deepseek         openai   api.deepseek.com    key:present default

plugins
  mcp1         stdio    auto-start
  mcp2         sse      auto-start
  foo          http     (failed: timeout)

[... full doctor report ...]
```

## Testing

**Unit tests:**
- Existing tests for `managementNotice` path already cover the dispatch
- `doctor` package tests already validate report generation

**Manual testing:**
```bash
cd desktop
wails dev
```

1. Input `/` in chat → verify `/doctor` appears in autocomplete with description
2. Input `/doctor` → verify notice shows runtime + doctor report
3. Test with MCP failures → verify warnings appear
4. Test while turn running → verify "turn running" status

## Tradeoffs

**Version field:** Controller doesn't have `version` string, so `doctor.Collect` gets empty version. Output shows "reasonix  doctor" instead of "reasonix v1.12.0 doctor". This is acceptable to avoid threading version through boot/Options, which would touch many files for cosmetic benefit.

**Alternative considered:** Store version in Controller struct via Options. Rejected: adds 3+ files of plumbing for a display-only field.

## Memory and Performance

- No performance impact — diagnostics only run on explicit `/doctor` command
- Memory: doctor report is ephemeral, discarded after notice emit
- Typical output: ~2-3KB text (runtime ~500B, doctor report ~1.5-2KB)

## Related Work

- PR #2631: CLI `reasonix doctor` command (merged)
- Existing management commands: `/mcp`, `/skill`, `/hooks`, `/model`

## References

- `internal/doctor/report.go` — static config collection
- `internal/control/slash.go` — management command dispatch
- `desktop/app.go: Commands()` — slash autocomplete source

