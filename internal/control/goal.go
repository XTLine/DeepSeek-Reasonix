package control

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"reasonix/internal/evidence"
	fileencoding "reasonix/internal/fileutil/encoding"
	"reasonix/internal/store"
)

const (
	goalContinueTurn   = "Continue pursuing the active goal. Do the next useful work and report your judgment with update_goal: continue (give the next concrete step), complete (you judge the goal finished), or blocked (explain why you cannot continue). Keep execution results and any verification limitations accurate."
	goalCompleteNotice = "Model reported the goal complete."
	unlimitedGoalTurns = -1

	// Bound the persisted novelty window. Signatures are compact hashes, and
	// retaining the most recent window is enough to stop short repeat cycles
	// without allowing an unbounded Goal sidecar.
	maxGoalProgressEvidence = 512
)

// Budget class aliases remain as sidecar/CLI compatibility metadata only.
const (
	budgetClassSimple   = BudgetClassSimple
	budgetClassWrite    = BudgetClassWrite
	budgetClassResearch = BudgetClassResearch
)

// Stop causes distinguish a safe pause from a genuine block. Removed numeric
// causes remain migration-only constants so old sidecars can be normalized.
const (
	stopCauseBudgetTurns   = "budget_turns" // legacy; the class-derived turn quota is gone
	stopCauseBudgetSpend   = "budget_spend"
	stopCauseBudgetTokens  = "budget_tokens"   // legacy; never written by current runtime
	stopCauseNoProgress    = "no_progress"     // legacy; never written by current runtime
	stopCauseGoalRunBudget = "goal_run_budget" // legacy; the per-Run round ceiling is gone
	stopCauseGoalStuck     = "goal_stuck"
	stopCauseEvaluator     = "evaluator_unavailable"
	stopCauseLegacyArchive = "legacy_archive"
	stopCauseManual        = "manual"
)

// budgetClassForLegacyMode translates old sidecars and deprecated CLI flags at
// the compatibility boundary. The active Goal runtime stores only budgetClass.
func budgetClassForLegacyMode(goal string, researchMode GoalResearchMode) string {
	switch researchMode {
	case GoalResearchOn:
		return budgetClassResearch
	case GoalResearchOff:
		if GoalNeedsWriteBudget(goal) {
			return budgetClassWrite
		}
		return budgetClassSimple
	default:
		return ClassifyGoalBudget(goal)
	}
}

// goalMachine owns the active goal FSM and its persistence. It is a strict
// leaf: methods take only machine locks and never call back into Controller.
// advance() takes already-gathered inputs so no disk/executor work holds mu.
type goalMachine struct {
	// mu guards the FSM fields below; every critical section under it is short
	// and non-blocking (no disk I/O, no executor calls).
	mu                 sync.Mutex
	goal               string
	status             string
	scopeID            string
	deliveryCheckpoint evidence.DeliveryCheckpoint
	block              string
	strict             bool
	continuationEpoch  uint64

	tokenBudget int // configured ceiling for an unattended loop; 0 = unbounded

	// Runtime statistics and optional user-selected spend state, persisted
	// across turns and restarts. turnsUsed and noProgressTurns are observational;
	// tokensLimit is non-zero only when the user configured a Goal token budget.
	budgetClass            string
	turnsUsed              int
	turnsLimit             int
	tokensUsed             int
	requestsUsed           int
	workDurationMs         int64
	tokensLimit            int // always 0 at runtime; deprecated hard limit
	noProgressTurns        int
	noProgressLimit        int
	lastContinuationReason string
	launch                 goalLaunchState
	goalActivationState
	lastEvaluatorReason string
	stopCause           string
	budgetExtensions    int // deprecated historical sidecar field
	progressEvidence    []string
	// stateExtra preserves fields written by a newer peer during read/modify/
	// write cycles. Known current fields always win on serialization.
	stateExtra map[string]json.RawMessage
	// legacyTaskID is retained only while a historical AutoResearch archive is
	// awaiting migration. It is serialized on fail-closed blocked sidecars so a
	// restart can retry the migration without treating the raw archive path as a
	// new Goal.
	legacyTaskID string

	// statePath is the persisted goal-state sidecar; empty disables persistence.
	statePath string
	// writeMu serializes goal-state disk writes so concurrent saves don't
	// interleave or land out of order. Taken OFF mu by writeState.
	writeMu sync.Mutex
}

