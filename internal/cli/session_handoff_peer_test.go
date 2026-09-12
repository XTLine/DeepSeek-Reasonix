package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/provider"
)

func TestCLIPeerEndpointRequiresTokenHostAndNoBrowserOrigin(t *testing.T) {
	p, err := newCLIPeerServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, attack := range []string{"token", "host", "origin"} {
		req, _ := http.NewRequest(http.MethodGet, "http://"+p.record.Address+"/ownership?session=x", nil)
		req.Header.Set("Authorization", "Bearer "+p.record.Token)
		switch attack {
		case "token":
			req.Header.Del("Authorization")
		case "host":
			req.Host = "attacker.example"
		case "origin":
			req.Header.Set("Origin", "http://attacker.example")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s accepted: %d", attack, resp.StatusCode)
		}
	}
	data, err := os.ReadFile(p.file)
	if err != nil {
		t.Fatal(err)
	}
	var record cliPeerRecord
	if json.Unmarshal(data, &record) != nil || record.WriterID != agent.SessionWriterID() {
		t.Fatal("invalid discovery record")
	}
	p.Close()
	if _, err := os.Stat(p.file); !os.IsNotExist(err) {
		t.Fatalf("record remained after close: %v", err)
	}
}

func newPeerTestTUI(t *testing.T) chatTUI {
	t.Helper()
	path := filepath.Join(t.TempDir(), "peer.jsonl")
	sess := agent.NewSession("system")
	sess.Add(provider.Message{Role: provider.RoleUser, Content: "last unsaved prompt"})
	executor := agent.New(nil, nil, sess, agent.Options{}, event.Discard)
	ctrl := control.New(control.Options{Executor: executor, SessionPath: path, Sink: event.Discard})
	leases := control.NewSessionLeaseKeeper()
	if err := leases.Rebind(path); err != nil {
		t.Fatal(err)
	}
	if err := leases.BindControllerAuthority(ctrl); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ctrl.Close(); leases.Release() })
	return chatTUI{ctrl: ctrl, leases: leases, pendingCommit: &[]string{}}
}

func TestCLIPeerHandoffPersistsAndRejectsThirdWriter(t *testing.T) {
	m := newPeerTestTUI(t)
	path := agent.CanonicalSessionPath(m.ctrl.SessionPath())
	req := cliPeerRequest{SessionPath: path, SourceWriterID: agent.SessionWriterID(), TargetWriterID: "next-process", Mode: "wait", op: "/handoff", ctx: context.Background(), reply: make(chan cliPeerResponse, 1)}
	next, cmd := m.handlePeerRequest(req)
	m = next.(chatTUI)
	if !m.peerBusy || cmd == nil {
		t.Fatal("handoff was not admitted")
	}
	result := cmd().(cliPeerDoneMsg)
	next, _ = m.finishPeerHandoff(result)
	m = next.(chatTUI)
	response := <-req.reply
	if response.err != nil {
		t.Fatal(response.err)
	}
	if m.peerGrant == nil || m.leases.HeldPath() != "" {
		t.Fatal("source retained writer ownership")
	}
	if _, err := agent.LoadSession(path); err != nil {
		t.Fatalf("handoff lost history: %v", err)
	}
	if err := m.ctrl.Snapshot(); err == nil {
		t.Fatal("yielded controller remained writable")
	}
	info, err := agent.LoadSessionLeaseInfo(path)
	if err != nil || info.HandoffTo != req.TargetWriterID || info.HandoffID != m.peerGrant.HandoffID {
		t.Fatalf("invalid reservation: %+v %v", info, err)
	}
	// Same request returns the identical grant after a lost HTTP response.
	_, _ = m.handlePeerRequest(req)
	if retry := <-req.reply; retry.err != nil || retry.grant.HandoffID != m.peerGrant.HandoffID {
		t.Fatal("retry changed the grant")
	}
	req.TargetWriterID = "third-process"
	_, _ = m.handlePeerRequest(req)
	if (<-req.reply).err == nil {
		t.Fatal("third writer stole the handoff")
	}
}

func TestCLIPeerExpiredDrainKeepsSourceWritable(t *testing.T) {
	m := newPeerTestTUI(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := releaseCLIPeerSession(ctx, m.ctrl, m.leases, cliPeerRequest{SessionPath: m.ctrl.SessionPath(), Mode: "wait"})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := m.ctrl.Snapshot(); err != nil {
		t.Fatalf("failed transfer damaged source: %v", err)
	}
	if m.leases.HeldPath() == "" {
		t.Fatal("failed transfer dropped lease")
	}
}

func TestCLIPeerAuthenticatedQueryDispatch(t *testing.T) {
	p, err := newCLIPeerServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	go func() {
		select {
		case req := <-p.requests:
			req.reply <- cliPeerResponse{view: cliOwnershipView{Holder: "cli", Running: true}}
		case <-p.done:
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+p.record.Address+"/ownership?session=opaque", nil)
	req.Header.Set("Authorization", "Bearer "+p.record.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var view cliOwnershipView
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil || view.Holder != "cli" || !view.Running {
		t.Fatalf("query: %+v %v", view, err)
	}
}
