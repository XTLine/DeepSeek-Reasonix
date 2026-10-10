package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"reasonix/internal/contract/provider"
	"reasonix/internal/contract/tool"
	"reasonix/internal/model/responses"
	"reasonix/internal/state/sessionstore"
)

func TestResponsesTerminalRejectsLostCalls(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []string
	}{
		{"unclosed_DONE", []string{
			`{"type":"response.output_text.delta","delta":"I will run the tool."}`,
			`{"type":"response.output_item.added","item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"echo"}}`,
			`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"text\":"}`,
			`[DONE]`,
		}},
		{"malformed_completed_neighbor", []string{
			`{"type":"response.output_text.delta","delta":"I will run the tool."}`,
			`{"type":"response.completed","response":{"id":"r","output":[{"id":"good","type":"function_call","call_id":"good","name":"echo","arguments":"{\"text\":\"good\"}"},{"id":"bad","type":"function_call","call_id":"bad","name":"echo","arguments":{}}]}}`,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests, executions atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if requests.Add(1) > 1 {
					fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"done\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"final\"}}\n")
					return
				}
				for _, e := range tc.events {
					fmt.Fprintf(w, "data: %s\n\n", e)
				}
			}))
			defer srv.Close()
			p := responses.New(responses.Config{Name: "fixture", APIKey: "local-fixture", BaseURL: srv.URL, Model: "fixture", Mode: "stateless"})
			reg := tool.NewRegistry()
			reg.Add(terminalCountingEcho{calls: &executions})
			a := New(p, reg, sessionstore.NewSession(""), Options{}, &recordSink{})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err := a.Run(ctx, "Run the required tool and report the result")
			if err == nil {
				t.Error("Run succeeded; want terminal call-integrity failure")
			}
			if requests.Load() != 1 {
				t.Errorf("requests=%d, want one refused attempt without retry", requests.Load())
			}
			if executions.Load() != 0 {
				t.Errorf("executions=%d, want zero", executions.Load())
			}
			for _, m := range a.Session().Snapshot() {
				if !m.LocalOnly && (m.Role == provider.RoleAssistant || m.Role == provider.RoleTool) {
					t.Errorf("committed refused output: %+v", m)
				}
			}
		})
	}
}

type terminalCountingEcho struct {
	echoTool
	calls *atomic.Int32
}

func (t terminalCountingEcho) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	t.calls.Add(1)
	return t.echoTool.Execute(ctx, args)
}

func TestResponsesTerminalToolIntegrity(t *testing.T) {
	const text = `{"type":"response.output_text.delta","item_id":"msg_1","content_index":0,"delta":"I will run the tool: partial-text-marker."}`
	const ready = `{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"id":"fc_ready","type":"function_call","call_id":"call_ready","name":"ready","status":"completed","arguments":"{}"}]}}`
	cases := []struct {
		name   string
		events []string
		want   terminalExpectation
	}{
		{name: "no_argument_tool_accepts_empty_object", events: []string{text, ready}, want: terminalExpectation{requests: 2, calls: map[string]terminalCallExpectation{"call_ready": {name: "ready", arguments: `{}`}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { runTerminalFixture(t, tc.events, tc.want) })
	}
}
