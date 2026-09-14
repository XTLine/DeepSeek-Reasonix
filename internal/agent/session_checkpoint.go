package agent

import "context"

import "reasonix/internal/provider"

// SessionCheckpointBoundary identifies a semantic durability barrier. A
// checkpoint is intentionally absent from ordinary message, todo, approval,
// and terminal appends; those use the session's write-behind policy.
type SessionCheckpointBoundary string

const (
	CheckpointBeforeModel   SessionCheckpointBoundary = "before_model"
	CheckpointBeforeTopTool SessionCheckpointBoundary = "before_top_level_tool"
)

// SessionCheckpointer makes the side-effect boundary explicit without coupling
// the agent loop to a concrete session backend.
type SessionCheckpointer interface {
	CheckpointSession(context.Context, SessionCheckpointBoundary) error
}

// SessionEventRecorder accepts exact, already-formed provider messages at the
// point the agent commits them. It avoids reconstructing the authoritative
// event log later by comparing mutable conversation snapshots.
type SessionEventRecorder interface {
	RecordSessionMessages(context.Context, string, []provider.Message) error
}

// SessionMessageMutationRecorder records an explicit mutation of one stable
// transcript message. Local recovery/authorization metadata often changes an
// existing message without changing provider-visible bytes; those mutations
// still need a typed event and must not be rediscovered later by diffing the
// mutable Session.Messages slice.
type SessionMessageMutationRecorder interface {
	RecordSessionMessageUpsert(context.Context, string, provider.Message) error
}

func (a *Agent) SetSessionCheckpointer(checkpointer SessionCheckpointer) {
	if a != nil {
		a.svc.sessionCheckpointer = checkpointer
	}
}

func (a *Agent) checkpointSession(ctx context.Context, boundary SessionCheckpointBoundary) error {
	if a == nil || a.svc.sessionCheckpointer == nil {
		return ctx.Err()
	}
	if err := a.svc.sessionCheckpointer.CheckpointSession(ctx, boundary); err != nil {
		return err
	}
	return ctx.Err()
}

func (a *Agent) appendCommittedMessages(ctx context.Context, reason string, messages ...provider.Message) error {
	if a == nil || len(messages) == 0 {
		return nil
	}
	for i := range messages {
		if messages[i].ID == "" {
			messages[i].ID = NewMessageID()
		}
	}
	if recorder, ok := a.svc.sessionCheckpointer.(SessionEventRecorder); ok {
		if err := recorder.RecordSessionMessages(ctx, reason, messages); err != nil {
			return err
		}
	}
	a.sess.conversation.AddBatch(messages...)
	return nil
}

// ModelHistorySnapshot returns the exact context projection that the next
// provider request would receive. Compaction recorders use it after an
// installed projection rather than deriving context from summary prose.
func (a *Agent) ModelHistorySnapshot() []provider.Message {
	if a == nil {
		return nil
	}
	return append([]provider.Message(nil), a.modelVisibleMessages()...)
}