// goalState is the serializable form of a running goal. New fields are
// safe-to-omit JSON: old readers ignore them, and restoreFromState re-derives
// defaults when they are missing.
type goalState struct {
	Goal               string                      `json:"goal,omitempty"`
	Status             string                      `json:"status,omitempty"`
	ResearchMode       GoalResearchMode            `json:"researchMode,omitempty"`
	AutoResearchTaskID string                      `json:"autoResearchTaskID,omitempty"`
	ScopeID            string                      `json:"scopeID,omitempty"`
	DeliveryCheckpoint evidence.DeliveryCheckpoint `json:"deliveryCheckpoint,omitempty"`
	Turns              int                         `json:"turns,omitempty"`
	Blocks             int                         `json:"blocks,omitempty"`
	Block              string                      `json:"block,omitempty"`
	Strict             bool                        `json:"strict,omitempty"`
	Todos              []evidence.TodoItem         `json:"todos,omitempty"`

	BudgetClass            string   `json:"budgetClass,omitempty"`
	TurnsUsed              int      `json:"turnsUsed,omitempty"`
	TurnsLimit             int      `json:"turnsLimit,omitempty"`
	TokensUsed             int      `json:"tokensUsed,omitempty"`
	RequestsUsed           int      `json:"requestsUsed,omitempty"`
	WorkDurationMs         int64    `json:"workDurationMs,omitempty"`
	TokensLimit            int      `json:"tokensLimit,omitempty"`
	NoProgressTurns        int      `json:"noProgressTurns,omitempty"`
	NoProgressLimit        int      `json:"noProgressLimit,omitempty"`
	LastContinuationReason string   `json:"lastContinuationReason,omitempty"`
	LastEvaluatorReason    string   `json:"lastEvaluatorReason,omitempty"`
	StopCause              string   `json:"stopCause,omitempty"`
	BudgetExtensions       int      `json:"budgetExtensions,omitempty"`
	ProgressEvidence       []string `json:"progressEvidence,omitempty"`
}

// goalAdvanceInput carries everything the FSM needs for one continuation step,
// gathered by the caller off the machine's lock. It commits model reports
// while honoring execution pauses and the current lifecycle identity.
type goalAdvanceInput struct {
	report           *goalTurnReport // validated update_goal report; nil when none
	todos            []evidence.TodoItem
	progressEvidence []string // host evidence identities visible after this turn
	pauseCause       string   // explicit spend boundary reported by the Agent
	pauseReason      string
	expectedEpoch    *uint64
}

// goalAdvanceResult reports the FSM step's outcome. data/path/ok describe the
// state to persist (built under mu when something changed); notice is surfaced
// to the user; cont reports whether the goal loop should continue; intercept
// (with interceptNotice) is the next synthetic turn's prompt.
type goalAdvanceResult struct {
	notice            string
	intercept         string
	interceptNotice   string
	cont              bool
	continuationEpoch uint64
	path              string
	data              []byte
	ok                bool
}

// goalContinuationSnapshot binds a continuation to the exact Goal lifecycle
// state admitted for its synthetic turn. The orchestrator uses these captured
// fields throughout the turn instead of re-reading a possibly replaced Goal.
type goalContinuationSnapshot struct {
	goal    string
	scopeID string
}

// goalStatePath derives a session's persisted goal-state sidecar.
func goalStatePath(sessionPath string) string {
	return store.SessionGoalState(sessionPath)
}

