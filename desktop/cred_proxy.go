package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
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
	model  string
	kind   string
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
	// Unauthenticated liveness endpoint for the desktop's reverse-tunnel
	// probe: the listener only exists behind the SSH reverse forward, so a
	// 204 here proves serve → remote loopback → tunnel → desktop end to end.
	if r.URL.Path == "/healthz" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	token := bearerToken(r.Header.Get("Authorization"))
	p.mu.Lock()
	route := p.routes[token]
	routeCount := len(p.routes)
	p.mu.Unlock()
	if route == nil {
		log.Printf("[remote] credProxy: MISS %s %s tokenPrefix=%q routeCount=%d", r.Method, r.URL.Path, tokenPrefix(token), routeCount)
		http.Error(w, "invalid credential proxy token", http.StatusUnauthorized)
		return
	}
	// The virtual token is the only caller-supplied auth; replace it with the
	// real key in the shape the provider kind expects, and hide the desktop
	// hop from the provider.
	switch route.kind {
	case "anthropic":
		r.Header.Del("Authorization")
		r.Header.Set("x-api-key", route.apiKey)
		r.Header.Set("anthropic-version", "2023-06-01")
	default: // openai-compatible
		r.Header.Set("Authorization", "Bearer "+route.apiKey)
	}
	r.Header.Del("X-Forwarded-For")
	if route.model != "" && r.Body != nil && (r.Method == http.MethodPost || r.Method == http.MethodPut) {
		const rewriteLimit = 8 << 20
		buffered, err := io.ReadAll(io.LimitReader(r.Body, rewriteLimit+1))
		switch {
		case err != nil:
			// Read failure: forward whatever arrived; the proxy cannot
			// repair a request body that broke mid-stream.
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(buffered))
		case int64(len(buffered)) > rewriteLimit:
			// Too large to buffer-and-rewrite: stream the buffered prefix
			// plus the unread remainder through untouched instead of
			// forwarding a truncated (corrupt) document.
			prefix := bytes.NewReader(buffered)
			r.Body = struct {
				io.Reader
				io.Closer
			}{io.MultiReader(prefix, r.Body), r.Body}
			r.ContentLength = -1
			r.Header.Del("Content-Length")
		default:
			_ = r.Body.Close()
			body := rewriteJSONModel(buffered, route.model)
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			r.Header.Set("Content-Length", strconv.Itoa(len(body)))
		}
	}
	route.proxy.ServeHTTP(w, r)
}

