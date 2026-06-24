package memorycompiler

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStartTurnEmptyStateDoesNotInjectIR(t *testing.T) {
	rt := New(t.TempDir())
	ctx, turn := rt.StartTurn(context.Background(), "fix a bug", nil)
	if ctx != "" {
		t.Fatalf("empty compiler state injected context:\n%s", ctx)
	}
	if turn == nil {
		t.Fatal("StartTurn returned nil turn")
	}
	turn.Finish(nil)
}

func TestFailureTraceCreatesConstraintIR(t *testing.T) {
	dir := t.TempDir()
	rt := New(dir)
	_, turn := rt.StartTurn(context.Background(), "fix a bug", nil)
	turn.RecordToolResults([]ToolRecord{
		{Name: "bash", Error: "exit status 1"},
		{Name: "bash", Error: "exit status 1"},
	})
	turn.Finish(nil)

	ctx, _ := rt.StartTurn(context.Background(), "continue", nil)
	if !strings.Contains(ctx, "<memory-compiler-execution>") {
		t.Fatalf("expected learned IR context, got:\n%s", ctx)
	}
	if !strings.Contains(ctx, "memory_v5_execution_contract") {
		t.Fatalf("expected compiled execution contract, got:\n%s", ctx)
	}
	if !strings.Contains(ctx, `"source_event":"continue"`) {
		t.Fatalf("expected source event to be compiled into the contract, got:\n%s", ctx)
	}
	if !strings.Contains(ctx, "avoid repeating bash") {
		t.Fatalf("expected repeated-error constraint, got:\n%s", ctx)
	}
	if !strings.Contains(ctx, "bugfix-reproduce-first") {
		t.Fatalf("expected matching strategy in IR, got:\n%s", ctx)
	}

	st := readState(t, dir)
	if len(st.Learnings) != 1 {
		t.Fatalf("learnings = %d, want 1", len(st.Learnings))
	}
	if len(st.Mutations) == 0 {
		t.Fatal("expected structured compiler mutations")
	}
	if st.Mutations[0].Status != "testing" || !st.Mutations[0].Applied {
		t.Fatalf("mutation = %+v, want applied testing mutation", st.Mutations[0])
	}
	if len(st.NoisyRefs) == 0 {
		t.Fatal("expected noisy memory pattern to be tracked")
	}
	if st.ExecutionState.CurrentPhase != "needs_followup" {
		t.Fatalf("phase = %q, want needs_followup", st.ExecutionState.CurrentPhase)
	}
	if len(st.ExecutionState.ActiveConstraints) == 0 {
		t.Fatal("expected active constraints from learning")
	}
	assertNode(t, st.Nodes, func(n MemoryNode) bool {
		return n.Type == "tool_result" && n.TruthLocked && n.Constraint != nil
	}, "truth-locked failed tool result")
	assertNode(t, st.Nodes, func(n MemoryNode) bool {
		return n.Quality == QualityCorrupted && strings.HasPrefix(n.ID, "noise:")
	}, "corrupted noise node")
	assertEdge(t, st.Edges, "contradicts")
	traces := readTraces(t, dir)
	last := traces[len(traces)-1]
	if len(last.CausalEdges) != 0 || len(last.ToolResults) != 0 || len(last.DecisionBranches) != 0 {
		t.Fatalf("execution trace should stay minimal, got %+v", last)
	}
	if last.Cost.ToolCalls != 2 || last.Cost.ToolErrors != 2 {
		t.Fatalf("cost metrics = %+v, want two tool calls and errors", last.Cost)
	}
	learningTraces := readLearningTraces(t, dir)
	learningLast := learningTraces[len(learningTraces)-1]
	if len(learningLast.CausalEdges) == 0 {
		t.Fatalf("expected causal learning trace edges, got %+v", learningLast)
	}
}

