// Run: tsx src/__tests__/remote-session-surface.test.tsx

import React from "react";
import { JSDOM } from "jsdom";
import { act } from "react";

import type { AppBindings } from "../lib/bridge";
import type { TabMeta } from "../lib/types";
import type { RemoteSessionApi } from "../lib/useRemoteSession";

let passed = 0;
let failed = 0;
function ok(value: boolean, label: string) {
  if (value) {
    process.stdout.write(`  PASS  ${label}\n`);
    passed += 1;
  } else {
    process.stdout.write(`  FAIL  ${label}\n`);
    failed += 1;
  }
}

console.log("\nRemote session surface + hook");
const dom = new JSDOM("<!doctype html><html><body><div id=\"root\"></div></body></html>", {
  pretendToBeVisual: true,
  url: "http://localhost/",
});
(globalThis as typeof globalThis & { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
globalThis.window = dom.window as unknown as Window & typeof globalThis;
globalThis.document = dom.window.document;
Object.defineProperty(globalThis, "navigator", { configurable: true, value: dom.window.navigator });
globalThis.HTMLElement = dom.window.HTMLElement;
globalThis.Event = dom.window.Event;
globalThis.KeyboardEvent = dom.window.KeyboardEvent;
Object.defineProperty(dom.window.HTMLElement.prototype, "attachEvent", { configurable: true, value: () => {} });
// Transcript's virtualization calls the global rAF; jsdom only exposes it on
// the (visual) window.
globalThis.requestAnimationFrame = dom.window.requestAnimationFrame?.bind(dom.window) ?? ((cb: FrameRequestCallback) => setTimeout(() => cb(Date.now()), 16) as unknown as number);
globalThis.cancelAnimationFrame = dom.window.cancelAnimationFrame?.bind(dom.window) ?? ((handle: number) => clearTimeout(handle));
Object.defineProperty(dom.window.HTMLElement.prototype, "detachEvent", { configurable: true, value: () => {} });

const tape: string[] = [];
window.go = { main: { App: {
  async RemoteTabSnapshot(tabId: string) {
    tape.push(`snapshot:${tabId}`);
    return { history: [], status: { label: "DeepSeek · Mock" } };
  },
  async SubmitRemoteTab(tabId: string, text: string) {
    tape.push(`submit:${tabId}:${text}`);
  },
  async CancelRemoteTab(tabId: string) {
    tape.push(`cancel:${tabId}`);
  },
  async ApproveRemoteTab(tabId: string, callId: string, decision: string) {
    tape.push(`approve:${tabId}:${callId}:${decision}`);
  },
  async AnswerRemoteTab(tabId: string, callId: string, answer: string) {
    tape.push(`answer:${tabId}:${callId}:${answer}`);
  },
  async OpenRemoteProjectTab(hostId: string, workspace: string, opts?: { newSession?: boolean }) {
    tape.push(`open:${hostId}:${workspace}:${opts?.newSession ? "new" : ""}`);
    return { ...remoteTab, remote: { hostId, workspace } };
  },
} as Partial<AppBindings> as AppBindings } };

const [{ createRoot }, { RemoteSessionSurface }, { LocaleProvider }, { useRemoteSession }, { __emitMockRemoteTab }] = await Promise.all([
  import("react-dom/client"),
  import("../components/RemoteSessionSurface"),
  import("../lib/i18n"),
  import("../lib/useRemoteSession"),
  import("../lib/bridge"),
]);

const remoteTab: TabMeta = {
  id: "tab-remote-1",
  scope: "project",
  workspaceRoot: "~/app",
  workspaceName: "app",
  topicId: "",
  topicTitle: "app",
  label: "gpu-box",
  ready: true,
  running: false,
  mode: "normal",
  active: true,
  cwd: "~/app",
  remote: { hostId: "gpu-box", workspace: "~/app" },
};

async function flush(ticks = 20) {
  for (let i = 0; i < ticks; i++) await Promise.resolve();
}

// The surface takes its session from the hook — the same wiring the app
// shell uses (the shared Transcript renders the content, the composer lives
// in the shell).
function RemoteSurfaceHarness({ tab }: { tab: TabMeta }) {
  const session = useRemoteSession(tab.id);
  return <RemoteSessionSurface tab={tab} session={session} />;
}

// ── Surface: shared Transcript renders reducer-driven items ──
const root = createRoot(document.getElementById("root")!);
await act(async () => {
  root.render(
    <LocaleProvider>
      <RemoteSurfaceHarness tab={remoteTab} />
    </LocaleProvider>,
  );
});
await act(async () => flush());

ok(document.querySelector(".remote-surface__log") === null, "no bespoke log rows — the shared Transcript owns rendering");
ok(!document.querySelector(".remote-surface__composer"), "the surface renders no composer of its own");

await act(async () => {
  __emitMockRemoteTab("tab-remote-1", "event", { kind: "turn_started" });
  __emitMockRemoteTab("tab-remote-1", "event", { kind: "reasoning", reasoning: "thinking hard" });
  __emitMockRemoteTab("tab-remote-1", "event", { kind: "text", text: "streaming answer" });
  await flush();
});
ok(document.body.textContent?.includes("streaming answer") === true, "serve text frames render through the local transcript pipeline");
ok(document.body.textContent?.includes("thinking hard") === true, "reasoning renders through the local pipeline");
{
  const snapshotsBefore = tape.filter((entry) => entry.startsWith("snapshot:")).length;
  await act(async () => {
    __emitMockRemoteTab("tab-remote-1", "state", { state: "ready" });
    await flush();
  });
  ok(tape.filter((entry) => entry.startsWith("snapshot:")).length > snapshotsBefore, "a ready transition re-syncs the snapshot (session reset / reconnect path)");
}
ok(!document.body.textContent?.includes("streaming answer"), "the re-synced snapshot replaces the old session content")

await act(async () => {
  __emitMockRemoteTab("tab-remote-1", "event", { kind: "turn_done" });
  await flush();
});

await act(async () => {
  __emitMockRemoteTab("tab-remote-1", "event", { kind: "approval_request", approval: { id: "call-9", tool: "bash", subject: "rm -rf /tmp/junk" } });
  await flush();
});
{
  const dialog = document.querySelector(".remote-surface__approval");
  ok(Boolean(dialog), "approval card renders");
  ok(dialog?.textContent?.includes("rm -rf /tmp/junk") === true, "approval subject renders");
  await act(async () => {
    [...document.querySelectorAll<HTMLButtonElement>("button")].find((b) => b.textContent?.trim() === "Allow")?.click();
    await flush();
  });
  ok(tape.includes("approve:tab-remote-1:call-9:allow"), "allow click forwards ApproveRemoteTab");
  ok(!document.querySelector(".remote-surface__approval"), "approval card clears after deciding");
}

await act(async () => {
  __emitMockRemoteTab("tab-remote-1", "event", { kind: "ask_request", ask: { id: "ask-7", questions: [{ id: "q1", prompt: "Deploy now?", options: [{ label: "yes" }, { label: "no" }] }] } });
  await flush();
});
{
  const dialog = document.querySelector(".remote-surface__ask");
  ok(Boolean(dialog), "ask card renders");
  ok(dialog?.textContent?.includes("Deploy now?") === true, "ask prompt renders");
  await act(async () => {
    [...document.querySelectorAll<HTMLButtonElement>("button")].find((b) => b.textContent?.trim() === "yes")?.click();
    await flush();
  });
  ok(tape.includes("answer:tab-remote-1:ask-7:yes"), "option click forwards AnswerRemoteTab");
}

await act(async () => {
  __emitMockRemoteTab("tab-remote-1", "state", { state: "serve_down", error: "tunnel closed" });
  await flush();
});
{
  const warning = document.querySelector(".remote-surface--warning");
  ok(Boolean(warning), "serve_down renders the warning state");
  ok(warning?.textContent?.includes("tunnel closed") === true, "serve error detail renders");
}

// ── Restored shell: the disconnected state renders a reconnect affordance
// whose click reopens the project (fresh blank session) ──
await act(async () => {
  __emitMockRemoteTab("tab-remote-1", "state", { state: "disconnected" });
  await flush();
});
{
  const shell = document.querySelector(".remote-surface--disconnected");
  ok(Boolean(shell), "disconnected renders the restored-shell state");
  const reconnect = [...document.querySelectorAll<HTMLButtonElement>("button")].find((b) => b.textContent?.includes("Reconnect"));
  ok(Boolean(reconnect), "the disconnected surface offers a reconnect button");
  await act(async () => {
    reconnect?.click();
    await flush();
  });
  ok(tape.includes("open:gpu-box:~/app:new"), "reconnect reopens the project with a fresh session");
}

await act(async () => root.unmount());

// ── Hook: optimistic user bubble + command forwarding ──
let probe: RemoteSessionApi | undefined;
function HookProbe() {
  probe = useRemoteSession("tab-remote-2");
  return null;
}
const probeRoot = createRoot(document.createElement("div"));
await act(async () => {
  probeRoot.render(
    <LocaleProvider>
      <HookProbe />
    </LocaleProvider>,
  );
});
await act(async () => flush());
ok(probe?.state === "connecting" || probe?.state === "ready", "hook exposes the tab state");
await act(async () => {
  await probe?.submit("run tests");
  await flush();
});
ok(Boolean(probe?.transcript.items.some((item) => item.kind === "user" && item.text === "run tests")), "submit adds the optimistic user bubble through the shared reducer");
await act(async () => {
  await probe?.cancelTurn();
  await probe?.approve("call-1", "allow");
  await probe?.answer("ask-1", "yes");
  await flush();
});
for (const want of [
  "submit:tab-remote-2:run tests",
  "cancel:tab-remote-2",
  "approve:tab-remote-2:call-1:allow",
  "answer:tab-remote-2:ask-1:yes",
]) {
  ok(tape.includes(want), `command forwarded: ${want}`);
}

await act(async () => probeRoot.unmount());
dom.window.close();
process.stdout.write(`\n${passed} passed, ${failed} failed\n`);
if (failed > 0) process.exit(1);
