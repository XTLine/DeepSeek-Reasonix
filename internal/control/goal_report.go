package control

import (
	"fmt"
	"reasonix/internal/tool"
	"strings"
	"sync"
)

// goalTurnRecorder is the per-turn recorder bound to one goal turn's scope and
// epoch. update_goal calls land here as candidate state; the FSM commits them
// only when the goal lifecycle still matches (scope + epoch), so late calls
// from a replaced or cleared goal are rejected. Usage events emitted during the
// turn are folded through the recorder into the goal's observational token total.
type goalTurnRecorder struct {
	mu               sync.Mutex
	machine          *goalMachine
	scopeID          string
	epoch            uint64
	recorded         bool
	terminal         bool
	status           string
	reason           string
	nextAction       string
	tokensUsed       int
	requestsUsed     int
	workDurationMs   int64
	durationRecorded bool
}

func (g *goalMachine) newTurnRecorder(scopeID string, epoch uint64) *goalTurnRecorder {
	return &goalTurnRecorder{machine: g, scopeID: scopeID, epoch: epoch}
}

// RecordGoalReport validates the report against the turn's goal lifecycle and
// records it as the turn's candidate disposition. Same-value repeats are
// idempotent; continue may upgrade to complete/blocked; complete and blocked
// are terminal and reject conflicting later calls.
func (r *goalTurnRecorder) RecordGoalReport(report tool.GoalReport) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.machine.turnActive(r.scopeID, r.epoch) {
		return "", fmt.Errorf("update_goal: the active goal changed during this turn — report ignored; no goal state was changed")
	}
	// The wire status "continue" maps to the FSM's internal "running" state;
	// retain the wire value for user-facing acknowledgements and errors.
	wireStatus := report.Status
	if report.Status == "continue" {
		report.Status = GoalStatusRunning
	}
	switch {
	case r.terminal:
		return "", fmt.Errorf("update_goal: this turn's disposition is already final (%s); conflicting %q report ignored", r.status, wireStatus)
	case !r.recorded:
		// first record
	case r.status == report.Status && r.reason == report.Reason && r.nextAction == report.NextAction:
		return fmt.Sprintf("update_goal: %s already recorded for this turn (identical report).", wireStatus), nil
	case r.status == GoalStatusRunning && (report.Status == GoalStatusComplete || report.Status == GoalStatusBlocked):
		// continue → terminal upgrade allowed.
	default:
		return "", fmt.Errorf("update_goal: conflicting reports this turn (%s then %s) — the later report was ignored", r.status, wireStatus)
	}
	r.recorded = true
	r.status = report.Status
	r.reason = report.Reason
	r.nextAction = report.NextAction
	if report.Status != GoalStatusRunning {
		r.terminal = true
	}
	return fmt.Sprintf("update_goal: %s recorded for this turn.", wireStatus), nil
}

func (r *goalTurnRecorder) addUsage(tokens int) {
	r.addUsageWithRequests(tokens, 0)
}

func (r *goalTurnRecorder) addUsageWithRequests(tokens, requests int) {
	if tokens <= 0 && requests <= 0 {
		return
	}
	r.mu.Lock()
	if r.machine.foldUsage(r.scopeID, r.epoch, tokens, requests) {
		if tokens > 0 {
			r.tokensUsed += tokens
		}
		if requests > 0 {
			r.requestsUsed += requests
		}
	}
	r.mu.Unlock()
}

func (r *goalTurnRecorder) usageTokens() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tokensUsed
}

func (r *goalTurnRecorder) addWorkDuration(durationMs int64) {
	if durationMs <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.durationRecorded {
		return
	}
	if r.machine.foldWorkDuration(r.scopeID, r.epoch, durationMs) {
		r.workDurationMs = durationMs
		r.durationRecorded = true
	}
}

// validReport returns the recorded report only when the goal lifecycle still
// matches the recorder's binding; stale (replaced/cleared) turns report nothing.
func (r *goalTurnRecorder) validReport(expectedEpoch uint64) *goalTurnReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.recorded || r.epoch != expectedEpoch || !r.machine.turnActive(r.scopeID, r.epoch) {
		return nil
	}
	return &goalTurnReport{status: r.status, reason: r.reason, nextAction: r.nextAction}
}

// goalTurnReport is the validated update_goal report for one goal turn.
type goalTurnReport struct {
	status     string
	reason     string
	nextAction string
}

// turnActive reports whether the machine's goal lifecycle matches the binding.
func (g *goalMachine) turnActive(scopeID string, epoch uint64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return !g.disarmed && strings.TrimSpace(g.goal) != "" && g.status == GoalStatusRunning &&
		g.scopeID == scopeID && g.continuationEpoch == epoch
}
