package session

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// SessionRef is the only execution identity used by the linear session
// service. Paths and UI selection are deliberately absent.
type SessionRef struct {
	HostID    string `json:"hostId"`
	SessionID string `json:"sessionId"`
}

func (r SessionRef) validate(hostID string) error {
	if r.HostID == "" || r.HostID != hostID {
		return fmt.Errorf("session: session host %q does not match service host %q", r.HostID, hostID)
	}
	return validateSessionID(r.SessionID)
}

var (
	ErrSessionNotRunning = errors.New("session runtime is not attached")
	ErrRuntimeBusy       = errors.New("session runtime already has an activity")
	ErrRuntimeBound      = errors.New("session runtime still has client bindings")
	ErrRuntimeRetiring   = errors.New("session runtime is retiring")
	ErrRecoveryRequired  = errors.New("session runtime requires recovery")
	ErrStaleActivity     = errors.New("session activity no longer owns commit authority")
)

type RuntimePhase string

const (
	RuntimeIdle             RuntimePhase = "idle"
	RuntimeRunning          RuntimePhase = "running"
	RuntimeCancelling       RuntimePhase = "cancelling"
	RuntimeRecoveryRequired RuntimePhase = "recovery_required"
	RuntimeClosed           RuntimePhase = "closed"
)

type RuntimeSnapshot struct {
	Ref              SessionRef   `json:"session"`
	Epoch            string       `json:"runtimeEpoch"`
	ActivityRevision uint64       `json:"activityRevision"`
	Phase            RuntimePhase `json:"phase"`
	Activity         string       `json:"activity,omitempty"`
	Session          Snapshot     `json:"sessionSnapshot"`
}

type CancelReceipt struct {
	Ref              SessionRef   `json:"session"`
	Accepted         bool         `json:"accepted"`
	RuntimeEpoch     string       `json:"runtimeEpoch,omitempty"`
	ActivityRevision uint64       `json:"activityRevision,omitempty"`
	Phase            RuntimePhase `json:"phase"`
}

// Runtime is the sole owner of a live Session and its write handle. It owns
// transient activity and cancellation; persisted running events never create
// a Runtime after process restart.
type Runtime struct {
	ref     SessionRef
	epoch   string
	session *Session
	owner   *Service
	// instance stamps the publish grant so a delayed owner can prove it still
	// refers to the exact instance it published.
	instance string

	mu         sync.Mutex
	phase      RuntimePhase
	activity   string
	revision   atomic.Uint64
	activityID uint64
	current    atomic.Pointer[Activity]
	closeDone  chan struct{}
	closeErr   error
}

// Activity is a generation-bound permit. Agent and tool work commit business
// events through it so a cancelled or replaced activity cannot publish a late
// result into the session. Diagnostic logging uses a separate non-business
// channel and does not regain this permit.
type Activity struct {
	runtime       *Runtime
	id            uint64
	name          string
	cancel        context.CancelFunc
	stopped       atomic.Bool
	done          chan struct{}
	doneOnce      sync.Once
	superviseOnce sync.Once
	// commitGate fences the final eligibility check and the in-memory commit
	// against cancellation without making Cancel wait for a storage lock.
	commitGate sync.RWMutex
}

func newRuntime(ref SessionRef, session *Session) *Runtime {
	runtime := &Runtime{ref: ref, epoch: randomID(), session: session, phase: RuntimeIdle}
	runtime.revision.Store(1)
	return runtime
}

func (r *Runtime) Ref() SessionRef { return r.ref }

func (r *Runtime) Session() *Session { return r.session }

func (r *Runtime) Snapshot() RuntimeSnapshot {
	state := r.activitySnapshot()
	state.Session = r.session.Snapshot()
	return state
}

func (r *Runtime) StateSnapshot() RuntimeSnapshot {
	state := r.activitySnapshot()
	state.Session = r.session.StateSnapshot()
	return state
}

