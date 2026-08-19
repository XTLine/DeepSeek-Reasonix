// Run: tsx src/__tests__/remote-connect-wizard.test.tsx

import { JSDOM } from "jsdom";
import React from "react";
import { act } from "react";

import type { AppBindings } from "../lib/bridge";
import type { RemoteHostView } from "../lib/types";

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

console.log("\nRemote connect wizard (three steps)");
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

const [{ createRoot }, { RemoteConnectWizard }, { LocaleProvider }, { useRemoteStore }] = await Promise.all([
  import("react-dom/client"),
  import("../components/RemoteConnectWizard"),
  import("../lib/i18n"),
  import("../store/remote"),
]);

// Bridge call tape: every remote method appends "<name>:<detail>".
const tape: string[] = [];
const savedHosts: RemoteHostView[] = [
  { id: "gpu-box", label: "gpu-box", host: "192.168.1.10", port: 22, user: "dev", identityFile: "~/.ssh/id_ed25519", proxyJump: "", defaultWorkspace: "", serveInstall: "auto", useSSHConfig: false },
  { id: "pw-box", label: "pw-box", host: "10.0.0.8", port: 22, user: "ops", identityFile: "", proxyJump: "", defaultWorkspace: "", serveInstall: "auto", useSSHConfig: false, passwordSet: true },
];
let hostCount = 0;

function setInput(input: HTMLInputElement, value: string) {
  const setter = Object.getOwnPropertyDescriptor(dom.window.HTMLInputElement.prototype, "value")?.set;
  setter?.call(input, value);
  input.dispatchEvent(new dom.window.Event("input", { bubbles: true }));
  input.dispatchEvent(new dom.window.Event("change", { bubbles: true }));
}

function buttonByText(text: string): HTMLButtonElement | undefined {
  return [...document.querySelectorAll<HTMLButtonElement>("button")].find((b) => b.textContent?.trim() === text);
}

async function flush(ticks = 20) {
  for (let i = 0; i < ticks; i++) await Promise.resolve();
}

// Real-time wait: the ConnectRemoteHost mock holds the connecting step for a
// 20ms macrotask so the log panel renders; microtask flushes cannot cover it.
function delay(ms: number): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setTimeout(resolve, ms);
  return promise;
}

const dirs: Record<string, Array<{ name: string; path: string; isDir: boolean }>> = {
  "/home/dev": [
    { name: "projects", path: "/home/dev/projects", isDir: true },
    { name: "notes.txt", path: "/home/dev/notes.txt", isDir: false },
  ],
  "/home/dev/projects": [{ name: "app", path: "/home/dev/projects/app", isDir: true }],
  "/home/dev/projects/web": [],
};

