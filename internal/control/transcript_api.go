package control

import (
	"context"
	"encoding/json"
	"errors"

	"reasonix/internal/provider"
	"reasonix/internal/session"
	"reasonix/internal/transcript"
	"reasonix/internal/turnevent"
)

var ErrTranscriptProjectionUnavailable = errors.New("transcript projection is unavailable")

type TranscriptReplayRequest struct {
	Identity transcript.Identity `json:"identity"`
	After    uint64              `json:"after"`
}

type TranscriptReplay struct {
	transcript.Boundary
	turnevent.ReplayView
}

type TranscriptProjectionAPI interface {
	TranscriptSnapshot(transcript.PageRequest) (transcript.Snapshot, error)
	TranscriptContent(transcript.ContentRequest) (transcript.ContentChunk, error)
	TranscriptReplay(TranscriptReplayRequest) (TranscriptReplay, error)
}

var _ TranscriptProjectionAPI = (*Controller)(nil)

// SetTurnSubmissionID is called under the transport's admission boundary.
func (c *Controller) SetTurnSubmissionID(submissionID string) {
	if ledger := c.turnEventLedger(); ledger != nil {
		ledger.SetSubmissionID(submissionID)
	}
}

// BindTranscriptRuntimeEpoch runs at the surface's idle runtime publication
// boundary. It takes only display/ledger leaf locks and invokes no callbacks.
func (c *Controller) BindTranscriptRuntimeEpoch(epoch string) {
	c.turnEvents.commitMu.Lock()
	defer c.turnEvents.commitMu.Unlock()
	ledger := c.turnEventLedger()
	if ledger == nil || ledger.ActiveTurnID() != "" {
		return
	}
	ledger.SetRuntimeEpoch(epoch)
	c.turnEvents.mu.RLock()
	p := c.turnEvents.projection
	c.turnEvents.mu.RUnlock()
	if p != nil {
		p.SetRuntimeEpoch(epoch)
	}
}

func (c *Controller) transcriptProjection() (*transcript.Projection, error) {
	if service, runtime, exclusive := c.v3Binding(); exclusive && service != nil && runtime != nil {
		messages, err := service.Query().History(context.Background(), runtime.Ref())
		if err != nil {
			return nil, errors.Join(ErrTranscriptProjectionUnavailable, err)
		}
		return c.v3TranscriptProjection(runtime.StateSnapshot(), messages)
	}
	c.turnEvents.mu.RLock()
	defer c.turnEvents.mu.RUnlock()
	if c.turnEvents.err != nil {
		return nil, errors.Join(ErrTranscriptProjectionUnavailable, c.turnEvents.err)
	}
	if c.turnEvents.projectionErr != nil {
		return nil, errors.Join(ErrTranscriptProjectionUnavailable, c.turnEvents.projectionErr)
	}
	if c.turnEvents.projection == nil {
		return nil, ErrTranscriptProjectionUnavailable
	}
	return c.turnEvents.projection, nil
}

func (c *Controller) v3TranscriptProjection(runtime session.RuntimeSnapshot, messages []provider.Message) (*transcript.Projection, error) {
	sequence := runtime.Session.EventSequence
	sessionID, epoch := runtime.Ref.SessionID, runtime.Epoch
	c.turnEvents.mu.RLock()
	if p := c.turnEvents.projection; p != nil && c.turnEvents.v3ProjectionSequence == sequence && c.turnEvents.v3ProjectionSession == sessionID && c.turnEvents.v3ProjectionEpoch == epoch {
		c.turnEvents.mu.RUnlock()
		return p, nil
	}
	c.turnEvents.mu.RUnlock()

	rows := transcript.History(messages, transcript.HistoryOptions{
		CheckpointTurns: c.CheckpointTurnsByMessageIndex(),
		SubmitContent: func(m provider.Message) string {
			return StripReferencedContextPrefix(StripComposePrefixes(m.Content))
		},
		UserContent: func(m provider.Message) string {
			if m.RawContent != "" {
				return m.RawContent
			}
			return StripReferencedContextPrefix(StripComposePrefixes(m.Content))
		},
	})
	identity := transcript.Identity{SessionID: sessionID, RuntimeEpoch: epoch}
	p, err := transcript.NewProjection(identity, rows, sequence)
	if err != nil {
		return nil, err
	}
	c.turnEvents.mu.Lock()
	// Do not replace a newer cache produced while conversion ran.
	if current := c.turnEvents.projection; current != nil && c.turnEvents.v3ProjectionSession == sessionID && c.turnEvents.v3ProjectionEpoch == epoch && c.turnEvents.v3ProjectionSequence > sequence {
		p = current
	} else {
		c.turnEvents.projection = p
		c.turnEvents.projectionErr = nil
		c.turnEvents.v3ProjectionSequence = sequence
		c.turnEvents.v3ProjectionSession = sessionID
		c.turnEvents.v3ProjectionEpoch = epoch
	}
	c.turnEvents.mu.Unlock()
	return p, nil
}