func (g *goalMachine) setStatePath(path string) {
	g.mu.Lock()
	g.statePath = path
	g.mu.Unlock()
}

// snapshot returns the fields Compose injects into outgoing turns.
func (g *goalMachine) snapshot() (goal, status string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.disarmed && g.status == GoalStatusRunning {
		return g.goal, GoalStatusStopped
	}
	return g.goal, g.status
}

func (g *goalMachine) goalText() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.goal
}

// continuationToken captures the Goal lifecycle that owns an outgoing turn.
// The matching assistant output may advance the FSM only while this epoch is
// still current.
func (g *goalMachine) continuationToken() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.continuationEpoch
}

func (g *goalMachine) deliveryScope() (id, task string, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.disarmed || strings.TrimSpace(g.goal) == "" || g.status != GoalStatusRunning {
		return "", "", false
	}
	if g.scopeID == "" {
		g.scopeID = newGoalScopeID()
	}
	return g.scopeID, g.goal, true
}

// goalScopeIDForTurn resolves the active goal scope for an outgoing turn: the
// continuation snapshot's scope, or the running goal's (assigning one when
// needed). ok=false means no active goal.
func (g *goalMachine) goalScopeIDForTurn(continuation *goalContinuationSnapshot) (string, bool) {
	if continuation != nil {
		return continuation.scopeID, true
	}
	id, _, ok := g.deliveryScope()
	return id, ok
}

func newGoalScopeID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return fmt.Sprintf("goal-%x", raw[:])
	}
	return fmt.Sprintf("goal-fallback-%d-%d", os.Getpid(), time.Now().UnixNano())
}

// active reports whether a goal is currently running.
func (g *goalMachine) active() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return !g.disarmed && strings.TrimSpace(g.goal) != "" && g.status == GoalStatusRunning
}

// statusForDisplay maps the empty zero status to "stopped" for frontends.
func (g *goalMachine) statusForDisplay() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.status == "" || g.disarmed && g.status == GoalStatusRunning {
		return GoalStatusStopped
	}
	return g.status
}