// ExecutionSnapshot returns the provider projection and lightweight turn
// boundaries without reconstructing the durable UI transcript.
func (r *Runtime) ExecutionSnapshot() RuntimeSnapshot {
	state := r.activitySnapshot()
	state.Session = r.session.ExecutionSnapshot()
	return state
}

func (r *Runtime) activitySnapshot() RuntimeSnapshot {
	r.mu.Lock()
	phase := r.phase
	if current := r.current.Load(); current != nil && current.stopped.Load() && phase == RuntimeRunning {
		phase = RuntimeCancelling
	}
	state := RuntimeSnapshot{Ref: r.ref, Epoch: r.epoch, ActivityRevision: r.revision.Load(), Phase: phase, Activity: r.activity}
	r.mu.Unlock()
	return state
}

// BeginActivity establishes cancellation ownership before any turn event or
// downstream work starts. finish only affects the exact activity generation.
func (r *Runtime) BeginActivity(parent context.Context, name string) (context.Context, func(error), error) {
	ctx, activity, err := r.BeginOwnedActivity(parent, name)
	if err != nil {
		return nil, nil, err
	}
	return ctx, activity.Finish, nil
}

func (r *Runtime) BeginOwnedActivity(parent context.Context, name string) (context.Context, *Activity, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.phase == RuntimeRecoveryRequired {
		return nil, nil, ErrRecoveryRequired
	}
	if r.phase == RuntimeClosed {
		return nil, nil, osClosedError()
	}
	if r.phase != RuntimeIdle {
		return nil, nil, ErrRuntimeBusy
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	r.phase, r.activity = RuntimeRunning, name
	r.activityID++
	r.revision.Add(1)
	activity := &Activity{runtime: r, id: r.activityID, name: name, cancel: cancel, done: make(chan struct{})}
	r.current.Store(activity)
	return ctx, activity, nil
}

func (a *Activity) AppendBatch(ctx context.Context, operationID string, events []Event) (Commit, error) {
	return a.Append(ctx, Batch{OperationID: operationID, Events: events})
}

// Append preserves the complete logical batch, including its turn identity,
// while fencing the commit against the exact activity generation.
//
// Validation and hashing run outside the runtime lock so they can never delay
// cancellation. Only the final eligibility check and the in-memory commit hold
// the activity commit gate; the physical write-behind is asynchronous.
func (a *Activity) Append(ctx context.Context, batch Batch) (Commit, error) {
	if a == nil || a.runtime == nil {
		return Commit{}, ErrStaleActivity
	}
	if a.stopped.Load() {
		return Commit{}, ErrStaleActivity
	}
	if err := ctx.Err(); err != nil {
		return Commit{}, err
	}
	runtime := a.runtime
	prepared, err := runtime.session.PrepareBatchContext(ctx, batch.OperationID, batch)
	if err != nil {
		return Commit{}, err
	}
	defer prepared.Release()
	a.commitGate.RLock()
	defer a.commitGate.RUnlock()
	runtime.mu.Lock()
	if runtime.current.Load() != a || runtime.activityID != a.id {
		runtime.mu.Unlock()
		return Commit{}, ErrStaleActivity
	}
	if a.stopped.Load() && !activityClosureBatch(batch) {
		runtime.mu.Unlock()
		return Commit{}, ErrStaleActivity
	}
	if runtime.phase != RuntimeRunning && runtime.phase != RuntimeCancelling {
		runtime.mu.Unlock()
		return Commit{}, ErrStaleActivity
	}
	runtime.mu.Unlock()
	return runtime.session.CommitPrepared(prepared)
}

func activityClosureBatch(batch Batch) bool {
	if len(batch.Events) == 0 {
		return false
	}
	for _, event := range batch.Events {
		switch event.Kind {
		case "turn/end", "interaction/resolved", "runtime/recovery", "assistant/attempt", "diagnostic":
		default:
			return false
		}
	}
	return true
}

func (a *Activity) Finish(_ error) {
	if a == nil || a.runtime == nil {
		return
	}
	a.doneOnce.Do(func() { close(a.done) })
	runtime := a.runtime
	a.stop()
	runtime.mu.Lock()
	if runtime.current.Load() != a || runtime.activityID != a.id || (runtime.phase != RuntimeRunning && runtime.phase != RuntimeCancelling) {
		runtime.mu.Unlock()
		return
	}
	runtime.current.Store(nil)
	runtime.activity = ""
	runtime.phase = RuntimeIdle
	runtime.revision.Add(1)
	owner := runtime.owner
	runtime.mu.Unlock()
	if owner != nil {
		_ = owner.closeIfUnbound(context.Background(), runtime)
	}
}

func (a *Activity) stop() bool {
	if a == nil || !a.stopped.CompareAndSwap(false, true) {
		return false
	}
	a.cancel()
	a.runtime.revision.Add(1)
	return true
}

// superviseCancellation belongs to the activity rather than any client. This
// preserves recovery isolation when Stop arrives through Serve or another
// host entry point without a live UI controller.
func (a *Activity) superviseCancellation() {
	if a == nil || a.runtime == nil {
		return
	}
	a.superviseOnce.Do(func() {
		go func() {
			timer := time.NewTimer(15 * time.Second)
			defer timer.Stop()
			select {
			case <-a.done:
				return
			case <-timer.C:
			}
			a.runtime.requireRecoveryFor(a)
		}()
	})
}

// Cancel reaches the immutable activity permit without acquiring the runtime
// mutex. A commit may be blocked below that mutex or in a persistence adapter;
// neither is allowed to delay delivery of the cancellation signal.
func (r *Runtime) Cancel() bool {
	activity := r.current.Load()
	if activity == nil {
		return false
	}
	activity.stop()
	activity.superviseCancellation()
	if r.mu.TryLock() {
		defer r.mu.Unlock()
		if r.current.Load() == activity && r.phase == RuntimeRunning {
			r.phase = RuntimeCancelling
		}
	}
	return true
}

func (r *Runtime) requireRecoveryFor(activity *Activity) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current.Load() != activity || r.activityID != activity.id || !activity.stopped.Load() {
		return
	}
	r.current.Store(nil)
	r.activityID++
	r.phase = RuntimeRecoveryRequired
	r.activity = activity.name
	r.revision.Add(1)
}

