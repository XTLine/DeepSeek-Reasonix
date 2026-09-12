package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"reasonix/internal/agent"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/provider"
	"reasonix/internal/remote/bootstrap"
	"reasonix/internal/serve"
	"reasonix/internal/store"
)

// startSourceProcess spawns an occupied-session holder through the
// TestCLIPeerProcessHelper child mode and returns its discovery record. The
// child's session carries one unsaved prompt, exactly like an interactive CLI
// with pending work when another CLI asks for the session.
func startSourceProcess(t *testing.T, path string) (cliPeerRecord, func()) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(executable, "-test.run=^TestCLIPeerProcessHelper$", "-test.v")
	child.Env = append(os.Environ(), "REASONIX_TEST_PEER_SESSION="+path)
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(stdout)
	ready := false
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), "peer-ready") {
			ready = true
			break
		}
	}
	if !ready {
		_ = stdin.Close()
		_ = child.Wait()
		t.Fatal("source process did not start")
	}
	data, err := os.ReadFile(filepath.Join(cliPeerDirectory(), fmt.Sprintf("cli-%d.json", child.Process.Pid)))
	if err != nil {
		t.Fatal(err)
	}
	var record cliPeerRecord
	if json.Unmarshal(data, &record) != nil {
		t.Fatal("invalid peer registration")
	}
	stop := func() {
		_, _ = stdin.Write([]byte("\n"))
		_ = stdin.Close()
		if err := child.Wait(); err != nil {
			t.Errorf("source process: %v", err)
		}
	}
	return record, stop
}

