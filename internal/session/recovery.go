package session

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"reasonix/internal/event"
)

// RecoverInterrupted closes persisted runtime authority that cannot survive a
// process restart. It never reruns a tool or restores an approval. Callers must
// hold the exclusive write handle returned by Open.
//
// This is a Session operation because it derives a closure batch from the
// projection; the physical handle only writes the resulting commit.
func (s *Session) RecoverInterrupted(ctx context.Context) (Commit, bool, error) {
	if s == nil {
		return Commit{}, false, fmt.Errorf("session: nil session")
	}
	if s.readOnly {
		return Commit{}, false, ErrReadOnly
	}
	// Restart recovery needs only live authority, never historical message
	// bodies or the provider workset. Using the full compatibility Snapshot here
	// would replay the entire durable transcript on every cold open.
	snapshot := s.StateSnapshot()
	turnID := snapshot.Projection.TurnID
	if turnID == "" {
		return Commit{}, false, nil
	}
	events := make([]Event, 0, len(snapshot.Projection.ActiveTools)+len(snapshot.Projection.Interactions)+1)
	toolIDs := make([]string, 0, len(snapshot.Projection.ActiveTools))
	for id := range snapshot.Projection.ActiveTools {
		toolIDs = append(toolIDs, id)
	}
	sort.Strings(toolIDs)
	for _, id := range toolIDs {
		payload, _ := json.Marshal(map[string]any{"id": id, "name": snapshot.Projection.ActiveTools[id], "state": "result_unknown", "error": "previous runtime exited before recording a result"})
		events = append(events, Event{Kind: "tool/result", Payload: payload})
	}
	requestIDs := make([]string, 0, len(snapshot.Projection.Interactions))
	for id := range snapshot.Projection.Interactions {
		requestIDs = append(requestIDs, id)
	}
	sort.Strings(requestIDs)
	for _, id := range requestIDs {
		payload, _ := json.Marshal(map[string]any{"id": id, "state": "unavailable"})
		events = append(events, Event{Kind: "interaction/resolved", Payload: payload})
	}
	// turn/end uses a strict stable payload; diagnostic detail remains outside
	// the required event until the codec explicitly versions that field.
	terminal, _ := json.Marshal(map[string]any{"status": event.TurnInterrupted})
	events = append(events, Event{Kind: "turn/end", Payload: terminal})
	manifest := s.Manifest()
	operationID := fmt.Sprintf("restart-recovery:%d:%s", manifest.WriterGeneration, turnID)
	commit, err := s.Append(ctx, Batch{OperationID: operationID, TurnID: turnID, Events: events})
	if err != nil {
		return Commit{}, false, err
	}
	if _, err := s.Flush(ctx); err != nil {
		return Commit{}, false, err
	}
	return commit, true, nil
}
