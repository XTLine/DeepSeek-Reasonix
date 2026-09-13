import { useState } from "react";
import { app } from "../lib/bridge";
import { useT } from "../lib/i18n";
import { useRemoteStore } from "../store/remote";
import { dismissRemoteServeUpdate, isRemoteServeUpdateDismissed } from "../lib/remoteServeUpdateDismissal";

// Advisory drift banner for the active remote session: the old Serve keeps
// serving, the user decides. Upgrade arms in place (two clicks) because it
// interrupts in-flight remote turns; Ignore is remembered per serve version.
export function RemoteServeUpdateBanner({ hostId, workspace }: { hostId: string; workspace: string }) {
  const t = useT();
  const server = useRemoteStore((s) => s.servers[hostId]?.[workspace]);
  const [armedVersion, setArmedVersion] = useState<string | null>(null);
  const [dismissedVersion, setDismissedVersion] = useState<string | null>(null);
  if (!server?.updateAvailable || !server.serveVersion) return null;
  if (dismissedVersion === server.serveVersion || isRemoteServeUpdateDismissed(hostId, workspace, server.serveVersion)) return null;
  const armed = armedVersion === server.serveVersion;
  const updating = server.state === "updating";
  return (
    <div className="banner banner--warning banner--actionable" data-testid="remote-serve-update-banner">
      <span className="banner__msg">{t("remote.serveUpdate.banner", { version: server.serveVersion })}</span>
      <span className="banner__spacer" />
      <button
        type="button"
        className={`btn btn--small${armed ? " btn--danger" : ""}`}
        disabled={updating}
        title={armed ? t("remote.serveUpdate.confirmTitle") : undefined}
        onClick={() => {
          if (updating) return;
          if (!armed) {
            setArmedVersion(server.serveVersion!);
            return;
          }
          setArmedVersion(null);
          void app.UpdateRemoteServer(hostId, workspace).catch(() => setArmedVersion(null));
        }}
      >
        {updating ? t("remote.serveUpdate.updating") : armed ? t("remote.serveUpdate.confirm") : t("remote.serveUpdate.update")}
      </button>
      <button
        type="button"
        className="btn btn--small"
        disabled={updating}
        onClick={() => {
          dismissRemoteServeUpdate(hostId, workspace, server.serveVersion!);
          setDismissedVersion(server.serveVersion!);
        }}
      >
        {t("remote.serveUpdate.ignore")}
      </button>
    </div>
  );
}
