package serve

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"reasonix/internal/agent"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/session"
)

func newExclusiveSessionServe(t *testing.T) (*Server, *control.Controller, *session.Service, session.SessionRef) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "sessions-v4")
	service, err := session.NewService("serve-test", session.NewFilesystemPersistence(root))
	if err != nil {
		t.Fatal(err)
	}
	exec := agent.New(nil, nil, agent.NewSession("system"), agent.Options{}, event.Discard)
	ctrl := control.New(control.Options{
		Executor: exec, SessionDir: t.TempDir(), SessionService: service, ExclusiveSession: true,
	})
	ref, err := ctrl.BindFreshSession(t.Context(), "current")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ctrl.Close)
	return New(ctrl, NewBroadcaster(), config.ServeConfig{}), ctrl, service, ref
}

func TestExclusiveV3SessionsAndResumeUseImmutableIdentity(t *testing.T) {
	srv, ctrl, service, current := newExclusiveSessionServe(t)
	target, err := service.Create(t.Context(), session.CreateOptions{SessionID: "target"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.Session().Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := service.Close(t.Context(), target.Ref()); err != nil {
		t.Fatal(err)
	}

	list := httptest.NewRecorder()
	srv.sessions(list, httptest.NewRequest(http.MethodGet, "/sessions", nil))
	var rows []sessionListEntry
	if err := json.Unmarshal(list.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	foundCurrent, foundTarget := false, false
	for _, row := range rows {
		if row.SessionID == current.SessionID && row.HostID == current.HostID && row.Current && row.Path == "" {
			foundCurrent = true
		}
		if row.SessionID == "target" && row.HostID == current.HostID && row.Path == "" {
			foundTarget = true
		}
	}
	if !foundCurrent || !foundTarget {
		t.Fatalf("v3 rows = %+v", rows)
	}

	resume := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/resume", strings.NewReader(`{"hostId":"serve-test","sessionId":"target"}`))
	srv.resume(resume, req)
	if resume.Code != http.StatusNoContent {
		t.Fatalf("resume status = %d: %s", resume.Code, resume.Body.String())
	}
	if got := resume.Header().Get(sessionIDHeader); got != "target" {
		t.Fatalf("resume session id = %q", got)
	}
	if got, ok := ctrl.SessionRef(); !ok || got.SessionID != "target" || ctrl.SessionPath() != "" {
		t.Fatalf("controller identity = %+v, bound=%v path=%q", got, ok, ctrl.SessionPath())
	}
}

func TestExclusiveV3MissingResumeDoesNotCreateOrReplaceCurrent(t *testing.T) {
	srv, ctrl, service, current := newExclusiveSessionServe(t)
	resume := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/resume", strings.NewReader(`{"hostId":"serve-test","sessionId":"missing"}`))
	srv.resume(resume, req)
	if resume.Code != http.StatusConflict {
		t.Fatalf("resume status = %d, want 409", resume.Code)
	}
	if got, ok := ctrl.SessionRef(); !ok || got != current {
		t.Fatalf("current identity changed to %+v, bound=%v", got, ok)
	}
	if _, err := service.Query().Snapshot(t.Context(), session.SessionRef{HostID: "serve-test", SessionID: "missing"}); err == nil {
		t.Fatal("missing Open created a session")
	}
}

func TestExclusiveV3RotationAndMutationFenceReturnSessionID(t *testing.T) {
	srv, ctrl, _, current := newExclusiveSessionServe(t)
	stale := httptest.NewRequest(http.MethodPost, "/cancel", nil)
	stale.Header.Set(expectedSessionIDHeader, "stale")
	if err := srv.expectedSessionErrorLocked(stale); err == nil {
		t.Fatal("stale immutable identity passed mutation fence")
	}
	matching := httptest.NewRequest(http.MethodPost, "/cancel", nil)
	matching.Header.Set(expectedSessionIDHeader, current.SessionID)
	if err := srv.expectedSessionErrorLocked(matching); err != nil {
		t.Fatalf("matching identity rejected: %v", err)
	}

	rotate := httptest.NewRecorder()
	srv.newSession(rotate, httptest.NewRequest(http.MethodPost, "/new", nil))
	if rotate.Code != http.StatusNoContent {
		t.Fatalf("new status = %d: %s", rotate.Code, rotate.Body.String())
	}
	ref, ok := ctrl.SessionRef()
	if !ok || ref.SessionID == current.SessionID || ref.SessionID == "" {
		t.Fatalf("rotated identity = %+v, bound=%v", ref, ok)
	}
	if got := rotate.Header().Get(sessionIDHeader); got != ref.SessionID {
		t.Fatalf("new response session id = %q, want %q", got, ref.SessionID)
	}
	if got := rotate.Header().Get(sessionPathHeader); got != "" {
		t.Fatalf("exclusive rotation exposed legacy path %q", got)
	}
}
