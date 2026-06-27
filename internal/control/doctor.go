package control

import (
	"fmt"
	"os"
	"strings"

	"reasonix/internal/config"
	"reasonix/internal/doctor"
)

// doctorListText returns combined runtime + config diagnostics as plain text,
// formatted for notice display. Mirrors modelListText/mcpListText pattern.
func (c *Controller) doctorListText() string {
	var b strings.Builder

	// Runtime section
	b.WriteString("runtime\n")

	// Turn status
	status := c.RuntimeStatus()
	turnStatus := "idle"
	if status.Running {
		if status.PendingPrompt {
			turnStatus = "pending approval"
		} else {
			turnStatus = "running"
		}
	}
	if status.CancelRequested {
		turnStatus = "canceling"
	}
	fmt.Fprintf(&b, "  turn         %s\n", turnStatus)

	// Model
	fmt.Fprintf(&b, "  model        %s\n", c.label)

	// Context usage
	used, window := c.ContextSnapshot()
	percent := 0.0
	if window > 0 {
		percent = float64(used) * 100.0 / float64(window)
	}
	fmt.Fprintf(&b, "  context      %s / %s (%.1f%%)\n",
		formatTokens(used), formatTokens(window), percent)

	// MCP status
	if h := c.Host(); h != nil {
		servers := h.ServerNames()
		failures := c.mcp.failures()
		connected := len(servers)
		failed := len(failures)
		if connected > 0 || failed > 0 {
			fmt.Fprintf(&b, "  mcp          %d connected", connected)
			if failed > 0 {
				fmt.Fprintf(&b, ", %d failed", failed)
			}
			b.WriteString("\n")
		}
	}

	// Session stats
	history := c.History()
	sessionPath := c.SessionPath()
	fmt.Fprintf(&b, "  session      %d messages", len(history))
	if sessionPath != "" {
		if info, err := os.Stat(sessionPath); err == nil {
			fmt.Fprintf(&b, ", %s", formatBytes(info.Size()))
		}
	}
	b.WriteString("\n")

	// Background jobs
	if status.BackgroundJobs > 0 {
		fmt.Fprintf(&b, "  background   %d jobs\n", status.BackgroundJobs)
	}

	// Warnings (置顶关键问题)
	var warnings []string
	if failures := c.mcp.failures(); len(failures) > 0 {
		for _, f := range failures {
			warnings = append(warnings, fmt.Sprintf("MCP %s failed: %s", f.Name, f.Error))
		}
	}
	if len(warnings) > 0 {
		fmt.Fprintf(&b, "  warnings     %s\n", strings.Join(warnings, "; "))
	}

	// Doctor report (static config)
	b.WriteString("\n")
	cfg, _ := config.Load()
	doctorReport := doctor.Collect(doctor.Options{
		Version: "", // Version not available in Controller; doctor will use empty
		Config:  cfg,
	})
	b.WriteString(doctor.RenderText(doctorReport))

	return strings.TrimRight(b.String(), "\n")
}

// formatTokens formats token count in human-readable form.
func formatTokens(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fK", float64(n)/1000.0)
	}
	return fmt.Sprintf("%d", n)
}

// formatBytes formats byte count in human-readable form.
func formatBytes(n int64) string {
	if n >= 1024*1024 {
		return fmt.Sprintf("%.1fMB", float64(n)/(1024*1024))
	}
	if n >= 1024 {
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	}
	return fmt.Sprintf("%dB", n)
}