// set installs a session-scoped goal (or clears it when goal is empty), resets
// the per-goal runtime counters, and returns the state to persist. ok is
// false (no persistence) when the goal is unchanged or no state path is
// configured.
func (g *goalMachine) set(goal, preferredBudgetClass string, todos []evidence.TodoItem) (string, []byte, bool) {
	goal = strings.TrimSpace(goal)
	if goal != "" && preferredBudgetClass == "" {
		preferredBudgetClass = ClassifyGoalBudget(goal)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if goal != "" && g.goal == goal && g.status == GoalStatusRunning {
		if !g.disarmed {
			if g.budgetClass != preferredBudgetClass {
				g.budgetClass = preferredBudgetClass
				return g.buildStateLocked(todos)
			}
			return "", nil, false
		}
		// Explicitly starting the restored objective preserves its identity and
		// actual accumulated usage, rather than creating a fresh budget.
		g.disarmed = false
		g.continuationEpoch++
		return g.buildStateLocked(todos)
	}
	g.installGoalLocked(goal, preferredBudgetClass)
	return g.buildStateLocked(todos)
}

// setLegacyArchiveBlocked atomically installs and blocks an explicit legacy
// archive goal. A concurrent Goal replacement cannot be blocked between two
// separate FSM mutations.
func (g *goalMachine) setLegacyArchiveBlocked(goal, preferredBudgetClass, reason string, todos []evidence.TodoItem) (string, []byte, bool) {
	return g.setLegacyArchiveBlockedWithTaskID(goal, preferredBudgetClass, reason, "", todos)
}

func (g *goalMachine) setLegacyArchiveBlockedWithTaskID(goal, preferredBudgetClass, reason, taskID string, todos []evidence.TodoItem) (string, []byte, bool) {
	goal = strings.TrimSpace(goal)
	taskID = strings.TrimSpace(taskID)
	if goal != "" && preferredBudgetClass == "" {
		preferredBudgetClass = ClassifyGoalBudget(goal)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.installGoalLocked(goal, preferredBudgetClass)
	if goal != "" {
		g.status = GoalStatusBlocked
	}
	g.stopCause = stopCauseLegacyArchive
	g.block = clipGoalReason(reason)
	g.legacyTaskID = taskID
	return g.buildStateLocked(todos)
}

func (g *goalMachine) installGoalLocked(goal, preferredBudgetClass string) {
	g.disarmed = false
	g.continuationEpoch++
	g.turnsUsed, g.tokensUsed, g.requestsUsed, g.noProgressTurns = 0, 0, 0, 0
	g.workDurationMs = 0
	g.block = ""
	g.lastContinuationReason, g.lastEvaluatorReason = "", ""
	g.stopCause = ""
	g.budgetExtensions = 0
	g.progressEvidence = nil
	if goal == "" {
		g.goal, g.status = "", GoalStatusStopped
		g.budgetClass = ""
		g.turnsLimit = 0
		g.noProgressLimit = 0
		g.scopeID = ""
		g.deliveryCheckpoint = evidence.DeliveryCheckpoint{}
	} else {
		g.goal, g.status = goal, GoalStatusRunning
		g.scopeID = newGoalScopeID()
		g.deliveryCheckpoint = evidence.DeliveryCheckpoint{ScopeID: g.scopeID}
		g.budgetClass = preferredBudgetClass
		g.turnsLimit = unlimitedGoalTurns
		g.tokensLimit = g.tokenBudget
		g.noProgressLimit = 0
	}
	// Installing a normal Goal always abandons any pending legacy migration.
	g.legacyTaskID = ""
}

func (g *goalMachine) setStrict(strict bool, todos []evidence.TodoItem) (string, []byte, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.strict = strict
	return g.buildStateLocked(todos)
}

// stop transitions a running goal to the given terminal status and clears the
// transient runtime bookkeeping. stopCause is cleared: a host stop is not a
// safe pause.
func (g *goalMachine) stop(status string, todos []evidence.TodoItem) (string, []byte, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.continuationEpoch++
	if strings.TrimSpace(g.goal) != "" && g.status == GoalStatusRunning {
		g.status = status
	}
	g.stopCause = ""
	g.noProgressTurns = 0
	return g.buildStateLocked(todos)
}

// pauseFor transitions a running goal to a safe pause: status blocked plus a
// stop cause, keeping every runtime counter for a later resume.
func (g *goalMachine) pauseFor(stopCause, reason string, todos []evidence.TodoItem) (string, []byte, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.continuationEpoch++
	if strings.TrimSpace(g.goal) != "" && g.status == GoalStatusRunning {
		g.status = GoalStatusBlocked
	}
	g.stopCause = stopCause
	if reason != "" {
		g.block = reason
	}
	return g.buildStateLocked(todos)
}

// resume re-enters a recoverable blocked/stopped goal without resetting scope
// or runtime history. Continuous Goals never extend a numeric quota.
func (g *goalMachine) resume(todos []evidence.TodoItem) (path string, data []byte, persist, resumed bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopCause == stopCauseLegacyArchive {
		// A legacy archive block is recoverable only through the read-only
		// archive boundary; never reinterpret it as an ordinary Goal resume.
		return "", nil, false, false
	}
	if strings.TrimSpace(g.goal) == "" || g.status == GoalStatusComplete {
		return "", nil, false, false
	}
	// A user-selected spend pause grants one fresh configured slice. Usage
	// remains cumulative; only the absolute threshold moves forward.
	spentBudget := g.stopCause == stopCauseBudgetSpend
	g.continuationEpoch++
	g.status = GoalStatusRunning
	g.disarmed = false
	g.block = ""
	g.stopCause = ""
	g.noProgressTurns = 0
	g.turnsLimit = unlimitedGoalTurns
	g.noProgressLimit = 0
	g.budgetExtensions = 0
	g.grantSpendSliceLocked(spentBudget)
	if g.scopeID == "" {
		g.scopeID = newGoalScopeID()
	}
	path, data, persist = g.buildStateLocked(todos)
	return path, data, persist, true
}

func (g *goalMachine) setDeliveryCheckpoint(checkpoint evidence.DeliveryCheckpoint, todos []evidence.TodoItem) (string, []byte, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.scopeID == "" || checkpoint.ScopeID != g.scopeID {
		return "", nil, false
	}
	g.deliveryCheckpoint = checkpoint
	return g.buildStateLocked(todos)
}

func (g *goalMachine) deliveryState() evidence.DeliveryCheckpoint {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.deliveryCheckpoint
}

// acceptContinuation checks an advance result before the orchestrator surfaces
// its notice. admitContinuation revalidates after synchronous notice callbacks
// and captures the Goal state at the synthetic-turn admission boundary.
func (g *goalMachine) acceptContinuation(res goalAdvanceResult) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !res.cont ||
		res.continuationEpoch != g.continuationEpoch ||
		strings.TrimSpace(g.goal) == "" ||
		g.status != GoalStatusRunning || g.disarmed {
		return "", false
	}
	return res.intercept, true
}

// admitContinuation atomically validates an advance result and captures the
// Goal state used to compose and scope its synthetic turn. Keeping validation
// and capture in one critical section prevents a stale intercept from being
// paired with a replacement Goal between those operations.
func (g *goalMachine) admitContinuation(res goalAdvanceResult) (goalContinuationSnapshot, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !res.cont ||
		res.continuationEpoch != g.continuationEpoch ||
		strings.TrimSpace(g.goal) == "" ||
		g.status != GoalStatusRunning || g.disarmed {
		return goalContinuationSnapshot{}, false
	}
	if g.scopeID == "" {
		g.scopeID = newGoalScopeID()
	}
	return goalContinuationSnapshot{
		goal:    g.goal,
		scopeID: g.scopeID,
	}, true
}

// advance commits a model report only at the owning normal turn boundary.
// Execution pauses and configured budgets take precedence over completion.
func (g *goalMachine) advance(in goalAdvanceInput) goalAdvanceResult {
	g.mu.Lock()
	defer g.mu.Unlock()
	if in.expectedEpoch != nil && *in.expectedEpoch != g.continuationEpoch {
		return goalAdvanceResult{cont: false}
	}
	if g.disarmed || strings.TrimSpace(g.goal) == "" || g.status != GoalStatusRunning {
		return goalAdvanceResult{cont: false}
	}
	g.continuationEpoch++
	// A top-level goal turn (the first turn or a synthetic continuation) counts
	// as an observational statistic; the in-Run model/tool loop is not re-counted.
	g.turnsUsed++
	var notice string
	var intercept string
	var interceptNotice string
	reportBlocked := in.report != nil && in.report.status == GoalStatusBlocked
	reportComplete := in.report != nil && in.report.status == GoalStatusComplete
	g.observeGoalProgress(in, reportBlocked || reportComplete)
	switch {
	case in.pauseCause != "":
		g.status = GoalStatusBlocked
		g.stopCause = in.pauseCause
		g.block = clipGoalReason(in.pauseReason)
		notice = "goal paused: " + g.block
	case g.tokensLimit > 0 && g.tokensUsed >= g.tokensLimit:
		g.status = GoalStatusBlocked
		g.stopCause = stopCauseBudgetSpend
		g.block = fmt.Sprintf("token budget reached (%d/%d tokens used)", g.tokensUsed, g.tokensLimit)
		notice = "goal paused: " + g.block
	case reportBlocked:
		g.status = GoalStatusBlocked
		g.block = cleanGoalBlockReason(in.report.reason)
		g.stopCause = ""
		g.lastContinuationReason = clipGoalReason(in.report.reason)
		notice = "goal blocked: " + g.block
	case reportComplete:
		g.goal = ""
		g.status = GoalStatusComplete
		g.block, g.stopCause = "", ""
		g.progressEvidence = nil
		g.lastContinuationReason, g.lastEvaluatorReason = "", ""
		notice = goalCompleteNotice
	default:
		if in.report != nil {
			g.lastContinuationReason = clipGoalReason(in.report.reason)
			intercept = in.report.nextAction
		}
	}
	res := goalAdvanceResult{
		notice:            notice,
		intercept:         intercept,
		interceptNotice:   interceptNotice,
		cont:              notice == "",
		continuationEpoch: g.continuationEpoch,
	}
	res.path, res.data, res.ok = g.buildStateLocked(in.todos)
	return res
}

// foldUsage attributes a turn's billable tokens to the goal, but only while the
// goal lifecycle still matches the recorder's scope+epoch; stale or replaced
// goals reject late usage.
func (g *goalMachine) foldUsage(scopeID string, epoch uint64, tokens, requests int) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if (tokens <= 0 && requests <= 0) || g.scopeID != scopeID || g.continuationEpoch != epoch {
		return false
	}
	if tokens > 0 {
		g.tokensUsed += tokens
	}
	if requests > 0 {
		g.requestsUsed += requests
	}
	return true
}

