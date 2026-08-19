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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"reasonix/internal/config"
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
	Name       string `json:"name"`
	Path       string `json:"path"`
	Title      string `json:"title"`
	Turns      int    `json:"turns"`
	Current    bool   `json:"current"`
	MtimeMilli int64  `json:"mtimeMilli"`
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
		var probe struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal([]byte(frame), &probe) == nil && probe.Kind == "turn_done" {
			// The serve generates the session title from the finished
			// conversation; pick it up shortly after the turn settles.
			a.goSafe("remoteTabTitle", func() {
				time.Sleep(1500 * time.Millisecond)
				a.refreshRemoteTabTitle(tabID)
			})
		}
	}
	// The stream died without an explicit stop: only the current generation
	// reacts. Treat it as a transient serve/tunnel drop, flag reconnecting,
	// and try one re-attach now; the host status hook retries again on the
	// next connected transition.
	a.remoteTabMu.Lock()
	stillCurrent := a.remoteTabs[tabID] != nil && a.remoteTabs[tabID].gen == gen
	a.remoteTabMu.Unlock()
	if stillCurrent && ctx.Err() == nil {
		a.emitRemoteTabState(tabID, "reconnecting", "")
		a.goSafe("remoteTabReattach", func() { a.reattachRemoteTab(tabID) })
	}
}

// servePost posts body and surfaces the response text in errors — the serve
// renders "session in use" lease refusals with their close hint in the body,
// and that hint must reach the tab surface.
func servePost(ctx context.Context, client *http.Client, url string, body []byte) error {
	if body == nil {
		body = []byte("{}")
	}
	resp, err := serveDo(ctx, client, http.MethodPost, url, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if msg := strings.TrimSpace(string(data)); msg != "" {
		return fmt.Errorf("%s: status %d: %s", url, resp.StatusCode, msg)
	}
	return fmt.Errorf("%s: status %d", url, resp.StatusCode)
}

// serveGet fetches a JSON member of the tab snapshot, returning the raw
// payload for verbatim passthrough.
func serveGet(ctx context.Context, client *http.Client, url string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: status %d", url, resp.StatusCode)
	}
	return json.RawMessage(data), nil
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

// commandContext bounds one proxied command. Boot context when available;
// the timeout keeps a wedged tunnel from hanging the binding call.
func commandContext(a *App) (context.Context, context.CancelFunc) {
	ctx := a.bootContext()
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, 15*time.Second)
}

// remoteTabCommandClient resolves a tabID to its live serve client. A tab
// that has not finished bootstrap, is reconnecting, or has failed is an
// error, not a silent no-op.
func (a *App) remoteTabCommandClient(tabID string) (*http.Client, string, error) {
	a.remoteTabMu.Lock()
	tab := a.remoteTabs[tabID]
	var client *http.Client
	var base string
	usable := tab != nil && tab.client != nil && tab.state != "reconnecting" && tab.state != "error"
	if usable {
		client, base = tab.client, tab.base
	}
	a.remoteTabMu.Unlock()
	if !usable {
		return nil, "", fmt.Errorf("remote tab %q is not connected", tabID)
	}
	return client, base, nil
}

func (a *App) SubmitRemoteTab(tabID, text string) error {
	client, base, err := a.remoteTabCommandClient(tabID)
	if err != nil {
		return err
	}
	ctx, cancel := commandContext(a)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"input": text})
	return servePost(ctx, client, base+"/submit", body)
}

func (a *App) CancelRemoteTab(tabID string) error {
	client, base, err := a.remoteTabCommandClient(tabID)
	if err != nil {
		return err
	}
	ctx, cancel := commandContext(a)
	defer cancel()
	return servePost(ctx, client, base+"/cancel", nil)
}

// ApproveRemoteTab answers a tool-approval request. Serve takes
// {id, allow, session, persist}; the frontend's decision string maps to the
// allow bool ("allow" ⇒ true), session/persist stay false.
func (a *App) ApproveRemoteTab(tabID, callID, decision string) error {
	client, base, err := a.remoteTabCommandClient(tabID)
	if err != nil {
		return err
	}
	ctx, cancel := commandContext(a)
	defer cancel()
	body, _ := json.Marshal(map[string]any{"id": callID, "allow": strings.EqualFold(strings.TrimSpace(decision), "allow")})
	return servePost(ctx, client, base+"/approve", body)
}

