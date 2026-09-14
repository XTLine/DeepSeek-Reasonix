import assert from "node:assert/strict";
import React, { act } from "react";
import { renderFilesWorkspace, waitFor } from "./workspace-panel-test-harness";
import { WorkspaceDockRegion, type WorkspaceDockRegionProps } from "../app-shell/WorkspaceDockRegion";
import { LocaleProvider } from "../lib/i18n";
import { performResourceAction, presentedFileRequestSnapshot } from "../lib/presentedFileNavigation";
import { useActivityBarStore } from "../store/activityBar";
import { useRemoteStore } from "../store/remote";

const reads: string[] = [];
const preview = (path: string, mode: string) => {
  reads.push(`${mode}:${path}`);
  return { path, body: `${mode} content ${path}`, size: 20, truncated: false, binary: false };
};
const { dom, root } = await renderFilesWorkspace({
  ReadPresentedFileForTab: async (_tab, _tool, path) => preview(path, "preview"),
  ReadPresentedFileSourceForTab: async (_tab, _tool, path) => preview(path, "source"),
  ListRemoteDir: async () => [],
  ReadRemoteFile: async (_host, path) => ({ ...preview(path, "remote"), mtimeUnix: 1 }),
});
const props: WorkspaceDockRegionProps = {
  visible: false, overlay: false, mode: "files", creation: false, showContext: false,
  t: key => key, onPickEntry: () => {}, remote: { onClose: () => {} }, context: {} as WorkspaceDockRegionProps["context"],
  workspace: { open: true, tabId: "navigation-session", cwd: "/repo", maximized: false, onClose: () => {}, onToggleMaximized: () => {} },
  workspaceKey: "navigation-test", workspaceRoot: "/repo",
};
const paint = (visible: boolean) => act(async () => root.render(<LocaleProvider><WorkspaceDockRegion {...props} visible={visible} /></LocaleProvider>));
await act(async () => { useActivityBarStore.setState({ workspaceRoot: "/repo", tabs: [], activeTabId: null }); });
await paint(false);
for (const hostId of ["local", "remote-test"]) {
  await act(async () => useRemoteStore.setState({ statuses: { "remote-test": { state: "connected" } } }));
  for (const action of ["preview", "source", "reveal-tree"] as const) {
    const path = `${hostId}-${action}.txt`;
    await act(async () => performResourceAction({ hostId, tabId: "navigation-session", path, source: "presented", toolCallId: "call" }, action));
    await paint(true);
    await waitFor(path, () => document.body.textContent?.includes(`content ${path}`) === true);
    assert.equal(presentedFileRequestSnapshot()?.dockTabId, useActivityBarStore.getState().activeTabId);
    await paint(true);
    assert(document.body.textContent?.includes(`content ${path}`));
    console.log("PASS real dock", hostId, action);
  }
  if (hostId === "local") {
    const original = useActivityBarStore.getState().activeTabId!;
    await act(async () => useActivityBarStore.getState().addTab("file", "Other files"));
    await act(async () => useActivityBarStore.getState().activateTab(original));
    await waitFor("restored presented resource", () => document.body.textContent?.includes("content local-reveal-tree.txt") === true);
    console.log("PASS presented resource identity survives view remount without replay");
  }
}
await act(async () => root.unmount());
dom.window.close();
assert(reads.length >= 6);
