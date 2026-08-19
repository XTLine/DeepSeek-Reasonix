import { CloudOff, Loader2, RefreshCw, TriangleAlert } from "lucide-react";
import { useT } from "../lib/i18n";
import { useRemoteSession } from "../lib/useRemoteSession";
import type { TabMeta } from "../lib/types";

/**
 * RemoteSessionSurface renders the active remote tab's session area. It is
 * the remote counterpart of the local transcript + composer surface: the
 * connection state machine owns the layout until the session is ready, then
 * the hydrated history renders with live frames appended as they arrive.
 */
export function RemoteSessionSurface({ tab }: { tab: TabMeta }) {
  const t = useT();
  const session = useRemoteSession(tab.id);
  if (!tab.remote) return null;

  // Alerts win over content; otherwise hydrated content renders as soon as
  // the snapshot lands — the state event may still be in flight (mount race).
  if (!session.hydrated && (session.state === "connecting" || session.state === "reconnecting")) {
    return (
      <div className="remote-surface remote-surface--waiting" role="status">
        <Loader2 size={18} className="remote-surface__spinner" aria-hidden="true" />
        <span>{t(session.state === "connecting" ? "remoteSurface.connecting" : "remoteSurface.reconnecting")}</span>
      </div>
    );
  }

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

  return (
    <div className="remote-surface remote-surface--ready">
      <div className="remote-surface__log" role="log">
        {session.history.length === 0 && !session.hydrated ? (
          <div className="remote-surface__empty">{t("remoteSurface.loadingHistory")}</div>
        ) : session.history.length === 0 ? (
          <div className="remote-surface__empty">{t("remoteSurface.emptyHistory")}</div>
        ) : (
          session.history.map((row, index) => (
            <div key={index} className={`remote-surface__row remote-surface__row--${row.role}`}>
              <span className="remote-surface__role">{row.role || "msg"}</span>
              <span className="remote-surface__text">{row.text}</span>
            </div>
          ))
        )}
        {session.frames.length > 0 ? (
          <div className="remote-surface__live">
            <RefreshCw size={12} className="remote-surface__spinner" aria-hidden="true" />
            <span>{t("remoteSurface.liveFrames", { count: String(session.frames.length) })}</span>
          </div>
        ) : null}
      </div>
    </div>
  );
}
