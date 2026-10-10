import { useCallback, useLayoutEffect, useMemo, useState } from "react";
import { reason } from "../i18n/kernel";
import { t } from "../i18n";
import type { AgentPort, RewindResult, RewindScope } from "../port/port";

export type RestoreNotice = { tx: string; files: number; working: boolean; error: string };

/** A restore receipt belongs to the conversation, not to a transcript card that
 *  the restore itself can remove. Session changes retire pending callbacks too. */
export function useRewindActions(port: AgentPort, reloadSession: () => void, onRestoreText: (text: string) => void, session = "", running = false) {
  const owner = useMemo(() => ({ active: true, epoch: 0, plans: new Map<string, number>(), notice: null as RestoreNotice | null,
    commit: null as { planId: string; promise: Promise<RewindResult> } | null, undo: null as Promise<void> | null }), [port, session]);
  const [notice, setNotice] = useState<{ owner: typeof owner; value: RestoreNotice | null } | null>(null);
  useLayoutEffect(() => {
    owner.active = true;
    return () => { owner.active = false; owner.epoch++; };
  }, [owner]);
  const publish = useCallback((value: RestoreNotice | null) => {
    owner.notice = value;
    setNotice({ owner, value });
  }, [owner]);
  useLayoutEffect(() => {
    if (running) { owner.epoch++; owner.plans.clear(); publish(null); }
  }, [owner, running, publish]);
  const onPrepareRewind = useCallback(async (turn: number, scope: RewindScope) => {
    if (!owner.active || owner.commit || owner.undo) throw new Error(t("正在还原…"));
    const epoch = owner.epoch;
    const plan = await port.prepareRewind(turn, scope);
    if (!owner.active || owner.epoch !== epoch) throw new Error(t("会话已切换，请重新选择还原位置"));
    owner.plans.set(plan.planId, plan.fileCount);
    return plan;
  }, [port, owner]);
  const onCommitRewind = useCallback((planId: string, text?: string) => {
    if (!owner.active) return Promise.reject(new Error(t("会话已切换，请重新选择还原位置")));
    if (owner.undo) return Promise.reject(new Error(t("正在还原…")));
    if (owner.commit) return owner.commit.planId === planId ? owner.commit.promise : Promise.reject(new Error(t("正在还原…")));
    const epoch = owner.epoch;
    const previous = owner.notice;
    if (previous) publish({ ...previous, working: true, error: "" });
    const operation = (async () => {
      const result = await port.commitRewind(planId);
      if (!owner.active || owner.epoch !== epoch) return result;
      const tx = result.undoAvailable ? result.transactionId : undefined;
      publish(tx ? { tx, files: result.deleted?.length ?? owner.plans.get(planId) ?? 0, working: false, error: "" } : null);
      if (result.conversationOk && text !== undefined) onRestoreText(text);
      reloadSession();
      return result;
    })();
    owner.commit = { planId, promise: operation };
    void operation.catch(() => {
      if (owner.active && owner.epoch === epoch) publish(previous);
    }).finally(() => {
      owner.plans.delete(planId);
      if (owner.commit?.promise === operation) owner.commit = null;
    });
    return operation;
  }, [port, owner, publish, reloadSession, onRestoreText]);
  const onUndoRewind = useCallback((transactionId: string) => {
    if (!owner.active || owner.notice?.tx !== transactionId) return Promise.resolve();
    if (owner.commit) return Promise.resolve();
    if (owner.undo) return owner.undo;
    const receipt = owner.notice;
    const epoch = owner.epoch;
    publish({ ...receipt, working: true, error: "" });
    const operation = (async () => {
      try {
        await port.undoRewind(transactionId);
        if (!owner.active || owner.epoch !== epoch) return;
        if (owner.notice?.tx === transactionId) publish(null);
        reloadSession();
      } catch (e) {
        if (owner.active && owner.epoch === epoch && owner.notice?.tx === transactionId) {
          publish({ ...receipt, working: false, error: reason(e) });
        }
      }
    })();
    owner.undo = operation;
    void operation.finally(() => { if (owner.undo === operation) owner.undo = null; });
    return operation;
  }, [port, owner, publish, reloadSession]);
  const dismissRestore = useCallback(() => { if (!owner.notice?.working) publish(null); }, [owner, publish]);
  const onPrepareFileRevert = useCallback((path: string) => port.prepareFileRevert(path), [port]);
  const onCommitFileRevert = useCallback(async (planId: string, resolution?: string) => {
    const epoch = owner.epoch;
    const result = await port.commitFileRevert(planId, resolution);
    if (owner.active && owner.epoch === epoch && (result.ok || result.transactionId)) publish(null);
    return result;
  }, [port, owner, publish]);
  return { restoreNotice: notice?.owner === owner ? notice.value : null, dismissRestore, onPrepareRewind, onCommitRewind, onUndoRewind, onPrepareFileRevert, onCommitFileRevert };
}
