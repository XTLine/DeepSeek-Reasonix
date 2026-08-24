import { CloudOff, Loader2, TriangleAlert } from "lucide-react";
import { useT } from "../lib/i18n";
import { Transcript } from "./Transcript";
import type { RemoteSessionApi } from "../lib/useRemoteSession";
import type { TabMeta } from "../lib/types";

type RemoteApproval = { id?: string; tool?: string; subject?: string };
type RemoteAskQuestion = { id?: string; prompt?: string; options?: Array<{ label?: string; description?: string }> };

/**
 * RemoteSessionSurface renders the active remote tab's content area with
 * the SAME Transcript component local tabs use — the session hook feeds the
 * shared reducer with serve frames, so items, live streaming, approvals, and
 * asks arrive in the local shapes. Only the connection state machine and
 * the approval/ask cards are remote-specific; the composer lives in the
 * app shell, shared with local tabs.
 */
export function RemoteSessionSurface({ tab, session }: { tab: TabMeta; session: RemoteSessionApi }) {
  const t = useT();
  if (!tab.remote) return null;

  if (session.state === "serve_down") {
    return (
      <div className="remote-surface remote-surface--warning" role="alert">
        <TriangleAlert size={18} aria-hidden="true" />
        <span>{t("remoteSurface.serveDown")}</span>
        {session.error ? <span className="remote-surface__detail">{session.error}</span> : null}
      </div>
    );
  }

  if (session.state === "error") {
    return (
      <div className="remote-surface remote-surface--error" role="alert">
        <CloudOff size={18} aria-hidden="true" />
        <span>{t("remoteSurface.error")}</span>
        {session.error ? <span className="remote-surface__detail">{session.error}</span> : null}
      </div>
    );
  }

  if (!session.hydrated && (session.state === "connecting" || session.state === "reconnecting")) {
    return (
      <div className="remote-surface remote-surface--waiting" role="status">
        <Loader2 size={18} className="remote-surface__spinner" aria-hidden="true" />
        <span>{t(session.state === "connecting" ? "remoteSurface.connecting" : "remoteSurface.reconnecting")}</span>
      </div>
    );
  }

  if (!session.hydrated) {
    // Ready but the history snapshot is still landing: skeleton rows instead
    // of a blank transcript — the same pattern as the local hydrate
    // placeholders and the tree's cold-start rows.
    return (
      <div className="remote-surface remote-surface--ready" role="status" aria-label={t("remoteSurface.connecting")}>
        <div className="remote-surface__skeleton" aria-hidden="true">
          <span className="remote-surface__skeleton-bar" />
          <span className="remote-surface__skeleton-bar remote-surface__skeleton-bar--short" />
          <span className="remote-surface__skeleton-bar remote-surface__skeleton-bar--short" />
          <span className="remote-surface__skeleton-bar" />
        </div>
      </div>
    );
  }

  const approval = session.transcript.approval as RemoteApproval | undefined;
  const ask = session.transcript.ask as { id?: string; questions?: RemoteAskQuestion[] } | undefined;

  return (
    <div className="remote-surface remote-surface--ready">
      <Transcript
        items={session.transcript.items}
        live={session.transcript.live}
        tabId={tab.id}
        running={session.transcript.running}
        checkpoints={session.transcript.checkpoints}
        onPrompt={() => {}}
        rewindDisabled
      />

      {approval ? (
        <div className="remote-surface__approval" role="alertdialog" aria-label={t("remoteSurface.approvalTitle")}>
          <div className="remote-surface__approval-head">
            <TriangleAlert size={14} aria-hidden="true" />
            <span>{approval.tool || t("remoteSurface.approvalTitle")}</span>
          </div>
          {approval.subject ? <pre className="remote-surface__approval-body">{approval.subject}</pre> : null}
          <div className="remote-surface__approval-actions">
            <button type="button" className="btn btn--small" onClick={() => void session.approve(approval.id ?? "", "deny")}>
              {t("remoteSurface.deny")}
            </button>
            <button type="button" className="btn btn--small btn--primary" onClick={() => void session.approve(approval.id ?? "", "allow")}>
              {t("remoteSurface.allow")}
            </button>
          </div>
        </div>
      ) : null}

      {ask?.questions?.length ? (
        <div className="remote-surface__ask" role="alertdialog" aria-label={t("remoteSurface.askTitle")}>
          {ask.questions.map((question, questionIndex) => (
            <div key={questionIndex} className="remote-surface__ask-question">
              <div className="remote-surface__ask-prompt">{question.prompt}</div>
              <div className="remote-surface__ask-options">
                {(question.options ?? []).map((option, optionIndex) => (
                  <button
                    key={option.label ?? optionIndex}
                    type="button"
                    className="btn btn--small"
                    title={option.description}
                    onClick={() => void session.answer(ask.id ?? "", option.label ?? "")}
                  >
                    {option.label}
                  </button>
                ))}
              </div>
            </div>
          ))}
        </div>
      ) : null}
    </div>
  );
}
