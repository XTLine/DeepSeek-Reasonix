// Run: tsx src/__tests__/remote-session-surface.test.tsx

import { JSDOM } from "jsdom";
import React from "react";
import { act } from "react";

import type { AppBindings, TabMeta } from "../lib/bridge";
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
Object.defineProperty(dom.window.HTMLElement.prototype, "detachEvent", { configurable: true, value: () => {} });

const tape: string[] = [];
const history = [
  { role: "user", content: "hello remote" },
  { role: "assistant", content: [{ type: "text", text: "part one" }, { type: "text", text: "part two" }] },
];
window.go = { main: { App: {
  async RemoteTabSnapshot(tabId: string) {
    tape.push(`snapshot:${tabId}`);
    return { history: JSON.parse(JSON.stringify(history)) };
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

// ── Surface: hydrates history, then live frames and state transitions ──
const root = createRoot(document.getElementById("root")!);
await act(async () => {
  root.render(
    <LocaleProvider>
      <RemoteSessionSurface tab={remoteTab} />
    </LocaleProvider>,
  );
});
await act(async () => flush());

ok(document.querySelectorAll(".remote-surface__row").length === 2, "history rows render after snapshot hydration");
const rows = [...document.querySelectorAll(".remote-surface__row")];
ok(rows[0]?.textContent?.includes("hello remote") === true, "string content renders");
ok(rows[1]?.textContent?.includes("part one") === true && rows[1]?.textContent?.includes("part two") === true, "part-array content joins");

await act(async () => {
  __emitMockRemoteTab("tab-remote-1", "event", { kind: "assistant" });
  __emitMockRemoteTab("tab-remote-1", "event", { kind: "assistant" });
  await flush();
});
ok(document.querySelector(".remote-surface__live")?.textContent?.includes("2") === true, "live frame count renders");

await act(async () => {
  __emitMockRemoteTab("tab-remote-1", "state", { state: "serve_down", error: "tunnel closed" });
  await flush();
});
{
  const warning = document.querySelector(".remote-surface--warning");
  ok(Boolean(warning), "serve_down renders the warning state");
  ok(warning?.textContent?.includes("tunnel closed") === true, "serve error detail renders");
}

await act(async () => {
  __emitMockRemoteTab("tab-remote-1", "state", { state: "ready" });
  await flush();
});

await act(async () => root.unmount());

// ── Hook: commands forward with the tab id ──
let probe: RemoteSessionApi | undefined;
function HookProbe() {
  probe = useRemoteSession("tab-remote-2");
  return null;
}
const secondRoot = createRoot(document.createElement("div"));
await act(async () => {
  secondRoot.render(
    <LocaleProvider>
      <HookProbe />
    </LocaleProvider>,
  );
});
await act(async () => flush());
ok(probe?.state === "connecting" || probe?.state === "ready", "hook exposes the tab state");
await act(async () => {
  await probe?.submit("run tests");
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

await act(async () => secondRoot.unmount());
dom.window.close();
process.stdout.write(`\n${passed} passed, ${failed} failed\n`);
if (failed > 0) process.exit(1);
