package agent

import (
	"context"

	"reasonix/internal/evidence"
	"reasonix/internal/plancontract"
)

// SetPlanContract records the approved plan this turn executes, or clears it
// when the turn runs without one. The coordinator sets it before every executor
// run so a turn never inherits the previous turn's plan.
func (a *Agent) SetPlanContract(plan *plancontract.Plan) {
	if a == nil {
		return
	}
	a.sess.todoMu.Lock()
	defer a.sess.todoMu.Unlock()
	if plan == nil {
		a.planContract = nil
		return
	}
	copied := *plan
	a.planContract = &copied
}

func (a *Agent) planContractSnapshot() *plancontract.Plan {
	if a == nil {
		return nil
	}
	a.sess.todoMu.Lock()
	defer a.sess.todoMu.Unlock()
	if a.planContract == nil {
		return nil
	}
	copied := *a.planContract
	return &copied
}

// withContractState supplies the current task list to compatibility tools.
// Acceptance and verification text stays in the approved plan's model context.
func (a *Agent) withContractState(ctx context.Context) context.Context {
	return evidence.WithTodoState(ctx, a.CanonicalTodoState())
}
