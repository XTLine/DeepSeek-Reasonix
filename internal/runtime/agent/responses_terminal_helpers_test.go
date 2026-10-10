package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"reasonix/internal/contract/event"
	"reasonix/internal/contract/provider"
	"reasonix/internal/contract/tool"
	"reasonix/internal/model/responses"
	"reasonix/internal/state/sessionstore"
)

const terminalFinalAnswerSSE = "data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_final\",\"content_index\":0,\"delta\":\"done\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_final\",\"status\":\"completed\",\"output\":[{\"id\":\"msg_final\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\",\"annotations\":[]}]}]}}\n\n"

const terminalSystemPrompt = "Stable system instructions for the Responses integrity fixture."

type terminalExpectation struct {
	requests   int
	calls      map[string]terminalCallExpectation
	err        error
	cause      provider.StreamFailureCause
	retries    int
	exactRetry bool
}

type terminalCallExpectation struct {
	name      string
	arguments string
}

func runTerminalFixture(t *testing.T, events []string, want terminalExpectation) {
	t.Helper()
	var captured terminalRequests
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestNo := captured.add(t, r)
		w.Header().Set("Content-Type", "text/event-stream")
		if requestNo > 1 {
			_, _ = io.WriteString(w, terminalFinalAnswerSSE)
			return
		}
		for _, e := range events {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", e)
		}
	}))
	defer srv.Close()
	sink := &recordSink{}
	a, executions := newTerminalAgent(srv.URL, sink)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := a.Run(ctx, "Run the required tool and report the result")
	if !errors.Is(err, want.err) {
		t.Errorf("Run error=%v, want %v", err, want.err)
	}
	if cause := provider.StreamFailureCauseOf(err); cause != want.cause {
		t.Errorf("stream failure=%q, want %q", cause, want.cause)
	}
	if want.err != nil {
		wrapped := fmt.Errorf("caller: %w", err)
		if !errors.Is(wrapped, want.err) || provider.StreamFailureCauseOf(wrapped) != want.cause {
			t.Errorf("wrapped failure lost identity: %v", wrapped)
		}
	}
	if provider.IsStreamInterrupted(err) {
		t.Errorf("unexpected retryable terminal error: %v", err)
	}
	bodies := captured.snapshot()
	if len(bodies) != want.requests {
		t.Errorf("requests=%d, want %d", len(bodies), want.requests)
	}
	retries := sink.kinds(event.Retrying)
	if len(retries) != want.retries {
		t.Errorf("automatic retries=%d, want %d", len(retries), want.retries)
	}
	for i, retry := range retries {
		if retry.RetryScope != event.RetryScopeStream || retry.RetryCause != provider.RetryCauseConnectionClosed || retry.RetryAttempt != i+1 {
			t.Errorf("retry=%+v, want stream connection-close retry %d", retry, i+1)
		}
	}
	if want.exactRetry && len(bodies) > 1 && !bytes.Equal(bodies[0], bodies[1]) {
		t.Errorf("EOF retry changed frozen request\nfirst=%s\nretry=%s", bodies[0], bodies[1])
	}
	assertTerminalExecutions(t, executions, want.calls)
}

type terminalRequests struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (r *terminalRequests) add(t *testing.T, req *http.Request) int {
	t.Helper()
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Errorf("read request: %v", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bodies = append(r.bodies, body)
	return len(r.bodies)
}

func (r *terminalRequests) snapshot() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]byte(nil), r.bodies...)
}

func newTerminalAgent(url string, sink event.Sink) (*Agent, *terminalExecutions) {
	executions := &terminalExecutions{calls: make(map[string]int)}
	reg := tool.NewRegistry()
	reg.Add(terminalEchoTool{executions: executions})
	reg.Add(terminalReadyTool{executions: executions})
	p := responses.New(responses.Config{Name: "local-terminal-fixture", APIKey: "fake-local-only", BaseURL: url, Model: "fixture-model", Mode: "stateless"})
	return New(p, reg, sessionstore.NewSession(terminalSystemPrompt), Options{}, sink), executions
}

func assertTerminalExecutions(t *testing.T, executions *terminalExecutions, want map[string]terminalCallExpectation) {
	t.Helper()
	expected := make(map[string]int, len(want))
	for _, call := range want {
		expected[call.name+"\x00"+call.arguments]++
	}
	executions.mu.Lock()
	defer executions.mu.Unlock()
	if !reflect.DeepEqual(executions.calls, expected) {
		t.Errorf("executions=%v, want %v", executions.calls, expected)
	}
}

type terminalExecutions struct {
	mu    sync.Mutex
	calls map[string]int
}

func (e *terminalExecutions) record(name string, args json.RawMessage) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls[name+"\x00"+string(args)]++
}

type terminalEchoTool struct {
	echoTool
	executions *terminalExecutions
}

func (t terminalEchoTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	t.executions.record(t.Name(), args)
	return t.echoTool.Execute(ctx, args)
}

type terminalReadyTool struct{ executions *terminalExecutions }

func (terminalReadyTool) Name() string        { return "ready" }
func (terminalReadyTool) Description() string { return "report readiness without arguments" }
func (terminalReadyTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
}
func (terminalReadyTool) ReadOnly() bool { return true }
func (t terminalReadyTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	t.executions.record(t.Name(), args)
	return "ready", nil
}