func TestSuccessTraceFeedsReusableStrategyAndGraph(t *testing.T) {
	dir := t.TempDir()
	rt := New(dir)
	_, turn := rt.StartTurn(context.Background(), "frontend ui setting", nil)
	turn.RecordToolResults([]ToolRecord{{Name: "go test", Output: "ok"}})
	turn.Finish(nil)

	ctx, _ := rt.StartTurn(context.Background(), "frontend ui setting", nil)
	if !strings.Contains(ctx, "frontend-visual-verify") {
		t.Fatalf("expected learned frontend strategy, got:\n%s", ctx)
	}
	if !strings.Contains(ctx, "go test succeeded") {
		t.Fatalf("expected tool-result memory reference, got:\n%s", ctx)
	}
	if !strings.Contains(ctx, "verify-ui") {
		t.Fatalf("expected strategy execution plan, got:\n%s", ctx)
	}

	st := readState(t, dir)
	assertNode(t, st.Nodes, func(n MemoryNode) bool {
		return n.Type == "decision" && strings.Contains(n.Content, "frontend-visual-verify")
	}, "decision node for selected strategy")
	assertEdge(t, st.Edges, "derived_from")
}

func TestGraphTraversalFiltersCorruptedMemoryAndExpandsConnectedNodes(t *testing.T) {
	now := time.Now().UTC()
	st := state{
		Nodes: []MemoryNode{
			{
				ID:         "seed",
				Type:       "tool_result",
				Content:    "validated source result",
				Timestamp:  now,
				Confidence: 0.9,
				Quality:    QualityHighSignal,
			},
			{
				ID:         "connected",
				Type:       "fact",
				Content:    "connected supporting constraint",
				Timestamp:  now,
				Confidence: 0.8,
				Quality:    QualityMediumSignal,
				Constraint: &Constraint{Type: "must_use", Text: "use connected graph evidence", Source: "connected"},
			},
			{
				ID:         "corrupted",
				Type:       "fact",
				Content:    "must never appear",
				Timestamp:  now,
				Confidence: 1,
				Quality:    QualityCorrupted,
				Constraint: &Constraint{Type: "must_use", Text: "bad constraint", Source: "corrupted"},
			},
		},
		Edges: []MemoryEdge{{From: "seed", To: "connected", Relation: "supports"}},
	}

	ir := buildIR("fix a bug", "fix a bug", st)
	got, err := json.Marshal(ir)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if !strings.Contains(text, "use connected graph evidence") {
		t.Fatalf("expected connected graph constraint, got:\n%s", text)
	}
	if strings.Contains(text, "must never appear") || strings.Contains(text, "bad constraint") {
		t.Fatalf("corrupted memory leaked into IR:\n%s", text)
	}
}

func TestMutationEvaluationLoopAcceptsAppliedMutation(t *testing.T) {
	dir := t.TempDir()
	rt := New(dir)
	_, turn := rt.StartTurn(context.Background(), "fix a bug", nil)
	turn.RecordToolResults([]ToolRecord{
		{Name: "bash", Error: "exit status 1"},
		{Name: "bash", Error: "exit status 1"},
	})
	turn.Finish(nil)

	ctx, turn := rt.StartTurn(context.Background(), "fix a bug", nil)
	if !strings.Contains(ctx, "avoid repeating bash") {
		t.Fatalf("expected mutation to affect next compiled contract, got:\n%s", ctx)
	}
	turn.RecordToolResults([]ToolRecord{{Name: "go test", Output: "ok"}})
	turn.Finish(nil)

	_, turn = rt.StartTurn(context.Background(), "fix a bug", nil)
	turn.RecordToolResults([]ToolRecord{{Name: "go test", Output: "ok"}})
	turn.Finish(nil)

	st := readState(t, dir)
	found := false
	for _, m := range st.Mutations {
		if strings.Contains(m.Reason, "avoid repeating bash") {
			found = true
			if m.Status != "accepted" {
				t.Fatalf("mutation status = %q, want accepted: %+v", m.Status, m)
			}
			if len(m.EvaluationTraceIDs) == 0 {
				t.Fatalf("mutation missing evaluation trace: %+v", m)
			}
			if len(m.EvaluationTraceIDs) < mutationMinEvalTrials {
				t.Fatalf("mutation accepted before minimum validation trials: %+v", m)
			}
		}
	}
	if !found {
		t.Fatalf("missing repeated bash mutation: %+v", st.Mutations)
	}
	traces := readTraces(t, dir)
	last := traces[len(traces)-1]
	if len(last.MutationEvaluations) != 0 {
		t.Fatalf("execution trace should not carry mutation telemetry: %+v", last)
	}
	learningTraces := readLearningTraces(t, dir)
	learningLast := learningTraces[len(learningTraces)-1]
	if len(learningLast.MutationEvaluations) == 0 {
		t.Fatalf("expected mutation evaluation in learning trace: %+v", learningLast)
	}
}

