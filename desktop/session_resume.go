package main

import (
	"fmt"
	"os"
	"strings"

	"reasonix/internal/boot"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/plugin"
	"reasonix/internal/session"
)

func (a *App) continueLegacySessionForTranscript(tab *WorkspaceTab, ctrl control.SessionAPI, sourcePath string, limit int, includeHistory, readOnly bool) (HistoryPage, error) {
	identity, ok := ctrl.(control.IdentityLifecycle)
	if !ok || !identity.UsesExclusiveSession() {
		return HistoryPage{}, fmt.Errorf("session identity protocol is unavailable")
	}

	a.runtimeRebuildMu.Lock()
	defer a.runtimeRebuildMu.Unlock()
	tab.turnStartMu.Lock()
	defer tab.turnStartMu.Unlock()

	current := a.controllerForTab(tab)
	if current != ctrl || current == nil {
		return HistoryPage{}, fmt.Errorf("tab runtime changed while continuing legacy session")
	}
	if current.RuntimeStatus().Running || current.RuntimeStatus().PendingPrompt {
		return HistoryPage{}, control.ErrTurnRunning
	}
	if err := current.Snapshot(); err != nil {
		return HistoryPage{}, err
	}
	if _, err := identity.ContinueLegacySession(a.bootContext(), sourcePath, ""); err != nil {
		return HistoryPage{}, err
	}
	a.syncTabSessionIdentity(tab, current)
	a.setTabReadOnly(tab.ID, readOnly)
	a.invalidatePromptHistoryCache()
	a.notifyTabRuntimeRebuilt(tab)
	if !includeHistory {
		return HistoryPage{Messages: []HistoryMessage{}}, nil
	}
	return historyPageFromMessagesForTab(tab, current, current.History(), 0, limit), nil
}

func (a *App) resumeCanonicalSessionForTranscript(tab *WorkspaceTab, ctrl control.SessionAPI, route string, limit int, includeHistory bool) (HistoryPage, error) {
	identity, ok := ctrl.(control.IdentityLifecycle)
	if !ok || !identity.UsesExclusiveSession() {
		return HistoryPage{}, fmt.Errorf("session identity protocol is unavailable")
	}
	service := identity.SessionService()
	ref, ok := sessionRefForRoute(service, route)
	if !ok {
		return HistoryPage{}, fmt.Errorf("invalid session identity")
	}

	a.runtimeRebuildMu.Lock()
	defer a.runtimeRebuildMu.Unlock()
	tab.turnStartMu.Lock()
	defer tab.turnStartMu.Unlock()

	current := a.controllerForTab(tab)
	if current != ctrl || current == nil {
		return HistoryPage{}, fmt.Errorf("tab runtime changed while opening session")
	}
	if current.RuntimeStatus().Running || current.RuntimeStatus().PendingPrompt {
		return HistoryPage{}, control.ErrTurnRunning
	}
	if currentRef, bound := identity.SessionRef(); !bound || currentRef != ref {
		if err := current.Snapshot(); err != nil {
			return HistoryPage{}, err
		}
		target, err := service.Query().Snapshot(a.bootContext(), ref)
		if err != nil {
			return HistoryPage{}, err
		}
		targetModel := strings.TrimSpace(target.Projection.ModelRef)
		if targetModel != "" {
			current, err = a.replaceControllerForSessionOpenLocked(tab, current, service, ref, targetModel)
			if err != nil {
				return HistoryPage{}, err
			}
		} else if _, err := identity.OpenSession(a.bootContext(), ref); err != nil {
			return HistoryPage{}, err
		}
	}
	a.syncTabSessionIdentity(tab, current)
	a.setTabReadOnly(tab.ID, false)
	a.invalidatePromptHistoryCache()
	a.notifyTabRuntimeRebuilt(tab)
	if !includeHistory {
		return HistoryPage{Messages: []HistoryMessage{}}, nil
	}
	return historyPageFromMessagesForTab(tab, current, current.History(), 0, limit), nil
}