// foldWorkDuration attributes one Run's cumulative assistant work duration to
// the Goal. The caller supplies the maximum WorkDurationMs among messages
// created by that Run, so multi-round cumulative values are not double-counted.
func (g *goalMachine) foldWorkDuration(scopeID string, epoch uint64, durationMs int64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if durationMs <= 0 || g.scopeID != scopeID || g.continuationEpoch != epoch {
		return false
	}
	g.workDurationMs += durationMs
	return true
}

// buildStateLocked marshals the current goal state for persistence. The caller
// holds mu; this only reads in-memory state, never touching disk. Returns ok=false
// when persistence is disabled (no state path). The matching writeState does the
// disk write OFF mu so the per-turn save can't stall a status poll.
func (g *goalMachine) buildStateLocked(todos []evidence.TodoItem) (path string, data []byte, ok bool) {
	if g.statePath == "" {
		return "", nil, false
	}
	state := goalState{
		Goal:                   g.goal,
		Status:                 g.status,
		ScopeID:                g.scopeID,
		DeliveryCheckpoint:     g.deliveryCheckpoint,
		Turns:                  g.turnsUsed,
		Block:                  g.block,
		Strict:                 g.strict,
		Todos:                  todos,
		BudgetClass:            g.budgetClass,
		TurnsUsed:              g.turnsUsed,
		TurnsLimit:             g.turnsLimit,
		TokensUsed:             g.tokensUsed,
		RequestsUsed:           g.requestsUsed,
		WorkDurationMs:         g.workDurationMs,
		TokensLimit:            g.tokensLimit,
		NoProgressTurns:        g.noProgressTurns,
		NoProgressLimit:        g.noProgressLimit,
		LastContinuationReason: g.lastContinuationReason,
		LastEvaluatorReason:    g.lastEvaluatorReason,
		StopCause:              g.stopCause,
		BudgetExtensions:       g.budgetExtensions,
		ProgressEvidence:       append([]string(nil), g.progressEvidence...),
	}
	// GoalResearchOff is a downgrade fence for ordinary Goal sidecars. A
	// fail-closed legacy migration keeps its task identity and compatibility mode
	// until the archive has been validated and the Goal-only state is committed.
	if g.legacyTaskID != "" && g.status == GoalStatusBlocked && g.stopCause == stopCauseLegacyArchive {
		state.AutoResearchTaskID = g.legacyTaskID
		state.ResearchMode = GoalResearchOn
	} else {
		state.ResearchMode = GoalResearchOff
	}
	b, err := marshalGoalState(state, g.stateExtra)
	if err != nil {
		slog.Warn("controller: marshal goal state", "err", err)
		return "", nil, false
	}
	return g.statePath, b, true
}