// AnswerRemoteTab answers an ask_request. Serve decodes event.AskAnswer
// (no json tags ⇒ fields marshal as QuestionID/Selected); callID doubles as
// the question id for the single-answer desktop shape.
func (a *App) AnswerRemoteTab(tabID, callID, answer string) error {
	client, base, err := a.remoteTabCommandClient(tabID)
	if err != nil {
		return err
	}
	ctx, cancel := commandContext(a)
	defer cancel()
	body, _ := json.Marshal(map[string]any{
		"id":      callID,
		"answers": []map[string]any{{"QuestionID": callID, "Selected": []string{answer}}},
	})
	return servePost(ctx, client, base+"/answer", body)
}

// RewindRemoteTab rewinds to a checkpoint. Serve identifies checkpoints by
// TURN index and takes {turn, scope}; the checkpointID string is that turn.
func (a *App) RewindRemoteTab(tabID, checkpointID string) error {
	client, base, err := a.remoteTabCommandClient(tabID)
	if err != nil {
		return err
	}
	ctx, cancel := commandContext(a)
	defer cancel()
	turn, convErr := strconv.Atoi(strings.TrimSpace(checkpointID))
	if convErr != nil {
		return fmt.Errorf("invalid checkpoint id %q: want the turn index", checkpointID)
	}
	body, _ := json.Marshal(map[string]any{"turn": turn, "scope": "both"})
	return servePost(ctx, client, base+"/rewind", body)
}

func (a *App) SetRemoteTabToolApprovalMode(tabID, mode string) error {
	client, base, err := a.remoteTabCommandClient(tabID)
	if err != nil {
		return err
	}
	ctx, cancel := commandContext(a)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"mode": mode})
	return servePost(ctx, client, base+"/tool-approval-mode", body)
}

func (a *App) SetRemoteTabGoal(tabID, goal string) error {
	client, base, err := a.remoteTabCommandClient(tabID)
	if err != nil {
		return err
	}
	ctx, cancel := commandContext(a)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"goal": goal})
	return servePost(ctx, client, base+"/goal", body)
}

// RemoteTabSnapshot mirrors the frontend shape: raw serve payloads passed
// through verbatim so the surface decides how to consume them.
type RemoteTabSnapshot struct {
	History     json.RawMessage `json:"history"`
	Context     json.RawMessage `json:"context,omitempty"`
	Todos       json.RawMessage `json:"todos,omitempty"`
	Checkpoints json.RawMessage `json:"checkpoints,omitempty"`
	Models      json.RawMessage `json:"models,omitempty"`
	Status      json.RawMessage `json:"status,omitempty"`
}

// RemoteTabSnapshot merges the serve's GET members in parallel. Only
// /history is required; the optional members degrade to absent on failure.
func (a *App) RemoteTabSnapshot(tabID string) (RemoteTabSnapshot, error) {
	client, base, err := a.remoteTabCommandClient(tabID)
	if err != nil {
		return RemoteTabSnapshot{}, err
	}
	ctx, cancel := commandContext(a)
	defer cancel()
	var snap RemoteTabSnapshot
	var wg sync.WaitGroup
	var mu sync.Mutex
	var historyErr error
	for path, dst := range map[string]*json.RawMessage{
		"/history":     &snap.History,
		"/context":     &snap.Context,
		"/todos":       &snap.Todos,
		"/checkpoints": &snap.Checkpoints,
		"/models":      &snap.Models,
		"/status":      &snap.Status,
	} {
		wg.Add(1)
		go func(path string, dst *json.RawMessage) {
			defer wg.Done()
			data, err := serveGet(ctx, client, base+path)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if path == "/history" && historyErr == nil {
					historyErr = err
				}
				return
			}
			*dst = data
		}(path, dst)
	}
	wg.Wait()
	if historyErr != nil {
		return RemoteTabSnapshot{}, historyErr
	}
	if len(snap.History) == 0 {
		return RemoteTabSnapshot{}, fmt.Errorf("remote tab %q: empty history", tabID)
	}
	return snap, nil
}