func (r *Runtime) RequireRecovery(activity string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.phase == RuntimeClosed {
		return
	}
	if current := r.current.Swap(nil); current != nil {
		current.stop()
	}
	r.activityID++
	r.phase = RuntimeRecoveryRequired
	r.activity = activity
	r.revision.Add(1)
}

// RecordRecovery appends terminal recovery facts after RequireRecovery has
// revoked the activity permit. It cannot be used by a stale worker to publish
// a tool result or another business-state transition.
func (r *Runtime) RecordRecovery(ctx context.Context, batch Batch) (Commit, error) {
	r.mu.Lock()
	if r.phase != RuntimeRecoveryRequired || !recoveryClosureBatch(batch) {
		r.mu.Unlock()
		return Commit{}, ErrStaleActivity
	}
	r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Commit{}, err
	}
	prepared, err := r.session.PrepareBatchContext(ctx, batch.OperationID, batch)
	if err != nil {
		return Commit{}, err
	}
	defer prepared.Release()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.phase != RuntimeRecoveryRequired || !recoveryClosureBatch(batch) {
		return Commit{}, ErrStaleActivity
	}
	return r.session.CommitPrepared(prepared)
}

func recoveryClosureBatch(batch Batch) bool {
	if !activityClosureBatch(batch) {
		return false
	}
	for _, event := range batch.Events {
		if event.Kind == "runtime/recovery" {
			return true
		}
	}
	return false
}

