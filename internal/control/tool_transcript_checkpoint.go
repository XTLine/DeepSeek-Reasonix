package control

import (
	"context"
	"reasonix/internal/agent"
)

// CheckpointSession implements agent.SessionCheckpointer. The boundary is
// intentionally semantic: ordinary todo, approval, assistant and turn-end
// events remain eligible for the write-behind batch.
func (c *Controller) CheckpointSession(ctx context.Context, boundary agent.SessionCheckpointBoundary) error {
	switch boundary {
	case agent.CheckpointBeforeModel, agent.CheckpointBeforeTopTool:
		if _, err := c.flushSessionEvents(ctx); err != nil {
			return err
		}
		return nil
	default:
		return nil
	}
}
