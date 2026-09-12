package cli

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/jobs"
	"reasonix/internal/provider"
	"reasonix/internal/tool"
)

type handoffBlockingProvider struct {
	started  chan struct{}
	finish   chan struct{}
	canceled chan struct{}
}

func (p *handoffBlockingProvider) Name() string { return "handoff-test" }
func (p *handoffBlockingProvider) Stream(ctx context.Context, _ provider.Request) (<-chan provider.Chunk, error) {
	chunks := make(chan provider.Chunk, 2)
	close(p.started)
	go func() {
		defer close(chunks)
		select {
		case <-ctx.Done():
			close(p.canceled)
		case <-p.finish:
			chunks <- provider.Chunk{Type: provider.ChunkText, Text: "completed before handoff"}
			chunks <- provider.Chunk{Type: provider.ChunkDone}
		}
	}()
	return chunks, nil
}

func awaitHandoffSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("runtime did not reach expected state")
	}
}

func TestCLIPeerHandoffRunningTurn(t *testing.T) {
	for _, mode := range []string{"wait", "interrupt", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			p := &handoffBlockingProvider{make(chan struct{}), make(chan struct{}), make(chan struct{})}
			path := filepath.Join(t.TempDir(), "running.jsonl")
			sess := agent.NewSession("system")
			executor := agent.New(p, tool.NewRegistry(), sess, agent.Options{}, event.Discard)
			ctrl := control.New(control.Options{Runner: executor, Executor: executor, SessionPath: path, Sink: event.Discard})
			leases := control.NewSessionLeaseKeeper()
			defer leases.Release()
			defer func() {
				ctrl.Cancel()
				deadline := time.Now().Add(5 * time.Second)
				for ctrl.Running() && time.Now().Before(deadline) {
					time.Sleep(10 * time.Millisecond)
				}
				ctrl.Close()
			}()
			if err := leases.Rebind(path); err != nil {
				t.Fatal(err)
			}
			if err := leases.BindControllerAuthority(ctrl); err != nil {
				t.Fatal(err)
			}
			ctrl.Send("finish this turn")
			awaitHandoffSignal(t, p.started)
			m := chatTUI{ctrl: ctrl, leases: leases, pendingCommit: &[]string{}}
			req := cliPeerRequest{SessionPath: path, SourceWriterID: agent.SessionWriterID(), TargetWriterID: "next-writer", Mode: mode, op: "/handoff", ctx: context.Background(), reply: make(chan cliPeerResponse, 1)}
			if mode == "timeout" {
				req.Mode = "wait"
			}
			if mode == "timeout" {
				reopen, err := ctrl.BeginSessionHandoff(true)
				if err != nil {
					t.Fatal(err)
				}
				m.peerBusy = true
				ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
				grant, err := releaseCLIPeerSession(ctx, ctrl, leases, req)
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("timeout: %v", err)
				}
				next, _ := m.finishPeerHandoff(cliPeerDoneMsg{request: req, reopen: reopen, grant: grant, err: err})
				m = next.(chatTUI)
				if m.peerBusy || m.peerGrant != nil || leases.HeldPath() != agent.CanonicalSessionPath(path) {
					t.Fatal("timeout damaged source ownership")
				}
				if err := ctrl.Snapshot(); err != nil {
					t.Fatalf("source cannot save after timeout: %v", err)
				}
				again, err := ctrl.BeginSessionHandoff(true)
				if err != nil {
					t.Fatalf("timeout did not reopen admission: %v", err)
				}
				again()
				select {
				case <-p.canceled:
					t.Fatal("timeout canceled source turn")
				default:
				}
				close(p.finish)
				deadline := time.Now().Add(5 * time.Second)
				for ctrl.Running() && time.Now().Before(deadline) {
					time.Sleep(10 * time.Millisecond)
				}
				if ctrl.Running() {
					t.Fatal("original turn did not finish after timeout")
				}
				again, err = ctrl.BeginSessionHandoff(false)
				if err != nil {
					t.Fatalf("source admission did not recover: %v", err)
				}
				again()
				return
			}
			next, cmd := m.handlePeerRequest(req)
			m = next.(chatTUI)
			if !m.peerBusy || cmd == nil {
				t.Fatal("running handoff not admitted")
			}
			if _, err := ctrl.BeginSessionHandoff(true); err == nil {
				t.Fatal("admission not sealed")
			}
			done := make(chan cliPeerDoneMsg, 1)
			go func() { done <- cmd().(cliPeerDoneMsg) }()
			if mode == "wait" {
				select {
				case <-done:
					t.Fatal("released during active turn")
				case <-p.canceled:
					t.Fatal("wait canceled turn")
				case <-time.After(150 * time.Millisecond):
				}
				if leases.HeldPath() == "" {
					t.Fatal("lease released before turn completion")
				}
				close(p.finish)
			} else {
				awaitHandoffSignal(t, p.canceled)
			}
			var result cliPeerDoneMsg
			select {
			case result = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("handoff did not complete")
			}
			next, _ = m.finishPeerHandoff(result)
			m = next.(chatTUI)
			if result.err != nil {
				t.Fatal(result.err)
			}
			if m.peerGrant == nil || leases.HeldPath() != "" {
				t.Fatal("source did not yield")
			}
			if err := ctrl.Snapshot(); err == nil {
				t.Fatal("yielded source can still save")
			}
			if mode == "wait" {
				loaded, err := agent.LoadSession(path)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, msg := range loaded.Snapshot() {
					if msg.Content == "completed before handoff" {
						found = true
					}
				}
				if !found {
					t.Fatal("handoff snapshot lost final answer")
				}
			}
		})
	}
}