func (c *Controller) TranscriptSnapshot(req transcript.PageRequest) (transcript.Snapshot, error) {
	p, err := c.transcriptProjection()
	if err != nil {
		return transcript.Snapshot{}, err
	}
	return p.Snapshot(req)
}

func (c *Controller) TranscriptContent(req transcript.ContentRequest) (transcript.ContentChunk, error) {
	p, err := c.transcriptProjection()
	if err != nil {
		return transcript.ContentChunk{}, err
	}
	return p.Content(req)
}

func (c *Controller) TranscriptReplay(req TranscriptReplayRequest) (TranscriptReplay, error) {
	replay, err := c.transcriptReplay(req)
	if err != nil {
		return replay, err
	}
	// The ledger's soft budget permits an oversized first event for progress.
	// Modern clients can obtain that data from the bounded snapshot/content
	// protocol instead. Never acknowledge a suffix the client cannot receive.
	encoded, err := json.Marshal(replay)
	if err != nil {
		return TranscriptReplay{}, err
	}
	if len(encoded)+1 > transcript.MaxResponseBytes {
		replay.Events = []turnevent.Envelope{}
		replay.ResetRequired, replay.HasMore = true, false
		replay.NextAfterSequence = req.After
	}
	return replay, nil
}

func (c *Controller) transcriptReplay(req TranscriptReplayRequest) (TranscriptReplay, error) {
	if _, runtime, exclusive := c.v3Binding(); exclusive && runtime != nil {
		p, err := c.transcriptProjection()
		if err != nil {
			return TranscriptReplay{}, err
		}
		boundary := p.Boundary()
		if boundary.Identity != req.Identity || req.After < boundary.CoveredThroughSeq {
			return TranscriptReplay{Boundary: boundary, ReplayView: turnevent.ReplayView{Events: []turnevent.Envelope{}, ResetRequired: true, LatestSequence: boundary.CoveredThroughSeq}}, nil
		}
		return TranscriptReplay{Boundary: boundary, ReplayView: turnevent.ReplayView{Events: []turnevent.Envelope{}, LatestSequence: boundary.CoveredThroughSeq, NextAfterSequence: boundary.CoveredThroughSeq}}, nil
	}
	c.turnEvents.commitMu.Lock()
	defer c.turnEvents.commitMu.Unlock()
	p, err := c.transcriptProjection()
	if err != nil {
		return TranscriptReplay{}, err
	}
	boundary := p.Boundary()
	if boundary.Identity != req.Identity {
		if boundary.Identity.SessionID != req.Identity.SessionID {
			return TranscriptReplay{}, errors.New("transcript replay session mismatch")
		}
		return TranscriptReplay{Boundary: boundary, ReplayView: turnevent.ReplayView{Events: []turnevent.Envelope{}, ResetRequired: true, LatestSequence: boundary.CoveredThroughSeq}}, nil
	}
	view, err := c.TurnEventReplay(req.After)
	return TranscriptReplay{Boundary: boundary, ReplayView: view}, err
}
