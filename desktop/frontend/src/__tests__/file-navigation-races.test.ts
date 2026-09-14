import assert from "node:assert/strict";
import { JSDOM } from "jsdom";
import { installDesktopHostStub } from "./desktopHostStub";
import { performResourceAction, presentedFileRequestSnapshot } from "../lib/presentedFileNavigation";
import { cancelFileNavigation } from "../lib/fileNavigationLifetime";
import { useActivityBarStore } from "../store/activityBar";
import { useBrowserPanelStore } from "../lib/browserPanelStore";

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (error: Error) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}
const dom = new JSDOM("", { url: "http://localhost" });
Object.assign(globalThis, { window: dom.window, document: dom.window.document });
const paths = new Map<string, ReturnType<typeof deferred<string>>>();
const revoked: string[] = [];
const stub = installDesktopHostStub({
  ResolveRemoteWorkspacePathForTab: (_tab: string, _host: string, _tool: string, path: string) => {
    const pending = deferred<string>(); paths.set(path, pending); return pending.promise;
  },
  CreatePresentedBrowserPreviewForTab: async () => "http://preview.test/one",
  RevokeWorkspaceBrowserPreview: async (url: string) => { revoked.push(url); },
});
const ref = { hostId: "remote", tabId: "session", source: "workspace" as const, toolCallId: "tool" };
const first = performResourceAction({ ...ref, path: "first" }, "preview");
const second = performResourceAction({ ...ref, path: "second" }, "source");
paths.get("second")!.resolve("/second"); await second;
const current = presentedFileRequestSnapshot();
paths.get("first")!.resolve("/first"); await first;
assert.equal(presentedFileRequestSnapshot(), current);
assert.equal(current?.ref.path, "/second");
const cancelled = performResourceAction({ ...ref, path: "cancelled" }, "preview");
useActivityBarStore.getState().activateTab(useActivityBarStore.getState().activeTabId!);
paths.get("cancelled")!.reject(new Error("obsolete failure")); await cancelled;
assert.equal(presentedFileRequestSnapshot(), null);

const opened = deferred<{ id: string }>();
const opening = deferred<void>();
const closed: string[] = [];
const host = {
  open: () => { opening.resolve(); return opened.promise; },
  close: async (id: string) => { closed.push(id); },
};
useBrowserPanelStore.setState({ host: host as unknown as NonNullable<ReturnType<typeof useBrowserPanelStore.getState>["host"]> });
const browser = performResourceAction({ hostId: "local", tabId: "session", source: "presented", toolCallId: "tool", path: "one.html" }, "browser");
await opening.promise;
cancelFileNavigation();
opened.resolve({ id: "only-owned-tab" });
await browser;
assert.deepEqual(revoked, ["http://preview.test/one"]);
assert(!useBrowserPanelStore.getState().tabs.some(tab => tab.id === "only-owned-tab"));
assert.deepEqual(closed, ["only-owned-tab"]);
stub.uninstall(); dom.window.close();
console.log("PASS navigation ordering, manual cancellation, obsolete errors and browser resource cleanup");
