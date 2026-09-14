package main

import (
	"fmt"

	"reasonix/internal/control"
)

func (a *App) deleteCanonicalSession(route string) error {
	service := a.desktopSessionService(a.activeSessionDir())
	ref, ok := sessionRefForRoute(service, route)
	if !ok {
		return fmt.Errorf("invalid session identity")
	}

	a.mu.RLock()
	var owner *WorkspaceTab
	for _, candidate := range a.runtimeTabsLocked() {
		if candidate != nil && candidate.currentSessionIdentity() == route {
			owner = candidate
			break
		}
	}
	a.mu.RUnlock()
	if owner != nil {
		ctrl := a.controllerForTab(owner)
		identity, identityOK := ctrl.(control.IdentityLifecycle)
		if !identityOK || !identity.UsesExclusiveSession() {
			return fmt.Errorf("session runtime identity is unavailable")
		}
		current, bound := identity.SessionRef()
		if !bound || current != ref {
			return fmt.Errorf("session runtime changed while deleting")
		}
		if _, err := a.clearActiveSessionRuntime(owner, ctrl); err != nil {
			return err
		}
		return nil
	}
	if err := service.Delete(a.bootContext(), ref); err != nil {
		return friendlySessionFileError(err)
	}
	a.invalidatePromptHistoryCache()
	a.emitProjectTreeChangedForSessionDirs(a.activeSessionDir())
	return nil
}
