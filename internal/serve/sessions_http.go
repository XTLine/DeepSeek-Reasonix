package serve

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"reasonix/internal/agent"
	"reasonix/internal/control"
	"reasonix/internal/session"
	"reasonix/internal/store"
)

type sessionListEntry struct {
	HostID     string `json:"hostId,omitempty"`
	SessionID  string `json:"sessionId,omitempty"`
	Name       string `json:"name"`
	Path       string `json:"path"`
	Title      string `json:"title,omitempty"`
	Turns      int    `json:"turns,omitempty"`
	Current    bool   `json:"current,omitempty"`
	Running    bool   `json:"running,omitempty"`
	TakenOver  bool   `json:"takenOver,omitempty"`
	MtimeMilli int64  `json:"mtimeMilli"`
}

// sessions lists saved sessions with event-log-aware titles and turn counts.
func (s *Server) sessions(w http.ResponseWriter, r *http.Request) {
	ctrl := s.ctl()
	dir := ctrl.SessionDir()
	if dir == "" {
		writeJSON(w, []any{})
		return
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		entries = nil
	} else if err != nil {
		writeJSON(w, []any{})
		return
	}
	current := agent.CanonicalSessionPath(ctrl.SessionPath())
	running := map[string]bool{}
	s.detachedMu.Lock()
	for path, detached := range s.detached {
		running[filepath.Clean(path)] = controllerHasActiveRuntimeWork(detached.ctrl)
	}
	s.detachedMu.Unlock()
	out := make([]sessionListEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !store.IsSessionTranscriptName(entry.Name()) {
			continue
		}
		path := agent.CanonicalSessionPath(filepath.Join(dir, entry.Name()))
		if agent.IsCleanupPending(path) {
			continue
		}
		mtime := agent.SessionContentModTime(path)
		cleanPath := agent.CanonicalSessionPath(path)
		row := sessionListEntry{
			Name:       strings.TrimSuffix(entry.Name(), ".jsonl"),
			Path:       path,
			Current:    cleanPath == current,
			Running:    running[cleanPath],
			TakenOver:  s.sessionMirrored(cleanPath) || leaseHeldByForeignRuntime(cleanPath),
			MtimeMilli: mtime.UnixMilli(),
		}
		if row.Current {
			row.Running = controllerHasActiveRuntimeWork(ctrl) && !row.TakenOver
		}
		first, turns, cached := agent.SessionPreviewCached(path)
		if !cached {
			first, turns = agent.SessionPreview(path)
		}
		if turns > 0 {
			row.Turns = turns
			row.Title = s.sessionTitle(r.Context(), entry.Name(), first, mtime.UnixNano())
		}
		out = append(out, row)
	}
	if concrete, ok := ctrl.(*control.Controller); ok {
		if service := concrete.SessionService(); service != nil {
			_, runtime, bound := concrete.SessionBinding()
			page, listErr := service.Query().List(r.Context(), "", 100)
			if listErr == nil {
				for _, info := range page.Sessions {
					row := sessionListEntry{
						HostID: info.Ref.HostID, SessionID: info.Ref.SessionID, Name: info.SessionID,
						Title: info.Title, Turns: info.Turns, MtimeMilli: info.CreatedAt.UnixMilli(),
						Current: bound && info.Ref == runtime.Ref(),
					}
					if live, exists := service.Runtime(info.Ref); exists {
						phase := live.Snapshot().Phase
						row.Running = phase == session.RuntimeRunning || phase == session.RuntimeCancelling || phase == session.RuntimeRecoveryRequired
					}
					out = append(out, row)
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].MtimeMilli > out[j].MtimeMilli })
	writeJSON(w, out)
}