func TestRecoveredToolFailureCountsAsSuccessfulOutcome(t *testing.T) {
	dir := t.TempDir()
	rt := New(dir)
	_, turn := rt.StartTurn(context.Background(), "fix a bug", nil)
	turn.RecordToolResults([]ToolRecord{
		{Name: "go test", Error: "failed before fix"},
		{Name: "go test", Output: "ok"},
	})
	turn.Finish(nil)

	traces := readTraces(t, dir)
	if got := traces[len(traces)-1].Outcome; got != "success" {
		t.Fatalf("outcome = %q, want success for recovered failure", got)
	}
	st := readState(t, dir)
	for _, s := range st.Strategies {
		if s.ID == "bugfix-reproduce-first" {
			if s.Successes != 1 || s.Failures != 0 {
				t.Fatalf("strategy counters = successes:%d failures:%d, want 1/0", s.Successes, s.Failures)
			}
			return
		}
	}
	t.Fatalf("bugfix strategy not found: %+v", st.Strategies)
}

func TestRuntimeStateUsesPrivateFilesAndSharedDirLock(t *testing.T) {
	dir := t.TempDir()
	rt1 := New(dir)
	rt2 := New(dir)
	if rt1.mu != rt2.mu {
		t.Fatal("runtimes for the same dir must share a lock")
	}
	_, turn := rt1.StartTurn(context.Background(), "fix a bug", nil)
	turn.RecordToolResults([]ToolRecord{{Name: "go test", Output: "ok"}})
	turn.Finish(nil)
	if runtime.GOOS == "windows" {
		return
	}
	for _, name := range []string{stateFile, tracesFile} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s permissions = %o, want 0600", name, info.Mode().Perm())
		}
	}
}

func TestCompilerContractOrderingIsStable(t *testing.T) {
	st := state{
		NoisyRefs: map[string]int{
			"z-noise": 2,
			"a-noise": 2,
		},
		Mutations: []CompilerMutation{
			{Target: "noise_filter", Change: "quarantine_pattern", Reason: "noise mutation", Applied: true, Status: "accepted"},
		},
	}
	ir1 := buildIR("fix a bug", "fix a bug", st)
	ir2 := buildIR("fix a bug", "fix a bug", st)
	got1, err := compileExecutionContract(ir1)
	if err != nil {
		t.Fatal(err)
	}
	got2, err := compileExecutionContract(ir2)
	if err != nil {
		t.Fatal(err)
	}
	if got1 != got2 {
		t.Fatalf("compiled contract is not stable:\n%s\n---\n%s", got1, got2)
	}
	if strings.Index(got1, "a-noise") > strings.Index(got1, "z-noise") {
		t.Fatalf("noisy refs were not emitted in stable order:\n%s", got1)
	}
}

func TestCompilerContractCanonicalizesSemanticOrder(t *testing.T) {
	ir1 := PlannerIR{
		Version:     version,
		Goal:        "fix a bug",
		SourceEvent: "fix a bug",
		RuntimeMode: "control",
		Constraints: []Constraint{
			{Type: "reference", Text: "z", Source: "z"},
			{Type: "avoid", Text: "a", Source: "a"},
		},
		RiskNotes: []string{"z risk", "a risk"},
		MemoryReferences: []MemoryRef{
			{ID: "m2", Content: "memory two", Influence: "reference"},
			{ID: "m1", Content: "memory one", Influence: "evidence"},
		},
		StrategySelection: &StrategyPick{Selected: "general", Reason: "default", Rejected: []RejectedStrategy{
			{ID: "z", Score: 0.1},
			{ID: "a", Score: 0.9},
		}},
	}
	ir2 := PlannerIR{
		Version:     version,
		Goal:        "fix a bug",
		SourceEvent: "fix a bug",
		RuntimeMode: "control",
		Constraints: []Constraint{
			{Type: "avoid", Text: "a", Source: "a"},
			{Type: "reference", Text: "z", Source: "z"},
		},
		RiskNotes: []string{"a risk", "z risk"},
		MemoryReferences: []MemoryRef{
			{ID: "m1", Content: "memory one", Influence: "evidence"},
			{ID: "m2", Content: "memory two", Influence: "reference"},
		},
		StrategySelection: &StrategyPick{Selected: "general", Reason: "default", Rejected: []RejectedStrategy{
			{ID: "a", Score: 0.9},
			{ID: "z", Score: 0.1},
		}},
	}
	got1, err := compileExecutionContract(ir1)
	if err != nil {
		t.Fatal(err)
	}
	got2, err := compileExecutionContract(ir2)
	if err != nil {
		t.Fatal(err)
	}
	if got1 != got2 {
		t.Fatalf("canonical contracts differ:\n%s\n---\n%s", got1, got2)
	}
	if !strings.Contains(got1, `"constraints":[`) || !strings.Contains(got1, `"memory_references":[`) {
		t.Fatalf("expected explicit canonical IR arrays, got:\n%s", got1)
	}
}

