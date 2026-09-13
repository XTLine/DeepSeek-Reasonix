package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"reasonix/internal/tool"
)

func TestGoalTerminalReportYieldsToPendingUserAndCancellation(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "queued user", true: "cancelled context"}[cancel], func(t *testing.T) {
			c, _, _ := goalRuntimeController(t, &scriptedTurns{}, nil)
			c.SetGoal("preserve user control")
			epoch := c.goals.continuationToken()
			rec := c.goals.newTurnRecorder(c.goals.scopeID, epoch)
			rec.addUsageWithRequests(17, 1)
			if _, err := rec.RecordGoalReport(tool.GoalReport{Status: "complete", Reason: "model declaration"}); err != nil {
				t.Fatal(err)
			}
			c.goalUsageTee.setActiveRecorder(rec)
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			if cancel {
				stop()
			} else {
				c.parkedTurns = append(c.parkedTurns, func(context.Context) error { return nil })
			}
			err := newTurnOrchestrator(c).continueGoal(ctx, epoch, nil)
			if cancel && !errors.Is(err, context.Canceled) || !cancel && err != nil {
				t.Fatalf("continuation error = %v", err)
			}
			if c.Goal() == "" || c.GoalStatus() == GoalStatusComplete {
				t.Fatal("old completion report overrode user control")
			}
			if rt := c.GoalRuntime(); rt.TokensUsed != 17 || rt.RequestsUsed != 1 {
				t.Fatalf("real usage lost or counted twice: %+v", rt)
			}
			if c.goalUsageTee.activeRecorder() != nil {
				t.Fatal("terminal boundary retained its recorder")
			}
		})
	}
}

func TestLoadInactiveLegacyGoalDoesNotWriteOrActivate(t *testing.T) {
	c, _, _ := goalRuntimeController(t, &scriptedTurns{}, nil)
	path := filepath.Join(t.TempDir(), "goal.json")
	c.goals.statePath = path
	c.LoadInactiveGoal("legacy metadata objective")
	if c.Goal() != "legacy metadata objective" || c.goals.active() {
		t.Fatal("legacy objective was dropped or activated")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("read-only load wrote a sidecar: %v", err)
	}
	scope := c.goals.scopeID
	c.SetGoal("legacy metadata objective")
	if !c.goals.active() || c.goals.scopeID != scope {
		t.Fatal("explicit start did not preserve and activate restored objective")
	}
}
