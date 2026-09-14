package control

import (
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/event"
	"reasonix/internal/session"
)

// CancelReceipt acknowledges a session-scoped Stop request. Accepted means the
// cancellation signal was processed; it does not claim that every owned
// operation has already exited.
type CancelReceipt struct {
	SessionRef       string `json:"sessionRef"`
	HeadID           string `json:"headId"`
	RuntimeEpoch     string `json:"runtimeEpoch"`
	Accepted         bool   `json:"accepted"`
	AlreadyIdle      bool   `json:"alreadyIdle"`
	RecoveryRequired bool   `json:"recoveryRequired"`
}

// CancelSession stops the activity owned by this captured controller. Callers
// do not need a turn id, and an idle cancellation is idempotently successful.
func (c *Controller) CancelSession() CancelReceipt {
	if c == nil {
		return CancelReceipt{Accepted: true, AlreadyIdle: true}
	}
	// Capture only owner-local fields before signalling. A full runtime snapshot
	// may wait behind a ledger or projection commit and therefore does not belong
	// on the acknowledgement path.
	c.mu.Lock()
	alreadyIdle := c.cancel == nil && !c.running && !c.finishing
	sessionRef := c.sessionPath
	c.mu.Unlock()
	headID := agent.BranchID(sessionRef)
	c.runtimeState.mu.Lock()
	epoch := c.runtimeState.snapshot.RuntimeEpoch
	recoveryRequired := c.runtimeState.snapshot.Phase == "recovery_required"
	c.runtimeState.mu.Unlock()
	service, runtime, exclusive := c.v3Binding()
	if exclusive && runtime != nil {
		sessionRef = runtime.Ref().SessionID
		headID = ""
	}
	cancelled := c.signalCancellation()
	if exclusive && runtime != nil && service != nil {
		if v3Receipt, err := service.CancelSession(runtime.Ref()); err == nil {
			epoch = v3Receipt.RuntimeEpoch
			alreadyIdle = v3Receipt.Phase == session.RuntimeIdle
			recoveryRequired = v3Receipt.Phase == session.RuntimeRecoveryRequired
		}
	}
	// Interaction teardown, status persistence, and Goal bookkeeping are
	// deliberately outside the receipt path. They may cross user callbacks or a
	// blocked event sink; the cancellation signal and watchdog are already live.
	go c.finishCancellation(cancelled)
	receipt := CancelReceipt{
		SessionRef: sessionRef, HeadID: headID, RuntimeEpoch: epoch,
		Accepted: true, AlreadyIdle: alreadyIdle, RecoveryRequired: recoveryRequired,
	}
	return receipt
}

// Cancel aborts the in-flight turn. A goroutine blocked awaiting approval
// unblocks via the cancelled context.
func (c *Controller) Cancel() {
	turnID, cancelled := c.cancelTurnLocked()
	c.finishCancel(turnID, cancelled)
}

// cancelLocked is retained for call sites already inside a typed prompt
// transition. Cancellation itself is independent of answer serialization.
func (c *Controller) cancelLocked() {
	turnID, cancelled := c.cancelTurnLocked()
	c.finishCancel(turnID, cancelled)
}

// cancelTurnLocked signals the turn before any observable work: the status
// emit that follows is a synchronous event barrier, and a stalled event lane
// must never keep the provider stream or a tool process alive after Stop.
func (c *Controller) cancelTurnLocked() (string, bool) {
	cancelled := c.signalCancellation()
	if !cancelled {
		return "", false
	}
	turnID := ""
	if ledger := c.turnEventLedger(); ledger != nil {
		turnID = ledger.ActiveTurnID()
	}
	c.promptOwner.CancelAll()
	c.approval.clearAll()
	return turnID, true
}

// signalCancellation is the complete synchronous Stop fast path. It touches
// only the activity owner, then starts supervision. No disk, event sink,
// interaction answerer, or turn-ledger operation may be added here.
func (c *Controller) signalCancellation() bool {
	c.mu.Lock()
	cancel := c.cancel
	firstSignal := cancel != nil && !c.canceling
	if cancel != nil {
		c.canceling = true
	}
	c.mu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	if _, runtime, exclusive := c.v3Binding(); exclusive && runtime != nil {
		// Runtime cancellation is an independent, session-scoped fast path and
		// does not consult a UI turn id or wait for persistence.
		runtime.Cancel()
	}
	if firstSignal {
		c.startCancellationWatchdog("")
	}
	return true
}

func (c *Controller) finishCancellation(cancelled bool) {
	turnID := ""
	if ledger := c.turnEventLedger(); ledger != nil {
		turnID = ledger.ActiveTurnID()
	}
	c.promptOwner.CancelAll()
	c.approval.clearAll()
	c.finishCancel(turnID, cancelled)
}

func (c *Controller) finishCancel(turnID string, cancelled bool) {
	defer c.refreshRuntimeState(event.Event{})
	if cancelled {
		c.emitTurnStatus(event.TurnCancelling, turnID)
	}
	if c.goals.active() {
		c.stopGoal(GoalStatusStopped)
	}
	if c.sessionEngineEnabled() {
		c.disarmGoalLifecycle("cancelled")
	}
}

// startCancellationWatchdog seals a turn whose activity ignored cancellation.
// It cannot kill an arbitrary Go goroutine, so runtime ownership remains held
// until the worker really exits.
func (c *Controller) startCancellationWatchdog(turnID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	done := c.activeDone
	c.mu.Unlock()
	// Every admitted runtime installs activeDone before publishing turn/start.
	// A nil channel can only come from a legacy embedder or a test that directly
	// mutates compatibility fields; there is no owned worker to supervise.
	if done == nil {
		return
	}
	go func() {
		timer := time.NewTimer(c.cancellationGrace())
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-done:
			return
		}

		c.mu.Lock()
		stillRunning := c.running && c.canceling && !c.closed
		c.mu.Unlock()
		if !stillRunning {
			return
		}
		if turnID == "" {
			if ledger := c.turnEventLedger(); ledger != nil {
				turnID = ledger.ActiveTurnID()
			}
		}
		recovery := &event.RecoveryStatus{
			State:                "recovery_required",
			Phase:                c.RuntimeStateSnapshot().Activity,
			Reason:               "cancellation_grace_expired",
			RequiresUserDecision: true,
		}
		if _, runtime, exclusive := c.v3Binding(); exclusive && runtime != nil {
			runtime.RequireRecovery(recovery.Phase)
		}
		_ = c.emitTurnEventChecked(event.Event{
			Kind:      event.TurnDone,
			TurnID:    turnID,
			Status:    event.TurnRecoveryRequired,
			Cancelled: true,
			Outcome:   "unknown",
			Recovery:  recovery,
		})
		c.refreshRuntimeState(event.Event{})
	}()
}
