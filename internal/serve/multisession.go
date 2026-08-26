package serve

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/boot"
	"reasonix/internal/control"
	"reasonix/internal/event"
)

// sessionTagSink stamps every event from one controller with that
// controller's current session path. This lets one Serve process keep several
// turns alive without sending a background session's frames to the foreground
// browser.
type sessionTagSink struct {
	bc   *Broadcaster
	path atomic.Pointer[string]
}

func newSessionTagSink(bc *Broadcaster) *sessionTagSink {
	return &sessionTagSink{bc: bc}
}

// SessionTagSink is exported for the CLI, which builds Serve's initial
// controller before the Server exists.
type SessionTagSink = sessionTagSink

func NewSessionTagSink(bc *Broadcaster) *SessionTagSink {
	return newSessionTagSink(bc)
}

func (s *sessionTagSink) SetPath(path string) {
	if path != "" {
		path = agent.CanonicalSessionPath(path)
	}
	s.path.Store(&path)
}

func (s *sessionTagSink) Path() string {
	if p := s.path.Load(); p != nil {
		return *p
	}
	return ""
}

func (s *sessionTagSink) Emit(e event.Event) {
	if path := s.Path(); path != "" {
		e.SessionPath = path
	}
	s.bc.Emit(e)
}

type detachedSession struct {
	path     string
	ctrl     control.SessionAPI
	keeper   *control.SessionLeaseKeeper
	tag      *sessionTagSink
	force    chan struct{}
	reattach chan struct{}
	done     chan struct{}
}

// RegisterSessionTag associates a controller built outside Server with its
// tagging sink. In-place /new and /resume operations can then advance the tag.
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

func (s *Server) forgetSessionTag(ctrl *control.Controller) {
	if ctrl == nil {
		return
	}
	s.tagsMu.Lock()
	delete(s.tags, ctrl)
	s.tagsMu.Unlock()
}

func (s *Server) closeTaggedController(ctrl *control.Controller) {
	if ctrl == nil {
		return
	}
	ctrl.Close()
	s.forgetSessionTag(ctrl)
}

func (s *Server) setControllerPath(ctrl *control.Controller, path string) {
	if path != "" {
		path = agent.CanonicalSessionPath(path)
	}
	if tag := s.tagFor(ctrl); tag != nil {
		tag.SetPath(path)
	}
	s.bc.SetCurrentSession(path)
}

// buildTagged creates a controller whose frames are session-tagged. The legacy
// two-argument test builder remains supported; tests that need to assert the
// complete boot contract can inject buildControllerWithOptions.
func (s *Server) buildTagged(ctx context.Context, ref string, inheritTemp bool) (*control.Controller, *sessionTagSink, error) {
	tag := newSessionTagSink(s.bc)
	opts := boot.Options{
		Model:       ref,
		Sink:        tag,
		Stderr:      os.Stderr,
		StatsSource: "serve",
	}
	if cur, ok := s.ctl().(*control.Controller); ok && cur != nil {
		opts.SessionDir = cur.SessionDir()
		opts.WorkspaceRoot = cur.WorkspaceRoot()
		if inheritTemp {
			opts.SessionTemp = cur.SessionTemp()
		}
	}

	var (
		ctrl *control.Controller
		err  error
	)
	switch {
	case s.buildControllerWithOptions != nil:
		ctrl, err = s.buildControllerWithOptions(ctx, ref, opts)
	case s.buildController != nil:
		ctrl, err = s.buildController(ctx, ref)
	default:
		ctrl, err = boot.Build(ctx, opts)
	}
	if err != nil {
		return nil, nil, err
	}
	s.RegisterSessionTag(ctrl, tag)
	slog.Info("serve: controller built", "model", ref, "sessionDir", opts.SessionDir)
	return ctrl, tag, nil
}

func (s *Server) detachedBusy(path string) bool {
	path = agent.CanonicalSessionPath(path)
	s.detachedMu.Lock()
	defer s.detachedMu.Unlock()
	_, ok := s.detached[path]
	return ok
}

// takeDetached transfers ownership from the close-on-idle watcher back to the
// request goroutine. Waiting for done is essential: without the acknowledgement
// the watcher can close an idle controller just after it is re-attached.
func (s *Server) takeDetached(path string) *detachedSession {
	path = agent.CanonicalSessionPath(path)
	s.detachedMu.Lock()
	d := s.detached[path]
	if d != nil {
		delete(s.detached, path)
	}
	s.detachedMu.Unlock()
	if d == nil {
		return nil
	}
	close(d.reattach)
	<-d.done
	return d
}

