import assert from "node:assert/strict";
import React, { act } from "react";
import { renderFilesWorkspace, waitFor } from "./workspace-panel-test-harness";
import { RemotePanel } from "../components/RemotePanel";
import { LocaleProvider } from "../lib/i18n";
import { useRemoteStore } from "../store/remote";

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>(yes => { resolve = yes; });
  return { promise, resolve };
}
type Preview = { path: string; body: string; size: number; mtimeUnix: number; binary: boolean; truncated: boolean };
const pending = new Map<string, ReturnType<typeof deferred<Preview>>>();
const { dom, root } = await renderFilesWorkspace({
  ListRemoteDir: async () => ["a.txt", "b.txt"].map(name => ({ name, path: name, isDir: false, size: 5 })),
  ReadRemoteFile: (_host, path) => {
    const read = deferred<Preview>(); pending.set(path, read); return read.promise;
  },
});
await act(async () => {
  useRemoteStore.setState({ explorerHostId: "remote", explorerTab: "files", statuses: { remote: { state: "connected" } } });
  root.render(<LocaleProvider><RemotePanel onClose={() => {}} tabId="one" dockTabId="dock" /></LocaleProvider>);
});
await waitFor("remote tree", () => document.querySelectorAll(".remote-tree__row").length === 2);
const click = async (name: string) => {
  const button = [...document.querySelectorAll<HTMLButtonElement>(".remote-tree__row")].find(node => node.textContent === name)!;
  await act(async () => button.click());
  await waitFor(`read ${name}`, () => pending.has(name));
};
await click("a.txt");
await click("b.txt");
const result = (path: string): Preview => ({ path, body: `CONTENT ${path}`, size: 10, mtimeUnix: 1, binary: false, truncated: false });
await act(async () => pending.get("b.txt")!.resolve(result("b.txt")));
await waitFor("new body", () => document.body.textContent?.includes("CONTENT b.txt") === true);
await act(async () => pending.get("a.txt")!.resolve(result("a.txt")));
assert(!document.body.textContent?.includes("CONTENT a.txt"));
assert(document.body.textContent?.includes("CONTENT b.txt"));
await click("a.txt");
await act(async () => root.unmount());
await act(async () => pending.get("a.txt")!.resolve(result("a.txt")));
assert.equal(document.querySelector(".remote-file-view"), null);
dom.window.close();
console.log("PASS actual remote panel: out-of-order reads and late completion after disposal");
