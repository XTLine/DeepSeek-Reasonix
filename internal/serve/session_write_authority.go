package serve

import (
	"fmt"

	"reasonix/internal/agent"
	"reasonix/internal/control"
)

// SetSessionLeases hands the server the session-lease keeper that guards its
// active session file. Call it before serving; a nil keeper leaves gating off.
func (s *Server) SetSessionLeases(k *control.SessionLeaseKeeper) error {
	s.leases = k
	if ctrl, ok := s.ctl().(*control.Controller); ok {
		ctrl.SetOnSessionRecovered(s.sessionRecoveryHandler(ctrl, k))
		if k != nil {
			return k.BindControllerAuthority(ctrl)
		}
	}
	return nil
}

func (s *Server) sessionRecoveryHandler(ctrl *control.Controller, k *control.SessionLeaseKeeper) func(control.SessionRecoveryInfo) error {
	if k == nil && s.tagFor(ctrl) == nil {
		return nil
	}
	return func(info control.SessionRecoveryInfo) error {
		if k != nil {
			if err := k.HandleSessionRecovered(info); err != nil {
				return err
			}
		}
		if err := s.moveDetachedRecovery(ctrl, info.RecoveryPath); err != nil {
			return err
		}
		if tag := s.tagFor(ctrl); tag != nil {
			tag.SetPath(info.RecoveryPath)
		}
		if s.ctl() == control.SessionAPI(ctrl) {
			s.bc.SetCurrentSession(info.RecoveryPath)
		}
		return nil
	}
}

// moveDetachedRecovery keeps the registry key aligned when a background
// controller forks to a recovery transcript after an autosave conflict.
func (s *Server) moveDetachedRecovery(ctrl *control.Controller, recoveryPath string) error {
	if ctrl == nil {
		return nil
	}
	recoveryPath = agent.CanonicalSessionPath(recoveryPath)
	if recoveryPath == "" {
		return nil
	}
	s.detachedMu.Lock()
	defer s.detachedMu.Unlock()
	for oldPath, detached := range s.detached {
		if detached.ctrl != control.SessionAPI(ctrl) {
			continue
		}
		if existing := s.detached[recoveryPath]; existing != nil && existing != detached {
			return fmt.Errorf("recovery session is already running in the background")
		}
		delete(s.detached, oldPath)
		detached.path = recoveryPath
		s.detached[recoveryPath] = detached
		return nil
	}
	return nil
}

// rebindSessionLease moves the server's session lease to path and rebinds the
// write authority generation. A nil keeper gates nothing (tests, embedded use).
func (s *Server) rebindSessionLease(path string) error {
	ctrl, _ := s.ctl().(*control.Controller)
	return s.rebindSessionLeaseFor(path, ctrl)
}

func (s *Server) rebindSessionLeaseFor(path string, ctrl *control.Controller) error {
	if s.leases == nil {
		return nil
	}
	if err := s.leases.Rebind(path); err != nil {
		return err
	}
	if ctrl != nil {
		return s.leases.BindControllerAuthority(ctrl)
	}
	return nil
}
