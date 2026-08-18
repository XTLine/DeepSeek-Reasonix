package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"time"
)

// The remote-tab bridge talks to the workspace Serve over the loopback
// tunnel using the same auth flow the web shell uses: one POST /auth/token
// exchanges the pre-shared token for an HttpOnly session cookie, so the
// token never appears in a request line or the Serve access log. All
// subsequent API and SSE requests carry the cookie from the client's jar.

// serveHandshake exchanges the pre-shared token for the session cookie.
// Serve replies 204 on success; the cookie lands in client's jar.
func serveHandshake(ctx context.Context, client *http.Client, base, token string) error {
	body, err := json.Marshal(map[string]string{"token": token})
	if err != nil {
		return err
	}
	resp, err := serveDo(ctx, client, http.MethodPost, base+"/auth/token", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return fmt.Errorf("serve auth handshake: status %d", resp.StatusCode)
}

// serveSessionEntry mirrors one GET /sessions row from the Serve.
type serveSessionEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Title string `json:"title"`
	Turns int    `json:"turns"`
}

// enterRemoteSession lands the tab inside a Serve session: POST /new for a
// fresh session, or POST /resume for a named one. /resume takes the session
// PATH (not the /sessions name), so a SessionName open resolves the name
// against GET /sessions first. A tab must never rest in a session-less
// ready shell.
func enterRemoteSession(ctx context.Context, client *http.Client, base string, opts RemoteTabOpenOptions) error {
	name := strings.TrimSpace(opts.SessionName)
	if opts.NewSession || name == "" {
		return servePost(ctx, client, base+"/new", nil)
	}
	sessions, err := serveSessions(ctx, client, base)
	if err != nil {
		return err
	}
	for _, s := range sessions {
		if s.Name == name {
			body, err := json.Marshal(map[string]string{"path": s.Path})
			if err != nil {
				return err
			}
			return servePost(ctx, client, base+"/resume", body)
		}
	}
	return fmt.Errorf("remote session %q not found", name)
}

// attachRemoteTabServe builds the tab's Serve client and starts its event
// pump. Ordering matters: the pump subscribes to /events BEFORE the session
// is entered, so the frames emitted by POST /new or /resume are not missed.
// ctx must outlive the call (the pump derives from it); the handshake and
// session entry run under a bounded sub-context.
func (a *App) attachRemoteTabServe(ctx context.Context, tabID, base, token string, opts RemoteTabOpenOptions) error {
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	jar, err := cookiejar.New(nil)
	if err != nil {
		return err
	}
	client := &http.Client{Jar: jar} // no overall timeout: /events is long-lived; per-call contexts bound the rest
	if err := serveHandshake(callCtx, client, base, token); err != nil {
		return err
	}

	a.remoteTabMu.Lock()
	tab := a.remoteTabs[tabID]
	if tab == nil {
		a.remoteTabMu.Unlock()
		return fmt.Errorf("remote tab %q closed during bootstrap", tabID)
	}
	tab.client = client
	tab.base = base
	tab.token = token
	pumpCtx, cancelPump := context.WithCancel(ctx)
	tab.cancel = cancelPump
	a.remoteTabMu.Unlock()

	a.goSafe("remoteTabPump", func() { a.remoteTabPump(pumpCtx, tabID, tab.gen) })
	return enterRemoteSession(callCtx, client, base, opts)
}

// remoteTabPump streams the Serve event feed for one tab generation and
// re-emits each data frame verbatim as remote-tab:{id}:event. It exits on
// ctx cancel (close/reconnect) or stream death; a generation mismatch means
// a newer pump owns the tab now, so it stays silent.
func (a *App) remoteTabPump(ctx context.Context, tabID string, gen uint64) {
	a.remoteTabMu.Lock()
	tab := a.remoteTabs[tabID]
	var client *http.Client
	var base string
	if tab != nil && tab.gen == gen {
		client, base = tab.client, tab.base
	}
	a.remoteTabMu.Unlock()
	if client == nil || base == "" {
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/events", nil)
	if err != nil {
		a.emitRemoteTabState(tabID, "error", err.Error())
		return
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			a.emitRemoteTabState(tabID, "error", err.Error())
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		a.emitRemoteTabState(tabID, "error", fmt.Sprintf("serve /events: status %d", resp.StatusCode))
		return
	}
	br := bufio.NewReader(resp.Body)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(line, "data:") {
			continue // ": ping" keepalives and other SSE fields
		}
		frame := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if frame == "" {
			continue
		}
		a.emitRemoteEvent(fmt.Sprintf("remote-tab:%s:event", tabID), json.RawMessage(frame))
	}
	// The stream died on its own. Only the current generation reports it;
	// a reconnect will supersede this pump silently.
	a.remoteTabMu.Lock()
	stillCurrent := a.remoteTabs[tabID] != nil && a.remoteTabs[tabID].gen == gen
	a.remoteTabMu.Unlock()
	if stillCurrent && ctx.Err() == nil {
		a.emitRemoteTabState(tabID, "error", "remote event stream closed")
	}
}

func servePost(ctx context.Context, client *http.Client, url string, body []byte) error {
	if body == nil {
		body = []byte("{}")
	}
	resp, err := serveDo(ctx, client, http.MethodPost, url, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("%s: status %d", url, resp.StatusCode)
}

func serveSessions(ctx context.Context, client *http.Client, base string) ([]serveSessionEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/sessions", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("serve /sessions: status %d", resp.StatusCode)
	}
	var out []serveSessionEntry
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func serveDo(ctx context.Context, client *http.Client, method, url string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json") // the csrf guard rejects non-JSON POSTs
	return client.Do(req)
}
