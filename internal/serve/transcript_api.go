package serve

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"

	"reasonix/internal/agent"
	"reasonix/internal/control"
	"reasonix/internal/session"
	"reasonix/internal/sessioncontent"
	"reasonix/internal/transcript"
)

func (s *Server) registerTranscriptRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /transcript/snapshot", s.transcriptSnapshot)
	mux.HandleFunc("GET /transcript/page", s.transcriptSnapshot)
	mux.HandleFunc("GET /transcript/content", s.transcriptContent)
	mux.HandleFunc("GET /transcript/replay", s.transcriptReplay)
	mux.HandleFunc("GET /session-history/page", s.sessionHistoryPage)
	mux.HandleFunc("GET /session-history/search", s.sessionHistorySearch)
	mux.HandleFunc("GET /session-history/content", s.sessionHistoryContent)
}

type sessionHistoryContentRequest struct {
	Ref    sessioncontent.Ref `json:"ref"`
	Offset int64              `json:"offset"`
	Length int64              `json:"length"`
}

type sessionHistoryContentResponse struct {
	Data       string `json:"data"`
	NextOffset int64  `json:"nextOffset"`
	Done       bool   `json:"done"`
}

func (s *Server) canonicalSessionQuery(w http.ResponseWriter, r *http.Request) (*session.Query, session.SessionRef, bool) {
	identity, ok := s.ctl().(control.IdentityLifecycle)
	if !ok || !identity.UsesExclusiveSession() {
		http.Error(w, "canonical session history is unavailable", http.StatusNotImplemented)
		return nil, session.SessionRef{}, false
	}
	ref, bound := identity.SessionRef()
	service := identity.SessionService()
	if !bound || service == nil || service.Query() == nil {
		http.Error(w, "canonical session identity is unavailable", http.StatusConflict)
		return nil, session.SessionRef{}, false
	}
	if requested := r.URL.Query().Get("sessionId"); requested != "" && requested != ref.SessionID {
		http.Error(w, "session history is not bound to this runtime", http.StatusConflict)
		return nil, session.SessionRef{}, false
	}
	return service.Query(), ref, true
}

func (s *Server) sessionHistoryPage(w http.ResponseWriter, r *http.Request) {
	s.bindMu.Lock()
	defer s.bindMu.Unlock()
	query, ref, ok := s.canonicalSessionQuery(w, r)
	if !ok {
		return
	}
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if _, err := fmt.Sscan(raw, &limit); err != nil {
			http.Error(w, "invalid history limit", http.StatusBadRequest)
			return
		}
	}
	page, err := query.HistoryPage(r.Context(), ref, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(page)
}

func (s *Server) sessionHistoryContent(w http.ResponseWriter, r *http.Request) {
	var request sessionHistoryContentRequest
	if !transcriptRequest(w, r, &request) {
		return
	}
	if request.Length <= 0 || request.Length > 1<<20 {
		http.Error(w, "invalid content range", http.StatusBadRequest)
		return
	}
	s.bindMu.Lock()
	defer s.bindMu.Unlock()
	query, ref, ok := s.canonicalSessionQuery(w, r)
	if !ok {
		return
	}
	data, err := query.ReadContent(r.Context(), ref, request.Ref, request.Offset, request.Length)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	next := request.Offset + int64(len(data))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sessionHistoryContentResponse{Data: base64.StdEncoding.EncodeToString(data), NextOffset: next, Done: next == request.Ref.Bytes})
}

func (s *Server) sessionHistorySearch(w http.ResponseWriter, r *http.Request) {
	s.bindMu.Lock()
	defer s.bindMu.Unlock()
	query, ref, ok := s.canonicalSessionQuery(w, r)
	if !ok {
		return
	}
	textQuery := r.URL.Query().Get("q")
	if len(textQuery) > 4096 {
		http.Error(w, "history search query is too large", http.StatusBadRequest)
		return
	}
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if _, err := fmt.Sscan(raw, &limit); err != nil {
			http.Error(w, "invalid history search limit", http.StatusBadRequest)
			return
		}
	}
	page, err := query.SearchHistory(r.Context(), ref, textQuery, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(page)
}

// transcriptRead binds each read to the selected controller. A file mirror
// cannot claim a live event cursor and explicitly declines this protocol.
func (s *Server) transcriptRead(w http.ResponseWriter, r *http.Request, read func(control.TranscriptProjectionAPI) (any, error)) {
	s.bindMu.Lock()
	defer s.bindMu.Unlock()
	ctrl := s.ctl()
	path := agent.CanonicalSessionPath(ctrl.SessionPath())
	if raw := r.URL.Query().Get("session"); raw != "" {
		requested, err := s.resolveSessionPath(raw)
		if err != nil || agent.CanonicalSessionPath(requested) != path {
			http.Error(w, "transcript session is not bound to this runtime", http.StatusConflict)
			return
		}
	}
	api, ok := ctrl.(control.TranscriptProjectionAPI)
	if !ok || s.sessionMirrored(path) {
		http.Error(w, "transcript projection is unavailable", http.StatusNotImplemented)
		return
	}
	value, err := read(api)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func transcriptRequest(w http.ResponseWriter, r *http.Request, dst any) bool {
	encoded := r.URL.Query().Get("request")
	if encoded == "" {
		return true
	}
	if len(encoded) > 8192 || json.Unmarshal([]byte(encoded), dst) != nil {
		http.Error(w, "invalid transcript request", http.StatusBadRequest)
		return false
	}
	return true
}

func (s *Server) transcriptSnapshot(w http.ResponseWriter, r *http.Request) {
	var req transcript.PageRequest
	if !transcriptRequest(w, r, &req) {
		return
	}
	s.transcriptRead(w, r, func(api control.TranscriptProjectionAPI) (any, error) { return api.TranscriptSnapshot(req) })
}

func (s *Server) transcriptContent(w http.ResponseWriter, r *http.Request) {
	var req transcript.ContentRequest
	if !transcriptRequest(w, r, &req) {
		return
	}
	s.transcriptRead(w, r, func(api control.TranscriptProjectionAPI) (any, error) { return api.TranscriptContent(req) })
}

func (s *Server) transcriptReplay(w http.ResponseWriter, r *http.Request) {
	var req control.TranscriptReplayRequest
	if !transcriptRequest(w, r, &req) {
		return
	}
	s.transcriptRead(w, r, func(api control.TranscriptProjectionAPI) (any, error) { return api.TranscriptReplay(req) })
}