// writeState preserves the existing best-effort behavior for background Goal
// progress. Callers that need transactional persistence use writeStateErr.
func (g *goalMachine) writeState(path string, data []byte) {
	if err := g.writeStateErr(path, data); err != nil {
		slog.Warn("controller: write goal state", "err", err)
	}
}

// terminalTodosFromState reads the persisted goal-state sidecar and returns its
// todo snapshot only after the goal has reached a terminal state. Running goal
// state is not refreshed on every todo_write, so its todos may be older than the
// transcript rebuilt by Agent.SetSession.
func (g *goalMachine) terminalTodosFromState(sessionPath string) ([]evidence.TodoItem, bool) {
	if strings.TrimSpace(sessionPath) == "" {
		return nil, false
	}
	data, err := fileencoding.ReadFileUTF8(goalStatePath(sessionPath))
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("controller: read goal state", "err", err)
		}
		return nil, false
	}
	var state goalState
	if err := json.Unmarshal(data, &state); err != nil {
		slog.Warn("controller: parse goal state", "err", err)
		return nil, false
	}
	switch state.Status {
	case GoalStatusComplete, GoalStatusBlocked, GoalStatusStopped:
	default:
		return nil, false
	}
	if len(state.Todos) == 0 {
		return nil, false
	}
	return append([]evidence.TodoItem(nil), state.Todos...), true
}