// Connection attempt counter: the first attempt fails (the wizard stays on
// the connecting step so its log panel stays observable); the retry succeeds.
let connectAttempts = 0;
// Platform-gate attempt counter: the first check models a Windows SSH host
// (uname reports MINGW64); later checks pass.
let platformAttempts = 0;
window.go = { main: { App: {
  async RemoteHosts() {
    tape.push("RemoteHosts");
    return savedHosts.slice();
  },
  async AddRemoteHost(input: RemoteHostView & { label?: string; host?: string }) {
    tape.push(`AddRemoteHost:${input.label}:${input.host}`);
    hostCount += 1;
    const view = { id: `new-${hostCount}`, label: String(input.label), host: String(input.host), port: 22, user: "", identityFile: "", proxyJump: "", defaultWorkspace: "", serveInstall: "npm", useSSHConfig: false } as RemoteHostView;
    savedHosts.push(view);
    return view;
  },
  async UpdateRemoteHost(id: string, input: RemoteHostView & { label?: string; host?: string }) {
    tape.push(`UpdateRemoteHost:${id}:${input.host}`);
    return savedHosts.find((h) => h.id === id) ?? savedHosts[0] as RemoteHostView;
  },
  async RemoteLastWorkspace() {
    tape.push("RemoteLastWorkspace");
    return "/home/dev";
  },
  async ListRemoteDir(_hostId: string, path: string) {
    tape.push(`ListRemoteDir:${path}`);
    return (dirs[path] ?? []).map((entry) => ({ ...entry, size: 0, mtimeUnix: 0, symlink: false }));
  },
  async MkdirRemote(_hostId: string, path: string) {
    tape.push(`MkdirRemote:${path}`);
    return undefined;
  },
  async ConnectRemoteHost(hostId: string) {
    tape.push(`ConnectRemoteHost:${hostId}`);
    // Hold 60ms so the connecting step mounts, then emit the kernel status
    // per attempt: first stopped+error → waitForRemoteConnection rejects
    // immediately and the wizard stays on the connecting step; later
    // attempts connected → the flow proceeds to the platform check.
    const { promise, resolve } = Promise.withResolvers<void>();
    setTimeout(resolve, 60);
    await promise;
    connectAttempts += 1;
    if (connectAttempts === 1) {
      useRemoteStore.getState().applyStatus({ hostId, state: "stopped", error: "ssh: handshake failed" });
      return undefined;
    }
    useRemoteStore.getState().applyStatus({ hostId, state: "connected" });
    return undefined;
  },
  // Platform gate: the first check models a Windows SSH host (uname reports
  // MINGW64), later checks pass.
  async CheckRemotePlatform(hostId: string) {
    tape.push(`CheckRemotePlatform:${hostId}`);
    platformAttempts += 1;
    if (platformAttempts === 1) {
      throw new Error('remote host platform check failed: unsupported remote OS "MINGW64_NT-10.0-19045" (V1 supports Linux and macOS)');
    }
    return undefined;
  },
  async AddRemoteProject(hostId: string, workspace: string) {
    tape.push(`AddRemoteProject:${hostId}:${workspace}`);
    return { hostId, workspace, title: `${hostId}:${workspace}` };
  },
  async OpenRemoteProjectTab(hostId: string, workspace: string) {
    tape.push(`OpenRemoteProjectTab:${hostId}:${workspace}`);
    return { id: "tab-1", scope: "project", workspaceRoot: workspace, workspaceName: "app", topicId: "", topicTitle: "", label: "", ready: true, running: false, mode: "normal", active: true, cwd: workspace, remote: { hostId, workspace } };
  },
} as Partial<AppBindings> as AppBindings } };

function WizardHarness() {
  return (
    <LocaleProvider>
      <RemoteConnectWizard
        onRefresh={async () => {
          tape.push("refresh");
        }}
        onClose={() => {
          tape.push("close");
        }}
      />
    </LocaleProvider>
  );
}

const rootElement = document.getElementById("root");
if (!rootElement) throw new Error("missing root");
const root = createRoot(rootElement);
await act(async () => {
  root.render(<WizardHarness />);
});
await act(async () => flush());

// ── Step ① initial state: stepper rail, config form ──
const railItems = () => [...document.querySelectorAll(".remote-wizard__rail-item")];
ok(railItems().length === 3, "stepper rail lists all three steps");
ok(railItems()[0]?.className.includes("--current") === true, "step 1 is current on open");
ok(railItems().every((item) => !item.className.includes("--done")), "no step is done on open");
ok(document.querySelectorAll(".remote-wizard__seg").length === 2, "auth and download use segmented sliders");
const hostInput = document.querySelector<HTMLInputElement>(".remote-wizard__suggest input");
ok(Boolean(hostInput), "config step shows the host input");

