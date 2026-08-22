// New-session routing shared by every "new session" entry point (sidebar
// quick action, classic sidebar button, app chrome, shortcut, palette).
// A remote tab must never feed its workspace into the local blank-session
// pipeline — the remote path opens a new session on the remote tab instead.

export type NewSessionTarget =
  | { kind: "remote"; hostId: string; workspace: string }
  | { kind: "blank"; scope: string; workspaceRoot: string };

type ActiveTabLike = {
  scope?: string;
  workspaceRoot?: string;
  remote?: { hostId: string; workspace: string } | null;
} | null | undefined;

export function newSessionTarget(activeTab: ActiveTabLike): NewSessionTarget {
  const remote = activeTab?.remote;
  if (remote && remote.hostId && remote.workspace) {
    return { kind: "remote", hostId: remote.hostId, workspace: remote.workspace };
  }
  const activeWorkspaceRoot = activeTab?.scope === "project" ? activeTab.workspaceRoot || "" : "";
  return { kind: "blank", scope: activeWorkspaceRoot ? "project" : "global", workspaceRoot: activeWorkspaceRoot };
}