// restoreFromState reloads Goal state from the sidecar. The sidecar is
// authoritative; active Goals are normalized to continuous-runtime sentinels.
// migrated means path/data were atomically rewritten (without a provider call).
// legacyTaskID is returned only
// so Controller can fill missing goal text from a historical archive.
func (g *goalMachine) restoreFromState(sessionPath string) (path string, data []byte, migrated bool, legacy legacyGoalRestore) {
	if strings.TrimSpace(sessionPath) == "" {
		return "", nil, false, legacyGoalRestore{}
	}
	// Ensure write path is bound even when the controller rebuilds.
	if g.statePath == "" {
		g.setStatePath(goalStatePath(sessionPath))
	}
	raw, err := fileencoding.ReadFileUTF8(goalStatePath(sessionPath))
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("controller: read goal state", "err", err)
		}
		return "", nil, false, legacyGoalRestore{}
	}
	var state goalState
	if err := json.Unmarshal(raw, &state); err != nil {
		slog.Warn("controller: parse goal state", "err", err)
		return "", nil, false, legacyGoalRestore{}
	}
	g.mu.Lock()
	g.stateExtra = goalStateUnknownFields(raw)
	g.goal = strings.TrimSpace(state.Goal)
	g.disarmed = true
	g.status = state.Status
	if g.status == "" {
		g.status = GoalStatusStopped
	}
	// Legacy task identity is migration-only compatibility data. It is returned to
	// the Controller's archive boundary and retained in the machine only while a
	// fail-closed migration remains pending.
	legacy = legacyGoalRestore{
		taskID: strings.TrimSpace(state.AutoResearchTaskID),
		todos:  append([]evidence.TodoItem(nil), state.Todos...),
	}
	// A task id is pending only when the sidecar has no Goal text. A legacy
	// sidecar that already contains an objective can be migrated directly and
	// must serialize as ordinary Goal state on the first write.
	if g.goal == "" {
		g.legacyTaskID = legacy.taskID
	} else {
		g.legacyTaskID = ""
	}
	g.scopeID = strings.TrimSpace(state.ScopeID)
	if g.scopeID == "" {
		g.scopeID = strings.TrimSpace(state.DeliveryCheckpoint.ScopeID)
	}
	if g.goal != "" && g.scopeID == "" {
		g.scopeID = newGoalScopeID()
	}
	g.deliveryCheckpoint = state.DeliveryCheckpoint
	if g.scopeID == "" {
		g.deliveryCheckpoint = evidence.DeliveryCheckpoint{}
	} else if g.deliveryCheckpoint.ScopeID == "" {
		g.deliveryCheckpoint.ScopeID = g.scopeID
	} else if g.deliveryCheckpoint.ScopeID != g.scopeID {
		g.deliveryCheckpoint = evidence.DeliveryCheckpoint{ScopeID: g.scopeID}
	}
	g.block = state.Block
	g.strict = state.Strict
	g.stopCause = state.StopCause
	g.budgetExtensions = state.BudgetExtensions
	g.progressEvidence, _ = mergeGoalProgressEvidence(nil, state.ProgressEvidence)
	g.lastContinuationReason = state.LastContinuationReason
	g.lastEvaluatorReason = state.LastEvaluatorReason
	// Old sidecars carry Turns (pre-budget counting); treat it as turn usage.
	g.turnsUsed = state.TurnsUsed
	if g.turnsUsed == 0 && state.Turns > 0 {
		g.turnsUsed = state.Turns
	}
	g.tokensUsed = state.TokensUsed
	g.requestsUsed = state.RequestsUsed
	g.workDurationMs = state.WorkDurationMs
	g.budgetClass = normalizeBudgetClass(g.goal, state.BudgetClass, state.ResearchMode)
	g.turnsLimit = state.TurnsLimit
	g.noProgressTurns = state.NoProgressTurns
	g.noProgressLimit = state.NoProgressLimit
	g.tokensLimit = state.TokensLimit
	// Normalize in memory only. Reading history must not write a sidecar;
	// the next ordinary save persists compatibility values under its lease.
	g.normalizeContinuousState(state.ResearchMode, legacy.taskID)
	g.continuationEpoch++
	legacy.epoch = g.continuationEpoch
	g.mu.Unlock()
	return "", nil, false, legacy
}