func TestStrategyExplorationIsDeterministicAndBounded(t *testing.T) {
	strategies := ensureBuiltInStrategies(nil)
	var exploredGoal string
	var firstPick StrategyPick
	for i := 0; i < 200; i++ {
		goal := "fix a bug variant " + string(rune('a'+i%26)) + "-" + string(rune('a'+(i/26)%26))
		ranked := rankStrategies(goal, strategies)
		pick := selectStrategy(goal, ranked)
		if pick.Mode == "explore" {
			exploredGoal = goal
			firstPick = pick
			break
		}
	}
	if exploredGoal == "" {
		t.Fatal("expected at least one deterministic exploration goal")
	}
	for i := 0; i < 5; i++ {
		got := selectStrategy(exploredGoal, rankStrategies(exploredGoal, strategies))
		if got.Selected != firstPick.Selected || got.Mode != firstPick.Mode {
			t.Fatalf("exploration selection is not deterministic: first=%+v got=%+v", firstPick, got)
		}
	}
	if firstPick.ExplorationRate != 0.1 {
		t.Fatalf("exploration rate = %v, want 0.1", firstPick.ExplorationRate)
	}
}

func TestTruthLockedNodeCannotBeOverwritten(t *testing.T) {
	now := time.Now().UTC()
	nodes := []MemoryNode{{
		ID:          "tool:trace:0",
		Type:        "tool_result",
		Content:     "original result",
		Timestamp:   now,
		Confidence:  0.95,
		Quality:     QualityHighSignal,
		TruthLocked: true,
	}}

	nodes = upsertNode(nodes, MemoryNode{
		ID:         "tool:trace:0",
		Type:       "tool_result",
		Content:    "overwritten result",
		Timestamp:  now.Add(time.Second),
		Confidence: 0.1,
		Quality:    QualityNoise,
	})
	if nodes[0].Content != "original result" {
		t.Fatalf("truth-locked node was overwritten: %+v", nodes[0])
	}
}

func readState(t *testing.T, dir string) state {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, stateFile))
	if err != nil {
		t.Fatal(err)
	}
	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func readTraces(t *testing.T, dir string) []ExecutionTrace {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, tracesFile))
	if err != nil {
		t.Fatal(err)
	}
	var traces []ExecutionTrace
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var tr ExecutionTrace
		if err := json.Unmarshal([]byte(line), &tr); err != nil {
			t.Fatal(err)
		}
		traces = append(traces, tr)
	}
	return traces
}

func readLearningTraces(t *testing.T, dir string) []LearningTrace {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, learningTracesFile))
	if err != nil {
		t.Fatal(err)
	}
	var traces []LearningTrace
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var tr LearningTrace
		if err := json.Unmarshal([]byte(line), &tr); err != nil {
			t.Fatal(err)
		}
		traces = append(traces, tr)
	}
	return traces
}

func assertNode(t *testing.T, nodes []MemoryNode, pred func(MemoryNode) bool, desc string) {
	t.Helper()
	for _, n := range nodes {
		if pred(n) {
			return
		}
	}
	t.Fatalf("missing node: %s\nnodes=%+v", desc, nodes)
}

func assertEdge(t *testing.T, edges []MemoryEdge, relation string) {
	t.Helper()
	for _, e := range edges {
		if e.Relation == relation {
			return
		}
	}
	t.Fatalf("missing %s edge: %+v", relation, edges)
}
