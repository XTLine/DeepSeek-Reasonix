package serve

import (
	"errors"
	"net/http"
	"strings"

	"reasonix/internal/agent"
)

const (
	sessionPathHeader         = "X-Reasonix-Session-Path"
	expectedSessionPathHeader = "X-Reasonix-Expected-Session-Path"
)

var errExpectedSessionChanged = errors.New("active session changed; retry on the current session")

// expectedSessionErrorLocked fences a foreground mutation to the session the
// caller displayed when it issued the request. The header is optional for
// compatibility with browser clients and older Desktop builds. bindMu must be
// held so validation and controller use share one publication epoch.
func (s *Server) expectedSessionErrorLocked(r *http.Request) error {
	return s.expectedSessionPathErrorLocked(r.Header.Get(expectedSessionPathHeader))
}

func (s *Server) expectedSessionPathErrorLocked(rawExpected string) error {
	expected := agent.CanonicalSessionPath(strings.TrimSpace(rawExpected))
	if expected == "" {
		return nil
	}
	actual := agent.CanonicalSessionPath(strings.TrimSpace(s.ctl().SessionPath()))
	if actual != expected {
		return errExpectedSessionChanged
	}
	return nil
}

func (s *Server) validateExpectedSessionLocked(w http.ResponseWriter, r *http.Request) bool {
	if err := s.expectedSessionErrorLocked(r); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return false
	}
	return true
}

func (s *Server) foregroundMutation(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.bindMu.Lock()
		defer s.bindMu.Unlock()
		if !s.validateExpectedSessionLocked(w, r) {
			return
		}
		next(w, r)
	}
}
