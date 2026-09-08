package control

import (
	"os"
	"path/filepath"
	"testing"

	"reasonix/internal/store"
)

func sessionPathForTest(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "session.jsonl")
}

func TestPostureStateRoundTrip(t *testing.T) {
	path := sessionPathForTest(t)
	want := sessionPosture{toolApprovalMode: ToolApprovalYolo, plan: true, qualityFloor: QualityFloorDelivery}
	if err := writePostureState(path, want); err != nil {
		t.Fatalf("writePostureState: %v", err)
	}
	got, ok := postureFromDisk(path)
	if !ok {
		t.Fatal("postureFromDisk reported missing sidecar after write")
	}
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

func TestPostureFromDiskMissing(t *testing.T) {
	got, ok := postureFromDisk(sessionPathForTest(t))
	if ok {
		t.Fatal("postureFromDisk reported a sidecar that does not exist")
	}
	if want := defaultSessionPosture(); got != want {
		t.Fatalf("missing sidecar posture = %+v, want %+v", got, want)
	}
}

func TestPostureFromDiskCorrupt(t *testing.T) {
	path := sessionPathForTest(t)
	if err := os.WriteFile(store.SessionPostureState(path), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write corrupt sidecar: %v", err)
	}
	got, ok := postureFromDisk(path)
	if ok {
		t.Fatal("postureFromDisk accepted a corrupt sidecar")
	}
	if got != defaultSessionPosture() {
		t.Fatalf("corrupt sidecar posture = %+v, want boot default", got)
	}
}

func TestPostureFromDiskNormalizesFields(t *testing.T) {
	path := sessionPathForTest(t)
	// Write through the file shape directly with legacy spellings: normalize
	// must canonicalize "bypass" and drop unknown floors.
	raw := []byte(`{"version":1,"toolApprovalMode":"bypass","planMode":true,"qualityFloor":"ultra"}`)
	if err := os.WriteFile(store.SessionPostureState(path), raw, 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	got, ok := postureFromDisk(path)
	if !ok {
		t.Fatal("postureFromDisk reported missing sidecar")
	}
	want := sessionPosture{toolApprovalMode: ToolApprovalYolo, plan: true}
	if got != want {
		t.Fatalf("normalized posture = %+v, want %+v", got, want)
	}
}

func TestPostureFromDiskEmptyFields(t *testing.T) {
	path := sessionPathForTest(t)
	if err := os.WriteFile(store.SessionPostureState(path), []byte(`{"version":1}`), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	got, ok := postureFromDisk(path)
	if !ok {
		t.Fatal("postureFromDisk reported missing sidecar")
	}
	if got != defaultSessionPosture() {
		t.Fatalf("empty-field posture = %+v, want boot default", got)
	}
}

func TestPostureStateEmptyPath(t *testing.T) {
	if err := writePostureState("", sessionPosture{toolApprovalMode: ToolApprovalYolo}); err != nil {
		t.Fatalf("writePostureState with empty path: %v", err)
	}
	got, ok := postureFromDisk("")
	if ok {
		t.Fatal("postureFromDisk reported a sidecar for an empty path")
	}
	if got != defaultSessionPosture() {
		t.Fatalf("empty-path posture = %+v, want boot default", got)
	}
}