// TestResumeEntriesConvergeOnPeerTakeover drives the three resume entries —
// startup resume, the in-TUI /resume refusal, and the /takeover picker —
// against the same occupied-session contract: the holder's final unsaved work
// survives, exactly one writer remains, and the holder stays read-only.
func TestResumeEntriesConvergeOnPeerTakeover(t *testing.T) {
	newLocalTUI := func(t *testing.T, home string) chatTUI {
		t.Helper()
		local := filepath.Join(home, "local.jsonl")
		saveTestSession(t, local, "local history")
		sess, err := agent.LoadSession(local)
		if err != nil {
			t.Fatal(err)
		}
		executor := agent.New(nil, nil, sess, agent.Options{}, event.Discard)
		ctrl := control.New(control.Options{Executor: executor, SessionDir: home, SessionPath: local, Sink: event.Discard})
		t.Cleanup(ctrl.Close)
		m := newChatTUI(ctrl, "", make(chan event.Event, 10), 80)
		m.leases = control.NewSessionLeaseKeeper()
		t.Cleanup(m.leases.Release)
		if err := m.leases.Rebind(local); err != nil {
			t.Fatal(err)
		}
		if err := m.leases.BindControllerAuthority(m.ctrl.(*control.Controller)); err != nil {
			t.Fatal(err)
		}
		return m
	}
	chooseAndWait := func(t *testing.T, m *chatTUI, id string) tea.Cmd {
		t.Helper()
		m.applyTakeoverQuery(m.pendingTakeoverCmd().(cliTakeoverQueryMsg))
		prompt := m.takeoverPrompt
		if prompt == nil || prompt.querying || prompt.picker == nil {
			t.Fatal("takeover prompt never became ready")
		}
		index := -1
		for i, item := range prompt.picker.items {
			if item.ID == id {
				index = i
			}
		}
		if index < 0 {
			t.Fatalf("choice %q missing from %v", id, prompt.picker.items)
		}
		prompt.picker.selected = index
		_, cmd := m.handleTakeoverKey(tea.KeyPressMsg{Code: tea.KeyEnter})
		return cmd
	}
	assertTakeoverOutcome := func(t *testing.T, m *chatTUI, path string, record cliPeerRecord) {
		t.Helper()
		if got := m.ctrl.SessionPath(); got != path {
			t.Fatalf("resumed path = %q, want %q", got, path)
		}
		history := m.ctrl.History()
		if len(history) == 0 || history[len(history)-1].Content != "latest unsaved peer prompt" {
			t.Fatalf("holder's final unsaved prompt was lost: %+v", history)
		}
		if got := m.leases.HeldPath(); got != agent.CanonicalSessionPath(path) {
			t.Fatalf("taker holds %q, want %q", got, path)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		view, err := queryCLIPeer(ctx, record, path)
		if err != nil || view.Holder != "cli" {
			t.Fatalf("handed-off holder reports %+v (err %v)", view, err)
		}
		contender, err := agent.TryAcquireSessionLease(path)
		if contender != nil {
			contender.Release()
		}
		if !errors.Is(err, agent.ErrSessionLeaseHeld) {
			t.Fatal("a third writer entered the handed-off session")
		}
	}

	t.Run("startup resume refusal", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("REASONIX_HOME", home)
		path := filepath.Join(home, "held.jsonl")
		record, stop := startSourceProcess(t, path)
		defer stop()
		leases := control.NewSessionLeaseKeeper()
		defer leases.Release()
		_, err := bindAndLoadCLIResume(leases, path, loadResumableSession)
		if !errors.Is(err, agent.ErrSessionLeaseHeld) {
			t.Fatalf("startup resume: %v", err)
		}
		binding, err := cliTakeoverHeldSessionMode(path, err, leases, nil, "wait")
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := cliPrepareTakeoverCandidate(binding, leases)
		if err != nil {
			t.Fatal(err)
		}
		executor := agent.New(nil, nil, loaded, agent.Options{}, event.Discard)
		ctrl := control.New(control.Options{Executor: executor, SessionPath: path, Sink: event.Discard})
		defer ctrl.Close()
		if err := commitResumedSession(binding, nil, ctrl, loaded, path); err != nil {
			t.Fatal(err)
		}
		if err := rebindCLIControllerAuthority(leases, ctrl); err != nil {
			t.Fatal(err)
		}
		m := &chatTUI{ctrl: ctrl, leases: leases}
		assertTakeoverOutcome(t, m, path, record)
	})

	t.Run("resume command records conflict", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("REASONIX_HOME", home)
		path := filepath.Join(home, "held.jsonl")
		m := newLocalTUI(t, home)
		record, stop := startSourceProcess(t, path)
		defer stop()
		leaseErr := m.leases.Rebind(path)
		if !errors.Is(leaseErr, agent.ErrSessionLeaseHeld) {
			t.Fatalf("/resume conflict: %v", leaseErr)
		}
		m.recordResumeConflict(path, leaseErr)
		if m.pendingTakeoverPath != path || m.takeoverPrompt == nil {
			t.Fatalf("conflict did not pin the target: path=%q prompt=%v", m.pendingTakeoverPath, m.takeoverPrompt)
		}
		cmd := chooseAndWait(t, &m, "wait")
		if cmd == nil {
			t.Fatal("confirmed choice did not start the takeover")
		}
		model, _ := m.finishTakeover(cmd().(cliTakeoverDoneMsg))
		final := model.(chatTUI)
		if final.takeoverPrompt != nil || final.pendingTakeoverPath != "" {
			t.Fatal("takeover prompt survived a successful resume")
		}
		assertTakeoverOutcome(t, &final, path, record)
	})

	t.Run("takeover picker resolves index", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("REASONIX_HOME", home)
		path := filepath.Join(home, "held.jsonl")
		m := newLocalTUI(t, home)
		saveTestSession(t, path, "saved base")
		record, stop := startSourceProcess(t, path)
		defer stop()
		m.runTakeoverSelection("/takeover 1")
		prompt := m.takeoverPrompt
		if prompt == nil || prompt.path != path {
			t.Fatalf("picker did not resolve the held session: %+v", prompt)
		}
		cmd := chooseAndWait(t, &m, "wait")
		if cmd == nil {
			t.Fatal("confirmed choice did not start the takeover")
		}
		model, _ := m.finishTakeover(cmd().(cliTakeoverDoneMsg))
		final := model.(chatTUI)
		assertTakeoverOutcome(t, &final, path, record)
	})
}

