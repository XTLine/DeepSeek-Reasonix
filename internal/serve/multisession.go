package serve

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"reasonix/internal/boot"
	"reasonix/internal/control"
	"reasonix/internal/event"
)

// sessionTagSink stamps every event from one controller with the session
// path currently recorded for it, so subscribers can route frames per
// session while current and detached controllers run side by side. The path
// is updated by the server whenever the controller's session changes
// (build, resume, new-session, model-switch carry-over, transitions).
type sessionTagSink struct {
	bc   *Broadcaster
	path atomic.Pointer[string]
}

func newSessionTagSink(bc *Broadcaster) *sessionTagSink {
	return &sessionTagSink{bc: bc}
}

// SessionTagSink is the exported alias for CLI wiring: the serve command
// builds its initial controller outside the Server, so it stamps frames by
// wrapping the broadcaster itself and registering the tag via
// RegisterSessionTag once the controller exists.
type SessionTagSink = sessionTagSink

// NewSessionTagSink builds that exported wrapper.
func NewSessionTagSink(bc *Broadcaster) *SessionTagSink {
	return newSessionTagSink(bc)
}

// SetPath records the controller's current session path. Empty keeps frames
// untagged (single-session compatibility).
func (s *sessionTagSink) SetPath(path string) {
	s.path.Store(&path)
}

func (s *sessionTagSink) Path() string {
	if p := s.path.Load(); p != nil {
		return *p
	}
	return ""
}

func (s *sessionTagSink) Emit(e event.Event) {
	if p := s.Path(); p != "" {
		e.SessionPath = p
	}
	s.bc.Emit(e)
}

// detachedSession is a controller moved to the background because its busy
// session was switched away from. It keeps running its turn to completion,
// keeps writing its own session file under its own lease keeper, and its
// frames stay tagged with its session path. The watcher closes it once idle.
type detachedSession struct {
	path     string
	ctrl     control.SessionAPI
	keeper   *control.SessionLeaseKeeper
	tag      *sessionTagSink
	force    chan struct{} // closed by CloseBackground to stop waiting for idle
	reattach chan struct{} // closed when the session is taken back as current
	done     chan struct{} // closed when fully closed or re-attached (tests/shutdown)
}

// RegisterSessionTag wires the tag sink of a controller built outside the
// server (the CLI-built initial controller) so in-place session changes
// (idle resume, /new) keep its frames stamped with the right path.
func (s *Server) RegisterSessionTag(ctrl *control.Controller, tag *sessionTagSink) {
	if ctrl == nil || tag == nil {
		return
	}
	s.tagsMu.Lock()
	if s.tags == nil {
		s.tags = map[*control.Controller]*sessionTagSink{}
	}
	s.tags[ctrl] = tag
	s.tagsMu.Unlock()
}

func (s *Server) tagFor(ctrl *control.Controller) *sessionTagSink {
	if ctrl == nil {
		return nil
	}
	s.tagsMu.Lock()
	defer s.tagsMu.Unlock()
	return s.tags[ctrl]
}

// setControllerPath records a controller's settled session path on its tag
// sink and, as the active controller, as the broadcaster's current session.
func (s *Server) setControllerPath(ctrl *control.Controller, path string) {
	if tag := s.tagFor(ctrl); tag != nil {
		tag.SetPath(path)
	}
	if path != "" {
		s.bc.SetCurrentSession(path)
	}
}

// buildTagged builds a fresh controller whose sink stamps frames with its
// session path. inheritTemp carries the logical-session temp directory across
// model switches; detached-swap builds must NOT inherit it (the temp belongs
// to the outgoing session).
func (s *Server) buildTagged(ctx context.Context, ref string, inheritTemp bool) (*control.Controller, *sessionTagSink, error) {
	tag := newSessionTagSink(s.bc)
	opts := boot.Options{
		Model:       ref,
		Sink:        tag,
		Stderr:      os.Stderr,
		StatsSource: "serve",
	}
	// The replacement must live where the current controller lives: boot
	// falls back to the GLOBAL session dir when opts.SessionDir is empty
	// (project-dir derivation is the CLI's job), which would silently move
	// every switched-to session into ~/.reasonix/sessions and flip the
	// /sessions listing to the wrong directory.
	if cur, ok := s.ctl().(*control.Controller); ok && cur != nil {
		opts.SessionDir = cur.SessionDir()
		opts.WorkspaceRoot = cur.WorkspaceRoot()
		if inheritTemp {
			opts.SessionTemp = cur.SessionTemp()
		}
	}
	if s.buildController != nil {
		ctrl, err := s.buildController(ctx, ref, opts)
		if err != nil {
			return nil, nil, err
		}
		s.RegisterSessionTag(ctrl, tag)
		return ctrl, tag, nil
	}
	ctrl, err := boot.Build(ctx, opts)
	if err != nil {
		return nil, nil, err
	}
	s.RegisterSessionTag(ctrl, tag)
	slog.Info("serve: controller built", "model", ref, "session", ctrl.SessionPath(), "sessionDir", opts.SessionDir)
	return ctrl, tag, nil
}