// ── Empty host+user: footer alert names both missing fields ──
{
  const userInput = [...document.querySelectorAll<HTMLInputElement>("input")].find((i) => i.placeholder.includes("root"));
  if (hostInput) setInput(hostInput, "");
  if (userInput) setInput(userInput, "");
  await act(async () => {
    buttonByText("Next")?.click();
    await Promise.resolve();
  });
  const alert = document.querySelector(".remote-wizard__footer [role='alert']");
  const text = alert?.textContent ?? "";
  ok(text.includes("Host") && text.includes("username") && text.includes("Password"), "empty form reports host, username, and password");
  ok(text.includes("⚠"), "footer alert uses the warning mark");
  if (userInput) setInput(userInput, "root");
}
// ── Saved-host suggestion: focus → dropdown → prefill ──
await act(async () => {
  hostInput?.dispatchEvent(new dom.window.Event("focusin", { bubbles: true }));
  hostInput?.dispatchEvent(new dom.window.Event("focus", { bubbles: false }));
  await Promise.resolve();
});
const suggestion = document.querySelector<HTMLButtonElement>(".remote-wizard__suggest-list button");
ok(Boolean(suggestion), "focusing the host input lists saved SSH connections");
await act(async () => {
  suggestion?.dispatchEvent(new dom.window.Event("mousedown", { bubbles: true, cancelable: true }));
  await Promise.resolve();
});
ok(hostInput?.value === "192.168.1.10", "picking a suggestion prefills the host");
const keyInput = [...document.querySelectorAll<HTMLInputElement>("input")].find((i) => i.value.includes("id_ed25519"));
ok(Boolean(keyInput), "saved key auth switches the form to key mode with the identity file");

{
  if (hostInput) setInput(hostInput, "");
  await act(async () => {
    hostInput?.dispatchEvent(new dom.window.Event("focusin", { bubbles: true }));
    hostInput?.dispatchEvent(new dom.window.Event("focus", { bubbles: false }));
    await Promise.resolve();
  });
  const listed = [...document.querySelectorAll<HTMLButtonElement>(".remote-wizard__suggest-list button")].find((b) => b.textContent?.includes("pw-box"));
  await act(async () => {
    listed?.dispatchEvent(new dom.window.Event("mousedown", { bubbles: true, cancelable: true }));
    await Promise.resolve();
  });
  const passwordInput = document.querySelector<HTMLInputElement>(".remote-wizard__field input[type='password']");
  ok((passwordInput?.placeholder ?? "").toLowerCase().includes("saved") || (passwordInput?.placeholder ?? "").includes("已保存"), "saved password host keeps a keep-existing placeholder");
  if (hostInput) setInput(hostInput, "");
  await act(async () => {
    hostInput?.dispatchEvent(new dom.window.Event("focusin", { bubbles: true }));
    hostInput?.dispatchEvent(new dom.window.Event("focus", { bubbles: false }));
    await Promise.resolve();
  });
  const gpuSuggestion = [...document.querySelectorAll<HTMLButtonElement>(".remote-wizard__suggest-list button")].find((b) => b.textContent?.includes("gpu-box"));
  await act(async () => {
    gpuSuggestion?.dispatchEvent(new dom.window.Event("mousedown", { bubbles: true, cancelable: true }));
    await Promise.resolve();
  });
}
// ── Next: first connect fails; the wizard stays on the connecting step ──
await act(async () => {
  buttonByText("Next")?.click();
  await delay(120);
  await flush();
});
ok(tape.includes("UpdateRemoteHost:gpu-box:192.168.1.10"), "next on a picked host updates it instead of adding a duplicate");
ok(!tape.some((entry) => entry.startsWith("AddRemoteHost:")), "no AddRemoteHost for a saved host");
// The failure path keeps the connecting step on screen (act has ended and
// the DOM is committed), so counting log lines here is reliable: at least
// two — connecting + failed.
{
  const logCount = document.querySelectorAll(".remote-wizard__log-line").length;
  ok(logCount >= 2, "connecting step streams the deployment log");
  const connectError = document.querySelector(".remote-wizard__connecting .remote-wizard__error");
  ok(Boolean(connectError?.textContent?.includes("ssh: handshake failed")), "failed connect surfaces the kernel error");
  ok(Boolean(buttonByText("Retry")), "retry action is offered after a failed connect");
}
// ── Retry #1: SSH connects, but the platform check rejects the host ──
await act(async () => {
  buttonByText("Retry")?.click();
  await delay(120);
  await flush();
});
ok(tape.includes("CheckRemotePlatform:gpu-box"), "a connected host runs the platform check before the workspace step");
ok(railItems()[1]?.className.includes("--current") === true, "an unsupported OS keeps the wizard on the connecting step");
{
  const platformError = document.querySelector(".remote-wizard__connecting .remote-wizard__error");
  ok(Boolean(platformError?.textContent?.includes("unsupported remote OS")), "the platform failure surfaces the kernel error");
  ok(!tape.some((entry) => entry.startsWith("ListRemoteDir")), "directory browsing never starts for an unsupported host");
  ok(Boolean(buttonByText("Retry")), "retry stays available after a platform rejection");
}
// ── Retry #2: the platform check passes and lands on step ③ ──
await act(async () => {
  buttonByText("Retry")?.click();
  await delay(120);
  await flush();
});
ok(railItems()[0]?.className.includes("--done") === true, "step 1 turns done (green check) after advancing");
ok(railItems()[2]?.className.includes("--current") === true, "step 3 is current after connecting");
ok(document.querySelector<HTMLInputElement>(".remote-wizard__path-input")?.value === "/home/dev", "workspace picker starts at RemoteLastWorkspace path");
ok(Boolean([...document.querySelectorAll(".remote-wizard__dir")].find((b) => b.textContent === "projects")), "directory entries render");
ok(Boolean([...document.querySelectorAll(".remote-wizard__file")].find((row) => row.textContent?.includes("notes.txt"))), "files render in the tree next to folders");
{
  const fileRow = [...document.querySelectorAll<HTMLButtonElement>(".remote-wizard__file")].find((row) => row.textContent?.includes("notes.txt"));
  await act(async () => {
    fileRow?.click();
    await Promise.resolve();
  });
  ok(fileRow?.className.includes("--selected") === true, "clicking a file highlights the row");
  ok(document.querySelector<HTMLInputElement>(".remote-wizard__path-input")?.value === "/home/dev", "clicking a file selects its parent directory as the workspace");
}