// TestSSHServeHandoffProcessHelper is the remote half of the SSH lifecycle
// test: a resident serve on this machine (as the desktop's SSH bootstrap would
// leave behind) holding the session with unsaved in-memory work. It exits when
// stdin closes and answers "probe" with its controller's latest message.
func TestSSHServeHandoffProcessHelper(t *testing.T) {
	home := os.Getenv("REASONIX_TEST_SERVE_HOME")
	if home == "" {
		return
	}
	t.Setenv("REASONIX_HOME", home)
	path := filepath.Join(home, "session.jsonl")
	sess := agent.NewSession("system")
	sess.Add(provider.Message{Role: provider.RoleUser, Content: "unsaved serve prompt"})
	bc := serve.NewBroadcaster()
	executor := agent.New(nil, nil, sess, agent.Options{}, bc)
	ctrl := control.New(control.Options{Executor: executor, SessionDir: home, SessionPath: path, Sink: bc})
	token := "lifecycle-test-token"
	server := serve.New(ctrl, bc, config.ServeConfig{AuthMode: "token", Token: token})
	leases := control.NewSessionLeaseKeeper()
	defer leases.Release()
	defer ctrl.Close()
	if err := leases.Rebind(path); err != nil {
		t.Fatal(err)
	}
	server.SetSessionLeases(leases)
	srv := httptest.NewServer(server.Handler())
	defer srv.Close()
	stateDir := config.RemoteStateDir()
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	addr := strings.TrimPrefix(srv.URL, "http://")
	state, err := bootstrap.MarshalState(bootstrap.ServeState{PID: os.Getpid(), Addr: addr, Workspace: home})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "serve-lifecycle.json"), state, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, store.RemoteServePortName("lifecycle")), []byte(addr), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, store.RemoteServeTokenName("lifecycle")), []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	fmt.Println("serve-ready")
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) != "probe" {
			continue
		}
		history := ctrl.History()
		last := ""
		if len(history) > 0 {
			last = history[len(history)-1].Content
		}
		fmt.Printf("serve-history %s\n", last)
	}
}

