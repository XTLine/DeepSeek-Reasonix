package cli

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
	"reasonix/internal/agent"
	"reasonix/internal/control"
	"reasonix/internal/i18n"
)

// A preview never resumes the target into a controller. The source controller
// and draft remain intact, with turn admission sealed until the preview ends.
type cliSessionPreview struct {
	path    string
	loading bool
	reopen  func()
}

type cliPreviewLoadedMsg struct {
	request *cliSessionPreview
	session *agent.Session
	err     error
}

func (m *chatTUI) openSessionPreview(path string) {
	if cliControllerHasActiveRuntimeWork(m.ctrl) || m.modelSwitchPending || m.peerBusy {
		m.notice(i18n.M.ResumeBusy)
		return
	}
	var reopen func()
	if m.preview != nil {
		reopen = m.preview.reopen
	} else if ctrl, ok := m.ctrl.(*control.Controller); ok && m.peerGrant == nil {
		var err error
		reopen, err = ctrl.BeginSessionHandoff(false)
		if err != nil {
			m.notice(err.Error())
			return
		}
	}
	p := &cliSessionPreview{path: path, loading: true, reopen: reopen}
	m.preview = p
	m.takeoverPrompt = nil
	m.pendingTakeoverCmd = func() tea.Msg {
		session, err := loadResumableSession(path)
		return cliPreviewLoadedMsg{request: p, session: session, err: err}
	}
}

func (m *chatTUI) applySessionPreview(msg cliPreviewLoadedMsg) {
	if m.preview != msg.request {
		return
	}
	m.preview.loading = false
	if msg.err != nil {
		m.closeSessionPreview()
		m.notice("view: " + msg.err.Error())
		return
	}
	m.clearTranscriptDisplay()
	m.commitLine(fmt.Sprintf("%s: %s", i18n.M.TakeoverView, msg.request.path))
	m.commitTranscriptSource(transcriptSource{kind: transcriptSourceReplayBundle, history: msg.session.Snapshot()})
	m.transcriptDirty, m.forceGotoBottom = true, true
}

func (m *chatTUI) closeSessionPreview() {
	if m.preview == nil {
		return
	}
	p := m.preview
	m.preview = nil
	m.clearTranscriptDisplay()
	m.commitTranscriptSource(transcriptSource{kind: transcriptSourceReplayBundle, history: m.ctrl.History()})
	m.transcriptDirty, m.forceGotoBottom = true, true
	if p.reopen != nil {
		p.reopen()
	}
}

func (m chatTUI) handlePreviewKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "r", "R":
		m.openTakeoverPrompt(m.preview.path)
	case "v", "V":
		m.openSessionPreview(m.preview.path)
	case "esc":
		m.closeSessionPreview()
	case "q", "Q", "ctrl+c", "ctrl+d":
		return m, shutdownNow
	case "pgup", "up":
		m.viewport.PageUp()
		m.markUserScrolled()
	case "pgdown", "down":
		m.viewport.PageDown()
		m.markUserScrolled()
	}
	return m, nil
}

func (m chatTUI) renderReadOnlyNotice() string {
	if m.preview != nil {
		return choicePanelStyle.Width(max(m.width, 10)).Render(i18n.M.TakeoverViewHint)
	}
	if m.peerGrant != nil {
		return choicePanelStyle.Width(max(m.width, 10)).Render(i18n.M.TakeoverYielded)
	}
	return ""
}