// RemoteSessionView mirrors one serve /sessions entry on the frontend side.
type RemoteSessionView struct {
	Name           string `json:"name"`
	Title          string `json:"title,omitempty"`
	Turns          int    `json:"turns,omitempty"`
	Current        bool   `json:"current,omitempty"`
	LastActivityAt int64  `json:"lastActivityAt,omitempty"`
	Pinned         bool   `json:"pinned,omitempty"`
}

// serveClientForRef resolves an HTTP client for a host+workspace: a live
// tab's client when one is open, otherwise a one-shot EnsureServer +
// handshake (no pump, no tab). The returned done() releases the one-shot
// context; for a live tab it is a no-op.
func (a *App) serveClientForRef(hostID, workspace string) (*http.Client, string, func(), error) {
	a.remoteTabMu.Lock()
	for _, tab := range a.remoteTabs {
		if tab.ref.HostID == hostID && tab.ref.Workspace == workspace && tab.client != nil {
			client, base := tab.client, tab.base
			a.remoteTabMu.Unlock()
			return client, base, func() {}, nil
		}
	}
	a.remoteTabMu.Unlock()

	rt, err := a.remoteRT()
	if err != nil {
		return nil, "", nil, err
	}
	ctx := a.bootContext()
	if ctx == nil {
		ctx = context.Background()
	}
	view, token, err := rt.EnsureServer(ctx, hostID, workspace)
	if err != nil || view.State != "ready" || view.LocalURL == "" {
		return nil, "", nil, fmt.Errorf("remote serve for %s:%s is not ready", hostID, workspace)
	}
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	jar, jarErr := cookiejar.New(nil)
	if jarErr != nil {
		cancel()
		return nil, "", nil, jarErr
	}
	client := &http.Client{Jar: jar}
	if err := serveHandshake(callCtx, client, view.LocalURL, token); err != nil {
		cancel()
		return nil, "", nil, err
	}
	return client, view.LocalURL, cancel, nil
}

func (a *App) RemoteProjectSessions(hostID, workspace string) ([]RemoteSessionView, error) {
	client, base, done, err := a.serveClientForRef(hostID, workspace)
	if err != nil {
		return nil, err
	}
	defer done()
	ctx, cancel := commandContext(a)
	defer cancel()
	entries, err := serveSessions(ctx, client, base)
	if err != nil {
		return nil, err
	}
	out := make([]RemoteSessionView, 0, len(entries))
	pinned := make([]RemoteSessionView, 0, len(entries))
	hasCurrent := false
	for _, e := range entries {
		title := strings.TrimSpace(e.Title)
		if override := remoteSessionTitleOverride(hostID, workspace, e.Name); override != "" {
			title = override
		}
		view := RemoteSessionView{
			Name:           e.Name,
			Title:          title,
			Turns:          e.Turns,
			Current:        e.Current,
			LastActivityAt: e.MtimeMilli,
			Pinned:         remoteSessionPinned(hostID, workspace, e.Name),
		}
		hasCurrent = hasCurrent || e.Current
		if view.Pinned {
			pinned = append(pinned, view)
		} else {
			out = append(out, view)
		}
	}
	// Desktop-view blank: the tab holds a fresh session the serve has not
	// written to disk yet — surface it like a freshly created local topic so
	// the tree renders one authoritative listing.
	if !hasCurrent {
		a.remoteTabMu.Lock()
		var blank *remoteTab
		for _, tab := range a.remoteTabs {
			if tab.ref.HostID == hostID && tab.ref.Workspace == workspace && tab.sessionReset {
				blank = tab
				break
			}
		}
		var synth *RemoteSessionView
		if blank != nil {
			synth = &RemoteSessionView{Name: "", Title: blank.topicTitle, Current: true, LastActivityAt: time.Now().UnixMilli()}
		}
		a.remoteTabMu.Unlock()
		if synth != nil {
			return append([]RemoteSessionView{*synth}, append(pinned, out...)...), nil
		}
	}
	return append(pinned, out...), nil
}