// TestSSHServeHandoffLifecycle exercises the full remote-CLI scenario against
// a real serve process discovered through the SSH bootstrap state files:
// takeover (serve snapshots and releases), mirrored frames, a desktop-style
// reclaim, and the CLI's cooperative yield back to serve.
func TestSSHServeHandoffLifecycle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	path := filepath.Join(home, "session.jsonl")
	// The serve validates that the transcript file exists before answering
	// ownership queries; a real resident serve always has a saved session.
	saveTestSession(t, path, "saved base")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, executable, "-test.run=^TestSSHServeHandoffProcessHelper$", "-test.v")
	child.Env = append(os.Environ(), "REASONIX_TEST_SERVE_HOME="+home)
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		if err := child.Wait(); err != nil {
			t.Errorf("serve process: %v", err)
		}
	}()
	scanner := bufio.NewScanner(stdout)
	ready := false
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), "serve-ready") {
			ready = true
			break
		}
	}
	if !ready {
		t.Fatal("serve did not start")
	}
	probeHistory := func(t *testing.T) string {
		t.Helper()
		if _, err := stdin.Write([]byte("probe\n")); err != nil {
			t.Fatal(err)
		}
		for scanner.Scan() {
			if line := scanner.Text(); strings.HasPrefix(line, "serve-history ") {
				return strings.TrimPrefix(line, "serve-history ")
			}
		}
		t.Fatal("serve never answered the history probe")
		return ""
	}

	records := discoverCLIServes()
	if len(records) != 1 {
		t.Fatalf("discovery found %d resident serves, want 1", len(records))
	}
	record := records[0]
	view, err := queryCLITakeover(ctx, path)
	if err != nil || view.Holder != "serve" || view.Mirrored {
		t.Fatalf("ownership before takeover: %+v %v", view, err)
	}

	leases := control.NewSessionLeaseKeeper()
	defer leases.Release()
	manager := newCLITakeoverManager(event.Discard, leases)
	defer func() {
		if err := manager.Close(); err != nil {
			t.Errorf("manager close: %v", err)
		}
	}()
	_, leaseErr := bindAndLoadCLIResume(leases, path, loadResumableSession)
	if !errors.Is(leaseErr, agent.ErrSessionLeaseHeld) {
		t.Fatalf("startup resume against serve: %v", leaseErr)
	}
	if !cliSessionTakeoverCandidate(leaseErr) {
		t.Fatal("resident serve was not identified as a takeover candidate")
	}
	binding, err := cliTakeoverHeldSessionMode(path, leaseErr, leases, manager, "wait")
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := cliPrepareTakeoverCandidate(binding, leases)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, msg := range loaded.Snapshot() {
		if msg.Content == "unsaved serve prompt" {
			found = true
		}
	}
	if !found {
		t.Fatal("takeover lost the serve's final unsaved message")
	}
	loaded.Add(provider.Message{Role: provider.RoleAssistant, Content: "cli takeover answer"})
	executor := agent.New(nil, nil, loaded, agent.Options{}, event.Discard)
	ctrl := control.New(control.Options{Executor: executor, SessionDir: home, SessionPath: path, Sink: event.Discard})
	defer ctrl.Close()
	manager.AttachController(ctrl)
	if err := commitResumedSession(binding, manager, ctrl, loaded, path); err != nil {
		t.Fatal(err)
	}
	if err := rebindCLIControllerAuthority(leases, ctrl); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.Snapshot(); err != nil {
		t.Fatalf("cli could not save its takeover turn: %v", err)
	}
	manager.Activate(binding)
	manager.Emit(event.Event{Kind: event.Text, Text: "mirrored turn"})
	if !manager.push(false) {
		t.Fatal("frame mirror push stopped")
	}
	if view, err := cliOwnership(ctx, binding.client, record, path); err != nil || !view.Mirrored {
		t.Fatalf("serve did not report the mirrored takeover: %+v %v", view, err)
	}

	yielded := make(chan struct{})
	manager.SetYieldCallback(func() { close(yielded) })
	reclaimDone := make(chan int, 1)
	go func() {
		body, _ := json.Marshal(map[string]any{"sessionPath": path, "mode": "wait", "timeoutMs": 20000})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, record.base+"/reclaim", strings.NewReader(string(body)))
		if err != nil {
			reclaimDone <- 0
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := binding.client.Do(req)
		if err != nil {
			reclaimDone <- 0
			return
		}
		defer resp.Body.Close()
		_, _ = bufio.NewReader(resp.Body).ReadByte()
		reclaimDone <- resp.StatusCode
	}()
	deadline := time.Now().Add(20 * time.Second)
	for !manager.Returned() && time.Now().Before(deadline) {
		manager.push(false)
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case <-yielded:
	case <-time.After(20 * time.Second):
		t.Fatal("reclaim never made the CLI yield")
	}
	select {
	case status := <-reclaimDone:
		if status != http.StatusNoContent {
			t.Fatalf("reclaim status = %d", status)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("reclaim never completed")
	}
	if !manager.Returned() {
		t.Fatal("manager did not mark the mirror returned")
	}
	if err := ctrl.Snapshot(); err == nil {
		t.Fatal("yielded CLI controller remained writable")
	}
	if got := probeHistory(t); got != "cli takeover answer" {
		t.Fatalf("serve resumed %q, want the CLI's latest message", got)
	}
	if view, err := cliOwnership(ctx, binding.client, record, path); err != nil || view.Mirrored || view.Holder != "serve" {
		t.Fatalf("serve did not re-own the session: %+v %v", view, err)
	}
	info, err := agent.LoadSessionLeaseInfo(path)
	if err != nil {
		t.Fatal(err)
	}
	if info == nil || info.HandoffTo != "" || info.PID != child.Process.Pid {
		t.Fatalf("lease after reclaim = %+v, want the serve process holding it", info)
	}
}