// clipGoalReason bounds a recorded reason for storage and display.
func clipGoalReason(reason string) string {
	reason = strings.TrimSpace(reason)
	const max = 400
	if r := []rune(reason); len(r) > max {
		return string(r[:max]) + "..."
	}
	return reason
}

func cleanGoalBlockReason(reason string) string {
	return strings.Trim(strings.TrimSpace(reason), " \t\r\n:：,，.。;；!！?？-—_[]()（）")
}

// ShortGoalForNotice collapses whitespace and truncates a goal for one-line UI.
func ShortGoalForNotice(goal string) string {
	goal = strings.Join(strings.Fields(goal), " ")
	runes := []rune(goal)
	const max = 160
	if len(runes) <= max {
		return goal
	}
	return string(runes[:max]) + "..."
}

// goalTodos snapshots the executor's canonical todos for goal-state persistence.
func (c *Controller) goalTodos() []evidence.TodoItem {
	if c.executor == nil {
		return nil
	}
	return c.executor.CanonicalTodoState()
}

// persistGoalState writes a freshly built goal state to disk, off c.mu. The
// executor guard preserves the original behavior of skipping persistence when
// no executor is attached.
func (c *Controller) persistGoalState(path string, data []byte, ok bool) {
	if !ok || c.executor == nil {
		return
	}
	c.goals.writeState(path, data)
}

func (c *Controller) persistGoalStateAtEpoch(epoch uint64, todos []evidence.TodoItem) (bool, error) {
	applied, err := c.goals.writeStateAtEpoch(epoch, todos)
	if err != nil {
		slog.Warn("controller: write goal state", "err", err)
	}
	return applied, err
}

func (c *Controller) restoreTerminalGoalTodos(sessionPath string) {
	if c.executor == nil {
		return
	}
	todos, ok := c.goals.terminalTodosFromState(sessionPath)
	if !ok {
		return
	}
	c.executor.ReplaceTodoState(todos)
}
