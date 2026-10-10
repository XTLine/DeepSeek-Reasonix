package responses

import (
	"context"
	"encoding/json"

	"reasonix/internal/contract/provider"
)

func terminalOutputError(output []json.RawMessage) error {
	for _, raw := range output {
		var envelope struct {
			Type      string          `json:"type"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(raw, &envelope) != nil || envelope.Type != "function_call" {
			continue
		}
		var item sseItem
		if len(envelope.Arguments) == 0 || envelope.Arguments[0] != '"' || json.Unmarshal(raw, &item) != nil || item.CallID == "" || item.Name == "" || (item.Status != "" && item.Status != "completed") {
			return provider.ErrInvalidFunctionCall
		}
	}
	return nil
}

func (t *turn) pendingCallError() error {
	closed := make(map[string]bool, len(t.calls))
	for _, call := range t.calls {
		if call.completed && call.id != "" {
			closed[call.id] = true
		}
	}
	for _, call := range t.calls {
		// Anonymous deltas may belong to a call recovered under a terminal item ID;
		// they do not establish another call obligation once a named call closes.
		if !call.completed && !closed[call.id] && (call.announced || call.name != "" || ((call.argChars > 0 || call.arguments != "") && len(closed) == 0)) {
			return provider.ErrUnfinishedFunctionCall
		}
	}
	return nil
}

func (t *turn) refuseCalls(ctx context.Context, err error) bool {
	t.terminal, t.failed = true, true
	t.responseID = ""
	t.c.ResetContext()
	_ = t.send(ctx, provider.Chunk{Type: provider.ChunkError, Err: err})
	return false
}
