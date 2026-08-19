package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"reasonix/internal/config"
)

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(strings.TrimRight(raw, "/") + "/")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// TestCredentialProxyAuthSwap covers the desktop key holder over real HTTP:
// the registered virtual token forwards to the provider with the real key,
// anything else is rejected without reaching the provider.
func TestCredentialProxyAuthSwap(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("model-ok"))
	}))
	defer upstream.Close()

	seedBridgeTestHost(t, "box")
	a := &App{}
	t.Cleanup(a.closeCredentialProxy)
	port, err := a.credentialProxyPort()
	if err != nil {
		t.Fatal(err)
	}
	const token = "virtual-tok"
	a.credProxy.setRoute(token, mustParseURL(t, upstream.URL), "sk-real-key")
	proxyURL := fmt.Sprintf("http://127.0.0.1:%d/v1/chat", port)

	do := func(auth string) (int, string) {
		req, err := http.NewRequest("POST", proxyURL, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", auth)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		buf := make([]byte, 64)
		n, _ := resp.Body.Read(buf)
		return resp.StatusCode, string(buf[:n])
	}

	if code, body := do("Bearer virtual-tok"); code != 200 || body != "model-ok" {
		t.Fatalf("valid token: code=%d body=%q", code, body)
	}
	if gotAuth != "Bearer sk-real-key" {
		t.Fatalf("upstream auth = %q, want the real key", gotAuth)
	}
	if code, _ := do("Bearer wrong"); code != 401 {
		t.Fatalf("wrong token: code=%d, want 401", code)
	}
	if gotAuth != "Bearer sk-real-key" {
		t.Fatalf("rejected request reached the upstream: %q", gotAuth)
	}
	if code, _ := do(""); code != 401 {
		t.Fatalf("missing token: code=%d, want 401", code)
	}
}

// TestCredentialProxyTokenStableAcrossRestarts: the virtual token derives
// from a persisted secret, so a restarted desktop keeps the same token (a
// reused remote serve keeps working); different hosts derive different
// tokens.
func TestCredentialProxyTokenStableAcrossRestarts(t *testing.T) {
	seedBridgeTestHost(t, "box")
	a1 := &App{}
	t1, _, _, err := a1.registerCredentialProxyRoute("box")
	if err != nil {
		t.Fatal(err)
	}
	a2 := &App{}
	t2, _, _, err := a2.registerCredentialProxyRoute("box")
	if err != nil {
		t.Fatal(err)
	}
	if t1 == "" || t1 != t2 {
		t.Fatalf("token drifted across App instances: %q vs %q", t1, t2)
	}
	t3, _, _, err := a2.registerCredentialProxyRoute("other")
	if err != nil {
		t.Fatal(err)
	}
	if t3 == t1 {
		t.Fatalf("different hosts share a token: %q", t1)
	}
}

// TestCredentialModeConfigRoundTrip pins the host entry field end to end.
func TestCredentialModeConfigRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	t.Setenv("HOME", home)
	if err := editUserConfig(func(c *config.Config) error {
		return c.UpsertRemoteHost(config.RemoteHostEntry{
			Name: "p", Host: "127.0.0.1", CredentialMode: "local-proxy",
		})
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := cfg.RemoteHost("p")
	if !ok || !entry.CredentialProxyEnabled() {
		t.Fatalf("credential mode did not round-trip: %+v", entry)
	}
	if v := credentialModeView(entry); v != "local-proxy" {
		t.Fatalf("view mode = %q", v)
	}
	if n := normalizeCredentialMode("bogus"); n != "" {
		t.Fatalf("bogus mode normalized to %q", n)
	}
}
