package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/control"
)

// Only reconcile reservations from a CLI request whose outcome this process
// could not confirm. Serve grants also carry mirror/return state and must not
// be reconstructed from the lease sidecar alone. A restarted CLI has a new
// writer identity and cannot consume the previous process's reservation.
var unconfirmedCLIHandoffs sync.Map // canonical session path -> source writer ID

func pendingCLIHandoff(path string) *agent.SessionLeaseInfo {
	source, ok := unconfirmedCLIHandoffs.Load(agent.CanonicalSessionPath(path))
	if !ok {
		return nil
	}
	info, err := agent.LoadSessionLeaseInfo(path)
	if err != nil || info == nil || info.WriterID != source || info.HandoffTo != agent.SessionWriterID() || info.HandoffID == "" || !time.Now().Before(info.HandoffExpiresAt) {
		return nil
	}
	return info
}

func localCLILease(info *agent.SessionLeaseInfo) bool {
	if info == nil || info.PID <= 0 || info.WriterID == "" {
		return false
	}
	host, err := os.Hostname()
	return err == nil && (info.Hostname == "" || strings.EqualFold(info.Hostname, host))
}

func findCLIPeer(info *agent.SessionLeaseInfo) (*cliPeerRecord, error) {
	if !localCLILease(info) {
		return nil, fmt.Errorf("session owner is not on this host")
	}
	path := filepath.Join(cliPeerDirectory(), fmt.Sprintf("cli-%d.json", info.PID))
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var record cliPeerRecord
	if json.Unmarshal(data, &record) != nil || record.PID != info.PID || record.WriterID != info.WriterID || len(record.Token) != 64 {
		return nil, fmt.Errorf("stale CLI owner registration")
	}
	host, port, err := net.SplitHostPort(record.Address)
	n, portErr := strconv.Atoi(port)
	if err != nil || host != "127.0.0.1" || portErr != nil || n < 1 || n > 65535 {
		return nil, fmt.Errorf("invalid CLI loopback endpoint")
	}
	return &record, nil
}

func cliPeerDo(ctx context.Context, record cliPeerRecord, method, route string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://"+record.Address+route, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+record.Token)
	req.Header.Set("Content-Type", "application/json")
	// Never send a discovery token through a configured HTTP proxy or redirect.
	transport := &http.Transport{DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("CLI handoff HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return json.Unmarshal(data, out)
}

func queryCLIPeer(ctx context.Context, record cliPeerRecord, path string) (cliOwnershipView, error) {
	var view cliOwnershipView
	err := cliPeerDo(ctx, record, http.MethodGet, "/ownership?"+url.Values{"session": {path}}.Encode(), nil, &view)
	return view, err
}

func acquireCLIPeerSession(path string, record cliPeerRecord, leases *control.SessionLeaseKeeper, manager *cliTakeoverManager, mode string) (*cliTakeoverBinding, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cliTakeoverTimeout+15*time.Second)
	defer cancel()
	body, _ := json.Marshal(cliPeerRequest{SessionPath: path, TargetWriterID: agent.SessionWriterID(), SourceWriterID: record.WriterID, Mode: mode})
	var grant cliTakeoverGrant
	err := cliPeerDo(ctx, record, http.MethodPost, "/handoff", body, &grant)
	if err != nil {
		// The first response may have been lost after release. The source stores
		// one grant per successor, making this reconciliation idempotent.
		retryCtx, retryCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer retryCancel()
		err = cliPeerDo(retryCtx, record, http.MethodPost, "/handoff", body, &grant)
		if err != nil {
			unconfirmedCLIHandoffs.Store(agent.CanonicalSessionPath(path), record.WriterID)
			return nil, fmt.Errorf("handoff outcome unconfirmed; retry resume to reconcile ownership: %w", err)
		}
	}
	if grant.SourceWriterID != record.WriterID || grant.TargetWriterID != agent.SessionWriterID() || grant.HandoffID == "" || grant.MirrorID != "" || grant.ReturnHandoffID != "" || agent.CanonicalSessionPath(grant.SessionPath) != agent.CanonicalSessionPath(path) {
		return nil, fmt.Errorf("invalid CLI handoff grant")
	}
	previous, err := leases.RebindDetachingWithHandoff(path, grant.SourceWriterID, grant.HandoffID)
	if err != nil {
		return nil, err
	}
	unconfirmedCLIHandoffs.Delete(agent.CanonicalSessionPath(path))
	binding := &cliTakeoverBinding{path: path, grant: grant, previous: previous}
	if manager != nil {
		current, _, _, _ := manager.snapshot()
		if current != nil && !manager.Returned() && agent.CanonicalSessionPath(current.path) != agent.CanonicalSessionPath(path) {
			binding.priorMirror = current
		}
	}
	return binding, nil
}
