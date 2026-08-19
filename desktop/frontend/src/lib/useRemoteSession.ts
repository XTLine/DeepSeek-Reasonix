import { useCallback, useEffect, useRef, useState } from "react";
import { app, onRemoteTabEvent, onRemoteTabState } from "./bridge";
import { initialState, reducer, type State } from "./useController";
import type { HistoryMessage, RemoteTabStateValue, WireEvent } from "./types";

// The remote session reuses the local transcript pipeline end to end: serve
// frames share the agent event wire form, so they run through the same
// reducer that drives local tabs, and /history hydrates through the same
// history action. The surface and composer therefore consume exactly the
// shapes the local UI consumes.

// RemoteSessionApi is the surface-facing contract of useRemoteSession.
export interface RemoteSessionApi {
  state: RemoteTabStateValue;
  error: string;
  transcript: State;
  hydrated: boolean;
  running: boolean;
  /** The serve's label for the active model, for the composer capsule. */
  modelLabel: string;
  submit: (text: string) => Promise<void>;
  cancelTurn: () => Promise<void>;
  approve: (callId: string, decision: string) => Promise<void>;
  answer: (callId: string, value: string) => Promise<void>;
}

export function useRemoteSession(tabId: string | undefined): RemoteSessionApi {
  const [state, setState] = useState<RemoteTabStateValue>("connecting");
  const [error, setError] = useState("");
  const [transcript, setTranscript] = useState<State>(initialState);
  const [modelLabel, setModelLabel] = useState("");
  const hydratedRef = useRef(false);
  const [hydrated, setHydrated] = useState(false);

  useEffect(() => {
    if (!tabId) return;
    setState("connecting");
    setError("")
    setTranscript(initialState)
    hydratedRef.current = false;
    setHydrated(false);
    let cancelled = false;

    // Hydrate from the snapshot; retry through the connecting window so a
    // late backend never leaves the surface empty. A forced run re-syncs
    // after a session reset or a reconnect: the snapshot reflects whatever
    // session the serve now holds.
    const hydrate = (force = false) => {
      if (force) {
        hydratedRef.current = false;
        setHydrated(false);
      }
      return hydrateLoop();
    };
    const hydrateLoop = async () => {
      for (let attempt = 0; attempt < 60 && !cancelled && !hydratedRef.current; attempt++) {
        try {
          const snap = await app.RemoteTabSnapshot(tabId);
          if (cancelled) return;
          const messages = Array.isArray(snap.history) ? (snap.history as HistoryMessage[]) : [];
          hydratedRef.current = true;
          setHydrated(true);
          const statusLabel = (snap.status as { label?: unknown } | undefined)?.label;
          setModelLabel(typeof statusLabel === "string" ? statusLabel : "");
          setTranscript((s) => reducer(s, { type: "history", messages }));
          return;
        } catch {
          // Executor form: the src tsconfig lib predates Promise.withResolvers.
          await new Promise<void>((resolve) => setTimeout(resolve, 500));
        }
      }
    };
    void hydrateLoop();

    const offState = onRemoteTabState(tabId, (s) => {
      if (cancelled) return;
      setState(s.state);
      setError(s.error ?? "");
      if (s.state === "ready") void hydrate(true);
    });
    const offEvent = onRemoteTabEvent(tabId, (raw) => {
      if (cancelled) return;
      setTranscript((s) => reducer(s, { type: "event", e: (raw ?? {}) as WireEvent }));
    });
    return () => {
      cancelled = true;
      offState();
      offEvent();
    };
  }, [tabId]);

  const submit = useCallback(async (text: string) => {
    if (!tabId) return;
    const trimmed = text.trim();
    if (!trimmed) return;
    // Optimistic user bubble, exactly like the local send path. seq rides
    // the reducer's counter; the submission id only needs uniqueness.
    setTranscript((s) => reducer(s, { type: "user", text: trimmed, seq: s.seq, submissionId: `remote-${Date.now()}` }));
    await app.SubmitRemoteTab(tabId, trimmed);
  }, [tabId]);

  const cancelTurn = useCallback(async () => {
    if (!tabId) return;
    await app.CancelRemoteTab(tabId);
  }, [tabId]);

  const approve = useCallback(async (callId: string, decision: string) => {
    if (!tabId) return;
    setTranscript((s) => ({ ...s, approval: undefined }));
    await app.ApproveRemoteTab(tabId, callId, decision);
  }, [tabId]);

  const answer = useCallback(async (callId: string, value: string) => {
    if (!tabId) return;
    setTranscript((s) => ({ ...s, ask: undefined }));
    await app.AnswerRemoteTab(tabId, callId, value);
  }, [tabId]);

  return { state, error, transcript, hydrated, running: transcript.running, modelLabel, submit, cancelTurn, approve, answer };
}
