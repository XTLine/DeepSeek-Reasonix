package main

// Real end-to-end self-test for the remote session + credential-channel
// watchdog, run only when REASONIX_E2E_SELFTEST=1 (it connects to the real
// host from the user config copy, sends two short messages through the
// credential proxy, and simulates an SSH blip by killing the connection from
// the remote side). Normal `go test` runs skip it.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"reasonix/internal/config"
)

type e2eEventLog struct {
	mu     sync.Mutex
	frames []string
	states []string
}

func (l *e2eEventLog) hook(name string, payload any) {
	data, _ := json.Marshal(payload)
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case strings.Contains(name, ":event"):
		l.frames = append(l.frames, string(data))
	case strings.Contains(name, ":state"):
		l.states = append(l.states, string(data))
	}
}

func (l *e2eEventLog) hasFrame(substr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, f := range l.frames {
		if strings.Contains(f, substr) {
			return true
		}
	}
	return false
}

func (l *e2eEventLog) waitForFrame(substr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if l.hasFrame(substr) {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// remoteConfigBaseURL reads the remote config's desktop-proxy base_url over
// the live SSH connection.
func remoteConfigBaseURL(t *testing.T, a *App, hostID string) string {
	t.Helper()
	rt, err := a.remoteRT()
	if err != nil {
		t.Fatal(err)
	}
	m := rt.(*desktopRemoteManager)
	c := m.client(hostID)
	if c == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fsys, err := c.SFTP()
	if err != nil {
		return ""
	}
	home, err := fsys.RealPath(ctx, "~")
	if err != nil {
		return ""
	}
	data, _, _, err := fsys.ReadFile(ctx, filepath.Join(home, ".reasonix", "config.toml"), 1<<20)
	if err != nil {
		return ""
	}
	for _, block := range strings.Split(string(data), "[[providers]]") {
		if strings.Contains(block, "reasonix-desktop-proxy") {
			for _, line := range strings.Split(block, "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "base_url") {
					return strings.Trim(strings.TrimPrefix(strings.SplitN(line, "=", 2)[1], " "), `"`)
				}
			}
		}
	}
	return ""
}

func killAppSSHFromRemote(t *testing.T) {
	t.Helper()
	// Kill every established sshd session on the remote (including this
	// probe's own): the app's transport breaks uncleanly and must reconnect.
	cmd := exec.Command("ssh", "-i", "/home/xtline/code/work/as_ssh",
		"-o", "ConnectTimeout=10", "root@118.196.79.86",
		`ss -K state established '( sport = :22 )' || true`)
	_ = cmd.Run()
}

func TestRemoteE2ECredentialWatchdog(t *testing.T) {
	if os.Getenv("REASONIX_E2E_SELFTEST") != "1" {
		t.Skip("set REASONIX_E2E_SELFTEST=1 to run the live remote self-test")
	}

	// Isolated desktop state; the real config copy provides the host entry.
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	t.Setenv("HOME", home)
	src, err := os.ReadFile(os.ExpandEnv("/home/xtline/.reasonix/config.toml"))
	if err != nil {
		t.Fatalf("read user config: %v", err)
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), src, 0o600); err != nil {
		t.Fatal(err)
	}
	// The temp HOME must still resolve the SSH identity/known_hosts, or the
	// connect stalls in the host-key TOFU prompt with no UI to answer it.
	if err := exec.Command("cp", "-r", "/home/xtline/.ssh", filepath.Join(home, ".ssh")).Run(); err != nil {
		t.Fatalf("copy .ssh: %v", err)
	}

	log := &e2eEventLog{}
	a := &App{remoteEventHook: log.hook}
	cleanupRemoteTabPumps(t, a)

	// 1. Open the workspace: real SSH connect + ensure serve + attach.
	done := make(chan TabMeta, 1)
	go func() {
		meta, err := a.OpenRemoteProjectTab("smoke-box", "pulsar-lite", RemoteTabOpenOptions{NewSession: true})
		if err == nil {
			done <- meta
		} else {
			t.Errorf("OpenRemoteProjectTab: %v", err)
			done <- TabMeta{}
		}
	}()
	var meta TabMeta
	select {
	case meta = <-done:
	case <-time.After(120 * time.Second):
		t.Fatal("OpenRemoteProjectTab timed out")
	}
	if meta.ID == "" {
		t.Fatal("open failed")
	}
	waitFor(t, 120*time.Second, 500*time.Millisecond, func() bool {
		a.remoteTabMu.Lock()
		tab := a.remoteTabs[meta.ID]
		state := ""
		if tab != nil {
			state = tab.state
		}
		a.remoteTabMu.Unlock()
		return state == "ready"
	})
	// settle: let the pump deliver initial frames
	time.Sleep(2 * time.Second)

	// 2. Baseline message through the credential proxy (real model call).
	// Settle first: the attach window can busy-detach a leftover turn, and
	// frames of non-displayed sessions are (correctly) routed away.
	if !waitFor(t, 60*time.Second, time.Second, func() bool {
		snap, err := a.RemoteTabSnapshot(meta.ID, RemoteTabSnapshotOptions{})
		if err != nil {
			return false
		}
		var st struct {
			Running bool `json:"running"`
		}
		return json.Unmarshal(snap.Status, &st) == nil && !st.Running
	}) {
		t.Fatal("serve never went idle after attach")
	}
	time.Sleep(1 * time.Second)
	if err := a.SubmitRemoteTab(meta.ID, "请只回复两个字符:pong"); err != nil {
		t.Fatalf("baseline submit: %v", err)
	}
	a.remoteTabMu.Lock()
	curPath := a.remoteTabs[meta.ID].currentSessionPath
	a.remoteTabMu.Unlock()
	if !log.waitForFrame(`"kind":"turn_done"`, 120*time.Second) {
		t.Fatal("baseline turn did not finish")
	}
	if log.hasFrame(`"kind":"provider_unreachable"`) {
		t.Fatal("baseline turn hit provider_unreachable")
	}
	t.Logf("baseline message OK (session=%s frames=%d)", curPath, len(log.frames))

	rt, _ := a.remoteRT()
	m := rt.(*desktopRemoteManager)
	beforeURL := remoteConfigBaseURL(t, a, "smoke-box")
	t.Logf("channel baseline base_url=%s", beforeURL)

	// 3. Simulate the SSH blip and wait for the watchdog to re-heal.
	killAppSSHFromRemote(t)
	healed := waitFor(t, 60*time.Second, 500*time.Millisecond, func() bool {
		url := remoteConfigBaseURL(t, a, "smoke-box")
		return url != "" && url != beforeURL
	})
	if !healed {
		t.Fatalf("watchdog did not rewrite base_url (still %q)", remoteConfigBaseURL(t, a, "smoke-box"))
	}
	afterURL := remoteConfigBaseURL(t, a, "smoke-box")
	t.Logf("watchdog re-healed base_url %s -> %s", beforeURL, afterURL)

	// The serve's in-memory providers must have been reloaded to the new
	// port: a follow-up message must succeed without provider_unreachable.
	log2 := &e2eEventLog{}
	a.remoteEventHook = log2.hook
	if err := a.SubmitRemoteTab(meta.ID, "请只回复两个字符:pong"); err != nil {
		t.Fatalf("post-blip submit: %v", err)
	}
	if !log2.waitForFrame(`"kind":"turn_done"`, 180*time.Second) {
		t.Fatal("post-blip turn did not finish")
	}
	if log2.hasFrame(`"kind":"provider_unreachable"`) {
		t.Fatal("post-blip turn hit provider_unreachable (providers not reloaded)")
	}
	t.Logf("post-blip message OK (frames=%d)", len(log2.frames))

	// 4. Cleanup: delete the test session, close the tab, disconnect.
	sessions, err := a.RemoteProjectSessions("smoke-box", "pulsar-lite")
	if err == nil {
		for _, s := range sessions {
			if s.Current {
				_ = a.DeleteRemoteProjectSession("smoke-box", "pulsar-lite", s.Name)
			}
		}
	}
	_ = m.Disconnect("smoke-box")
}

func waitFor(t *testing.T, timeout, step time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(step)
	}
	return false
}

var _ = config.Load // keep import when branches shift
