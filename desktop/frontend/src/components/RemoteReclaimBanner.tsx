import { useEffect, useState } from "react";
import { useT } from "../lib/i18n";

export function RemoteReclaimBanner({
  tabId,
  busyTabId,
  reclaimBlocked = false,
  onReclaim,
}: {
  tabId: string;
  busyTabId: string | null;
  reclaimBlocked?: boolean;
  onReclaim: (tabId: string) => void;
}) {
  const t = useT();
  const [armedTabId, setArmedTabId] = useState<string | null>(null);
  useEffect(() => { setArmedTabId(null); }, [reclaimBlocked]);
  const armed = armedTabId === tabId;
  const busy = busyTabId !== null;

  return (
    <div className="banner banner--warning banner--actionable">
      <span className="banner__msg">{t(reclaimBlocked ? "takeover.remoteUnregistered" : "takeover.remoteBanner")}</span>
      <span className="banner__spacer" />
      {!reclaimBlocked && <button
        type="button"
        className={`btn btn--small${armed ? " btn--danger" : ""}`}
        disabled={busy}
        title={armed ? t("takeover.reclaimConfirm") : t("takeover.reclaimTitle")}
        onClick={() => {
          if (busy || !tabId) return;
          if (!armed) {
            setArmedTabId(tabId);
            return;
          }
          setArmedTabId(null);
          onReclaim(tabId);
        }}
      >
        {busyTabId === tabId
          ? t("takeover.reclaiming")
          : armed
            ? t("takeover.reclaimConfirmButton")
            : t("takeover.reclaim")}
      </button>}
    </div>
  );
}
