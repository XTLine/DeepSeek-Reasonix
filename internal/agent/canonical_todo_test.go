package agent

import (
	"testing"

	"reasonix/internal/event"
	"reasonix/internal/evidence"
	"reasonix/internal/provider"
)

func TestFinalReadinessFallsBackToCanonicalTodos(t *testing.T) {
	ran := evidence.Receipt{ToolName: "bash", Success: true, Write: true, Mutation: true, Command: "git push"}
	open := []evidence.TodoItem{{Content: "push", Status: "completed"}, {Content: "rebase", Status: "pending"}}

	// Turn did work (a successful bash) but issued no todo_write this turn, so the
	// per-turn ledger has no list — the canonical state must still gate inside a
	// closed-loop turn.
	a := &Agent{task: taskRuntime{ledger: readinessLedger(ran)}, sess: sessionRuntime{todoState: open}, turn: turnRuntime{deliveryScopeActive: true}}
	if got := a.ReadinessResult(); !got.Ready {
		t.Fatalf("cross-turn gate = %q, want it to report incomplete canonical todos", got.Reason)
	}

	// A turn that touched nothing (pure Q&A) must never gate on stale canonical state.
	idle := &Agent{task: taskRuntime{ledger: evidence.NewLedger()}, sess: sessionRuntime{todoState: open}, turn: turnRuntime{deliveryScopeActive: true}}
	if got := idle.ReadinessResult(); !got.Ready {
		t.Fatalf("no-work turn gated on canonical todos: %+v", got)
	}

	// All canonical items completed → no gate even after work.
	done := &Agent{task: taskRuntime{ledger: readinessLedger(ran)}, sess: sessionRuntime{todoState: []evidence.TodoItem{{Content: "push", Status: "completed"}}}, turn: turnRuntime{deliveryScopeActive: true}}
	if got := done.ReadinessResult(); !got.Ready {
		t.Fatalf("completed canonical todos still gated: %+v", got)
	}
}

func TestRebuildTodoStateIgnoresHistoricalPendingSignoff(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{
			ID: "t1", Name: "todo_write",
			Arguments: `{"todos":[{"content":"a","status":"in_progress"},{"content":"b","status":"pending"}]}`,
		}}},
		{Role: provider.RoleTool, ToolCallID: "t1", Name: "todo_write", Content: "Todos updated"},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{
			ID: "c1", Name: "complete_step", Arguments: `{"step":"b"}`,
		}}},
		{Role: provider.RoleTool, ToolCallID: "c1", Name: "complete_step", Content: "signed off"},
	}
	a := &Agent{}
	a.rebuildTodoState(msgs)
	if a.sess.todoState[0].Status != "in_progress" || a.sess.todoState[1].Status != "pending" {
		t.Fatalf("historical pending signoff restored invalid state: %+v", a.sess.todoState)
	}
}

func TestSetTodoStateNormalizesLegacyOutOfOrderSnapshot(t *testing.T) {
	a := &Agent{}
	a.setTodoState([]evidence.TodoItem{
		{Content: "first", Status: "in_progress"},
		{Content: "second", Status: "completed"},
	})
	if a.sess.todoState[0].Status != "in_progress" || a.sess.todoState[1].Status != "completed" {
		t.Fatalf("legacy snapshot was not normalized: %+v", a.sess.todoState)
	}
}

func TestRebuildTodoStateDoesNotReplayRetiredCompleteSteps(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{
			ID: "t1", Name: "todo_write",
			Arguments: `{"todos":[{"content":"a","status":"in_progress"},{"content":"b","status":"pending"}]}`,
		}}},
		{Role: provider.RoleTool, ToolCallID: "t1", Name: "todo_write", Content: "Todos updated"},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{
			ID: "c1", Name: "complete_step", Arguments: `{"step":"a"}`,
		}}},
		{Role: provider.RoleTool, ToolCallID: "c1", Name: "complete_step", Content: "signed off"},
	}
	a := &Agent{}
	a.rebuildTodoState(msgs)

	if len(a.sess.todoState) != 2 {
		t.Fatalf("rebuilt %d todos, want 2", len(a.sess.todoState))
	}
	if a.sess.todoState[0].Status != "in_progress" {
		t.Fatalf("retired complete_step changed canonical state: %+v", a.sess.todoState[0])
	}
	if a.sess.todoState[1].Status != "pending" {
		t.Fatalf("retired complete_step promoted another item: %+v", a.sess.todoState[1])
	}
}

