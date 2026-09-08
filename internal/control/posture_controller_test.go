package control

import (
	"testing"

	"reasonix/internal/agent"
	"reasonix/internal/event"
)

func newPostureTestController(t *testing.T) *Controller {
	t.Helper()
	session := agent.NewSession("sys")
	exec := agent.New(nil, nil, session, agent.Options{}, event.Discard)
	return New(Options{Executor: exec})
}

func bindPostureTestPath(t *testing.T, c *Controller, path string) {
	t.Helper()
	c.mu.Lock()
	c.sessionPath = path
	c.sessionSettings.postureReady = true
	c.sessionSettings.postureDisk = defaultSessionPosture()
	c.mu.Unlock()
}

func TestControllerPersistsExplicitPostureChanges(t *testing.T) {
	c := newPostureTestController(t)
	path := sessionPathForTest(t)
	bindPostureTestPath(t, c, path)

	c.SetToolApprovalMode(ToolApprovalYolo)
	got, ok := postureFromDisk(path)
	if !ok || got.toolApprovalMode != ToolApprovalYolo || got.plan || got.qualityFloor != "" {
		t.Fatalf("after yolo: sidecar = %+v ok=%v, want yolo/plan=false/floor=", got, ok)
	}

	c.SetQualityFloor(QualityFloorDelivery)
	got, _ = postureFromDisk(path)
	if got.qualityFloor != QualityFloorDelivery || got.toolApprovalMode != ToolApprovalYolo {
		t.Fatalf("after floor: sidecar = %+v, want floor delivery and mode yolo kept", got)
	}

	c.SetPlanMode(true)
	got, _ = postureFromDisk(path)
	if !got.plan || got.qualityFloor != QualityFloorDelivery || got.toolApprovalMode != ToolApprovalYolo {
		t.Fatalf("after plan: sidecar = %+v, want plan=true and other axes kept", got)
	}
}

func TestControllerRestoreSessionPostureAppliesDiskValues(t *testing.T) {
	path := sessionPathForTest(t)
	want := sessionPosture{toolApprovalMode: ToolApprovalYolo, plan: true, qualityFloor: QualityFloorDelivery}
	if err := writePostureState(path, want); err != nil {
		t.Fatalf("seed sidecar: %v", err)
	}
	c := newPostureTestController(t)
	c.restoreSessionPosture(path, false)

	if got := c.ToolApprovalMode(); got != want.toolApprovalMode {
		t.Fatalf("mode = %q, want %q", got, want.toolApprovalMode)
	}
	if got := c.PlanMode(); got != want.plan {
		t.Fatalf("plan = %v, want %v", got, want.plan)
	}
	if got := c.QualityFloor(); got != want.qualityFloor {
		t.Fatalf("floor = %q, want %q", got, want.qualityFloor)
	}
}

func TestControllerRestoreMissingSidecarKeepsPostureAndBaselines(t *testing.T) {
	c := newPostureTestController(t)
	path := sessionPathForTest(t)
	c.SetToolApprovalMode(ToolApprovalYolo)
	if _, ok := postureFromDisk(path); ok {
		t.Fatal("setter wrote a sidecar before a session path was bound")
	}
	c.restoreSessionPosture(path, false)
	if got := c.ToolApprovalMode(); got != ToolApprovalYolo {
		t.Fatalf("missing sidecar reset mode to %q, want running yolo kept", got)
	}
	if _, ok := postureFromDisk(path); ok {
		t.Fatal("restore wrote a sidecar for a session that never recorded one")
	}
	// Real restore points bind the session path before restoring; persist then
	// writes to that path.
	c.mu.Lock()
	c.sessionPath = path
	c.mu.Unlock()
	// The running posture became the baseline: a later explicit change writes
	// the full posture, including the kept yolo axis.
	c.SetQualityFloor(QualityFloorDelivery)
	got, ok := postureFromDisk(path)
	if !ok || got.qualityFloor != QualityFloorDelivery || got.toolApprovalMode != ToolApprovalYolo {
		t.Fatalf("after floor change: sidecar = %+v ok=%v, want delivery + kept yolo", got, ok)
	}
}

func TestControllerRestoreFreshPathSkipsDisk(t *testing.T) {
	path := sessionPathForTest(t)
	if err := writePostureState(path, sessionPosture{toolApprovalMode: ToolApprovalYolo, plan: true}); err != nil {
		t.Fatalf("seed sidecar: %v", err)
	}
	c := newPostureTestController(t)
	c.SetToolApprovalMode(ToolApprovalAuto)
	c.restoreSessionPosture(path, true)
	if got := c.ToolApprovalMode(); got != ToolApprovalAuto {
		t.Fatalf("fresh restore applied disk mode %q, want running auto kept", got)
	}
	if got := c.PlanMode(); got {
		t.Fatal("fresh restore applied disk plan")
	}
}

func TestControllerRestorePostureAfterExplicitChangeIsIdempotent(t *testing.T) {
	path := sessionPathForTest(t)
	if err := writePostureState(path, sessionPosture{toolApprovalMode: ToolApprovalYolo}); err != nil {
		t.Fatalf("seed sidecar: %v", err)
	}
	c := newPostureTestController(t)
	c.restoreSessionPosture(path, false)
	// A second restore of the same sidecar must not rotate anything: same mode
	// skips the approval re-apply, and the disk baseline already matches.
	c.restoreSessionPosture(path, false)
	if got := c.ToolApprovalMode(); got != ToolApprovalYolo {
		t.Fatalf("second restore mode = %q, want %q", got, ToolApprovalYolo)
	}
	got, ok := postureFromDisk(path)
	if !ok || got != (sessionPosture{toolApprovalMode: ToolApprovalYolo}) {
		t.Fatalf("sidecar after restores = %+v ok=%v, want unchanged yolo", got, ok)
	}
}
