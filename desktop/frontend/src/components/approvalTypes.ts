import type { ComposerInsertRequest, ToolApprovalMode, WireApproval } from "../lib/types";

export type ApprovalModalProps = {
  approval: WireApproval;
  onAnswer: (allow: boolean, session: boolean, persist: boolean) => void;
  onResolveRecovery?: (action: "continue" | "continue_task" | "revise", feedback?: string) => void;
  onRevisePlan?: (text: string) => void;
  onExitPlan?: () => void;
  onStop: () => void;
  cwd?: string;
  tabId?: string;
  workspaceScopeKey?: string;
  insertRequest?: ComposerInsertRequest | null;
  onRevisionActiveChange?: (active: boolean) => void;
  toolApprovalMode?: ToolApprovalMode;
};
