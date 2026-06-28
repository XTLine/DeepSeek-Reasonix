package control

import (
	"fmt"
	"os"
	"strings"

	"reasonix/internal/config"
	"reasonix/internal/doctor"
)

// DoctorText returns runtime and static diagnostics as plain text. It only reads
// controller state through short-held locks, so it remains available while a turn
// is running or waiting for approval.
func (c *Controller) DoctorText() string {
	var b strings.Builder

	b.WriteString("runtime\n")
	status := c.RuntimeStatus()
	turn := "idle"
	if status.Running {
		turn = "running"
		if status.PendingPrompt {
			turn = "pending approval"
		}
	}
	if status.CancelRequested {
		turn = "canceling"
	}
	fmt.Fprintf(&b, "  turn         %s\n", turn)
	fmt.Fprintf(&b, "  model        %s\n", valueOrText(c.label, "(none)"))

	used, window := c.ContextSnapshot()
	percent := 0.0
	if window > 0 {
		percent = float64(used) * 100 / float64(window)
	}
	fmt.Fprintf(&b, "  context      %s / %s (%.1f%%)\n", formatTokens(used), formatTokens(window), percent)

	connected := 0
	if h := c.Host(); h != nil {
		connected = len(h.ServerNames())
	}
	failures := c.mcp.failures()
	fmt.Fprintf(&b, "  mcp          %d connected", connected)
	if len(failures) > 0 {
		fmt.Fprintf(&b, ", %d failed", len(failures))
	}
	b.WriteString("\n")

	history := c.History()
	fmt.Fprintf(&b, "  session      %d messages", len(history))
	if path := c.SessionPath(); path != "" {
		if info, err := os.Stat(path); err == nil {
			fmt.Fprintf(&b, ", %s", formatBytes(info.Size()))
		}
	}
	b.WriteString("\n")
	if status.BackgroundJobs > 0 {
		fmt.Fprintf(&b, "  background   %d jobs\n", status.BackgroundJobs)
	}
	if len(failures) > 0 {
		warnings := make([]string, 0, len(failures))
		for _, f := range failures {
			warnings = append(warnings, fmt.Sprintf("MCP %s failed: %s", f.Name, redactMCPError(f.Error)))
		}
		fmt.Fprintf(&b, "  warnings     %s\n", strings.Join(warnings, "; "))
	}

	cfg, err := config.LoadForRoot(c.workspaceRoot)
	if err != nil {
		fmt.Fprintf(&b, "  warnings     config load failed: %v\n", err)
	}
	b.WriteString("\n")
	b.WriteString(doctor.RenderText(doctor.Collect(doctor.Options{
		Version:    c.version,
		Config:     cfg,
		WorkingDir: c.workspaceRoot,
		SessionDir: c.sessionDir,
	})))
	return strings.TrimRight(b.String(), "\n")
}

func formatTokens(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fK", float64(n)/1000)
	}
	return fmt.Sprintf("%d", n)
}

func formatBytes(n int64) string {
	if n >= 1024*1024 {
		return fmt.Sprintf("%.1fMB", float64(n)/(1024*1024))
	}
	if n >= 1024 {
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	}
	return fmt.Sprintf("%dB", n)
}

func valueOrText(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// redactMCPError truncates long error messages and redacts common credential
// patterns (env dumps, auth headers, tokens) to prevent leaking sensitive MCP
// connection details when /doctor output is shared.
func redactMCPError(msg string) string {
	const maxLen = 150
	// Redact common credential leak patterns: replace value portions after known keys.
	// Simple suffix removal after the marker — covers "token=abc", "password=xyz", etc.
	for _, marker := range []string{
		"Authorization: Bearer ",
		"Bearer ",
		"token=",
		"password=",
		"api_key=",
		"apikey=",
		"API_KEY=",
		"APIKEY=",
	} {
		if idx := strings.Index(msg, marker); idx >= 0 {
			start := idx + len(marker)
			// Find the end of the value (space, quote, bracket, or end-of-string).
			end := start
			for end < len(msg) && msg[end] != ' ' && msg[end] != '"' && msg[end] != '\'' && msg[end] != ',' && msg[end] != ')' && msg[end] != ']' {
				end++
			}
			msg = msg[:start] + "[REDACTED]" + msg[end:]
		}
	}
	// Special case: "Authorization:" without "Bearer" following.
	msg = strings.ReplaceAll(msg, "Authorization:", "Authorization:[REDACTED]")
	// Truncate long errors to avoid walls of text in the one-line warnings field.
	if len(msg) > maxLen {
		msg = msg[:maxLen] + "..."
	}
	return msg
}
