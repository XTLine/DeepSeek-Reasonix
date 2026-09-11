package agent

import (
	"reasonix/internal/event"
	"reasonix/internal/evidence"
	"reasonix/internal/taskcontract"
)

// readinessPauseActive reports whether an unmet final-readiness requirement may
// pause the turn and hand the user a recovery card.
//
// Delivery and closed-loop Goal/Plan turns pause on their readiness contract.
// Standard reports quality gaps in its completion summary and ends normally.
func (a *Agent) readinessPauseActive(check finalReadinessCheck) bool {
	if a == nil {
		return false
	}
	return a.turn.constraints.PolicyFloor == taskcontract.PolicyFloorDelivery ||
		a.closedLoopActive() || a.planContractSnapshot() != nil
}

// handleReadinessGap resolves an unmet final-readiness requirement: a resumable
// pause carrying the structured gap, or an allowed-readiness audit when the
// active policy ends normally and reports the gap in its summary instead.
// stopped reports whether the caller must end the turn with err.
func (a *Agent) handleReadinessGap(readiness finalReadinessCheck) (stopped bool, err error) {
	if readiness.reason == "" {
		return false, nil
	}
	// Standard ends with its answer/quality summary. Delivery and Goal hand
	// the structured gap to the controller, which exposes an explicit recovery
	// action or lets the Goal FSM decide whether to continue.
	if !a.readinessPauseActive(readiness) {
		event.RecordReadinessAudit(a.svc.sink, readiness.audit(evidence.ReadinessAllowed, a.turn.readinessRecovered))
		return false, nil
	}
	event.RecordReadinessAudit(a.svc.sink, readiness.audit(evidence.ReadinessErrored, false))
	a.pending.finalReadinessRecovery = true
	a.persistFinalReadinessRecovery(readiness.missingIDs())
	gaps := a.readinessOperationGaps()
	reason := readiness.reason
	if named := describeReadinessGaps(gaps); named != "" {
		reason += "; " + named
	}
	return true, &FinalReadinessError{
		Attempts:          1,
		Reason:            reason,
		Missing:           readiness.missingIDs(),
		ContinuationClass: readiness.continuationClass(),
		ProgressKey:       readiness.progressSignature(),
		Operations:        gaps,
	}
}
