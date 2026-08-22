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

globalThis.getComputedStyle = dom.window.getComputedStyle.bind(dom.window);

const tape: string[] = [];
let dedupeBlocked = false;
window.go = { main: { App: {
  async RemoteTabSnapshot(tabId: string, opts?: { members?: string[] }) {
    tape.push(`snapshot:${tabId}${opts?.members ? `:${opts.members.join(",")}` : ""}`);
    if (tabId === "tab-chain") {
      return {
        history: [{ role: "assistant", content: "chain-hello", turn: 1 }],
        status: { label: "DeepSeek · Chain", running: false },
      };
    }
    if (tabId === "tab-dedupe") {
      if (!dedupeBlocked) {
        dedupeBlocked = true;
        const release = new Promise<void>((resolve) => {
          (globalThis as typeof globalThis & { __releaseDedupeSnapshot?: () => void }).__releaseDedupeSnapshot = resolve;
        });
        await release;
      }
      return {
        history: [{ role: "assistant", content: "dedupe-hello", turn: 1 }],
        status: { label: "DeepSeek · Dedupe", running: false },
      };
    }
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
  async SetActiveTab(tabID: string) {
    tape.push(`setActive:${tabID}`);
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

// ── Restored shell: disconnected activation shows connecting and revives ──
{
  const shellTab: TabMeta = {
    ...remoteTab,
    id: "tab-shell",
    topicTitle: "shell",
    remoteState: "disconnected",
  };
  let shellSession: RemoteSessionApi | undefined;
  function ShellHarness() {
    shellSession = useRemoteSession(shellTab.id, shellTab.remoteState);
    return <RemoteSessionSurface tab={shellTab} session={shellSession} />;
  }
  const host = document.createElement("div");
  document.body.appendChild(host);
  const shellMount = createRoot(host);
  await act(async () => {
    shellMount.render(
      <LocaleProvider>
        <ShellHarness />
      </LocaleProvider>,
    );
  });
  await act(async () => flush());
  ok(!host.querySelector(".remote-surface--disconnected"), "disconnected never parks on the restored-shell placeholder");
  ok(Boolean(host.querySelector(".remote-surface--waiting")), "disconnected activation shows the connecting surface");
  ok(host.textContent?.includes("Connecting to the remote session") === true, "connecting copy replaces the reconnect affordance");
  ok(tape.includes("setActive:tab-shell"), "disconnected mount kicks SetActiveTab revive/bootstrap");
  ok(shellSession?.state === "connecting", "hook treats initial disconnected as connecting");
  await act(async () => shellMount.unmount());
  host.remove();
}

// Mid-flight disconnected events also refuse the placeholder.
await act(async () => {
  __emitMockRemoteTab("tab-remote-1", "state", { state: "disconnected" });
  await flush();
});
{
  ok(!document.querySelector(".remote-surface--disconnected"), "live disconnected events do not render the placeholder");
  ok(Boolean(document.querySelector(".remote-surface--waiting")), "live disconnected events show connecting instead");
}

await act(async () => root.unmount());

// ── Bridge→surface hydrate chain: snapshot return clears spinner and paints history ──
{
  const chainTab: TabMeta = { ...remoteTab, id: "tab-chain", topicTitle: "chain" };
  let chainSession: RemoteSessionApi | undefined;
  function ChainHarness() {
    chainSession = useRemoteSession(chainTab.id);
    return <RemoteSessionSurface tab={chainTab} session={chainSession} />;
  }
  const host = document.createElement("div");
  document.body.appendChild(host);
  const chainMount = createRoot(host);
  await act(async () => {
    chainMount.render(
      <LocaleProvider>
        <ChainHarness />
      </LocaleProvider>,
    );
  });
  await act(async () => {
    __emitMockRemoteTab("tab-chain", "state", { state: "connecting" });
    await flush();
  });
  await act(async () => {
    await new Promise<void>((resolve) => setTimeout(resolve, 30));
    await flush(40);
  });
  await act(async () => {
    __emitMockRemoteTab("tab-chain", "state", { state: "ready" });
    await flush(40);
    await new Promise<void>((resolve) => setTimeout(resolve, 30));
    await flush(40);
  });
  ok(tape.some((entry) => entry.startsWith("snapshot:tab-chain")), "hydrate chain calls RemoteTabSnapshot");
  ok(chainSession?.hydrated === true, "hydrate chain marks the session hydrated");
  ok(!host.querySelector(".remote-surface__spinner"), "hydrate chain clears the waiting spinner");
  ok(
    Boolean(chainSession?.transcript.items.some((item) => item.kind === "assistant" && item.text?.includes("chain-hello"))),
    "hydrate chain paints returned history into the transcript items",
  );
  await act(async () => chainMount.unmount());
  host.remove();
}

// ── Ready hydrate coalesce: overlapping ready/mount must not stampede snapshots ──
{
  const dedupeTab: TabMeta = { ...remoteTab, id: "tab-dedupe", topicTitle: "dedupe" };
  let dedupeSession: RemoteSessionApi | undefined;
  function DedupeHarness() {
    dedupeSession = useRemoteSession(dedupeTab.id);
    return <RemoteSessionSurface tab={dedupeTab} session={dedupeSession} />;
  }
  const host = document.createElement("div");
  document.body.appendChild(host);
  const mount = createRoot(host);
  const before = tape.filter((entry) => entry.startsWith("snapshot:tab-dedupe")).length;
  await act(async () => {
    mount.render(
      <LocaleProvider>
        <DedupeHarness />
      </LocaleProvider>,
    );
  });
  await act(async () => flush(20));
  // Mount hydrate is in-flight (blocked). A ready force must coalesce, not open a second concurrent fetch.
  await act(async () => {
    __emitMockRemoteTab("tab-dedupe", "state", { state: "ready" });
    __emitMockRemoteTab("tab-dedupe", "state", { state: "ready" });
    await flush(20);
  });
  const inFlight = tape.filter((entry) => entry.startsWith("snapshot:tab-dedupe")).length - before;
  ok(inFlight === 1, "ready/mount hydrate coalesce keeps a single in-flight snapshot");
  await act(async () => {
    (globalThis as typeof globalThis & { __releaseDedupeSnapshot?: () => void }).__releaseDedupeSnapshot?.();
    await new Promise<void>((resolve) => setTimeout(resolve, 30));
    await flush(40);
  });
  ok(dedupeSession?.hydrated === true, "coalesced hydrate still settles");
  ok(
    Boolean(dedupeSession?.transcript.items.some((item) => item.kind === "assistant" && item.text?.includes("dedupe-hello"))),
    "coalesced hydrate paints history",
  );
  await act(async () => mount.unmount());
  host.remove();
}

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