// replaceControllerForSessionOpenLocked prepares an Agent for the target session's
// recorded model before publishing it to the tab. The caller holds
// runtimeRebuildMu and tab.turnStartMu, so the source remains usable until the
// target model, writer, and event projection have all been validated.
func (a *App) replaceControllerForSessionOpenLocked(tab *WorkspaceTab, current control.SessionAPI, service *session.Service, ref session.SessionRef, targetModel string) (control.SessionAPI, error) {
	if tab == nil || current == nil || service == nil {
		return nil, fmt.Errorf("session runtime changed while opening session")
	}
	transition, err := a.reserveSessionRuntimePath(tab, sessionRoute(ref.SessionID))
	if err != nil {
		return nil, userFacingSessionLeaseError("", err)
	}
	committed := false
	defer func() {
		if !committed {
			a.rollbackSessionRuntimePath(transition)
		}
	}()
	snap := a.tabRuntimeSnapshot(tab)
	root := strings.TrimSpace(snap.workspaceRoot)
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}
	cfg, err := config.LoadForRoot(root)
	if err != nil {
		return nil, err
	}
	sharedHost := a.lookupSharedHost(snap.sharedHostKey)
	extensionGeneration := a.currentExtensionGeneration()
	candidate, err := a.buildTabControllerBootFenced(a.bootContext(), extensionGeneration, boot.Options{
		Model:                    targetModel,
		RequireKey:               false,
		StatsSource:              "desktop",
		TaskStore:                a.taskStore(),
		OnConfigLoadWarnings:     a.configLoadWarningsHandler(),
		Sink:                     a.desktopControllerSink(snap.sink, cfg.Notifications),
		WorkspaceRoot:            root,
		SessionDir:               sessionDirForSnapshot(snap),
		SessionService:           service,
		EffortOverride:           cloneStringPtr(snap.effort),
		SharedHost:               sharedHost,
		BrowserExecutor:          a.browserExecutorForTab(tab),
		MCPHostProfile:           plugin.HostProfileDesktopApps,
		CleanupPendingReconciler: reconcileDesktopCleanupPending,
		SubagentParentLive:       a.subagentParentProbeForBuild(tab),
		SessionRecoveryMeta:      a.tabSessionRecoveryMeta(tab),
		PinnedContextLoader:      pinnedContextLoader(root),
		OnSessionRecovered:       a.handleTabSessionRecovered(tab),
		OnSessionTransition:      a.handleTabSessionTransition(tab),
		BeforeInboxDispatch:      a.beforeInboxDispatch,
		OnSessionTitleChanged:    a.onSessionTitleChanged,
	})
	if err != nil {
		return nil, err
	}
	discard := true
	defer func() {
		if discard {
			candidate.Close()
		}
	}()
	candidateIdentity, ok := candidate.(control.IdentityLifecycle)
	if !ok || !candidateIdentity.UsesExclusiveSession() {
		return nil, fmt.Errorf("replacement session identity protocol is unavailable")
	}
	if _, err := candidateIdentity.OpenSession(a.bootContext(), ref); err != nil {
		return nil, err
	}
	a.bindControllerDisplayRecorder(candidate)
	candidate.EnableInteractiveApproval()
	runtime := snap.normalizedRuntime()
	applyTabToolApprovalModeToController(candidate, runtime.toolApprovalMode)
	applyTabQualityFloorToController(candidate, runtime.qualityFloor)
	runtime.collaborationMode = "normal"
	runtime.legacyGoal = ""
	if candidate.PlanMode() {
		runtime.collaborationMode = "plan"
	} else if candidate.GoalStatus() == control.GoalStatusRunning && strings.TrimSpace(candidate.Goal()) != "" {
		runtime.collaborationMode = "goal"
		runtime.legacyGoal = strings.TrimSpace(candidate.Goal())
	}

	a.mu.Lock()
	if tab.removed || a.tabs[tab.ID] != tab || tab.Ctrl != current {
		a.mu.Unlock()
		return nil, fmt.Errorf("tab runtime changed while opening session")
	}
	if err := a.authorizeTabReplacementLocked(tab, candidate, "opening session", "session-open"); err != nil {
		a.mu.Unlock()
		return nil, err
	}
	if !a.commitSessionRuntimePathLocked(transition) {
		a.mu.Unlock()
		return nil, fmt.Errorf("tab runtime changed while opening session")
	}
	tab.Ctrl = candidate
	tab.SessionID = ref.SessionID
	tab.SessionPath = ""
	tab.model = targetModel
	tab.Label = candidate.Label()
	applyNormalizedRuntimeToTabLocked(tab, runtime)
	tab.Ready = true
	clearTabStartupError(tab)
	a.bindSessionRuntimeKeyLocked(tab, tab.currentSessionIdentity())
	a.supersedeTabBuildLocked(tab)
	a.saveTabsLocked()
	epoch := a.advanceSessionRuntimeEpochLocked(tab)
	committed = true
	a.mu.Unlock()

	retireReplacedController(current, candidate)
	discard = false
	a.notifyTabRuntimeRebuiltAtEpoch(tab, epoch)
	return candidate, nil
}
