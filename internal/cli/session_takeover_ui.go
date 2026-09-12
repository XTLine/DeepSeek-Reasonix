package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"reasonix/internal/agent"
	"reasonix/internal/control"
	"reasonix/internal/i18n"
)

// This request is owned by the TUI. Workers only receive its immutable path;
// all controller publication happens back on the Update goroutine.
type cliTakeoverPrompt struct {
	path     string
	busy     bool
	querying bool
	err      error
	picker   *quickPicker
}

type cliTakeoverQueryMsg struct {
	request *cliTakeoverPrompt
	view    cliOwnershipView
	err     error
}

type cliTakeoverDoneMsg struct {
	request *cliTakeoverPrompt
	binding *cliTakeoverBinding
	loaded  *agent.Session
	err     error
	reopen  func()
}

func (m *chatTUI) openTakeoverPrompt(path string) {
	p := &cliTakeoverPrompt{path: path, querying: true}
	m.takeoverPrompt = p
	m.pendingTakeoverPath = path
	m.pendingTakeoverCmd = func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		view, err := queryCLITakeover(ctx, path)
		return cliTakeoverQueryMsg{request: p, view: view, err: err}
	}
}

func (m *chatTUI) applyTakeoverQuery(msg cliTakeoverQueryMsg) {
	if m.takeoverPrompt != msg.request {
		return
	}
	p := m.takeoverPrompt
	p.querying, p.err = false, msg.err
	items := []quickPickerItem{{ID: "cancel", Label: i18n.M.TakeoverCancel}, {ID: "view", Label: i18n.M.TakeoverView}}
	if msg.err == nil {
		items = append(items, quickPickerItem{ID: "wait", Label: i18n.M.TakeoverWait})
		if msg.view.Running {
			items = append(items, quickPickerItem{ID: "interrupt", Label: i18n.M.TakeoverInterrupt})
		}
	}
	p.picker = &quickPicker{title: i18n.M.TakeoverTitle, items: items}
}

func (m chatTUI) handleTakeoverKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	p := m.takeoverPrompt
	if p.busy {
		return m, nil
	}
	if msg.String() == "esc" || msg.String() == "ctrl+c" {
		m.takeoverPrompt = nil
		return m, nil
	}
	if p.querying || p.picker == nil {
		return m, nil
	}
	choice := p.picker.handleKey(msg)
	if choice.cancelled || choice.choice != nil && choice.choice.ID == "cancel" {
		m.takeoverPrompt = nil
		return m, nil
	}
	if choice.choice == nil {
		return m, nil
	}
	if choice.choice.ID == "view" {
		m.openSessionPreview(p.path)
		return m, nil
	}
	if cliControllerHasActiveRuntimeWork(m.ctrl) || m.modelSwitchPending {
		m.notice(i18n.M.ResumeBusy)
		return m, nil
	}
	mode := choice.choice.ID
	var reopen func()
	if ctrl, ok := m.ctrl.(*control.Controller); ok && m.peerGrant == nil && m.preview == nil {
		var err error
		reopen, err = ctrl.BeginSessionHandoff(false)
		if err != nil {
			m.notice(err.Error())
			return m, nil
		}
	}
	p.busy = true
	ctrl, leases, manager := m.ctrl, m.leases, m.takeover
	path := p.path
	yielded := m.peerGrant != nil
	return m, func() tea.Msg {
		result := cliTakeoverDoneMsg{request: p, reopen: reopen}
		if validator, ok := ctrl.(interface{ ValidateSessionModel(string) error }); ok {
			if result.err = validator.ValidateSessionModel(path); result.err != nil {
				return result
			}
		}
		if !yielded {
			result.err = ctrl.Snapshot()
		}
		if result.err != nil {
			return result
		}
		binding, err := cliAcquireFreeSession(path, leases, manager)
		if errors.Is(err, agent.ErrSessionLeaseHeld) {
			binding, err = cliTakeoverHeldSessionMode(path, err, leases, manager, mode)
		}
		result.binding, result.err = binding, err
		if err == nil {
			result.loaded, result.err = cliPrepareTakeoverCandidate(binding, leases)
		}
		if result.err != nil && binding != nil {
			result.err = errors.Join(result.err, cliReturnFailedTakeover(binding, leases, manager))
			result.binding = nil
		}
		return result
	}
}

func (m chatTUI) finishTakeover(msg cliTakeoverDoneMsg) (tea.Model, tea.Cmd) {
	defer func() {
		if msg.reopen != nil {
			msg.reopen()
		}
	}()
	if m.takeoverPrompt != msg.request {
		if msg.binding != nil {
			_ = cliReturnFailedTakeover(msg.binding, m.leases, m.takeover)
		}
		return m, nil
	}
	p := m.takeoverPrompt
	p.busy = false
	err := msg.err
	if err == nil {
		err = msg.binding.commitPrevious(m.takeover)
		if err != nil {
			err = errors.Join(err, cliReturnFailedTakeover(msg.binding, m.leases, m.takeover))
		}
	}
	if err == nil {
		m.ctrl.Resume(msg.loaded, p.path)
		err = bindChatTUIAuthority(&m)
	}
	if err != nil {
		p.err = err
		m.notice("takeover: " + err.Error())
	} else {
		if m.preview != nil {
			if m.preview.reopen != nil {
				m.preview.reopen()
			}
			m.preview = nil
		}
		m.peerGrant = nil
		if m.peerReopen != nil {
			m.peerReopen()
			m.peerReopen = nil
		}
		m.pendingTakeoverPath = ""
		m.takeoverPrompt = nil
		if m.takeover != nil && msg.binding.grant.MirrorID != "" {
			m.takeover.AttachController(m.ctrl)
			m.takeover.Activate(msg.binding)
		}
		m.replayActiveBranch(i18n.M.ResumedTitle)
		m.notice(i18n.M.TakeoverDone)
	}
	if m.takeoverShutdown != nil {
		shutdown := *m.takeoverShutdown
		m.takeoverShutdown = nil
		return m, func() tea.Msg { return shutdown }
	}
	return m, nil
}

func (m chatTUI) renderTakeoverPrompt() string {
	p := m.takeoverPrompt
	if p == nil {
		return ""
	}
	if p.busy || p.querying {
		return choicePanelStyle.Width(max(m.width, 10)).Render(i18n.M.TakeoverBusy)
	}
	content := p.picker.render(m.width)
	if p.err != nil {
		content += "\n" + fmt.Sprint(p.err)
	}
	return content
}

func (m *chatTUI) runTakeoverSelection(input string) {
	args := tokenizeArgs(input)
	path := m.pendingTakeoverPath
	if len(args) > 1 {
		path = strings.TrimSpace(args[1])
		if index, err := strconv.Atoi(path); err == nil {
			entries := resumeEntries(m.ctrl.SessionDir())
			if index < 1 || index > len(entries) {
				m.notice(i18n.M.NoSessionToResume)
				return
			}
			path = entries[index-1].session.Path
		}
	}
	if path == "" {
		m.notice(i18n.M.NoSessionToResume)
		return
	}
	m.openTakeoverPrompt(path)
}
