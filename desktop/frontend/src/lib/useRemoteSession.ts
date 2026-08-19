import { useCallback, useEffect, useRef, useState } from "react";
import { app, onRemoteTabEvent, onRemoteTabState } from "../lib/bridge";
import type { RemoteTabStateValue } from "../lib/types";

export type RemoteHistoryRow = { role: string; text: string };

type HistoryMessage = { role?: unknown; content?: unknown };

// historyRow flattens one serve /history message into a display row. The
// serve wire form follows the provider message shape: content is either a
// plain string or an array of typed parts.
function historyRow(message: HistoryMessage): RemoteHistoryRow {
  const role = typeof message.role === "string" ? message.role : "";
  let text = "";
  const content = message.content;
  if (typeof content === "string") {
    text = content;
  } else if (Array.isArray(content)) {
    text = content
      .map((part) => (part && typeof part === "object" && "text" in part && typeof (part as { text?: unknown }).text === "string" ? (part as { text: string }).text : ""))
      .filter(Boolean)
      .join("\n");
  }
  return { role, text };
}

// RemoteSessionApi is the surface-facing contract of useRemoteSession.
export interface RemoteSessionApi {
  state: RemoteTabStateValue;
  error: string;
  history: RemoteHistoryRow[];
  frames: unknown[];
  hydrated: boolean;
  submit: (text: string) => Promise<void>;
  cancelTurn: () => Promise<void>;
  approve: (callId: string, decision: string) => Promise<void>;
  answer: (callId: string, value: string) => Promise<void>;
}

export function useRemoteSession(tabId: string | undefined): RemoteSessionApi {
  const [state, setState] = useState<RemoteTabStateValue>("connecting");
  const [error, setError] = useState("");
  const [history, setHistory] = useState<RemoteHistoryRow[]>([]);
  const [frames, setFrames] = useState<unknown[]>([]);
  const hydratedRef = useRef(false);

  useEffect(() => {
    if (!tabId) return;
    setState("connecting");
    setError("");
    setHistory([]);
    setFrames([]);
    hydratedRef.current = false;
    let cancelled = false;

    // Hydrate from the snapshot; retry through the connecting window so a
    // late backend never leaves the surface empty.
    const hydrate = async () => {
      for (let attempt = 0; attempt < 60 && !cancelled && !hydratedRef.current; attempt++) {
        try {
          const snap = await app.RemoteTabSnapshot(tabId);
          if (cancelled) return;
          const rows = Array.isArray(snap.history) ? (snap.history as HistoryMessage[]).map(historyRow) : [];
          hydratedRef.current = true;
          setHistory(rows);
          return;
        } catch {
          // Executor form: the src tsconfig lib predates Promise.withResolvers.
          await new Promise<void>((resolve) => setTimeout(resolve, 500));
        }
      }
    };
    void hydrate();

    const offState = onRemoteTabState(tabId, (s) => {
      if (cancelled) return;
      setState(s.state);
      setError(s.error ?? "");
      if (s.state === "ready" && !hydratedRef.current) void hydrate();
    });
    const offEvent = onRemoteTabEvent(tabId, (frame) => {
      if (cancelled) return;
      setFrames((current) => [...current, frame]);
    });
    return () => {
      cancelled = true;
      offState();
      offEvent();
    };
  }, [tabId]);

  const submit = useCallback(async (text: string) => {
    if (!tabId) return;
    await app.SubmitRemoteTab(tabId, text);
  }, [tabId]);

  const cancelTurn = useCallback(async () => {
    if (!tabId) return;
    await app.CancelRemoteTab(tabId);
  }, [tabId]);

  const approve = useCallback(async (callId: string, decision: string) => {
    if (!tabId) return;
    await app.ApproveRemoteTab(tabId, callId, decision);
  }, [tabId]);

  const answer = useCallback(async (callId: string, value: string) => {
    if (!tabId) return;
    await app.AnswerRemoteTab(tabId, callId, value);
  }, [tabId]);

  return { state, error, history, frames, hydrated: hydratedRef.current, submit, cancelTurn, approve, answer };
}
