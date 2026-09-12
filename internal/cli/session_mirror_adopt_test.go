package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/eventwire"
)

func TestCLIOrdinaryResumeRegistersMirrorAndReturnsLease(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.jsonl"), filepath.Join(dir, "b.jsonl")
	saveTestSession(t, a, "first")
	saveTestSession(t, b, "second")
	session, err := agent.LoadSession(a)
	if err != nil {
		t.Fatal(err)
	}
	leases := control.NewSessionLeaseKeeper()
	defer leases.Release()
	if err := leases.Rebind(a); err != nil {
		t.Fatal(err)
	}
	ctrl := control.New(control.Options{Executor: agent.New(nil, nil, session, agent.Options{}, event.Discard), SessionDir: dir, SessionPath: a})
	defer ctrl.Close()
	if err := leases.BindControllerAuthority(ctrl); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var adopted, ended []string
	var frames []eventwire.Event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/auth/token":
			w.WriteHeader(http.StatusNoContent)
		case "/adopt":
			var body struct {
				SessionPath string `json:"sessionPath"`
				WriterID    string `json:"writerId"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.WriterID != agent.SessionWriterID() || body.SessionPath != leases.HeldPath() {
				t.Error("adopt did not identify the current writer and lease")
			}
			adopted = append(adopted, body.SessionPath)
			_ = json.NewEncoder(w).Encode(cliTakeoverGrant{SessionPath: body.SessionPath, MirrorID: body.SessionPath, ReturnHandoffID: "return", SourceWriterID: "serve", TargetWriterID: body.WriterID})
		case "/external/frames":
			var body struct {
				Frames []eventwire.Event `json:"frames"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			frames = append(frames, body.Frames...)
			_ = json.NewEncoder(w).Encode(map[string]bool{"reclaimRequested": false})
		case "/mirror-end":
			var body struct {
				SessionPath string `json:"sessionPath"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			ended = append(ended, body.SessionPath)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	previous := discoverCLIServesForTakeover
	discoverCLIServesForTakeover = func() []cliServeRecord { return []cliServeRecord{{base: srv.URL}} }
	defer func() { discoverCLIServesForTakeover = previous }()
	m := newCLITakeoverManager(event.Discard, leases)
	m.AttachController(ctrl)
	defer m.Close()
	// First discovery uses an ordinary lease, with no /handoff grant.
	m.push(true)
	if binding, _, _, _ := m.snapshot(); binding == nil || binding.path != agent.CanonicalSessionPath(a) {
		t.Fatal("ordinary session was not adopted")
	}
	m.Emit(event.Event{Kind: event.Text, Text: "live TUI output"})
	m.push(false)
	tui := &chatTUI{ctrl: ctrl, leases: leases, takeover: m}
	if err := tui.commitSessionSwitch(b); err != nil {
		t.Fatal(err)
	}
	m.push(true)
	if binding, _, _, _ := m.snapshot(); binding == nil || binding.path != agent.CanonicalSessionPath(b) {
		t.Fatal("resumed session was not adopted")
	}
	if err := m.returnLease(); err != nil {
		t.Fatal(err)
	}
	if leases.HeldPath() != "" || !m.Returned() {
		t.Fatal("return kept TUI write ownership")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(adopted) != 2 || len(ended) != 2 || ended[0] != agent.CanonicalSessionPath(a) || ended[1] != agent.CanonicalSessionPath(b) {
		t.Fatalf("adopted=%v ended=%v", adopted, ended)
	}
	if len(frames) != 1 || frames[0].Text != "live TUI output" {
		t.Fatalf("mirror frames=%+v", frames)
	}
}

func TestCLIOrdinaryMirrorDiscoveryRetriesAndRejectsWrongGrant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	leases := control.NewSessionLeaseKeeper()
	defer leases.Release()
	if err := leases.Rebind(path); err != nil {
		t.Fatal(err)
	}
	ctrl := control.New(control.Options{SessionPath: path})
	defer ctrl.Close()
	m := newCLITakeoverManager(event.Discard, leases)
	m.AttachController(ctrl)
	previous := discoverCLIServesForTakeover
	defer func() { discoverCLIServesForTakeover = previous }()
	discoverCLIServesForTakeover = func() []cliServeRecord { return nil }
	m.push(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/token" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_ = json.NewEncoder(w).Encode(cliTakeoverGrant{SessionPath: path, MirrorID: "mirror", SourceWriterID: "serve", TargetWriterID: "another-writer", ReturnHandoffID: "return"})
	}))
	defer srv.Close()
	discoverCLIServesForTakeover = func() []cliServeRecord { return []cliServeRecord{{base: srv.URL}} }
	m.discoverAfter = time.Time{}
	m.push(true)
	if binding, _, _, _ := m.snapshot(); binding != nil || m.Reclaiming() {
		t.Fatal("invalid discovery changed ownership")
	}
	if leases.HeldPath() != agent.CanonicalSessionPath(path) {
		t.Fatal("discovery lost ordinary lease")
	}
}
