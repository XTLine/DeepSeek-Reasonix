package agent

// The host's canonical task list: the state that outlives a turn because it
// never rides in the prompt, so a later turn still sees an unfinished plan.

import (
	"encoding/json"

	"reasonix/internal/evidence"
)

// SeedTodoState initializes the canonical task list from a host-generated
// starter list, such as an approved plan. A new host seed replaces stale state
// from earlier work so the model can update the plan it just displayed.
func (a *Agent) SeedTodoState(todos []evidence.TodoItem) {
	if len(todos) == 0 {
		return
	}
	a.setTodoState(todos)
}

// ReplaceTodoState mirrors a host-generated todo list into the canonical state.
// It is used when the host, rather than the model, owns the full state transition.
func (a *Agent) ReplaceTodoState(todos []evidence.TodoItem) {
	a.setTodoState(todos)
	a.recordTodoState(a.CanonicalTodoState())
}

// CanonicalTodoState returns a copy of the host-reconstructed task list.
func (a *Agent) CanonicalTodoState() []evidence.TodoItem {
	a.sess.todoMu.Lock()
	defer a.sess.todoMu.Unlock()
	return append([]evidence.TodoItem(nil), a.sess.todoState...)
}

// CurrentTaskTodoState returns only the latest successful todo_write retained
// in the current evidence ledger. Unlike CanonicalTodoState, it never falls
// back to a prior user turn.
func (a *Agent) CurrentTaskTodoState() []evidence.TodoItem {
	if a == nil || a.task.ledger == nil {
		return nil
	}
	todos, ok := a.task.ledger.LatestTodos()
	if !ok {
		return nil
	}
	return append([]evidence.TodoItem(nil), todos...)
}

// recordTodoState logs the host-advanced list as a synthetic todo_write receipt
// so the per-turn final gate (which reads the ledger's latest todo_write) sees
// the advance — the model no longer has to re-send a todo_write to mark the
// completion. It bypasses the todo_write tool, so the completion-transition
// guard never runs on it.
func (a *Agent) recordTodoState(todos []evidence.TodoItem) {
	if a.task.ledger == nil {
		return
	}
	args, err := json.Marshal(map[string]any{"todos": todos})
	if err != nil {
		return
	}
	a.task.ledger.Record(evidence.ReceiptFromToolCall("todo_write", json.RawMessage(args), true, true))
}
