package control

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"reasonix/internal/agent"
	"reasonix/internal/provider"
)

// EnsureSessionPath pins a fresh auto-save file for this controller when none is
// set yet and a session dir is configured — the "fresh session" branch every
// surface runs right after building a controller. It is a no-op once a resume or
// continue has already pinned a path (SessionPath() != ""), so callers can run a
// conditional Resume and then invoke this unconditionally. Centralises the
// per-surface copies of this logic (the CLI chat/serve fresh branches and the
// bot's former ensureControllerSessionPath).
func (c *Controller) EnsureSessionPath() {
	if _, ok := c.SessionRef(); ok {
		return
	}
	if service, _, exclusive := c.v3Binding(); exclusive && service != nil {
		if _, err := c.BindFreshSession(context.Background(), ""); err != nil {
			c.failTurnEventLedger(err)
		}
		return
	}
	if c.SessionPath() != "" || c.SessionDir() == "" {
		return
	}
	c.SetFreshSessionPath(agent.NewSessionPath(c.SessionDir(), c.Label()))
}

// AdoptHistory makes a freshly built controller continue an existing
// conversation in path: it resumes the carried messages there when there are
// any, otherwise just points auto-save at path. An empty path with no messages
// is a no-op. This is the shared kernel of the model/effort switch across the
// CLI, the HTTP server, and ACP — each computes path its own way
// (ContinueSessionPath for the CLI/serve, the pinned transcript for ACP) and
// hands the carried history (Controller.History()) here. Keeping the
// Resume/SetSessionPath choice in one place avoids the orphaned-duplicate class
// of bug (#2807) recurring as each surface copied it.
func (c *Controller) AdoptHistory(msgs []provider.Message, path string) {
	if c.sessionEngineEnabled() {
		if _, ok := c.SessionRef(); ok {
			if len(msgs) > 0 {
				if err := c.replaceSessionEventProjection(context.Background(), "explicit-history-adopt", msgs); err != nil {
					slog.Warn("controller: record adopted v3 history", "err", err)
					c.failTurnEventLedger(err)
				} else {
					c.restoreExecutorFromSessionEvents()
				}
			} else {
				c.restoreExecutorFromSessionEvents()
			}
			return
		}
		if path == "" {
			// A hot rebuild may carry an in-memory conversation that never had
			// persistent identity. Keep it as the candidate model/UI state; the
			// first real input will create one v3 session and seed these exact
			// messages. This is not a committed session until that admission.
			if len(msgs) > 0 && c.executor != nil {
				c.executor.SetSession(agent.NewSession("").CloneWithMessages(msgs))
			}
			return
		}
		if path != "" {
			if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
				if _, err := c.BindFreshSession(context.Background(), ""); err != nil {
					c.failTurnEventLedger(err)
					return
				}
				if len(msgs) > 0 {
					if err := c.replaceSessionEventProjection(context.Background(), "fresh-history-adopt", msgs); err != nil {
						c.failTurnEventLedger(err)
					} else {
						c.restoreExecutorFromSessionEvents()
					}
				}
				return
			}
			if _, err := c.ContinueLegacySession(context.Background(), path, ""); err != nil {
				slog.Warn("controller: legacy continue into v3 failed", "path", path, "err", err)
				c.failTurnEventLedger(err)
			}
		}
		return
	}
	if len(msgs) > 0 {
		if path != "" {
			if loaded, err := agent.LoadSession(path); err == nil && loaded != nil {
				if resumed, ok := loaded.CloneWithMessagesIfCompatible(msgs); ok {
					c.Resume(resumed, path)
					return
				}
			}
		}
		c.Resume(agent.NewSession("").CloneWithMessages(msgs), path)
	} else if path != "" {
		// Even an empty transcript can carry session-scoped sidecars such as a
		// running or blocked Goal. Resume a persisted empty session so controller
		// rebuilds preserve that state; fall back to a plain binding for a fresh
		// path that has not been saved yet.
		if loaded, err := agent.LoadSession(path); err == nil && loaded != nil {
			c.Resume(loaded, path)
			return
		}
		c.SetSessionPath(path)
	}
}

// AdoptRebuiltModelContext applies a model/settings rebuild to an already
// bound v3 session. It changes only the provider-visible context; UI history
// and stable message identity remain sourced from the original event stream.
func (c *Controller) AdoptRebuiltModelContext(msgs []provider.Message) error {
	if !c.sessionEngineEnabled() {
		return errors.New("model-context adoption requires an exclusive v3 session")
	}
	if len(msgs) == 0 {
		if snapshot, ok := c.sessionEventSnapshot(); ok {
			msgs = snapshot.Projection.ModelMessages
		}
	}
	return c.replaceSessionModelContext(context.Background(), msgs, "agent-rebuild")
}
