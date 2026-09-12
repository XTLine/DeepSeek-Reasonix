package cli

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"reasonix/internal/agent"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/fileutil"
	"reasonix/internal/i18n"
)

// A per-process endpoint exposes only ownership and cooperative release. It
// cannot submit prompts or execute tools. Requests are admitted by the TUI so
// session/model switches cannot race the captured controller.
type cliPeerRecord struct {
	PID      int    `json:"pid"`
	WriterID string `json:"writerId"`
	Address  string `json:"address"`
	Token    string `json:"token"`
}

type cliPeerServer struct {
	record   cliPeerRecord
	file     string
	server   *http.Server
	requests chan cliPeerRequest
	done     chan struct{}
	once     sync.Once
}

type cliPeerRequest struct {
	SessionPath    string `json:"sessionPath"`
	TargetWriterID string `json:"targetWriterId"`
	SourceWriterID string `json:"sourceWriterId"`
	Mode           string `json:"mode"`
	op             string
	ctx            context.Context
	reply          chan cliPeerResponse
}

type cliPeerResponse struct {
	view  cliOwnershipView
	grant *cliTakeoverGrant
	err   error
}

type cliPeerDoneMsg struct {
	request cliPeerRequest
	grant   cliTakeoverGrant
	reopen  func()
	err     error
}

func cliPeerDirectory() string { return filepath.Join(config.ReasonixHomeDir(), "cli-handoff") }

func newCLIPeerServer(directory string) (*cliPeerServer, error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		listener.Close()
		return nil, err
	}
	p := &cliPeerServer{
		record:   cliPeerRecord{PID: os.Getpid(), WriterID: agent.SessionWriterID(), Address: listener.Addr().String(), Token: hex.EncodeToString(secret)},
		requests: make(chan cliPeerRequest), done: make(chan struct{}),
	}
	p.file = filepath.Join(directory, fmt.Sprintf("cli-%d.json", p.record.PID))
	p.server = &http.Server{Handler: http.HandlerFunc(p.handle), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, IdleTimeout: 10 * time.Second}
	data, err := json.Marshal(p.record)
	if err == nil {
		err = fileutil.AtomicWriteFileStrict(p.file, data, 0600)
	}
	if err != nil {
		listener.Close()
		return nil, err
	}
	go func() { _ = p.server.Serve(listener) }()
	return p, nil
}

func (p *cliPeerServer) Close() {
	if p == nil {
		return
	}
	p.once.Do(func() {
		close(p.done)
		_ = p.server.Close()
		// A stale instance must not remove a replacement's registration.
		data, err := os.ReadFile(p.file)
		var current cliPeerRecord
		if err == nil && json.Unmarshal(data, &current) == nil && current.Token == p.record.Token {
			_ = os.Remove(p.file)
		}
	})
}