// RenameRemoteProjectSession sets a desktop-owned display title for a
// remote session (empty clears it, falling back to the serve title). A live
// tab whose serve currently holds that session adopts the title
// immediately.
func (a *App) RenameRemoteProjectSession(hostID, workspace, name, title string) error {
	setRemoteSessionTitleOverride(hostID, workspace, name, title)

	a.remoteTabMu.Lock()
	var live *remoteTab
	for _, tab := range a.remoteTabs {
		if tab.ref.HostID == hostID && tab.ref.Workspace == workspace && tab.client != nil {
			live = tab
			break
		}
	}
	a.remoteTabMu.Unlock()
	if live == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	entries, err := serveSessions(ctx, live.client, live.base)
	if err != nil {
		return nil // the override is persisted; the tab syncs on its next listing
	}
	for _, entry := range entries {
		if !entry.Current || entry.Name != name {
			continue
		}
		next := strings.TrimSpace(title)
		if next == "" {
			next = strings.TrimSpace(entry.Title)
		}
		if next == "" {
			next = remoteWorkspaceName(workspace)
		}
		a.remoteTabMu.Lock()
		changed := live.topicTitle != next
		if changed {
			live.topicTitle = next
		}
		a.remoteTabMu.Unlock()
		if changed {
			a.emitRemoteEvent("remote-tab:opened", remoteTabMeta(live, live.hostLabel))
			a.saveTabsFromRemote()
		}
		return nil
	}
	return nil
}

// resumeRemoteTabSession switches a live tab to a listed session: POST
// /resume, adopt its title, clear the blank mark, and re-emit ready so the
// surface re-syncs its snapshot.
func (a *App) resumeRemoteTabSession(tabID, name string) {
	a.remoteTabMu.Lock()
	tab := a.remoteTabs[tabID]
	if tab == nil || tab.client == nil {
		a.remoteTabMu.Unlock()
		return
	}
	client, base := tab.client, tab.base
	a.remoteTabMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	entries, err := serveSessions(ctx, client, base)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.Name != name {
			continue
		}
		body, _ := json.Marshal(map[string]string{"path": entry.Path})
		if err := servePost(ctx, client, base+"/resume", body); err != nil {
			a.emitRemoteTabState(tabID, "error", err.Error())
			return
		}
		title := strings.TrimSpace(entry.Title)
		if title == "" {
			title = name
		}
		a.remoteTabMu.Lock()
		tab.topicTitle = title
		tab.sessionReset = false
		a.remoteTabMu.Unlock()
		a.emitRemoteEvent("remote-tab:opened", remoteTabMeta(tab, tab.hostLabel))
		a.saveTabsFromRemote()
		a.emitRemoteTabState(tabID, "ready", "")
		return
	}
	a.emitRemoteTabState(tabID, "error", fmt.Sprintf("remote session %q not found", name))
}

// SetRemoteSessionPinned pins a remote session listing row (desktop-owned,
// same model as local topic pins).
func (a *App) SetRemoteSessionPinned(hostID, workspace, name string, pinned bool) error {
	setRemoteSessionPinned(hostID, workspace, name, pinned)
	return nil
}

// SetRemoteProjectTitle renames a pinned remote project's display title in
// the registry; the tree group label prefers it over the workspace name.
func (a *App) SetRemoteProjectTitle(hostID, workspace, title string) error {
	return editUserConfig(func(c *config.Config) error {
		entry, ok := c.RemoteProject(hostID, workspace)
		if !ok {
			return fmt.Errorf("remote project %s:%s is not pinned", hostID, workspace)
		}
		entry.Title = strings.TrimSpace(title)
		return c.UpsertRemoteProject(entry)
	})
}

func (a *App) DeleteRemoteProjectSession(hostID, workspace, name string) error {
	client, base, done, err := a.serveClientForRef(hostID, workspace)
	if err != nil {
		return err
	}
	defer done()
	ctx, cancel := commandContext(a)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"name": name})
	return servePost(ctx, client, base+"/delete-session", body)
}

