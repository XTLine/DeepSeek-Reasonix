package control

import (
	"encoding/json"
	"strings"
	"testing"

	"reasonix/internal/agent"
	"reasonix/internal/event"
	goaldomain "reasonix/internal/goal"
	"reasonix/internal/session"
	"reasonix/internal/tool"
)

func TestGoalDiagnosticExportReadsCompleteDurableV3Log(t *testing.T) {
	service, err := session.NewService("desktop", session.NewFilesystemPersistence(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := service.Create(t.Context(), session.CreateOptions{SessionID: "goal-diagnostic"})
	if err != nil {
		t.Fatal(err)
	}
	toolPayload := json.RawMessage(`{"id":"call-1","name":"bash","output":"full diagnostic output; Authorization: Bearer secret-token-123456; api_key=sk-proj-1234567890abcdef"}`)
	if _, err := runtime.Session().AppendBatch(t.Context(), "tool-evidence", []session.Event{{Kind: "tool/result", Payload: toolPayload}}); err != nil {
		t.Fatal(err)
	}
	machine := goaldomain.NewMachine(nil, func() string { return "goal-1" })
	if _, err := machine.Create(goaldomain.CreateRequest{Objective: "diagnose the goal"}); err != nil {
		t.Fatal(err)
	}
	goalPayload, err := machine.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Session().AppendBatch(t.Context(), "goal:goal-1:1:create", []session.Event{{Kind: "goal/state", Payload: goalPayload}}); err != nil {
		t.Fatal(err)
	}
	exec := agent.New(nil, tool.NewRegistry(), agent.NewSession("system"), agent.Options{}, event.Discard)
	c := New(Options{Executor: exec, Sink: event.Discard, SessionService: service, SessionRuntime: runtime, ExclusiveSession: true})
	t.Cleanup(c.Close)
	payload, err := c.ExportGoalDiagnostics(t.Context(), GoalDiagnosticMetadata{ApplicationVersion: "1.2.3", BuildCommit: "abc", ProtocolVersion: 4, Capabilities: []string{"goal-lifecycle-v2"}})
	if err != nil {
		t.Fatal(err)
	}
	text := string(payload)
	var document map[string]any
	if err := json.Unmarshal(payload, &document); err != nil {
		t.Fatalf("diagnostic export is not valid JSON: %v\n%s", err, text)
	}
	for _, want := range []string{`"schemaVersion": 1`, `"applicationVersion": "1.2.3"`, `"sessionCodec": "` + session.Codec + `"`, `"full diagnostic output`, `"goal-lifecycle-v2"`, `"activationChanges"`, `"activation": "armed"`, `"inferred": true`, `"persistenceStatus": "ready"`, `"unavailable"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("diagnostic export missing %s:\n%s", want, text)
		}
	}
	for _, secret := range []string{"secret-token-123456", "sk-proj-1234567890abcdef"} {
		if strings.Contains(text, secret) {
			t.Fatalf("diagnostic export leaked credential %q", secret)
		}
	}
}
