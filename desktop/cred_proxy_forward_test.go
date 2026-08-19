package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"reasonix/internal/remote/forward"
	"reasonix/internal/remote/sshtest"
)

// TestCredentialProxyReverseForwardEndToEnd drives the real SSH forwarding
// protocol against an sshtest server: the "remote" loopback listener forwards
// connections back through the SSH channel to the desktop-side target, which
// is exactly the path local-proxy model calls take. The helper is idempotent.
func TestCredentialProxyReverseForwardEndToEnd(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("proxy-ok"))
	}))
	defer target.Close()
	targetPort := target.Listener.Addr().(*net.TCPAddr).Port

	srv := sshtest.Start(t, sshtest.Options{})
	cfg := &ssh.ClientConfig{User: "t", HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second}
	cl, err := ssh.Dial("tcp", srv.Addr, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	set := forward.NewSet(nil)
	set.Attach(cl)
	lc := newLifecycleSSHClient(nil)
	lc.forwards = set

	if err := ensureCredentialProxyForward(lc, "box", targetPort); err != nil {
		t.Fatal(err)
	}
	// Idempotent: a second ensure neither errors nor adds a second forward.
	if err := ensureCredentialProxyForward(lc, "box", targetPort); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if n := len(set.List()); n != 1 {
		t.Fatalf("forward count = %d, want 1", n)
	}

	// Dial the REMOTE-side bind address. sshtest runs on localhost, so the
	// remote loopback port is reachable here; the bytes travel
	// dial → ssh server → forwarded-tcpip → ssh client → target.
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(fmt.Sprintf("http://127.0.0.1:%d/hello", credentialProxyRemotePort))
	if err != nil {
		t.Fatalf("reverse forward dial: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "proxy-ok" {
		t.Fatalf("body = %q, want proxy-ok", body)
	}
}
