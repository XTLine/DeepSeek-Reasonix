package main

import (
	"fmt"
	"io"
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
	a.credProxy.setRoute(token, mustParseURL(t, upstream.URL), "sk-real-key", "", "")
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

// TestCredentialProxyRewritesRequestModel: desktop owns the current model, so
// the proxy replaces the serve's request-body model with the desktop selection
// before the real provider sees it. The provider must also see its OWN host
// in the Host header — the inbound loopback host must not leak through
// (CloudFront-fronted APIs 403 a foreign Host).
func TestCredentialProxyRewritesRequestModel(t *testing.T) {
	var gotBody, gotHost string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		gotBody = string(buf)
		gotHost = r.Host
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
	a.credProxy.setRoute(token, mustParseURL(t, upstream.URL), "sk-real-key", "deepseek-v4-pro", "openai")

	req, err := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", port), strings.NewReader(`{"model":"deepseek-v4-flash","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer virtual-tok")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(gotBody, `"model":"deepseek-v4-pro"`) {
		t.Fatalf("upstream body = %q, want rewritten model deepseek-v4-pro", gotBody)
	}
	if strings.Contains(gotBody, "deepseek-v4-flash") {
		t.Fatalf("upstream still saw the serve's model: %q", gotBody)
	}
	if want := strings.TrimPrefix(strings.TrimPrefix(upstream.URL, "http://"), "http://"); gotHost != want {
		t.Fatalf("upstream Host = %q, want the upstream's own host %q", gotHost, want)
	}
}

// TestCredentialProxyTokenStableAcrossRestarts: the virtual token derives
// from a persisted secret, so a restarted desktop keeps the same token (a
// reused remote serve keeps working); different hosts and different
// workspaces derive different tokens.
func TestCredentialProxyTokenStableAcrossRestarts(t *testing.T) {
	seedBridgeTestHost(t, "box")
	a1 := &App{}
	i1, err := a1.registerCredentialProxyRoute("box", "~/app")
	if err != nil {
		t.Fatal(err)
	}
	a2 := &App{}
	i2, err := a2.registerCredentialProxyRoute("box", "~/app")
	if err != nil {
		t.Fatal(err)
	}
	if i1.token == "" || i1.token != i2.token {
		t.Fatalf("token drifted across App instances: %q vs %q", i1.token, i2.token)
	}
	i3, err := a2.registerCredentialProxyRoute("other", "~/app")
	if err != nil {
		t.Fatal(err)
	}
	if i3.token == i1.token {
		t.Fatalf("different hosts share a token: %q", i1.token)
	}
	i4, err := a2.registerCredentialProxyRoute("box", "~/other")
	if err != nil {
		t.Fatal(err)
	}
	if i4.token == i1.token {
		t.Fatalf("different workspaces share a token: %q", i1.token)
	}
}

// TestCredentialProxyLegacyTokenDerivation pins the pre-split host-level
// derivation: reused serves installed before per-workspace tokens present it,
// so the legacy route must stay derivable and distinct from workspace tokens.
func TestCredentialProxyLegacyTokenDerivation(t *testing.T) {
	legacy := credentialProxyTokenFor("secret", "box", "")
	sameLegacy := credentialProxyTokenFor("secret", "box", "")
	ws := credentialProxyTokenFor("secret", "box", "~/app")
	if legacy == "" || legacy != sameLegacy {
		t.Fatalf("legacy token unstable: %q vs %q", legacy, sameLegacy)
	}
	if legacy == ws {
		t.Fatalf("legacy token collides with a workspace token: %q", legacy)
	}
	if otherHost := credentialProxyTokenFor("secret", "other", ""); otherHost == legacy {
		t.Fatalf("legacy token collides across hosts: %q", legacy)
	}
}

// TestCredentialProxyAnthropicAuthShape: an anthropic-kind route swaps the
// virtual token for x-api-key (+ anthropic-version) instead of a bearer
// header.
func TestCredentialProxyAnthropicAuthShape(t *testing.T) {
	var gotKey, gotVersion, gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	seedBridgeTestHost(t, "box")
	a := &App{}
	t.Cleanup(a.closeCredentialProxy)
	port, err := a.credentialProxyPort()
	if err != nil {
		t.Fatal(err)
	}
	a.credProxy.setRoute("virtual-tok", mustParseURL(t, upstream.URL), "sk-real-key", "", "anthropic")
	req, err := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/v1/messages", port), strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer virtual-tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if gotKey != "sk-real-key" {
		t.Fatalf("x-api-key = %q, want the real key", gotKey)
	}
	if gotVersion == "" {
		t.Fatal("anthropic-version header missing")
	}
	if gotAuth != "" {
		t.Fatalf("Authorization header leaked to the anthropic upstream: %q", gotAuth)
	}
}

// TestRewriteJSONModelGuards: a literal null body must pass through without
// panicking (assigning into a nil map would), and a non-JSON body stays
// untouched.
func TestRewriteJSONModelGuards(t *testing.T) {
	if got := rewriteJSONModel([]byte("null"), "m"); string(got) != "null" {
		t.Fatalf("null body rewritten: %q", got)
	}
	if got := rewriteJSONModel([]byte("not json"), "m"); string(got) != "not json" {
		t.Fatalf("non-JSON body rewritten: %q", got)
	}
	if got := rewriteJSONModel([]byte(`{"model":"a"}`), ""); string(got) != `{"model":"a"}` {
		t.Fatalf("empty model rewrote the body: %q", got)
	}
	if got := rewriteJSONModel([]byte(`{"model":"a"}`), "b"); !strings.Contains(string(got), `"model":"b"`) {
		t.Fatalf("model not rewritten: %q", got)
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