func TestRebuildTodoStateSkipsFailedCompleteStep(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{
			ID: "t1", Name: "todo_write",
			Arguments: `{"todos":[{"content":"a","status":"in_progress"}]}`,
		}}},
		{Role: provider.RoleTool, ToolCallID: "t1", Name: "todo_write", Content: "Todos updated"},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{
			ID: "c1", Name: "complete_step", Arguments: `{"step":"a"}`,
		}}},
		{Role: provider.RoleTool, ToolCallID: "c1", Name: "complete_step", Content: "error: no evidence"},
	}
	a := &Agent{}
	a.rebuildTodoState(msgs)

	if a.sess.todoState[0].Status == "completed" {
		t.Fatalf("a failed complete_step must not advance canonical state: %+v", a.sess.todoState[0])
	}
}

func TestRebuildTodoStateRequiresToolResults(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{
			ID: "t1", Name: "todo_write",
			Arguments: `{"todos":[{"content":"a","status":"in_progress"}]}`,
		}}},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{
			ID: "c1", Name: "complete_step", Arguments: `{"step":"a"}`,
		}}},
	}
	a := &Agent{}
	a.rebuildTodoState(msgs)
	if len(a.sess.todoState) != 0 {
		t.Fatalf("todo_write without tool result rebuilt canonical state: %+v", a.sess.todoState)
	}

	msgs = append(msgs[:1],
		provider.Message{Role: provider.RoleTool, ToolCallID: "t1", Name: "todo_write", Content: "Todos updated"},
		msgs[1],
	)
	a.rebuildTodoState(msgs)
	if len(a.sess.todoState) != 1 {
		t.Fatalf("successful todo_write did not rebuild canonical state: %+v", a.sess.todoState)
	}
	if got := a.sess.todoState[0].Status; got != "in_progress" {
		t.Fatalf("complete_step without tool result changed status to %q", got)
	}
}

func TestRebuildTodoStateHonorsEmptyTodoWriteClear(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{
			ID:        "t1",
			Name:      "todo_write",
			Arguments: `{"todos":[{"content":"a","status":"in_progress"}]}`,
		}}},
		{Role: provider.RoleTool, ToolCallID: "t1", Name: "todo_write", Content: "Todos updated"},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{
			ID:        "t2",
			Name:      "todo_write",
			Arguments: `{"todos":[]}`,
		}}},
		{Role: provider.RoleTool, ToolCallID: "t2", Name: "todo_write", Content: "Todos updated"},
	}
	a := &Agent{}
	a.rebuildTodoState(msgs)
	if len(a.sess.todoState) != 0 {
		t.Fatalf("empty todo_write should clear rebuilt canonical state: %+v", a.sess.todoState)
	}
}

func TestSeedTodoState(t *testing.T) {
	a := &Agent{svc: agentServices{sink: event.Discard}}
	todos := []evidence.TodoItem{
		{Content: "step 1", Status: "in_progress"},
		{Content: "step 2", Status: "pending"},
	}
	a.SeedTodoState(todos)
	if len(a.sess.todoState) != 2 {
		t.Fatalf("SeedTodoState: got %d items, want 2", len(a.sess.todoState))
	}
	if a.sess.todoState[0].Status != "in_progress" {
		t.Fatalf("SeedTodoState: first item status = %q, want in_progress", a.sess.todoState[0].Status)
	}
}

func TestSeedTodoStateReplacesExisting(t *testing.T) {
	a := &Agent{svc: agentServices{sink: event.Discard}, sess: sessionRuntime{todoState: []evidence.TodoItem{
		{Content: "existing", Status: "in_progress"},
	}}}
	a.SeedTodoState([]evidence.TodoItem{
		{Content: "new", Status: "in_progress"},
	})
	if len(a.sess.todoState) != 1 || a.sess.todoState[0].Content != "new" {
		t.Fatalf("SeedTodoState did not replace existing state: %+v", a.sess.todoState)
	}
}