func (p *cliPeerServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.Host != p.record.Address || r.Header.Get("Origin") != "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+p.record.Token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	req := cliPeerRequest{op: r.URL.Path, ctx: r.Context(), reply: make(chan cliPeerResponse, 1)}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/ownership":
		req.SessionPath = r.URL.Query().Get("session")
	case r.Method == http.MethodPost && r.URL.Path == "/handoff":
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if req.SourceWriterID != p.record.WriterID || req.TargetWriterID == "" || req.TargetWriterID == req.SourceWriterID || (req.Mode != "wait" && req.Mode != "interrupt") {
			http.Error(w, "invalid writer or mode", http.StatusBadRequest)
			return
		}
	default:
		http.NotFound(w, r)
		return
	}
	if strings.TrimSpace(req.SessionPath) == "" {
		http.Error(w, "missing session", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), cliTakeoverTimeout+15*time.Second)
	defer cancel()
	select {
	case p.requests <- req:
	case <-ctx.Done():
		return
	case <-p.done:
		return
	}
	select {
	case result := <-req.reply:
		if result.err != nil {
			http.Error(w, result.err.Error(), http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if result.grant != nil {
			_ = json.NewEncoder(w).Encode(result.grant)
		} else {
			_ = json.NewEncoder(w).Encode(result.view)
		}
	case <-ctx.Done():
		http.Error(w, "handoff result pending; retry to reconcile ownership", http.StatusGatewayTimeout)
	case <-p.done:
	}
}

func waitForCLIPeer(p *cliPeerServer) tea.Cmd {
	if p == nil {
		return nil
	}
	return func() tea.Msg {
		select {
		case req := <-p.requests:
			return req
		case <-p.done:
			return nil
		}
	}
}

func (m chatTUI) handlePeerRequest(req cliPeerRequest) (tea.Model, tea.Cmd) {
	next := waitForCLIPeer(m.peer)
	reject := func(err error) (tea.Model, tea.Cmd) { req.reply <- cliPeerResponse{err: err}; return m, next }
	if req.ctx.Err() != nil {
		return reject(req.ctx.Err())
	}
	path := agent.CanonicalSessionPath(req.SessionPath)
	if m.ctrl == nil || path != agent.CanonicalSessionPath(m.ctrl.SessionPath()) {
		return reject(fmt.Errorf("session is no longer selected in this CLI"))
	}
	// An accepted request remains queryable after the HTTP client disconnects.
	// Only that successor can retrieve its grant; a third writer cannot steal it.
	if m.peerGrant != nil && m.peerGrant.SessionPath == path {
		if req.op == "/ownership" {
			req.reply <- cliPeerResponse{view: cliOwnershipView{Holder: "cli"}}
			return m, next
		}
		if m.peerGrant.TargetWriterID != req.TargetWriterID {
			return reject(fmt.Errorf("session already handed off to another writer"))
		}
		req.reply <- cliPeerResponse{grant: m.peerGrant}
		return m, next
	}
	if m.leases == nil || m.leases.HeldPath() != path {
		return reject(fmt.Errorf("CLI no longer holds this session"))
	}
	if req.op == "/ownership" {
		req.reply <- cliPeerResponse{view: cliOwnershipView{Holder: "cli", Running: cliControllerHasActiveRuntimeWork(m.ctrl)}}
		return m, next
	}
	if m.peerBusy || m.modelSwitchPending || m.takeoverPrompt != nil || (m.takeover != nil && (m.takeover.Reclaiming() || m.takeover.hasMirror())) {
		return reject(fmt.Errorf("session is already switching or mirrored by Serve"))
	}
	ctrl, ok := m.ctrl.(*control.Controller)
	if !ok {
		return reject(fmt.Errorf("controller cannot cooperate"))
	}
	reopen, err := ctrl.BeginSessionHandoff(true)
	if err != nil {
		return reject(err)
	}
	m.peerBusy = true
	m.notice(i18n.M.TakeoverBusy)
	leases := m.leases
	return m, tea.Batch(next, func() tea.Msg {
		result := cliPeerDoneMsg{request: req, reopen: reopen}
		// Deliberately independent of the HTTP request: after admission only
		// completion or rollback may reopen the source controller.
		ctx, cancel := context.WithTimeout(context.Background(), cliTakeoverTimeout)
		defer cancel()
		result.grant, result.err = releaseCLIPeerSession(ctx, ctrl, leases, req)
		return result
	})
}

func releaseCLIPeerSession(ctx context.Context, ctrl control.SessionAPI, leases *control.SessionLeaseKeeper, req cliPeerRequest) (cliTakeoverGrant, error) {
	grant := cliTakeoverGrant{SessionPath: agent.CanonicalSessionPath(req.SessionPath), SourceWriterID: req.SourceWriterID, TargetWriterID: req.TargetWriterID}
	if req.Mode == "interrupt" {
		ctrl.Cancel()
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for cliControllerHasActiveRuntimeWork(ctrl) {
		select {
		case <-ctx.Done():
			return grant, fmt.Errorf("session did not become idle: %w", ctx.Err())
		case <-ticker.C:
		}
	}
	if err := ctx.Err(); err != nil {
		return grant, err
	}
	if agent.CanonicalSessionPath(ctrl.SessionPath()) != grant.SessionPath || leases.HeldPath() != grant.SessionPath {
		return grant, fmt.Errorf("session changed during handoff")
	}
	if err := ctrl.Snapshot(); err != nil {
		return grant, err
	}
	id := make([]byte, 24)
	if _, err := rand.Read(id); err != nil {
		return grant, err
	}
	grant.HandoffID = hex.EncodeToString(id)
	if err := leases.ReleaseForHandoff(req.TargetWriterID, grant.HandoffID); err != nil {
		return grant, err
	}
	return grant, nil
}

func (m chatTUI) finishPeerHandoff(msg cliPeerDoneMsg) (tea.Model, tea.Cmd) {
	m.peerBusy = false
	if msg.err != nil {
		msg.reopen()
		m.notice("handoff: " + msg.err.Error())
	} else {
		m.peerGrant = &msg.grant
		m.peerReopen = msg.reopen
		m.notice(i18n.M.TakeoverYielded)
	}
	msg.request.reply <- cliPeerResponse{grant: &msg.grant, err: msg.err}
	if m.takeoverShutdown != nil {
		shutdown := *m.takeoverShutdown
		m.takeoverShutdown = nil
		return m, func() tea.Msg { return shutdown }
	}
	return m, nil
}

func (m *cliTakeoverManager) hasMirror() bool {
	if m == nil {
		return false
	}
	current, _, _, _ := m.snapshot()
	return current != nil && !m.Returned()
}

func (m chatTUI) handleYieldedKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "r", "R":
		m.openTakeoverPrompt(m.ctrl.SessionPath())
	case "q", "Q", "esc", "ctrl+c", "ctrl+d":
		return m, shutdownNow
	case "pgup":
		m.viewport.PageUp()
	case "pgdown":
		m.viewport.PageDown()
	}
	return m, nil
}