func (s *Server) registerDetached(ctrl control.SessionAPI, keeper *control.SessionLeaseKeeper, tag *sessionTagSink) (*detachedSession, error) {
	if ctrl == nil {
		return nil, fmt.Errorf("cannot detach a nil controller")
	}
	path := agent.CanonicalSessionPath(ctrl.SessionPath())
	if path == "" {
		return nil, fmt.Errorf("cannot detach a session without a path")
	}
	if tag == nil {
		if concrete, ok := ctrl.(*control.Controller); ok {
			tag = s.tagFor(concrete)
		}
	}
	if keeper != nil {
		if concrete, ok := ctrl.(*control.Controller); ok {
			concrete.SetOnSessionRecovered(s.sessionRecoveryHandler(concrete, keeper))
		}
	}
	d := &detachedSession{
		path: path, ctrl: ctrl, keeper: keeper, tag: tag,
		force: make(chan struct{}), reattach: make(chan struct{}), done: make(chan struct{}),
	}
	s.detachedMu.Lock()
	if s.detached == nil {
		s.detached = map[string]*detachedSession{}
	}
	if _, exists := s.detached[path]; exists {
		s.detachedMu.Unlock()
		return nil, fmt.Errorf("session is already running in the background")
	}
	s.detached[path] = d
	s.detachedMu.Unlock()
	slog.Info("serve: session detached", "session", path, "running", controllerHasActiveRuntimeWork(ctrl))
	go s.watchDetached(d)
	return d, nil
}

func (s *Server) watchDetached(d *detachedSession) {
	interval := 200 * time.Millisecond
	forced := false
	for controllerHasActiveRuntimeWork(d.ctrl) && !forced {
		timer := time.NewTimer(interval)
		select {
		case <-d.reattach:
			if !timer.Stop() {
				<-timer.C
			}
			close(d.done)
			return
		case <-d.force:
			if !timer.Stop() {
				<-timer.C
			}
			forced = true
		case <-timer.C:
			if interval < 2*time.Second {
				interval *= 2
			}
		}
	}

	// Claim close ownership only while the registry still points at d. A
	// concurrent takeDetached removes it first and receives the live controller.
	s.detachedMu.Lock()
	owns := s.detached[d.path] == d
	if owns {
		delete(s.detached, d.path)
	}
	s.detachedMu.Unlock()
	if !owns {
		close(d.done)
		return
	}
	d.ctrl.Close()
	if d.keeper != nil {
		d.keeper.Release()
	}
	if concrete, ok := d.ctrl.(*control.Controller); ok {
		s.forgetSessionTag(concrete)
	}
	slog.Info("serve: background session closed", "session", d.path, "forced", forced)
	close(d.done)
}

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
	}
	for _, d := range detached {
		<-d.done
	}
}

// busyDetach publishes a fresh controller before demoting a busy controller.
// Every failure before publication restores the original lease ownership.
func (s *Server) busyDetach(ctx context.Context, cur *control.Controller, targetPath string, loadTarget func(*control.Controller) error) error {
	newCtrl, tag, err := s.buildTagged(ctx, currentModelRef(cur), false)
	if err != nil {
		return err
	}
	if targetPath == "" {
		newCtrl.EnsureSessionPath()
		targetPath = newCtrl.SessionPath()
	}
	targetPath = agent.CanonicalSessionPath(targetPath)
	if targetPath == "" {
		s.closeTaggedController(newCtrl)
		return fmt.Errorf("replacement session has no path")
	}

	demoted, err := s.leases.RebindDetaching(targetPath)
	if err != nil {
		s.closeTaggedController(newCtrl)
		return err
	}
	if loadTarget != nil {
		if err := loadTarget(newCtrl); err != nil {
			s.closeTaggedController(newCtrl)
			s.rollbackDetach(demoted)
			return err
		}
	}
	tag.SetPath(targetPath)
	newCtrl.EnableInteractiveApproval()
	newCtrl.SetOnSessionRecovered(s.sessionRecoveryHandler(newCtrl, s.leases))
	if s.leases != nil {
		if err := s.leases.BindControllerAuthority(newCtrl); err != nil {
			s.closeTaggedController(newCtrl)
			s.rollbackDetach(demoted)
			return err
		}
	}

	s.mu.Lock()
	if s.ctrl != control.SessionAPI(cur) {
		s.mu.Unlock()
		s.closeTaggedController(newCtrl)
		s.rollbackDetach(demoted)
		return errReplacedDuringBind
	}
	s.ctrl = newCtrl
	s.mu.Unlock()

	if _, err := s.registerDetached(cur, demoted, nil); err != nil {
		// bindMu prevents another foreground swap here. Roll publication back so
		// a registry failure cannot strand a running controller.
		s.mu.Lock()
		s.ctrl = cur
		s.mu.Unlock()
		s.closeTaggedController(newCtrl)
		s.rollbackDetach(demoted)
		return err
	}
	s.setControllerPath(newCtrl, targetPath)
	s.bc.ResetSessionPath(targetPath)
	return nil
}

var errReplacedDuringBind = &replacedDuringBindError{}

type replacedDuringBindError struct{}

func (*replacedDuringBindError) Error() string { return "session changed during switch" }

func (s *Server) rollbackDetach(demoted *control.SessionLeaseKeeper) {
	if demoted != nil && s.leases != nil {
		s.leases.Adopt(demoted)
	}
}

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
