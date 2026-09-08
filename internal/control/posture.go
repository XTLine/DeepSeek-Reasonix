package control

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"reasonix/internal/fileutil"
	fileencoding "reasonix/internal/fileutil/encoding"
	"reasonix/internal/store"
)

// The composer posture — tool approval mode, plan flag, and quality floor — is
// per-session state, not controller state: a session switch must restore the
// target session's posture instead of inheriting the replaced controller's.
// Goal state keeps its own sidecar (store.SessionGoalState).
const postureStateVersion = 1

// sessionPosture is one session's persisted composer axes. The zero value is
// the boot default: ask, no plan, no explicit quality floor.
type sessionPosture struct {
	toolApprovalMode string // ask | auto | dontask | yolo
	plan             bool
	qualityFloor     string // "" | standard | delivery
}

func defaultSessionPosture() sessionPosture {
	return sessionPosture{toolApprovalMode: ToolApprovalAsk}
}

func (p sessionPosture) normalized() sessionPosture {
	p.toolApprovalMode = normalizeToolApprovalMode(p.toolApprovalMode)
	p.qualityFloor = strings.TrimSpace(p.qualityFloor)
	switch p.qualityFloor {
	case "", QualityFloorStandard, QualityFloorDelivery:
	default:
		// Unknown floors predate the current set; treating them as unset is
		// safe for every controller that rejects unknown floor values.
		p.qualityFloor = ""
	}
	return p
}

// postureStateFile is the on-disk shape of the posture sidecar.
type postureStateFile struct {
	Version          int    `json:"version"`
	ToolApprovalMode string `json:"toolApprovalMode,omitempty"`
	PlanMode         bool   `json:"planMode,omitempty"`
	QualityFloor     string `json:"qualityFloor,omitempty"`
}

// postureFromDisk loads and normalizes the posture sidecar of sessionPath. ok
// is false — and the boot default is returned — when the sidecar is missing,
// unreadable, or corrupt, so the caller keeps the posture the controller
// already carries instead of replacing it with defaults.
func postureFromDisk(sessionPath string) (sessionPosture, bool) {
	path := store.SessionPostureState(sessionPath)
	if path == "" {
		return defaultSessionPosture(), false
	}
	raw, err := fileencoding.ReadFileUTF8(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("controller: read posture state", "err", err)
		}
		return defaultSessionPosture(), false
	}
	var file postureStateFile
	if err := json.Unmarshal(raw, &file); err != nil {
		slog.Warn("controller: parse posture state", "err", err)
		return defaultSessionPosture(), false
	}
	return (sessionPosture{
		toolApprovalMode: file.ToolApprovalMode,
		plan:             file.PlanMode,
		qualityFloor:     file.QualityFloor,
	}).normalized(), true
}

// writePostureState persists the three composer axes beside the transcript.
func writePostureState(sessionPath string, p sessionPosture) error {
	path := store.SessionPostureState(sessionPath)
	if path == "" {
		return nil
	}
	p = p.normalized()
	data, err := json.Marshal(postureStateFile{
		Version:          postureStateVersion,
		ToolApprovalMode: p.toolApprovalMode,
		PlanMode:         p.plan,
		QualityFloor:     p.qualityFloor,
	})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return fileutil.AtomicWriteFile(path, data, 0o644)
}
