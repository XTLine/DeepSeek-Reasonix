package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"reasonix/internal/control"
	"reasonix/internal/session"
	"reasonix/internal/sessioncontent"
	"reasonix/internal/transcript"
)

type RemoteTranscriptSnapshot struct {
	Supported bool                 `json:"supported"`
	Snapshot  *transcript.Snapshot `json:"snapshot,omitempty"`
}

func (a *App) remoteTranscriptRead(tabID, route string, request any, destination any) (bool, error) {
	client, base, err := a.remoteTabCommandClient(tabID)
	if err != nil {
		return false, err
	}
	a.remoteTabMu.Lock()
	tab := a.remoteTabs[tabID]
	if tab == nil || tab.client != client {
		a.remoteTabMu.Unlock()
		return false, fmt.Errorf("remote transcript runtime changed")
	}
	gen, sessionPath := tab.gen, tab.routing.currentPath
	a.remoteTabMu.Unlock()
	encoded, err := json.Marshal(request)
	if err != nil {
		return false, err
	}
	query := url.Values{"request": []string{string(encoded)}}
	if sessionPath != "" {
		query.Set("session", sessionPath)
	}
	ctx, cancel := commandContext(a)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, serveURL(base, route)+"?"+query.Encode(), nil)
	if err != nil {
		return false, err
	}
	response, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	const maxResponseBytes = transcript.MaxResponseBytes
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return false, err
	}
	if len(body) > maxResponseBytes {
		return false, fmt.Errorf("remote transcript response exceeds limit")
	}
	a.remoteTabMu.Lock()
	current := a.remoteTabs[tabID]
	valid := current == tab && current.gen == gen && current.client == client && current.routing.currentPath == sessionPath
	a.remoteTabMu.Unlock()
	if !valid {
		return false, fmt.Errorf("remote transcript response belongs to a replaced session")
	}
	switch response.StatusCode {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return false, nil
	case http.StatusOK:
	default:
		return false, fmt.Errorf("remote transcript read failed (HTTP %d)", response.StatusCode)
	}
	// Old Serve builds may route an unknown GET to their HTML index. Only
	// explicit protocol data enables the new projection; versions are not guessed.
	var header struct {
		ProtocolVersion int  `json:"protocolVersion"`
		Stale           bool `json:"stale"`
	}
	if route != "/transcript/content" {
		if json.Unmarshal(body, &header) != nil || header.ProtocolVersion != transcript.ProtocolVersion {
			return false, nil
		}
	}
	if err := json.Unmarshal(body, destination); err != nil {
		return false, fmt.Errorf("invalid remote transcript response: %w", err)
	}
	return true, nil
}

func (a *App) RemoteTranscriptSnapshotForTab(tabID string, req transcript.PageRequest) (RemoteTranscriptSnapshot, error) {
	var snap transcript.Snapshot
	supported, err := a.remoteTranscriptRead(tabID, "/transcript/snapshot", req, &snap)
	if err != nil || !supported {
		return RemoteTranscriptSnapshot{Supported: false}, err
	}
	return RemoteTranscriptSnapshot{Supported: true, Snapshot: &snap}, nil
}

func (a *App) RemoteTranscriptPageForTab(tabID string, req transcript.PageRequest) (transcript.Snapshot, error) {
	var snap transcript.Snapshot
	supported, err := a.remoteTranscriptRead(tabID, "/transcript/page", req, &snap)
	if err == nil && !supported {
		err = control.ErrTranscriptProjectionUnavailable
	}
	return snap, err
}

func (a *App) RemoteTranscriptContentForTab(tabID string, req transcript.ContentRequest) (transcript.ContentChunk, error) {
	var chunk transcript.ContentChunk
	supported, err := a.remoteTranscriptRead(tabID, "/transcript/content", req, &chunk)
	if err == nil && !supported {
		err = control.ErrTranscriptProjectionUnavailable
	}
	return chunk, err
}

func (a *App) RemoteTranscriptReplayForTab(tabID string, req control.TranscriptReplayRequest) (control.TranscriptReplay, error) {
	var replay control.TranscriptReplay
	supported, err := a.remoteTranscriptRead(tabID, "/transcript/replay", req, &replay)
	if err == nil && !supported {
		err = control.ErrTranscriptProjectionUnavailable
	}
	return replay, err
}