func rewriteJSONModel(body []byte, model string) []byte {
	if model == "" || len(body) == 0 {
		return body
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil || payload == nil {
		// Unparseable or a literal null body ("null" decodes into a nil
		// map): assigning into nil would panic, and there is nothing to
		// rewrite — pass the body through untouched.
		return body
	}
	payload["model"] = model
	out, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return out
}

func (p *credentialProxy) setRoute(token string, upstream *url.URL, apiKey, model, kind string) {
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	// Stream model responses (SSE) through without buffering.
	proxy.FlushInterval = -1
	// NewSingleHostReverseProxy's director rewrites the URL but leaves
	// req.Host untouched, so the inbound loopback Host (127.0.0.1:<proxy
	// port>, or the remote tunnel port) would travel to the provider as the
	// Host header — CloudFront-fronted APIs answer a foreign Host with 403.
	// Route by the upstream's own host instead.
	upstreamHost := upstream.Host
	director := proxy.Director
	proxy.Director = func(req *http.Request) {
		director(req)
		req.Host = upstreamHost
	}
	if kind == "" {
		kind = "openai"
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.routes[token] = &credProxyRoute{proxy: proxy, apiKey: apiKey, model: model, kind: kind}
}

// tokenPrefix leaks only a non-reversible prefix for diagnosis.
func tokenPrefix(t string) string {
	if len(t) > 8 {
		return t[:8]
	}
	return t
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

// credentialProxySecret loads (creating on first use) the persisted random
// secret every virtual token derives from. Rotating it revokes all tokens.
func (a *App) credentialProxySecret() (string, error) {
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
	return p.CredentialProxySecret, nil
}

// credentialProxyTokenFor derives a virtual token. Tokens are stable across
// desktop restarts, so a reused remote serve keeps working; an empty
// workspace derives the legacy host-level token installed by serves from
// before the per-workspace split.
func credentialProxyTokenFor(secret, hostID, workspace string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("reasonix-credential-proxy:" + hostID))
	if workspace != "" {
		mac.Write([]byte(":" + workspace))
	}
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

func (a *App) credentialProxyToken(hostID, workspace string) (string, error) {
	secret, err := a.credentialProxySecret()
	if err != nil {
		return "", err
	}
	return credentialProxyTokenFor(secret, hostID, workspace), nil
}

// credentialProxyRouteInfo is everything a serve bootstrap needs to install
// the desktop hop on the remote: the virtual token, the model name and
// provider kind the remote provider entry should carry, and the proxy's
// loopback port.
type credentialProxyRouteInfo struct {
	token string
	model string
	kind  string
	port  int
}

// proxyUpstream is the resolved desktop-side provider a route forwards to.
type proxyUpstream struct {
	apiKey string
	url    *url.URL
	model  string
	kind   string
}

// resolveProxyProvider resolves a desktop model ref into the upstream the
// credential proxy should forward to, including the auth-header shape its
// provider kind expects.
func resolveProxyProvider(cfg *config.Config, ref string) (proxyUpstream, error) {
	entry, ok := cfg.ResolveModel(ref)
	if !ok {
		return proxyUpstream{}, fmt.Errorf("credential proxy: model %q has no provider", ref)
	}
	apiKey := config.ResolveCredential(entry.APIKeyEnv).Value
	if apiKey == "" {
		return proxyUpstream{}, fmt.Errorf("credential proxy: %s is not set — the local key is required in local-proxy mode", entry.APIKeyEnv)
	}
	base := strings.TrimSpace(entry.BaseURL)
	if base == "" {
		base = "https://api.openai.com"
	}
	upstream, err := url.Parse(strings.TrimRight(base, "/") + "/")
	if err != nil {
		return proxyUpstream{}, fmt.Errorf("credential proxy: provider base_url: %w", err)
	}
	kind := strings.TrimSpace(entry.Kind)
	if kind == "" {
		kind = "openai"
	}
	return proxyUpstream{apiKey: apiKey, url: upstream, model: entry.Model, kind: kind}, nil
}

// registerCredentialProxyRoute prepares local-proxy credential mode for one
// workspace: starts the desktop key holder, resolves the model that
// workspace's tabs run (falling back to the desktop default), and registers
// the workspace's virtual token. The legacy host-level token is re-registered
// afterwards so serves reused from before the split stay authenticated.
func (a *App) registerCredentialProxyRoute(hostID, workspace string) (credentialProxyRouteInfo, error) {
	cfg, err := config.Load()
	if err != nil {
		return credentialProxyRouteInfo{}, err
	}
	ref := strings.TrimSpace(cfg.DefaultModel)
	if wsModel := a.desktopModelForWorkspace(hostID, workspace); wsModel != "" {
		ref = wsModel
	}
	info, err := a.applyCredentialProxyModel(hostID, workspace, ref)
	if err != nil {
		return credentialProxyRouteInfo{}, err
	}
	a.applyLegacyCredentialProxyRoute(hostID)
	return info, nil
}

// desktopModelForWorkspace deterministically picks the model to install for a
// workspace's serve: the most recently set model among that workspace's tabs.
// Map iteration order must never decide this.
func (a *App) desktopModelForWorkspace(hostID, workspace string) string {
	a.remoteTabMu.Lock()
	defer a.remoteTabMu.Unlock()
	best := ""
	var bestSeq uint64
	for _, tab := range a.remoteTabs {
		if tab == nil || tab.ref.HostID != hostID || tab.ref.Workspace != workspace || strings.TrimSpace(tab.model) == "" {
			continue
		}
		if tab.modelSeq >= bestSeq {
			best, bestSeq = tab.model, tab.modelSeq
		}
	}
	return best
}

// desktopModelForHost is the host-wide variant used by the legacy token route.
func (a *App) desktopModelForHost(hostID string) string {
	a.remoteTabMu.Lock()
	defer a.remoteTabMu.Unlock()
	best := ""
	var bestSeq uint64
	for _, tab := range a.remoteTabs {
		if tab == nil || tab.ref.HostID != hostID || strings.TrimSpace(tab.model) == "" {
			continue
		}
		if tab.modelSeq >= bestSeq {
			best, bestSeq = tab.model, tab.modelSeq
		}
	}
	return best
}

// applyCredentialProxyModel resolves ref on the desktop, starts the proxy if
// needed, and (re)binds the workspace's virtual token to that provider.
func (a *App) applyCredentialProxyModel(hostID, workspace, ref string) (credentialProxyRouteInfo, error) {
	cfg, err := config.Load()
	if err != nil {
		return credentialProxyRouteInfo{}, err
	}
	up, err := resolveProxyProvider(cfg, ref)
	if err != nil {
		return credentialProxyRouteInfo{}, err
	}
	port, err := a.credentialProxyPort()
	if err != nil {
		return credentialProxyRouteInfo{}, err
	}
	a.credProxyMu.Lock()
	proxy := a.credProxy
	a.credProxyMu.Unlock()
	if proxy == nil {
		return credentialProxyRouteInfo{}, fmt.Errorf("credential proxy: not running")
	}
	token, err := a.credentialProxyToken(hostID, workspace)
	if err != nil {
		return credentialProxyRouteInfo{}, err
	}
	proxy.setRoute(token, up.url, up.apiKey, up.model, up.kind)
	return credentialProxyRouteInfo{token: token, model: up.model, kind: up.kind, port: port}, nil
}

// applyLegacyCredentialProxyRoute re-binds the pre-split host-level token to
// the host's effective default model. Best effort: serves already running
// keep their in-memory route until the next ensure re-registers it.
func (a *App) applyLegacyCredentialProxyRoute(hostID string) {
	cfg, err := config.Load()
	if err != nil {
		return
	}
	ref := strings.TrimSpace(cfg.DefaultModel)
	if hostModel := a.desktopModelForHost(hostID); hostModel != "" {
		ref = hostModel
	}
	if ref == "" {
		return
	}
	up, err := resolveProxyProvider(cfg, ref)
	if err != nil {
		return
	}
	a.credProxyMu.Lock()
	proxy := a.credProxy
	a.credProxyMu.Unlock()
	if proxy == nil {
		return
	}
	token, err := a.credentialProxyToken(hostID, "")
	if err != nil {
		return
	}
	proxy.setRoute(token, up.url, up.apiKey, up.model, up.kind)
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