// detachedBusy reports whether a session path currently has a live
// background controller (delete-session guard).
func (s *Server) detachedBusy(path string) bool {
	s.detachedMu.Lock()
	defer s.detachedMu.Unlock()
	_, busy := s.detached[path]
	return busy
}

// takeDetached removes a background controller from the registry because its
// session is being re-attached as the current one, and signals its watcher to
// stand down (the controller must not be closed by the watcher anymore).
func (s *Server) takeDetached(path string) *detachedSession {
	s.detachedMu.Lock()
	d := s.detached[path]
	if d != nil {
		delete(s.detached, path)
	}
	s.detachedMu.Unlock()
	if d != nil {
		select {
		case <-d.reattach:
		default:
			close(d.reattach)
		}
	}
	return d
}

// registerDetached moves a controller to the background registry and starts
// its close-on-idle watcher. The controller's lease must already live in its
// own keeper (Split/RebindDetaching); it keeps running to completion and
// snapshots to its own session file — Close is deferred until idle because a
// Close mid-turn cancels the turn.
func (s *Server) registerDetached(ctrl control.SessionAPI, keeper *control.SessionLeaseKeeper, tag *sessionTagSink) *detachedSession {
	if ctrl == nil {
		if keeper != nil {
			keeper.Release()
		}
		return nil
	}
	if tag == nil {
		if c, ok := ctrl.(*control.Controller); ok {
			tag = s.tagFor(c)
		}
	}
	// Recovery branches on a background controller must move ITS lease, not
	// the server's current one.
	if keeper != nil {
		if c, ok := ctrl.(interface {
			SetOnSessionRecovered(func(control.SessionRecoveryInfo) error)
		}); ok {
			c.SetOnSessionRecovered(sessionLeaseRecoveryHandler(keeper))
		}
	}
	d := &detachedSession{path: ctrl.SessionPath(), ctrl: ctrl, keeper: keeper, tag: tag, force: make(chan struct{}), reattach: make(chan struct{}), done: make(chan struct{})}
	slog.Info("serve: session detached to background", "session", d.path, "running", controllerHasActiveRuntimeWork(ctrl))
	s.detachedMu.Lock()
	if s.detached == nil {
		s.detached = map[string]*detachedSession{}
	}
	s.detached[d.path] = d
	s.detachedMu.Unlock()
	go s.watchDetached(d)
	return d
}

// watchDetached closes a background controller once it has no active work
// left. Its turn already snapshotted on completion (the turn goroutine owns
// persistence); Close then tears down jobs and the keeper releases the lease.
func (s *Server) watchDetached(d *detachedSession) {
	interval := 200 * time.Millisecond
	for controllerHasActiveRuntimeWork(d.ctrl) {
		select {
		case <-d.reattach: // taken back as the current session: never close it here
			close(d.done)
			return
		case <-d.force: // shutdown: stop waiting, Close cancels the turn
		case <-time.After(interval):
		}
		if interval < 2*time.Second {
			interval *= 2
		}
	}
	d.ctrl.Close()
	if d.keeper != nil {
		d.keeper.Release()
	}
	s.detachedMu.Lock()
	if s.detached[d.path] == d {
		delete(s.detached, d.path)
	}
	s.detachedMu.Unlock()
	if c, ok := d.ctrl.(*control.Controller); ok {
		s.tagsMu.Lock()
		delete(s.tags, c)
		s.tagsMu.Unlock()
	}
	slog.Info("serve: background session closed", "session", d.path)
	close(d.done)
}

// WaitForDetachedIdle blocks until every background controller has closed
// (shutdown/tests). It does not force-close; CloseBackground does.
func (s *Server) WaitForDetachedIdle() {
	s.detachedMu.Lock()
	detached := make([]*detachedSession, 0, len(s.detached))
	for _, d := range s.detached {
		detached = append(detached, d)
	}
	s.detachedMu.Unlock()
	for _, d := range detached {
		<-d.done
	}
}