// remoteTabPost marshals body and forwards one command to the tab's serve.
func (a *App) remoteTabPost(tabID, path string, body map[string]any) error {
	client, base, err := a.remoteTabCommandClient(tabID)
	if err != nil {
		return err
	}
	ctx, cancel := commandContext(a)
	defer cancel()
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	return servePost(ctx, client, base+path, payload)
}

// remoteTabGet fetches one raw JSON member from the tab's serve.
func (a *App) remoteTabGet(tabID, path string) (json.RawMessage, error) {
	client, base, err := a.remoteTabCommandClient(tabID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := commandContext(a)
	defer cancel()
	return serveGet(ctx, client, base+path)
}

func (a *App) SetRemoteTabModel(tabID, ref string) error {
	return a.remoteTabPost(tabID, "/model", map[string]any{"ref": ref})
}

func (a *App) SetRemoteTabEffort(tabID, level string) error {
	return a.remoteTabPost(tabID, "/effort", map[string]any{"level": level})
}

// SetRemoteTabPlanMode toggles plan mode on the active remote session.
func (a *App) SetRemoteTabPlanMode(tabID string, on bool) error {
	return a.remoteTabPost(tabID, "/plan", map[string]any{"on": on})
}

func (a *App) CompactRemoteTab(tabID string) error {
	return a.remoteTabPost(tabID, "/compact", nil)
}

// ForkRemoteTab branches the session at a checkpoint turn; name may be empty.
func (a *App) ForkRemoteTab(tabID string, turn int, name string) error {
	return a.remoteTabPost(tabID, "/fork", map[string]any{"turn": turn, "name": name})
}

// SummarizeRemoteTab summarizes a turn; mode is "from" or "upto".
func (a *App) SummarizeRemoteTab(tabID string, turn int, mode string) error {
	return a.remoteTabPost(tabID, "/summarize", map[string]any{"turn": turn, "mode": mode})
}

// ForgetRemoteTab deletes a saved memory by name on the remote host.
func (a *App) ForgetRemoteTab(tabID, name string) error {
	return a.remoteTabPost(tabID, "/forget", map[string]any{"name": name})
}

// RemoteTabBranches returns the raw branch list for the rewind picker.
func (a *App) RemoteTabBranches(tabID string) (json.RawMessage, error) {
	return a.remoteTabGet(tabID, "/branches")
}

// RemoteTabSkills returns the remote host's discoverable skills.
func (a *App) RemoteTabSkills(tabID string) (json.RawMessage, error) {
	return a.remoteTabGet(tabID, "/skills")
}

// refreshRemoteTabTitle adopts the serve's LLM-generated title for the
// current session. The previous title survives in the session history; a
// changed title is pushed to the frontend through the tab-opened channel,
// which the chrome merges into the existing strip entry.
func (a *App) refreshRemoteTabTitle(tabID string) {
	a.remoteTabMu.Lock()
	tab := a.remoteTabs[tabID]
	if tab == nil || tab.titleRefreshInFlight || tab.client == nil {
		a.remoteTabMu.Unlock()
		return
	}
	tab.titleRefreshInFlight = true
	client, base := tab.client, tab.base
	a.remoteTabMu.Unlock()
	defer func() {
		a.remoteTabMu.Lock()
		if tab := a.remoteTabs[tabID]; tab != nil {
			tab.titleRefreshInFlight = false
		}
		a.remoteTabMu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	entries, err := serveSessions(ctx, client, base)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.Current {
			continue
		}
		title := strings.TrimSpace(entry.Title)
		if title == "" {
			return
		}
		a.remoteTabMu.Lock()
		changed := tab.topicTitle != title
		if changed {
			tab.topicTitle = title
		}
		tab.sessionReset = false
		a.remoteTabMu.Unlock()
		if changed {
			a.emitRemoteEvent("remote-tab:opened", remoteTabMeta(tab, tab.hostLabel))
			a.saveTabsFromRemote()
		}
		return
	}
}

// resetRemoteTabSession starts a fresh serve session in an existing tab:
// the previous session stays in the remote history list, the tab switches
// to the new (empty) one, and the ready state tells the frontend to
// re-sync its snapshot.
func (a *App) resetRemoteTabSession(tabID string) {
	a.remoteTabMu.Lock()
	tab := a.remoteTabs[tabID]
	if tab == nil {
		a.remoteTabMu.Unlock()
		return
	}
	if tab.client == nil {
		// Bootstrap still in flight: enter a fresh session when it lands.
		tab.newSession = true
		tab.sessionName = ""
		a.remoteTabMu.Unlock()
		return
	}
	client, base := tab.client, tab.base
	// Same default title a fresh local session gets, so the strip and the
	// tree both read "新的会话" until the conversation earns a real title.
	tab.topicTitle = a.localizedDefaultTopicTitle()
	tab.sessionReset = true
	a.remoteTabMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := servePost(ctx, client, base+"/new", nil); err != nil {
		a.emitRemoteTabState(tabID, "error", err.Error())
		return
	}
	a.emitRemoteTabState(tabID, "ready", "")
}

// remoteTabMetas returns chrome metas for every open remote tab (in strip
// order) plus the currently highlighted remote tab id ("" when a local tab is
// active).
func (a *App) remoteTabMetas() ([]TabMeta, string) {
	a.remoteTabMu.Lock()
	defer a.remoteTabMu.Unlock()
	ids := a.orderedRemoteTabIDsLocked()
	metas := make([]TabMeta, 0, len(ids))
	for _, id := range ids {
		if tab := a.remoteTabs[id]; tab != nil {
			metas = append(metas, remoteTabMeta(tab, tab.hostLabel))
		}
	}
	return metas, a.remoteActiveTabID
}

// orderedRemoteTabIDsLocked returns the remote strip order with self-repair:
// registry keys missing from the order append in sorted order (mirrors
// orderedTabIDsLocked for the local side). Caller holds remoteTabMu.
func (a *App) orderedRemoteTabIDsLocked() []string {
	seen := make(map[string]bool, len(a.remoteTabOrder))
	out := make([]string, 0, len(a.remoteTabs))
	for _, id := range a.remoteTabOrder {
		if a.remoteTabs[id] != nil && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	var missing []string
	for id := range a.remoteTabs {
		if !seen[id] {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	return append(out, missing...)
}

// remoteTabsFileEntries snapshots the persisted remote tab section (entries
// plus strip order plus the active remote id). Called from the tab-file write
// path — lock order tabsSaveMu → remoteTabMu.
func (a *App) remoteTabsFileEntries() ([]desktopRemoteTabEntry, []string, string) {
	a.remoteTabMu.Lock()
	defer a.remoteTabMu.Unlock()
	ids := a.orderedRemoteTabIDsLocked()
	entries := make([]desktopRemoteTabEntry, 0, len(ids))
	for _, id := range ids {
		tab := a.remoteTabs[id]
		if tab == nil {
			continue
		}
		entries = append(entries, desktopRemoteTabEntry{
			ID:         tab.id,
			HostID:     tab.ref.HostID,
			Workspace:  tab.ref.Workspace,
			TopicTitle: tab.topicTitle,
		})
	}
	order := append([]string(nil), ids...)
	if len(order) == 0 {
		order = nil
	}
	return entries, order, a.remoteActiveTabID
}

// CloseRemoteTab tears down one remote tab: the SSE pump stops and the
// registry entry goes away. The remote serve and the SSH connection stay
// untouched — other tabs on the same host keep running.
func (a *App) CloseRemoteTab(tabID string) error {
	a.remoteTabMu.Lock()
	tab := a.remoteTabs[tabID]
	delete(a.remoteTabs, tabID)
	a.remoteTabOrder = removeRemoteTabOrderID(a.remoteTabOrder, tabID)
	if a.remoteActiveTabID == tabID {
		a.remoteActiveTabID = ""
	}
	var cancel context.CancelFunc
	if tab != nil {
		cancel = tab.cancel
	}
	a.remoteTabMu.Unlock()
	if cancel != nil {
		cancel()
	}
	a.saveTabsFromRemote()
	return nil
}

// remoteTabsHostStatus reacts to SSH transitions for every open tab on the
// host: losing the tunnel suspends the pumps, a regained connection
// re-attaches each tab to the still-running remote serve, and a terminal
// failure parks the tabs in error.
func (a *App) remoteTabsHostStatus(hostID, state, errText string) {
	switch state {
	case "connecting", "reconnecting":
		a.suspendRemoteTabPumps(hostID, "reconnecting", "")
	case "connected":
		a.resumeRemoteTabs(hostID)
	case "stopped":
		a.suspendRemoteTabPumps(hostID, "error", errText)
	}
}

func (a *App) suspendRemoteTabPumps(hostID, state, errText string) {
	a.remoteTabMu.Lock()
	for _, tab := range a.remoteTabs {
		if tab.ref.HostID != hostID || tab.state == "disconnected" {
			// A restored shell was never connected this run: host status
			// transitions must not flip it into a runtime state.
			continue
		}
		tab.gen++
		if tab.cancel != nil {
			tab.cancel()
			tab.cancel = nil
		}
		tab.state = state
		tab.err = errText
	}
	a.remoteTabMu.Unlock()
}

// resumeRemoteTabs re-attaches every suspended tab of a reconnected host.
// The remote serve kept running through the SSH drop, so re-attachment only
// rebuilds the tunnel client and the event pump; the serve still holds the
// active session, so no session re-entry is needed.
func (a *App) resumeRemoteTabs(hostID string) {
	a.remoteTabMu.Lock()
	tabIDs := make([]string, 0, 2)
	for id, tab := range a.remoteTabs {
		if tab.ref.HostID == hostID && tab.state == "reconnecting" {
			tabIDs = append(tabIDs, id)
		}
	}
	a.remoteTabMu.Unlock()
	for _, tabID := range tabIDs {
		a.goSafe("remoteTabReattach", func() { a.reattachRemoteTab(tabID) })
	}
}

// reattachRemoteTab rebuilds one tab's serve client and pump after the
// host connection came back. Any failure leaves the tab in reconnecting —
// the next connected transition retries.
func (a *App) reattachRemoteTab(tabID string) {
	a.remoteTabMu.Lock()
	tab := a.remoteTabs[tabID]
	if tab == nil || tab.state != "reconnecting" {
		a.remoteTabMu.Unlock()
		return
	}
	hostID, workspace := tab.ref.HostID, tab.ref.Workspace
	a.remoteTabMu.Unlock()

	rt, err := a.remoteRT()
	if err != nil {
		return
	}
	ctx := a.bootContext()
	if ctx == nil {
		ctx = context.Background()
	}
	view, token, err := rt.EnsureServer(ctx, hostID, workspace)
	if err != nil || view.State != "ready" || view.LocalURL == "" {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	jar, jarErr := cookiejar.New(nil)
	if jarErr != nil {
		return
	}
	client := &http.Client{Jar: jar}
	if err := serveHandshake(callCtx, client, view.LocalURL, token); err != nil {
		return
	}

	a.remoteTabMu.Lock()
	if cur := a.remoteTabs[tabID]; cur != tab || tab.state != "reconnecting" {
		a.remoteTabMu.Unlock()
		return
	}
	tab.gen++
	if tab.cancel != nil {
		tab.cancel()
	}
	tab.client = client
	tab.base = view.LocalURL
	tab.token = token
	gen := tab.gen
	pumpCtx, cancelPump := context.WithCancel(ctx)
	tab.cancel = cancelPump
	a.remoteTabMu.Unlock()

	a.goSafe("remoteTabPump", func() { a.remoteTabPump(pumpCtx, tabID, gen) })
	a.emitRemoteTabState(tabID, "ready", "")
}

// listTabsWithRemote merges the remote strip entries into a local tab list.
// A highlighted remote tab deactivates every local entry so the strip shows
// exactly one active tab.
func (a *App) listTabsWithRemote(local []TabMeta) []TabMeta {
	remote, remoteActive := a.remoteTabMetas()
	if remoteActive != "" {
		for i := range local {
			local[i].Active = false
		}
	}
	if len(remote) == 0 {
		return enrichTabMetas(local)
	}
	return append(enrichTabMetas(local), remote...)
}