func (r *Runtime) close(ctx context.Context) error {
	r.mu.Lock()
	if r.closeDone != nil {
		done := r.closeDone
		r.mu.Unlock()
		<-done
		return r.closeErr
	}
	if r.phase == RuntimeRunning || r.phase == RuntimeCancelling || r.phase == RuntimeRecoveryRequired {
		r.mu.Unlock()
		return ErrRuntimeBusy
	}
	// Seal admission in the same critical section as the idle check. The
	// irreversible close has one uncancellable result for every caller.
	r.closeDone = make(chan struct{})
	r.current.Store(nil)
	r.phase = RuntimeClosed
	r.activity = ""
	r.revision.Add(1)
	r.mu.Unlock()
	r.closeErr = r.session.close(context.Background())
	close(r.closeDone)
	return r.closeErr
}

func osClosedError() error { return errors.New("session runtime is closed") }

// Service applies DSH's prepare/publish/exact-detach rule. Candidate handles
// are opened outside the registry lock; only the exact published Runtime can
// later unregister itself.
type Service struct {
	hostID      string
	persistence SessionPersistence

	mu         sync.Mutex
	active     map[SessionRef]*Runtime
	closed     map[SessionRef]error
	preparing  map[SessionRef]*prepareRuntime
	bindings   map[*Runtime]int
	retiring   map[*Runtime]chan struct{}
	retireIdle map[*Runtime]bool
	query      *Query
	revision   atomic.Uint64
}

func (s *Service) Cancel(ref SessionRef) (RuntimeSnapshot, error) {
	runtime, ok := s.Runtime(ref)
	if !ok {
		return RuntimeSnapshot{}, ErrSessionNotRunning
	}
	runtime.Cancel()
	return runtime.StateSnapshot(), nil
}

// CancelSession is the public session-scoped Stop contract. A missing runtime
// is already idle and therefore succeeds idempotently; no caller-supplied turn
// id participates in routing or authorization.
func (s *Service) CancelSession(ref SessionRef) (CancelReceipt, error) {
	if err := ref.validate(s.hostID); err != nil {
		return CancelReceipt{}, err
	}
	runtime, ok := s.Runtime(ref)
	if !ok {
		return CancelReceipt{Ref: ref, Accepted: true, Phase: RuntimeIdle}, nil
	}
	if runtime.Cancel() {
		return CancelReceipt{
			Ref:              ref,
			Accepted:         true,
			RuntimeEpoch:     runtime.epoch,
			ActivityRevision: runtime.revision.Load(),
			Phase:            RuntimeCancelling,
		}, nil
	}
	snapshot := runtime.activitySnapshot()
	return CancelReceipt{Ref: ref, Accepted: true, RuntimeEpoch: snapshot.Epoch, ActivityRevision: snapshot.ActivityRevision, Phase: snapshot.Phase}, nil
}

func (s *Service) Flush(ctx context.Context, ref SessionRef) (DurableReceipt, error) {
	runtime, ok := s.Runtime(ref)
	if !ok {
		return DurableReceipt{}, ErrSessionNotRunning
	}
	return runtime.session.Flush(ctx)
}

// ContinueLegacy freezes one legacy head, publishes its deterministic final
// session, then attaches that exact session. It never writes the source and it
// does not accept the caller's pending submission; hosts enqueue the unchanged
// submission only after this method returns the new immutable identity.
func (s *Service) ContinueLegacy(ctx context.Context, sourcePath, headID string) (*Runtime, MigrationResult, error) {
	filesystem, ok := s.persistence.(*FilesystemPersistence)
	if !ok {
		return nil, MigrationResult{}, errors.New("session: persistence does not support legacy migration")
	}
	result, err := migrateLegacyHeadForHost(ctx, sourcePath, filesystem.Root, headID)
	if err != nil {
		return nil, result, err
	}
	runtime, err := s.openRuntime(ctx, SessionRef{HostID: s.hostID, SessionID: result.TargetID})
	return runtime, result, err
}

