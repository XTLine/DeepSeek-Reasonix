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

// restoreSessionPosture applies the posture the bound session recorded on
// disk. A missing or corrupt sidecar leaves the running posture in place and
// re-baselines the disk record to it, so a session that never persisted a
// posture keeps the controller's values without writing. Plan and floor take
// their full application paths; the approval mode only re-applies when it
// changed, because a same-value replay would drain pending approvals.
func (c *Controller) restoreSessionPosture(sessionPath string, fresh bool) {
	path := strings.TrimSpace(sessionPath)
	if path == "" {
		return
	}
	var disk sessionPosture
	var ok bool
	if !fresh {
		disk, ok = postureFromDisk(path)
	}
	modeNow := c.ToolApprovalMode()
	c.mu.Lock()
	if ok {
		c.sessionSettings.postureDisk = disk
	} else {
		c.sessionSettings.postureDisk = sessionPosture{
			toolApprovalMode: modeNow,
			plan:             c.sessionSettings.planMode,
			qualityFloor:     c.sessionSettings.qualityFloor,
		}
	}
	disk = c.sessionSettings.postureDisk
	c.sessionSettings.postureReady = true
	plan := disk.plan
	floor := disk.qualityFloor
	c.mu.Unlock()
	c.SetPlanMode(plan)
	if disk.toolApprovalMode != modeNow {
		c.ApplyToolApprovalMode(disk.toolApprovalMode)
	}
	c.mu.Lock()
	if floor != c.sessionSettings.qualityFloor {
		c.sessionSettings.qualityFloor = floor
	}
	c.mu.Unlock()
}

// persistSessionPosture writes the bound session's posture sidecar whenever
// the running posture diverges from the last disk baseline. Best-effort: a
// write failure only warns, matching the desktop tab-store semantics; the
// baseline advances only after a successful write so the next change retries.
func (c *Controller) persistSessionPosture() {
	mode := c.ToolApprovalMode()
	c.mu.Lock()
	ready := c.sessionSettings.postureReady
	path := c.sessionPath
	disk := c.sessionSettings.postureDisk
	plan := c.sessionSettings.planMode
	floor := c.sessionSettings.qualityFloor
	c.mu.Unlock()
	if !ready || strings.TrimSpace(path) == "" {
		return
	}
	current := (sessionPosture{
		toolApprovalMode: mode,
		plan:             plan,
		qualityFloor:     floor,
	}).normalized()
	if current == disk {
		return
	}
	if err := writePostureState(path, current); err != nil {
		slog.Warn("controller: write posture state", "err", err)
		return
	}
	c.mu.Lock()
	c.sessionSettings.postureDisk = current
	c.mu.Unlock()
}