await act(async () => {
  [...document.querySelectorAll<HTMLButtonElement>(".remote-wizard__dir")].find((b) => b.textContent === "projects")?.click();
  await flush();
});
ok(Boolean([...document.querySelectorAll(".remote-wizard__dir")].find((b) => b.textContent === "app")), "drilling into a directory lists its children");
ok(!document.querySelector(".remote-wizard__mkdir"), "workspace step has no create-folder controls");

// ── Finish: register project → open remote tab (in order) ──
await act(async () => {
  buttonByText("Connect and open")?.click();
  await flush();
});
const addAt = tape.indexOf("AddRemoteProject:gpu-box:/home/dev/projects");
const openAt = tape.indexOf("OpenRemoteProjectTab:gpu-box:/home/dev/projects");
ok(addAt >= 0 && openAt > addAt, "finish registers the remote project before opening the remote tab");
ok(tape.includes("refresh"), "tree refresh runs after finish");
ok(tape.includes("close"), "wizard closes after a successful finish");

await act(async () => root.unmount());

// ── Second harness: brand-new host goes through AddRemoteHost ──
const secondRootEl = document.createElement("div");
document.body.appendChild(secondRootEl);
const secondRoot = createRoot(secondRootEl);
await act(async () => {
  secondRoot.render(<WizardHarness />);
});
await act(async () => flush());
const newHostInput = document.querySelector<HTMLInputElement>(".remote-wizard__suggest input");
const newUserInput = [...document.querySelectorAll<HTMLInputElement>("input")].find((i) => i.placeholder.includes("root"));
const newPasswordInput = document.querySelector<HTMLInputElement>(".remote-wizard__field input[type='password']");
await act(async () => {
  if (newHostInput) setInput(newHostInput, "10.9.8.7");
  if (newUserInput) setInput(newUserInput, "root");
  await Promise.resolve();
});
await act(async () => {
  if (newPasswordInput) setInput(newPasswordInput, "s3cret");
  await Promise.resolve();
});
await act(async () => {
  buttonByText("Next")?.click();
  await flush();
});
ok(tape.some((entry) => entry.startsWith("AddRemoteHost:10.9.8.7:10.9.8.7")), "a new host is added (label defaults to the host)");

await act(async () => secondRoot.unmount());
dom.window.close();
process.stdout.write(`\n${passed} passed, ${failed} failed\n`);
if (failed > 0) process.exit(1);