// ContinueImported resolves the paired legacy transcript and retired event
// sidecar as one frozen migration decision. It refuses divergent histories
// instead of letting a caller accidentally resume whichever source it opened
// first.
func (s *Service) ContinueImported(ctx context.Context, sourcePath, headID string) (*Runtime, ImportResult, error) {
	filesystem, ok := s.persistence.(*FilesystemPersistence)
	if !ok {
		return nil, ImportResult{}, errors.New("session: persistence does not support imported sessions")
	}
	result, err := importSourceForLegacy(ctx, sourcePath, filesystem.Root, headID)
	if err != nil {
		return nil, result, err
	}
	runtime, err := s.openRuntime(ctx, SessionRef{HostID: s.hostID, SessionID: result.TargetID})
	return runtime, result, err
}

// ContinuePrototype is the explicit, fail-closed bridge for the retired
// sidecar codec. Unknown required events or conflicting tails remain read-only.
func (s *Service) ContinuePrototype(ctx context.Context, sourceDir string) (*Runtime, PrototypeImportResult, error) {
	filesystem, ok := s.persistence.(*FilesystemPersistence)
	if !ok {
		return nil, PrototypeImportResult{}, errors.New("session: persistence does not support prototype import")
	}
	result, err := ImportPrototype(ctx, sourceDir, filesystem.Root)
	if err != nil {
		return nil, result, err
	}
	runtime, err := s.openRuntime(ctx, SessionRef{HostID: s.hostID, SessionID: result.TargetID})
	return runtime, result, err
}

// ContinueStoredPreview upgrades a pre-ownership linear store selected by its
// former session id. The old directory remains read-only; execution resumes on
// the deterministic final-codec identity returned here.
func (s *Service) ContinueStoredPreview(ctx context.Context, sessionID string) (*Runtime, PrototypeImportResult, error) {
	filesystem, ok := s.persistence.(*FilesystemPersistence)
	if !ok {
		return nil, PrototypeImportResult{}, errors.New("session: persistence does not support preview import")
	}
	if err := validateSessionID(sessionID); err != nil {
		return nil, PrototypeImportResult{}, err
	}
	sourceDir, err := filesystem.sessionDir(sessionID, true)
	if err != nil {
		return nil, PrototypeImportResult{}, err
	}
	frozen, err := freezePairedPreview(ctx, sourceDir)
	if err != nil {
		return nil, PrototypeImportResult{}, err
	}
	result, err := importFrozenPreview(ctx, frozen, filesystem.Root)
	if err != nil {
		return nil, result, err
	}
	runtime, err := s.openRuntime(ctx, SessionRef{HostID: s.hostID, SessionID: result.TargetID})
	return runtime, result, err
}

// Fork creates an independent child at the exact end event of a completed
// turn. No message-count inference is involved.
func (s *Service) Fork(ctx context.Context, ref SessionRef, afterTurnID, childID string) (*Runtime, error) {
	runtime, ok := s.Runtime(ref)
	if !ok {
		return nil, ErrSessionNotRunning
	}
	turn, ok := completedTurn(runtime.session.Snapshot().Projection.Turns, afterTurnID)
	if !ok {
		return nil, fmt.Errorf("session: completed turn %q not found", afterTurnID)
	}
	return s.forkAt(ctx, runtime, turn.EndSequence, childID)
}

// Rewind creates a child from the event immediately before beforeTurnID.
func (s *Service) Rewind(ctx context.Context, ref SessionRef, beforeTurnID, childID string) (*Runtime, error) {
	runtime, ok := s.Runtime(ref)
	if !ok {
		return nil, ErrSessionNotRunning
	}
	turn, ok := completedTurn(runtime.session.Snapshot().Projection.Turns, beforeTurnID)
	if !ok {
		return nil, fmt.Errorf("session: completed turn %q not found", beforeTurnID)
	}
	return s.forkAt(ctx, runtime, turn.StartSequence-1, childID)
}

func completedTurn(turns []TurnBoundary, id string) (TurnBoundary, bool) {
	for _, turn := range turns {
		if turn.TurnID == id {
			return turn, true
		}
	}
	return TurnBoundary{}, false
}