// CloseBackground force-closes every background controller. Process teardown
// only: Close cancels in-flight turns (bounded job grace), which the normal
// close-on-idle path never does.
func (s *Server) CloseBackground() {
	s.detachedMu.Lock()
	detached := make([]*detachedSession, 0, len(s.detached))
	for _, d := range s.detached {
		detached = append(detached, d)
	}
	s.detachedMu.Unlock()
	for _, d := range detached {
		select {
		case <-d.force:
		default:
			close(d.force)
		}
		<-d.done
	}
}

// busyDetach swaps the current controller for a brand-new one bound to the
// target session, moving the busy current controller to the background — the
// serve-side twin of the desktop's session detach. Both controllers then run
// in parallel, each writing its own session file. Callers hold bindMu.
//
// loadTarget builds the replacement's initial session: for a resume it
// returns the loaded target session; for a fresh session it returns nil and
// the replacement keeps its built-in fresh session.
func (s *Server) busyDetach(ctx context.Context, cur *control.Controller, targetPath string, loadTarget func(*control.Controller) error) error {
	// Build the replacement first: on failure the current session stays
	// untouched.
	ref := currentModelRef(cur)
	newCtrl, tag, err := s.buildTagged(ctx, ref, false)
	if err != nil {
		return err
	}
	if targetPath == "" {
		// Fresh-session target: the replacement's own built-in session.
		newCtrl.EnsureSessionPath()
		targetPath = newCtrl.SessionPath()
	}
	// Acquire the target lease and split the current one out atomically; a
	// held target fails without touching the current session.
	demoted, err := s.leases.RebindDetaching(targetPath)
	if err != nil {
		newCtrl.Close()
		return err
	}
	if loadTarget != nil {
		if err := loadTarget(newCtrl); err != nil {
			newCtrl.Close()
			s.rollbackDetach(demoted)
			return err
		}
	}
	path := targetPath
	tag.SetPath(path)
	newCtrl.EnableInteractiveApproval()
	newCtrl.SetOnSessionRecovered(sessionLeaseRecoveryHandler(s.leases))
	if s.leases != nil {
		if err := s.leases.BindControllerAuthority(newCtrl); err != nil {
			newCtrl.Close()
			s.rollbackDetach(demoted)
			return err
		}
	}
	// Publish the replacement (identity check against cur), then move the
	// busy current controller to the background with its split-off lease.
	s.mu.Lock()
	if s.ctrl != control.SessionAPI(cur) {
		s.mu.Unlock()
		newCtrl.Close()
		s.rollbackDetach(demoted)
		return errReplacedDuringBind
	}
	s.ctrl = newCtrl
	s.mu.Unlock()
	if demoted != nil {
		s.registerDetached(cur, demoted, nil)
	} else {
		// No lease split (persistence disabled): still detach the controller
		// so it finishes in the background rather than being closed mid-turn.
		s.registerDetached(cur, nil, nil)
	}
	s.setControllerPath(newCtrl, path)
	s.bc.ResetSession(path)
	return nil
}

// errReplacedDuringBind marks a lost race with another binding operation.
var errReplacedDuringBind = &replacedDuringBindError{}

type replacedDuringBindError struct{}

func (*replacedDuringBindError) Error() string { return "session changed during switch" }

// rollbackDetach undoes a RebindDetaching split after a later step failed:
// the target lease the keeper acquired is released and the demoted lease is
// adopted back, restoring the previous controller's binding untouched.
func (s *Server) rollbackDetach(demoted *control.SessionLeaseKeeper) {
	if demoted == nil || s.leases == nil {
		return
	}
	s.leases.Adopt(demoted)
}

// cancelDetachedForProviderHeal aborts every background controller after a
// provider-config heal (POST /providers/reload). Their in-memory providers
// were built from the pre-heal config and still dial the old endpoint (a
// drifted reverse-tunnel port), so their in-flight turns are doomed; Cancel
// hands each to the close-on-idle watcher, and the next switch to those
// sessions builds fresh controllers from the healed config. Without this, a
// re-attached background session keeps calling the dead port forever.
func (s *Server) cancelDetachedForProviderHeal() {
	s.detachedMu.Lock()
	detached := make([]*detachedSession, 0, len(s.detached))
	for _, d := range s.detached {
		detached = append(detached, d)
	}
	s.detachedMu.Unlock()
	for _, d := range detached {
		slog.Info("serve: provider heal cancels background session", "session", d.path)
		d.ctrl.Cancel()
	}
}