// TestCLIPeerReservationSurvivesSourceExit covers the lost-response case
// across real processes: the successor's handoff response never arrives (the
// source exits first), the outcome stays unconfirmed for a plain retry, but
// the durable reservation still names the intended successor — and only that
// writer can reconcile it.
func TestCLIPeerReservationSurvivesSourceExit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	path := filepath.Join(home, "session.jsonl")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, executable, "-test.run=^TestCLIPeerProcessHelper$", "-test.v")
	child.Env = append(os.Environ(), "REASONIX_TEST_PEER_SESSION="+path)
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- child.Wait() }()
	scanner := bufio.NewScanner(stdout)
	ready := false
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), "peer-ready") {
			ready = true
			break
		}
	}
	if !ready {
		t.Fatal("source process did not start")
	}
	data, err := os.ReadFile(filepath.Join(cliPeerDirectory(), fmt.Sprintf("cli-%d.json", child.Process.Pid)))
	if err != nil {
		t.Fatal(err)
	}
	var record cliPeerRecord
	if json.Unmarshal(data, &record) != nil {
		t.Fatal("invalid peer registration")
	}
	body, _ := json.Marshal(cliPeerRequest{SessionPath: path, TargetWriterID: agent.SessionWriterID(), SourceWriterID: record.WriterID, Mode: "wait"})
	var grant cliTakeoverGrant
	if err := cliPeerDo(ctx, record, http.MethodPost, "/handoff", body, &grant); err != nil {
		t.Fatal(err)
	}
	if grant.HandoffID == "" {
		t.Fatal("source did not grant the handoff")
	}

	// The response arrived here, but the successor crashed before consuming the
	// reservation: kill the source, then retry the acquisition the way a fresh
	// resume would.
	if _, err := stdin.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	_ = stdin.Close()
	select {
	case <-waited:
	case <-time.After(20 * time.Second):
		t.Fatal("source process never exited")
	}
	if _, err := os.Stat(filepath.Join(cliPeerDirectory(), fmt.Sprintf("cli-%d.json", child.Process.Pid))); !os.IsNotExist(err) {
		t.Fatalf("registration survived source exit: %v", err)
	}
	leases := control.NewSessionLeaseKeeper()
	defer leases.Release()
	_, err = acquireCLIPeerSession(path, record, leases, nil, "wait")
	if err == nil || !strings.Contains(err.Error(), "unconfirmed") {
		t.Fatalf("retry after response loss = %v, want an unconfirmed outcome", err)
	}
	// The source released its lease into a reservation, so the OS lock is free
	// while the reservation itself stays durable for the named successor.
	info, err := agent.LoadSessionLeaseInfo(path)
	if err != nil {
		t.Fatal(err)
	}
	if info == nil || info.HandoffTo != agent.SessionWriterID() || info.HandoffID != grant.HandoffID {
		t.Fatalf("reservation = %+v, want it still naming this successor", info)
	}
	if !time.Now().UTC().Before(info.HandoffExpiresAt) {
		t.Fatal("reservation already expired")
	}
	contender, err := agent.TryAcquireSessionLease(path)
	if contender != nil {
		contender.Release()
	}
	if !errors.Is(err, agent.ErrSessionLeaseHeld) {
		t.Fatalf("uninvited writer acquired the reserved session: %v", err)
	}
	if err := leases.RebindWithHandoff(path, record.WriterID, grant.HandoffID); err != nil {
		t.Fatalf("intended successor could not reconcile: %v", err)
	}
	if got := leases.HeldPath(); got != agent.CanonicalSessionPath(path) {
		t.Fatalf("reconciled keeper holds %q", got)
	}
}