func (s *Service) forkAt(ctx context.Context, parent *Runtime, sequence uint64, childID string) (*Runtime, error) {
	filesystem, ok := s.persistence.(*FilesystemPersistence)
	if !ok {
		return nil, errors.New("session: persistence does not support filesystem fork")
	}
	if childID == "" {
		childID = randomID()
	}
	if err := validateSessionID(childID); err != nil {
		return nil, err
	}
	childDir := filepath.Join(filesystem.Root, childID)
	if _, err := parent.session.Fork(ctx, childDir, childID, sequence); err != nil {
		return nil, err
	}
	return s.openRuntime(ctx, SessionRef{HostID: s.hostID, SessionID: childID})
}

func (s *Service) Close(ctx context.Context, ref SessionRef) error {
	if err := ref.validate(s.hostID); err != nil {
		return err
	}
	s.mu.Lock()
	runtime := s.active[ref]
	closedErr, closed := s.closed[ref]
	s.mu.Unlock()
	if runtime == nil {
		if closed {
			return closedErr
		}
		return ErrSessionNotRunning
	}
	return s.closeOwned(ctx, runtime, "")
}

// closeOwned is the teardown entry point for a RuntimeOwner holding one exact
// instance grant. A delayed old disposer must never close its same-ID
// successor, and a client-bound runtime is never torn down underneath it.
func (s *Service) closeOwned(ctx context.Context, runtime *Runtime, instance string) error {
	if runtime == nil {
		return ErrSessionNotRunning
	}
	if err := runtime.ref.validate(s.hostID); err != nil {
		return err
	}
	if instance != "" && runtime.instance != instance {
		return ErrSessionNotRunning
	}
	s.mu.Lock()
	if s.bindings[runtime] != 0 {
		s.mu.Unlock()
		return ErrRuntimeBound
	}
	if s.active[runtime.ref] != runtime {
		s.mu.Unlock()
		return runtime.close(ctx)
	}
	if done := s.retiring[runtime]; done != nil {
		s.mu.Unlock()
		select {
		case <-done:
			return runtime.close(ctx)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	done := make(chan struct{})
	s.retiring[runtime] = done
	s.mu.Unlock()
	err := runtime.close(ctx)
	s.mu.Lock()
	delete(s.retiring, runtime)
	if !errors.Is(err, ErrRuntimeBusy) && s.active[runtime.ref] == runtime {
		delete(s.active, runtime.ref)
		delete(s.retireIdle, runtime)
		s.closed[runtime.ref] = err
		s.revision.Add(1)
	}
	close(done)
	s.mu.Unlock()
	return err
}

// Detach removes a runtime only if it is still the exact published instance.
// It is used by host callbacks that may arrive after a replacement.
func (s *Service) Detach(runtime *Runtime) bool {
	if runtime == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active[runtime.ref] != runtime {
		return false
	}
	delete(s.active, runtime.ref)
	s.revision.Add(1)
	return true
}

type ObserveResult struct {
	Runtime *RuntimeSnapshot `json:"runtime,omitempty"`
	Events  EventPage        `json:"events"`
}

func (s *Service) Observe(ctx context.Context, ref SessionRef, cursor uint64, limit int) (ObserveResult, error) {
	if err := ref.validate(s.hostID); err != nil {
		return ObserveResult{}, err
	}
	if runtime, ok := s.Runtime(ref); ok {
		// Observe reports runtime state plus an explicitly paged event tail. It
		// must not duplicate the provider model workset into every poll.
		snapshot := runtime.StateSnapshot()
		page, err := runtime.session.AcceptedPage(ctx, cursor, limit)
		return ObserveResult{Runtime: &snapshot, Events: page}, err
	}
	handle, err := s.persistence.Open(ref.SessionID, ReadOnly)
	if err != nil {
		return ObserveResult{}, err
	}
	defer handle.Close(context.Background())
	page, err := handle.Read(ctx, cursor, limit)
	return ObserveResult{Events: page}, err
}