// remoteSessionHistoryRead uses the capability negotiated during the
// authenticated Serve handshake. Unlike the compatibility transcript API,
// canonical history is session-ID based and does not accept arbitrary paths.
func (a *App) remoteSessionHistoryRead(tabID, route string, query url.Values, destination any, maxResponseBytes int64) (bool, error) {
	client, base, err := a.remoteTabCommandClient(tabID)
	if err != nil {
		return false, err
	}
	a.remoteTabMu.Lock()
	tab := a.remoteTabs[tabID]
	if tab == nil || tab.client != client {
		a.remoteTabMu.Unlock()
		return false, fmt.Errorf("remote session history runtime changed")
	}
	if !tab.capabilities[serveCapabilitySessionContentV1] {
		a.remoteTabMu.Unlock()
		return false, nil
	}
	gen, sessionID, sessionPath := tab.gen, tab.session.sessionID, tab.routing.currentPath
	a.remoteTabMu.Unlock()
	if query == nil {
		query = make(url.Values)
	}
	if sessionID != "" {
		query.Set("sessionId", sessionID)
	}
	ctx, cancel := commandContext(a)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, serveURL(base, route)+"?"+query.Encode(), nil)
	if err != nil {
		return false, err
	}
	response, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return false, err
	}
	if int64(len(body)) > maxResponseBytes {
		return false, fmt.Errorf("remote session history response exceeds limit")
	}
	a.remoteTabMu.Lock()
	current := a.remoteTabs[tabID]
	valid := current == tab && current.gen == gen && current.client == client &&
		current.session.sessionID == sessionID && current.routing.currentPath == sessionPath
	a.remoteTabMu.Unlock()
	if !valid {
		return false, fmt.Errorf("remote session history response belongs to a replaced session")
	}
	switch response.StatusCode {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return false, nil
	case http.StatusOK:
	default:
		return false, fmt.Errorf("remote session history read failed (HTTP %d): %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, destination); err != nil {
		return false, fmt.Errorf("invalid remote session history response: %w", err)
	}
	return true, nil
}

// RemoteSessionHistoryPageForTab reads one fixed-snapshot canonical history
// page. The Serve enforces the 500-message and 2 MiB page budgets.
func (a *App) RemoteSessionHistoryPageForTab(tabID, cursor string, limit int) (session.MessageHistoryPage, error) {
	query := make(url.Values)
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	if limit > 0 {
		query.Set("limit", fmt.Sprint(limit))
	}
	var page session.MessageHistoryPage
	supported, err := a.remoteSessionHistoryRead(tabID, "/session-history/page", query, &page, session.HistoryPageMaxBytes+(64<<10))
	if err == nil && !supported {
		err = control.ErrTranscriptProjectionUnavailable
	}
	return page, err
}

// RemoteSessionHistoryContentForTab reads at most one MiB after Serve proves
// that the content reference belongs to the selected session.
func (a *App) RemoteSessionHistoryContentForTab(tabID string, ref sessioncontent.Ref, offset int64) (SessionHistoryContentChunk, error) {
	if offset < 0 || offset > ref.Bytes {
		return SessionHistoryContentChunk{}, fmt.Errorf("invalid session history content offset")
	}
	if offset == ref.Bytes {
		return SessionHistoryContentChunk{NextOffset: offset, Done: true}, nil
	}
	length := min(int64(sessionHistoryContentChunkBytes), ref.Bytes-offset)
	request, err := json.Marshal(map[string]any{"ref": ref, "offset": offset, "length": length})
	if err != nil {
		return SessionHistoryContentChunk{}, err
	}
	query := url.Values{"request": []string{string(request)}}
	var chunk SessionHistoryContentChunk
	supported, err := a.remoteSessionHistoryRead(tabID, "/session-history/content", query, &chunk, 2<<20)
	if err == nil && !supported {
		err = control.ErrTranscriptProjectionUnavailable
	}
	return chunk, err
}

func (a *App) RemoteSearchSessionHistoryForTab(tabID, textQuery, cursor string, limit int) (session.SearchHistoryPage, error) {
	query := url.Values{"q": []string{textQuery}}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	if limit > 0 {
		query.Set("limit", fmt.Sprint(limit))
	}
	var page session.SearchHistoryPage
	supported, err := a.remoteSessionHistoryRead(tabID, "/session-history/search", query, &page, session.HistoryPageMaxBytes+(64<<10))
	if err == nil && !supported {
		err = control.ErrTranscriptProjectionUnavailable
	}
	return page, err
}
