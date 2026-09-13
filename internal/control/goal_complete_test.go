package control

import (
	"reasonix/internal/evidence"
	"testing"
)

func TestGoalAcceptsModelCompletionWithUnfinishedTodos(t *testing.T) {
	todos := []evidence.TodoItem{{Content: "Check work", Status: "pending"}}
	g := &goalMachine{goal: "work", status: GoalStatusRunning}
	result := g.advance(goalAdvanceInput{report: &goalTurnReport{status: GoalStatusComplete}, todos: todos})
	if result.cont || g.status != GoalStatusComplete || todos[0].Status != "pending" {
		t.Fatalf("model declaration must preserve facts: result=%+v todos=%+v", result, todos)
	}
}

func TestGoalNoReportContinuesWithoutQualityDecision(t *testing.T) {
	g := &goalMachine{goal: "work", status: GoalStatusRunning}
	for range 4 {
		result := g.advance(goalAdvanceInput{})
		if !result.cont || result.intercept != "" || g.status != GoalStatusRunning {
			t.Fatalf("missing report should keep goal active: %+v", result)
		}
	}
}

func TestGoalExecutionPauseBeatsModelCompletion(t *testing.T) {
	g := &goalMachine{goal: "work", status: GoalStatusRunning}
	result := g.advance(goalAdvanceInput{
		report:     &goalTurnReport{status: GoalStatusComplete},
		pauseCause: stopCauseBudgetSpend, pauseReason: "budget exhausted",
	})
	if result.cont || g.status != GoalStatusBlocked || g.stopCause != stopCauseBudgetSpend {
		t.Fatalf("execution boundary must take precedence: %+v", result)
	}
}