func TestCLIPeerHandoffWaitsForBackgroundJob(t *testing.T) {
	path := filepath.Join(t.TempDir(), "background.jsonl")
	jm := jobs.NewManager(event.Discard)
	ctrl := control.New(control.Options{Executor: agent.New(nil, nil, agent.NewSession("system"), agent.Options{}, event.Discard), Jobs: jm, SessionPath: path, Sink: event.Discard})
	leases := control.NewSessionLeaseKeeper()
	defer leases.Release()
	defer ctrl.Close()
	if err := leases.Rebind(path); err != nil {
		t.Fatal(err)
	}
	if err := leases.BindControllerAuthority(ctrl); err != nil {
		t.Fatal(err)
	}
	started, finish := make(chan struct{}), make(chan struct{})
	jm.StartForSession(agent.BranchID(path), "task", "background", func(ctx context.Context, _ io.Writer) (string, error) {
		close(started)
		select {
		case <-finish:
			return "done", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	awaitHandoffSignal(t, started)
	m := chatTUI{ctrl: ctrl, leases: leases, pendingCommit: &[]string{}}
	req := cliPeerRequest{SessionPath: path, SourceWriterID: agent.SessionWriterID(), TargetWriterID: "next-writer", Mode: "wait", op: "/handoff", ctx: context.Background(), reply: make(chan cliPeerResponse, 1)}
	next, cmd := m.handlePeerRequest(req)
	m = next.(chatTUI)
	if !m.peerBusy || cmd == nil {
		t.Fatal("handoff not admitted")
	}
	done := make(chan cliPeerDoneMsg, 1)
	go func() { done <- cmd().(cliPeerDoneMsg) }()
	select {
	case <-done:
		t.Fatal("released before background job completed")
	case <-time.After(150 * time.Millisecond):
	}
	if leases.HeldPath() == "" {
		t.Fatal("lease released during job")
	}
	close(finish)
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		next, _ = m.finishPeerHandoff(result)
		if next.(chatTUI).peerGrant == nil {
			t.Fatal("source did not yield")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handoff did not finish")
	}
}

// A disk reservation alone is insufficient: recovery must correspond to this
// process's unconfirmed CLI request and name this writer as the successor.
func TestCLIPeerResumeDoesNotConsumeUnrelatedReservation(t *testing.T) {
	for _, scenario := range []string{"no CLI request", "different source", "different target"} {
		t.Run(scenario, func(t *testing.T) {
			m := newPeerTestTUI(t)
			path := m.ctrl.SessionPath()
			if err := m.ctrl.Snapshot(); err != nil {
				t.Fatal(err)
			}
			target := agent.SessionWriterID()
			if scenario == "different target" {
				target = "another-receiver"
			}
			if err := m.leases.ReleaseForHandoff(target, "reserved-generation"); err != nil {
				t.Fatal(err)
			}
			key := agent.CanonicalSessionPath(path)
			defer unconfirmedCLIHandoffs.Delete(key)
			if scenario != "no CLI request" {
				source := agent.SessionWriterID()
				if scenario == "different source" {
					source = "unrelated-source"
				}
				unconfirmedCLIHandoffs.Store(key, source)
			}
			receiver := control.NewSessionLeaseKeeper()
			defer receiver.Release()
			loaded := false
			_, err := bindAndLoadCLIResume(receiver, path, func(string) (*agent.Session, error) { loaded = true; return nil, nil })
			if !errors.Is(err, agent.ErrSessionLeaseHeld) || loaded {
				t.Fatalf("unrelated reservation consumed: err=%v loaded=%v", err, loaded)
			}
			info, err := agent.LoadSessionLeaseInfo(path)
			if err != nil || info == nil || info.HandoffID != "reserved-generation" || info.HandoffTo != target {
				t.Fatalf("reservation changed: %+v %v", info, err)
			}
		})
	}
}
