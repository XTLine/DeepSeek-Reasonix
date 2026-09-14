package control

import (
	"context"
	"errors"

	"reasonix/internal/event"
	"reasonix/internal/extension"
)

// runSynchronousTurn owns the blocking transport lifecycle. Durable steer
// acknowledgement is intentionally shared with the asynchronous completion
// path, while follow-up dispatch remains owned by the synchronous frontend so
// its response sink stays bound for every queued turn.
func (c *Controller) runSynchronousTurn(
	ctx context.Context,
	onAdmitted func() error,
	run func(context.Context) error,
) error {
	if err := c.ensureWriteAuthorityReady(); err != nil {
		return err
	}
	if ledger := c.turnEventLedger(); ledger != nil && ledger.CurrentStatus() == event.TurnRecoveryRequired {
		return ErrRecoveryRequired
	}
	ctx, cancel := context.WithCancel(extension.ContextWithRuntimeOwner(ctx, c.RuntimeOwner()))
	c.mu.Lock()
	// Finishing is part of the gate: TurnDone is still fanning out. Closed
	// seals a torn-down controller. Blocking callers get an error rather than
	// parking because they already own and enforce the request boundary.
	if c.running || c.finishing || c.rotating || c.closed {
		c.mu.Unlock()
		cancel()
		return ErrTurnRunning
	}
	if c.rejectDrainingGenerationLocked() {
		c.mu.Unlock()
		cancel()
		c.emitDrainingNotice()
		return ErrRuntimeDraining
	}
	c.cancel = cancel
	c.activeDone = make(chan struct{})
	c.running = true
	c.canceling = false
	c.mu.Unlock()
	c.refreshRuntimeState(event.Event{})
	runtimeCtx, runtimeActivity, runtimeErr := c.beginSessionRuntimeActivity(ctx, "turn")
	if runtimeErr != nil {
		finish := func() {
			c.mu.Lock()
			c.running = false
			if c.activeDone != nil {
				close(c.activeDone)
				c.activeDone = nil
			}
			c.cancel = nil
			c.canceling = false
			c.mu.Unlock()
			c.refreshRuntimeState(event.Event{})
			cancel()
		}
		finish()
		return runtimeErr
	}
	ctx = runtimeCtx
	finish := func() {
		c.mu.Lock()
		c.running = false
		if c.activeDone != nil {
			close(c.activeDone)
			c.activeDone = nil
		}
		c.cancel = nil
		c.canceling = false
		c.mu.Unlock()
		c.refreshRuntimeState(event.Event{})
		c.finishSessionRuntimeActivity(runtimeActivity)
		c.kickGoalDriver()
		cancel()
	}
	if onAdmitted != nil {
		if err := onAdmitted(); err != nil {
			finish()
			return err
		}
	}
	defer event.RecordTurnCompletion(c.sink)
	defer func() {
		finish()
		c.onInboxTurnDone()
	}()
	// Blocking transports use the same host-owned turn boundary as interactive
	// submissions. The agent's TurnStarted notification is deliberately a
	// duplicate display event; it cannot create runtime ownership or a durable
	// turn by itself.
	run = c.prepareTurnAdmission(run)
	runErr := run(ctx)
	if ledger := c.turnEventLedger(); ledger != nil && ledger.ActiveTurnID() != "" && !ledger.CurrentStatus().Terminal() {
		cancelled := errors.Is(ctx.Err(), context.Canceled)
		done := event.Event{Kind: event.TurnDone, Err: runErr, Cancelled: cancelled, Outcome: turnOutcome(runErr)}
		if cancelled {
			done.Status = event.TurnInterrupted
		} else if runErr != nil {
			done.Status = event.TurnFailed
		} else {
			done.Status = event.TurnCompleted
		}
		if terminalErr := c.emitTurnEventChecked(done); terminalErr != nil {
			runErr = errors.Join(runErr, terminalErr)
		}
	}
	return runErr
}
