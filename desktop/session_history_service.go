package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"reasonix/internal/control"
	"reasonix/internal/session"
	"reasonix/internal/sessioncontent"
)

const sessionHistoryContentChunkBytes = 1 << 20

// SessionHistoryContentChunk is one bounded binary chunk from a canonical
// content reference. Data is base64 so the desktop JSON contract never
// converts arbitrary attachment bytes through UTF-8 strings.
type SessionHistoryContentChunk struct {
	Data       string `json:"data"`
	NextOffset int64  `json:"nextOffset"`
	Done       bool   `json:"done"`
}

// SessionHistoryPageForTab is the canonical v4 history endpoint. Unlike the
// compatibility HistorySlice API, it is identity based and obtains its fixed
// snapshot directly from the shared session read model.
func (a *App) SessionHistoryPageForTab(tabID, cursor string, limit int) (session.MessageHistoryPage, error) {
	query, ref, err := a.canonicalSessionQuery(tabID)
	if err != nil {
		return session.MessageHistoryPage{}, err
	}
	return query.HistoryPage(context.Background(), ref, cursor, limit)
}

func (a *App) SearchSessionHistoryForTab(tabID, textQuery, cursor string, limit int) (session.SearchHistoryPage, error) {
	query, ref, err := a.canonicalSessionQuery(tabID)
	if err != nil {
		return session.SearchHistoryPage{}, err
	}
	return query.SearchHistory(context.Background(), ref, textQuery, cursor, limit)
}

// SessionHistoryContentForTab reads the next bounded chunk only after Query
// proves that the reference belongs to this session's durable view.
func (a *App) SessionHistoryContentForTab(tabID string, ref sessioncontent.Ref, offset int64) (SessionHistoryContentChunk, error) {
	query, sessionRef, err := a.canonicalSessionQuery(tabID)
	if err != nil {
		return SessionHistoryContentChunk{}, err
	}
	if offset < 0 || offset > ref.Bytes {
		return SessionHistoryContentChunk{}, errors.New("invalid session history content offset")
	}
	if offset == ref.Bytes {
		return SessionHistoryContentChunk{NextOffset: offset, Done: true}, nil
	}
	length := min(int64(sessionHistoryContentChunkBytes), ref.Bytes-offset)
	data, err := query.ReadContent(context.Background(), sessionRef, ref, offset, length)
	if err != nil {
		return SessionHistoryContentChunk{}, err
	}
	next := offset + int64(len(data))
	return SessionHistoryContentChunk{Data: base64.StdEncoding.EncodeToString(data), NextOffset: next, Done: next == ref.Bytes}, nil
}

func (a *App) canonicalSessionQuery(tabID string) (*session.Query, session.SessionRef, error) {
	a.mu.RLock()
	tab := a.tabByIDLocked(tabID)
	var ctrl control.SessionAPI
	if tab != nil {
		ctrl = tab.Ctrl
	}
	a.mu.RUnlock()
	if ctrl == nil {
		return nil, session.SessionRef{}, fmt.Errorf("tab %q is not ready", tabID)
	}
	identity, ok := ctrl.(control.IdentityLifecycle)
	if !ok || !identity.UsesExclusiveSession() {
		return nil, session.SessionRef{}, errors.New("canonical session history is unavailable")
	}
	ref, bound := identity.SessionRef()
	service := identity.SessionService()
	if !bound || service == nil || service.Query() == nil {
		return nil, session.SessionRef{}, errors.New("canonical session identity is unavailable")
	}
	return service.Query(), ref, nil
}
