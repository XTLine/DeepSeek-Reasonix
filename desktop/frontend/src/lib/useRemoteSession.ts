import { useCallback, useEffect, useRef, useState } from "react";
import { app, onRemoteTabEvent, onRemoteTabState } from "./bridge";
import { initialState, reducer, type State } from "./useController";
import type { HistoryMessage, RemoteTabStateValue, WireEvent } from "./types";

// The remote session reuses the local transcript pipeline end to end: serve
// frames share the agent event wire form, so they run through the same
// reducer that drives local tabs, and /history hydrates through the same
// history action. The surface and composer therefore consume exactly the
// shapes the local UI consumes.

// remoteStatusToAction maps the serve's raw /status payload onto the shared
// backend_status action so the remote surface reuses the local tab's running
// reconciliation (including its staleness guards). The serve reports the
// fields it knows; the rest stay undefined and the reducer keeps prior values.
function remoteStatusToAction(status: unknown, snapshotAt: number) {
  const raw = (status ?? null) as { running?: unknown; pendingPrompt?: unknown; backgroundJobs?: unknown; cancelRequested?: unknown; cancellable?: unknown } | null;
  return {
    type: "backend_status" as const,
    running: raw?.running === true,
    pendingPrompt: raw?.pendingPrompt === undefined ? undefined : raw.pendingPrompt === true,
    backgroundJobs: typeof raw?.backgroundJobs === "number" ? raw.backgroundJobs : undefined,
    cancelRequested: raw?.cancelRequested === undefined ? undefined : raw.cancelRequested === true,
    cancellable: raw?.cancellable === undefined ? (raw?.running === true) : raw.cancellable === true,
    snapshotAt,
  };
}

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

export function useRemoteSession(tabId: string | undefined, initial?: RemoteTabStateValue): RemoteSessionApi {
  const [state, setState] = useState<RemoteTabStateValue>(initial ?? "connecting");
  const [error, setError] = useState("");
  const [transcript, setTranscript] = useState<State>(initialState);
  const [modelLabel, setModelLabel] = useState("");
  const [hydrated, setHydrated] = useState(false);
  const hydratedRef = useRef(false);

  useEffect(() => {
    if (!tabId) return;
    // A restored disconnected shell seeds its state from the meta; live state
    // then flows through remote-tab:{id}:state events once a connect begins.
    const start = initial ?? "connecting";
    setState(start);
    setError("")
    setTranscript(initialState)
    hydratedRef.current = false;
    setHydrated(false);
    let cancelled = false;
    // Never start the snapshot retry loop on a shell with no connection: the
    // ready transition triggers the first hydration instead. (initial is
    // deliberately not a dependency — only the mount-time snapshot matters.)
    const skipHydrate = start === "disconnected";

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
          const status = (snap.status ?? null) as { label?: unknown } | null;
          setModelLabel(typeof status?.label === "string" ? status.label : "");
          setTranscript((s) => {
            let next = reducer(s, { type: "history", messages });
            // Hydrate doubles as the post-reconnect running reconciliation:
            // whatever the serve reports about its current state lands now,
            // not only after the next watchdog tick.
            next = reducer(next, remoteStatusToAction(snap.status, Date.now()));
            return next;
          });
          return;
        } catch {
          // Executor form: the src tsconfig lib predates Promise.withResolvers.
          await new Promise<void>((resolve) => setTimeout(resolve, 500));
        }
      }
    };
    if (!skipHydrate) void hydrateLoop();

    const offState = onRemoteTabState(tabId, (s) => {
      if (cancelled) return;
      setState(s.state);
      setError(s.error ?? "");
      if (s.state === "ready") {
        void hydrate(true);
      } else {
        // Leaving ready can only mean the serve connection dropped. A turn
        // that was running is now unobservable — stop the pill instead of
        // spinning forever on a turn_done that can never arrive.
        setTranscript((prev) => (prev.running || prev.turnActive ? reducer(prev, { type: "turn_interrupted" }) : prev));
      }
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

  // Running-state watchdog: while the pill claims a turn is running, poll the
  // serve's /status and feed it through the shared backend_status reducer.
  // This is the remote twin of the local tab's reconcile loop — a lost
  // turn_done frame (dropped SSE, slow-consumer drop, half-dead tunnel) then
  // clears within one tick instead of spinning forever.
  useEffect(() => {
    if (!tabId || !hydrated || state !== "ready" || !transcript.running) return;
    let cancelled = false;
    const reconcile = async () => {
      try {
        const snap = await app.RemoteTabSnapshot(tabId);
        if (cancelled) return;
        setTranscript((s) => reducer(s, remoteStatusToAction(snap.status, Date.now())));
      } catch {
        // Transient; the next tick retries.
      }
    };
    const timer = window.setInterval(reconcile, 30_000);
    return () => {
      cancelled = true;
      window.clearInterval(timer);
    };
  }, [tabId, hydrated, state, transcript.running]);

  const submit = useCallback(async (text: string) => {
    if (!tabId) return;
    const trimmed = text.trim();
    if (!trimmed) return;
    // Optimistic user bubble, exactly like the local send path. seq rides
    // the reducer's counter; the submission id only needs uniqueness.
    const submissionId = `remote-${Date.now()}`;
    setTranscript((s) => reducer(s, { type: "user", text: trimmed, seq: s.seq, submissionId }));
    try {
      await app.SubmitRemoteTab(tabId, trimmed);
    } catch (e) {
      // Roll the optimistic running flag back — a refused/failed submit must
      // never leave the pill spinning (same contract as the local send path).
      const error = `Send failed: ${e instanceof Error ? e.message : String(e)}`;
      setTranscript((s) => reducer(s, { type: "send_failed", submissionId, error }));
      throw e;
    }
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
