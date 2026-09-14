package control

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reasonix/internal/agent"
	"reasonix/internal/agent/testutil"
	"reasonix/internal/event"
	"reasonix/internal/provider"
	"reasonix/internal/tool"
	"strings"
	"testing"
	"time"
)

type checkpointProbeTool struct {
	name    string
	started chan struct{}
}

func (t checkpointProbeTool) Name() string            { return t.name }
func (t checkpointProbeTool) Description() string     { return "checkpoint test" }
func (t checkpointProbeTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t checkpointProbeTool) ReadOnly() bool          { return false }
func (t checkpointProbeTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	if t.started != nil {
		close(t.started)
		<-ctx.Done()
		return "", ctx.Err()
	}
	return "write completed", nil
}

func TestToolCheckpointSurvivesReloadWhileNextWriterRuns(t *testing.T) {
	started := make(chan struct{})
	reg := tool.NewRegistry()
	reg.Add(checkpointProbeTool{name: "first"})
	reg.Add(checkpointProbeTool{name: "second", started: started})
	calls := []provider.ToolCall{{ID: "c1", Name: "first", Arguments: `{}`}, {ID: "c2", Name: "second", Arguments: `{}`}}
	mock := testutil.NewMock("test", testutil.Turn{Reasoning: "original reasoning", ToolCalls: calls})
	session := agent.NewSession("system")
	exec := agent.New(mock, reg, session, agent.Options{}, event.Discard)
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	sink, done, _ := collectSink()
	c := newOwnedTestController(t, Options{Runner: exec, Executor: exec, Sink: sink, SessionDir: dir, SessionPath: path})
	t.Cleanup(c.Close)
	c.Submit("run both")
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("second tool did not start")
	}
	loaded := loadDurableSessionProjection(t, path)
	completed := false
	for _, m := range loaded.Messages {
		if m.Role != provider.RoleTool {
			continue
		}
		if m.ToolCallID == "c1" {
			completed = strings.HasPrefix(m.Content, "write completed") && provider.ToolResultRunState(m) == provider.ToolRunCompleted
		}
	}
	if !completed || loaded.ActiveTools["c2"] != "second" {
		t.Fatalf("completed=%v active=%v history=%+v", completed, loaded.ActiveTools, loaded.Messages)
	}
	c.Cancel()
	waitForDone(t, done)
}
