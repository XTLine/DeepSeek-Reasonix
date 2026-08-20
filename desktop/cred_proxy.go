package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"

	"reasonix/internal/config"
)

// Local-proxy credential mode: the remote serve's model calls route back to
// this desktop over the SSH reverse tunnel, and this proxy swaps the virtual
// token for the real provider key. The real key lives only in desktop memory
// and the local .env — it never crosses the wire.

// credentialProxyProviderName is the provider entry the bootstrap installs in
// the remote config; the serve launches with --model <name>.
const credentialProxyProviderName = "reasonix-desktop-proxy"

type credProxyRoute struct {
	proxy  *httputil.ReverseProxy
	apiKey string
}

// credentialProxy is the desktop-side key holder: a loopback HTTP endpoint
// that authenticates requests by virtual token and forwards them to the real
// provider with the real key. One instance serves the whole app.
type credentialProxy struct {
	mu     sync.Mutex
	ln     net.Listener
	server *http.Server
	port   int
	routes map[string]*credProxyRoute
}

func (p *credentialProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r.Header.Get("Authorization"))
	p.mu.Lock()
	route := p.routes[token]
	p.mu.Unlock()
	if route == nil {
		http.Error(w, "invalid credential proxy token", http.StatusUnauthorized)
		return
	}
	// The virtual token is the only caller-supplied auth; replace it with the
	// real key and hide the desktop hop from the provider.
	r.Header.Set("Authorization", "Bearer "+route.apiKey)
	r.Header.Del("X-Forwarded-For")
	route.proxy.ServeHTTP(w, r)
}

func (p *credentialProxy) setRoute(token string, upstream *url.URL, apiKey string) {
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	// Stream model responses (SSE) through without buffering.
	proxy.FlushInterval = -1
	p.mu.Lock()
	defer p.mu.Unlock()
	p.routes[token] = &credProxyRoute{proxy: proxy, apiKey: apiKey}
}

func (p *credentialProxy) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.server != nil {
		_ = p.server.Close()
	}
	if p.ln != nil {
		_ = p.ln.Close()
	}
}

func bearerToken(header string) string {
	prefix, value, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(prefix, "Bearer") {
		return ""
	}
	return strings.TrimSpace(value)
}

// credentialProxyPort returns the proxy's loopback port, starting the proxy
// on first use.
func (a *App) credentialProxyPort() (int, error) {
	a.credProxyMu.Lock()
	defer a.credProxyMu.Unlock()
	if a.credProxy != nil {
		return a.credProxy.port, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("credential proxy: listen: %w", err)
	}
	p := &credentialProxy{ln: ln, port: ln.Addr().(*net.TCPAddr).Port, routes: map[string]*credProxyRoute{}}
	p.server = &http.Server{Handler: p}
	a.credProxy = p
	a.goSafe("credentialProxy", func() { _ = p.server.Serve(ln) })
	return p.port, nil
}

func (a *App) closeCredentialProxy() {
	a.credProxyMu.Lock()
	defer a.credProxyMu.Unlock()
	if a.credProxy != nil {
		a.credProxy.close()
		a.credProxy = nil
	}
}

// credentialProxyToken derives the host's virtual token. It is stable across
// desktop restarts (derived from a persisted random secret) so a reused
// remote serve keeps working after the desktop restarts; rotating the secret
// revokes every token at once.
func (a *App) credentialProxyToken(hostID string) (string, error) {
	remotePrefsMu.Lock()
	defer remotePrefsMu.Unlock()
	p := loadRemotePrefs()
	if p.CredentialProxySecret == "" {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("credential proxy: generate secret: %w", err)
		}
		p.CredentialProxySecret = hex.EncodeToString(buf)
		saveRemotePrefs(p)
		if loadRemotePrefs().CredentialProxySecret == "" {
			return "", fmt.Errorf("credential proxy: secret did not persist")
		}
	}
	mac := hmac.New(sha256.New, []byte(p.CredentialProxySecret))
	mac.Write([]byte("reasonix-credential-proxy:" + hostID))
	return hex.EncodeToString(mac.Sum(nil))[:32], nil
}

// registerCredentialProxyRoute starts the proxy (if needed), resolves the
// desktop's default provider as the upstream, and registers the host's
// virtual token. It returns the token to inject into the remote serve, the
// model name the remote provider entry should carry, and the proxy's
// loopback port.
func (a *App) registerCredentialProxyRoute(hostID string) (string, string, int, error) {
	cfg, err := config.Load()
	if err != nil {
		return "", "", 0, err
	}
	entry, ok := cfg.ResolveModel(cfg.DefaultModel)
	if !ok {
		return "", "", 0, fmt.Errorf("credential proxy: default model %q has no provider", cfg.DefaultModel)
	}
	apiKey := config.ResolveCredential(entry.APIKeyEnv).Value
	if apiKey == "" {
		return "", "", 0, fmt.Errorf("credential proxy: %s is not set — the local key is required in local-proxy mode", entry.APIKeyEnv)
	}
	base := strings.TrimSpace(entry.BaseURL)
	if base == "" {
		base = "https://api.openai.com"
	}
	upstream, err := url.Parse(strings.TrimRight(base, "/") + "/")
	if err != nil {
		return "", "", 0, fmt.Errorf("credential proxy: provider base_url: %w", err)
	}
	port, err := a.credentialProxyPort()
	if err != nil {
		return "", "", 0, err
	}
	token, err := a.credentialProxyToken(hostID)
	if err != nil {
		return "", "", 0, err
	}
	a.credProxy.setRoute(token, upstream, apiKey)
	return token, entry.Model, port, nil
}

// credentialModeView returns the host entry's normalized credential mode for
// views ("" reads as "remote" — the default).
func credentialModeView(h config.RemoteHostEntry) string {
	if h.CredentialProxyEnabled() {
		return "local-proxy"
	}
	return "remote"
}

// normalizeCredentialMode validates an input credential mode.
func normalizeCredentialMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "local-proxy":
		return "local-proxy"
	default:
		return ""
	}
}
