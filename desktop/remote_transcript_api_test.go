package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"reasonix/internal/session"
	"reasonix/internal/sessioncontent"
	"reasonix/internal/transcript"
)

func remoteTranscriptFixture(server *httptest.Server) (*App, *remoteTab) {
	tab := &remoteTab{id: "remote", state: "ready", client: server.Client(), base: server.URL, gen: 1,
		routing: remoteTabSessionRouting{currentPath: "/session.jsonl"}}
	return &App{remoteTabs: map[string]*remoteTab{tab.id: tab}}, tab
}

func TestRemoteTranscriptNegotiatesOldServeWithoutMutation(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented, http.StatusOK} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/transcript/snapshot" || r.URL.Query().Get("session") != "/session.jsonl" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(status)
				_, _ = w.Write([]byte("<html>old Serve index</html>"))
			}))
			defer server.Close()
			app, tab := remoteTranscriptFixture(server)
			result, err := app.RemoteTranscriptSnapshotForTab(tab.id, transcript.PageRequest{})
			if err != nil || result.Supported || result.Snapshot != nil {
				t.Fatalf("negotiation = %+v, %v", result, err)
			}
			if tab.state != "ready" || tab.gen != 1 {
				t.Fatal("capability probe changed the connection")
			}
		})
	}
}

func TestRemoteTranscriptRejectsLateSessionResponse(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_ = json.NewEncoder(w).Encode(transcript.Snapshot{Boundary: transcript.Boundary{ProtocolVersion: 1, SnapshotID: "old"}})
	}))
	defer server.Close()
	app, tab := remoteTranscriptFixture(server)
	done := make(chan error, 1)
	go func() { _, err := app.RemoteTranscriptSnapshotForTab(tab.id, transcript.PageRequest{}); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	app.remoteTabMu.Lock()
	tab.routing.currentPath = "/new-session.jsonl"
	app.remoteTabMu.Unlock()
	close(release)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "replaced session") {
		t.Fatalf("late response accepted: %v", err)
	}
}

func TestRemoteTabMetadataDoesNotRequestHistory(t *testing.T) {
	var historyReads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/history" {
			historyReads.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/status" {
			_, _ = w.Write([]byte(`{"sessionPath":"/session.jsonl","running":false}`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()
	app, tab := remoteTranscriptFixture(server)
	metadata, err := app.RemoteTabMetadata(tab.id)
	if err != nil || historyReads.Load() != 0 || len(metadata.History) != 0 {
		t.Fatalf("metadata requested history: count=%d error=%v", historyReads.Load(), err)
	}
}

func TestRemoteCanonicalSessionHistoryUsesNegotiatedIdentity(t *testing.T) {
	ref := sessioncontent.Ref{Digest: strings.Repeat("a", 64), Bytes: 3, MediaType: "text/plain"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("sessionId"); got != "canonical" {
			t.Errorf("sessionId = %q", got)
		}
		switch r.URL.Path {
		case "/session-history/page":
			if r.URL.Query().Get("cursor") != "next" || r.URL.Query().Get("limit") != "7" {
				t.Errorf("page query = %q", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(session.MessageHistoryPage{Messages: []session.PersistentMessage{{MessageID: "m1", Role: "user", ContentRef: &ref}}, SnapshotSequence: 9})
		case "/session-history/content":
			var request struct {
				Ref    sessioncontent.Ref `json:"ref"`
				Offset int64              `json:"offset"`
				Length int64              `json:"length"`
			}
			if err := json.Unmarshal([]byte(r.URL.Query().Get("request")), &request); err != nil {
				t.Fatal(err)
			}
			if request.Ref.Digest != ref.Digest || request.Offset != 0 || request.Length != 3 {
				t.Errorf("content request = %+v", request)
			}
			_ = json.NewEncoder(w).Encode(SessionHistoryContentChunk{Data: base64.StdEncoding.EncodeToString([]byte("big")), NextOffset: 3, Done: true})
		case "/session-history/search":
			if r.URL.Query().Get("q") != "needle" || r.URL.Query().Get("cursor") != "older" || r.URL.Query().Get("limit") != "5" {
				t.Errorf("search query = %q", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(session.SearchHistoryPage{Hits: []session.SearchHistoryHit{{MessageID: "m1", Preview: "needle"}}, SnapshotSequence: 9})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	app, tab := remoteTranscriptFixture(server)
	tab.capabilities = map[string]bool{serveCapabilitySessionContentV1: true}
	tab.session.sessionID = "canonical"
	page, err := app.RemoteSessionHistoryPageForTab(tab.id, "next", 7)
	if err != nil || page.SnapshotSequence != 9 || len(page.Messages) != 1 {
		t.Fatalf("page = %+v, %v", page, err)
	}
	chunk, err := app.RemoteSessionHistoryContentForTab(tab.id, ref, 0)
	if err != nil || chunk.Data != base64.StdEncoding.EncodeToString([]byte("big")) || !chunk.Done {
		t.Fatalf("chunk = %+v, %v", chunk, err)
	}
	search, err := app.RemoteSearchSessionHistoryForTab(tab.id, "needle", "older", 5)
	if err != nil || len(search.Hits) != 1 || search.Hits[0].MessageID != "m1" {
		t.Fatalf("search = %+v, %v", search, err)
	}
}

func TestRemoteCanonicalSessionHistoryRequiresCapability(t *testing.T) {
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reads.Add(1) }))
	defer server.Close()
	app, tab := remoteTranscriptFixture(server)
	if _, err := app.RemoteSessionHistoryPageForTab(tab.id, "", 0); err == nil {
		t.Fatal("canonical history unexpectedly enabled")
	}
	if reads.Load() != 0 {
		t.Fatalf("network reads = %d", reads.Load())
	}
}
