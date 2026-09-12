package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/provider"
)

// This helper runs a real controller and endpoint with a different WriterID
// and OS process lock. No model or external service is needed.
func TestCLIPeerProcessHelper(t *testing.T) {
	path := os.Getenv("REASONIX_TEST_PEER_SESSION")
	if path == "" {
		return
	}
	t.Setenv("REASONIX_HOME", filepath.Dir(path))
	sess := agent.NewSession("system")
	sess.Add(provider.Message{Role: provider.RoleUser, Content: "latest unsaved peer prompt"})
	ctrl := control.New(control.Options{Executor: agent.New(nil, nil, sess, agent.Options{}, event.Discard), SessionPath: path, Sink: event.Discard})
	leases := control.NewSessionLeaseKeeper()
	defer leases.Release()
	defer ctrl.Close()
	if err := leases.Rebind(path); err != nil {
		t.Fatal(err)
	}
	if err := leases.BindControllerAuthority(ctrl); err != nil {
		t.Fatal(err)
	}
	p, err := newCLIPeerServer(cliPeerDirectory())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	m := chatTUI{ctrl: ctrl, leases: leases, pendingCommit: &[]string{}}
	stop := make(chan struct{})
	go func() { _, _ = bufio.NewReader(os.Stdin).ReadByte(); close(stop) }()
	fmt.Println("peer-ready")
	for {
		select {
		case <-stop:
			return
		case req := <-p.requests:
			next, cmd := m.handlePeerRequest(req)
			m = next.(chatTUI)
			if cmd != nil {
				result := cmd().(cliPeerDoneMsg)
				next, _ = m.finishPeerHandoff(result)
				m = next.(chatTUI)
			}
		}
	}
}

func TestCLIPeerTakeoverAcrossProcessesPreservesLatestHistory(t *testing.T) {
	for _, mode := range []string{"wait", "interrupt"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("REASONIX_HOME", home)
			path := filepath.Join(home, "session.jsonl")
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
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
			defer func() {
				_ = stdin.Close()
				if err := child.Wait(); err != nil {
					t.Errorf("source process: %v", err)
				}
			}()
			scanner := bufio.NewScanner(stdout)
			ready := false
			for scanner.Scan() {
				if strings.Contains(scanner.Text(), "peer-ready") {
					ready = true
					break
				}
			}
			if !ready {
				t.Fatal("source did not start")
			}
			view, err := queryCLITakeover(ctx, path)
			if err != nil || view.Holder != "cli" {
				t.Fatalf("discovery: %+v %v", view, err)
			}
			leases := control.NewSessionLeaseKeeper()
			defer leases.Release()
			leaseErr := leases.Rebind(path)
			if !errors.Is(leaseErr, agent.ErrSessionLeaseHeld) {
				t.Fatalf("missing real process conflict: %v", leaseErr)
			}
			binding, err := cliTakeoverHeldSessionMode(path, leaseErr, leases, nil, mode)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := cliPrepareTakeoverCandidate(binding, leases)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, msg := range loaded.Snapshot() {
				if msg.Content == "latest unsaved peer prompt" {
					found = true
				}
			}
			if !found {
				t.Fatal("receiver lost source's final unsaved message")
			}
			contender, err := agent.TryAcquireSessionLease(path)
			if contender != nil {
				contender.Release()
			}
			if !errors.Is(err, agent.ErrSessionLeaseHeld) {
				t.Fatal("a second writer could enter after handoff")
			}
		})
	}
}
