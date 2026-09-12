package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"
	"reasonix/internal/agent"
	"reasonix/internal/control"
)

func TestTakeoverChoiceTracksRunningStateAndRejectsStaleQuery(t *testing.T) {
	m := chatTUI{}
	m.openTakeoverPrompt("first")
	old := m.takeoverPrompt
	m.openTakeoverPrompt("second")
	m.applyTakeoverQuery(cliTakeoverQueryMsg{request: old, view: cliOwnershipView{Running: true}})
	if !m.takeoverPrompt.querying {
		t.Fatal("stale query changed selection")
	}
	for _, running := range []bool{false, true} {
		m.applyTakeoverQuery(cliTakeoverQueryMsg{request: m.takeoverPrompt, view: cliOwnershipView{Running: running}})
		want := 3
		if running {
			want++
		}
		if len(m.takeoverPrompt.picker.items) != want {
			t.Fatal("incorrect takeover choices")
		}
	}
	next, cmd := m.handleTakeoverKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if next.(chatTUI).takeoverPrompt != nil || cmd != nil {
		t.Fatal("cancel initiated a handoff")
	}
}

func TestCLIServeTakeoverQueriesOwnershipAndReloadsAfterRelease(t *testing.T) {
	for _, mode := range []string{"wait", "interrupt"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session #.jsonl")
			saveTestSession(t, path, "before")
			holder := control.NewSessionLeaseKeeper()
			defer holder.Release()
			if err := holder.Rebind(path); err != nil {
				t.Fatal(err)
			}
			var requested string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/auth/token":
					w.WriteHeader(http.StatusNoContent)
				case "/ownership":
					if r.URL.Query().Get("session") != path {
						t.Error("opaque path lost")
					}
					_ = json.NewEncoder(w).Encode(cliOwnershipView{Holder: "serve", Running: true})
				case "/handoff":
					var body struct {
						Mode           string
						TargetWriterID string
					}
					_ = json.NewDecoder(r.Body).Decode(&body)
					requested = body.Mode
					if err := holder.ReleaseForHandoff(body.TargetWriterID, "forward"); err != nil {
						t.Error(err)
					}
					_ = json.NewEncoder(w).Encode(cliTakeoverGrant{SessionPath: path, MirrorID: "mirror", HandoffID: "forward", ReturnHandoffID: "return", SourceWriterID: agent.SessionWriterID(), TargetWriterID: body.TargetWriterID})
				default:
					w.WriteHeader(http.StatusNoContent)
				}
			}))
			defer srv.Close()
			previous := discoverCLIServesForTakeover
			discoverCLIServesForTakeover = func() []cliServeRecord { return []cliServeRecord{{pid: os.Getpid(), base: srv.URL, token: "test"}} }
			defer func() { discoverCLIServesForTakeover = previous }()
			if _, err := queryCLITakeover(context.Background(), path); err != nil {
				t.Fatal(err)
			}
			leases := control.NewSessionLeaseKeeper()
			defer leases.Release()
			err := leases.Rebind(path)
			if !errors.Is(err, agent.ErrSessionLeaseHeld) {
				t.Fatal("missing conflict")
			}
			binding, err := cliTakeoverHeldSessionMode(path, err, leases, nil, mode)
			if err != nil {
				t.Fatal(err)
			}
			if requested != mode {
				t.Fatalf("mode %q, want %q", requested, mode)
			}
			if _, err := cliPrepareTakeoverCandidate(binding, leases); err != nil {
				t.Fatal(err)
			}
		})
	}
}
