// @vitest-environment jsdom
import "./testkit";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { Pane } from "./Pane";
import { MockPort } from "../port/mock";
import type { HistoryMessage, RewindResult } from "../port/port";
import type { RuntimeView } from "../port/hub";

afterEach(() => { cleanup(); localStorage.clear(); vi.restoreAllMocks(); });
const original: HistoryMessage[] = [
  { role: "user", content: "Restore this turn", msgIndex: 0 },
  { role: "assistant", content: "Updated file.txt", msgIndex: 1 },
];
const props = {
  rt: { id: "p1", root: "/sample", name: "Sample", sessionPath: "session-a" } as RuntimeView,
  title: "Sample", active: true, visible: true, sideHost: null, side: false,
  onFocus() {}, onReport() {}, onSessionChanged() {}, pulse: 0, findPulse: 0,
  onSettings() {}, needsProject: false, onOpenProject() {}, onKeepHere() {},
  theme: "dark", dockW: 560, dockMax: 880, onDockW() {},
};
async function open(conversation = true, holdInitialStatus = false, runtimePath: string | undefined = "session-a") {
  const port = new MockPort();
  let history = original;
  let session = "session-a";
  const status = await port.status();
  const statusReads = vi.spyOn(port, "status").mockImplementation(async () => ({ ...status, sessionPath: session }));
  let resolveStatus: (() => void) | undefined;
  if (holdInitialStatus) statusReads.mockReturnValueOnce(new Promise((resolve) => { resolveStatus = () => resolve({ ...status, sessionPath: session }); }));
  const reads = vi.spyOn(port, "history").mockImplementation(async () => history);
  vi.spyOn(port, "checkpoints").mockImplementation(async () => history.length ? [{ turn: 1, prompt: "Restore this turn", files: 1, msgIndex: 0 }] : []);
  vi.spyOn(port, "prepareRewind").mockResolvedValue({ planId: "plan-1", turn: 1, coverage: "full", canFiles: true, canConversation: true, fileCount: 1, requiresConfirmation: false });
  vi.spyOn(port, "commitRewind").mockImplementation(async () => {
    if (conversation) history = [];
    return { ok: true, conversationOk: conversation, transactionId: "tx-1", undoAvailable: true, deleted: ["file.txt"] };
  });
  const undo = vi.spyOn(port, "undoRewind").mockImplementation(async () => { history = original; });
  const view = render(<Pane {...props} rt={{ ...props.rt, sessionPath: runtimePath }} port={port} />);
  await screen.findByRole("button", { name: "回到这里" });
  return { port, reads, undo, view, resolveStatus, setSession: (next: string) => { session = next; } };
}
async function restore(scope = "代码和对话") {
  fireEvent.click(screen.getByRole("button", { name: "回到这里" }));
  fireEvent.click(screen.getByRole("menuitem", { name: new RegExp(scope) }));
}

it.each([true, false])("keeps undo through the real Pane reload with conversation removal %j", async (conversation) => {
  const { reads, undo } = await open(conversation);
  const before = reads.mock.calls.length;
  await restore(conversation ? "代码和对话" : "只还原代码");
  await waitFor(() => expect(reads.mock.calls.length).toBeGreaterThan(before));
  await waitFor(() => expect(!!screen.queryByRole("button", { name: "回到这里" })).toBe(!conversation));
  expect(screen.getByText("已还原 1 个文件")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "撤销这次还原" }));
  await waitFor(() => expect(undo).toHaveBeenCalledExactlyOnceWith("tx-1"));
  await screen.findByRole("button", { name: "回到这里" });
  expect(screen.queryByRole("button", { name: "撤销这次还原" })).toBeNull();
});

it("keeps undo after unrelated rerender, supports a refused undo retry and repeated restore", async () => {
  const h = await open();
  await restore();
  await screen.findByRole("button", { name: "撤销这次还原" });
  h.view.rerender(<Pane {...props} port={h.port} title="Renamed conversation" />);
  h.undo.mockRejectedValueOnce(new Error("Tracked file changed"));
  fireEvent.click(screen.getByRole("button", { name: "撤销这次还原" }));
  await screen.findByRole("alert", { name: "" });
  expect(screen.getByText("Tracked file changed")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "撤销这次还原" }));
  await screen.findByRole("button", { name: "回到这里" });
  await restore();
  await screen.findByRole("button", { name: "撤销这次还原" });
  expect(h.port.commitRewind).toHaveBeenCalledTimes(2);
});

it("allows explicit dismissal without invoking undo", async () => {
  const h = await open(); await restore();
  await screen.findByRole("button", { name: "撤销这次还原" });
  fireEvent.click(screen.getByRole("button", { name: "知道了" }));
  expect(screen.queryByText("已还原 1 个文件")).toBeNull(); expect(h.undo).not.toHaveBeenCalled();
});

it("clears the receipt when host status changes even while runtime metadata is stale", async () => {
  const h = await open(); await restore();
  await screen.findByRole("button", { name: "撤销这次还原" });
  h.setSession("session-b");
  h.view.rerender(<Pane {...props} port={h.port} pulse={1} />);
  await waitFor(() => expect(screen.queryByRole("button", { name: "撤销这次还原" })).toBeNull());
  expect(h.undo).not.toHaveBeenCalled();
});

it("does not publish a pending old-session restore or append its prompt to the new session", async () => {
  const h = await open();
  let resolve!: (value: RewindResult) => void;
  vi.mocked(h.port.commitRewind).mockReturnValue(new Promise((done) => { resolve = done; }));
  await restore(); await waitFor(() => expect(h.port.commitRewind).toHaveBeenCalled());
  h.view.rerender(<Pane key="takeover-1" {...props} port={h.port} rt={{ ...props.rt, sessionPath: "session-b" }} />);
  await screen.findByRole("button", { name: "回到这里" });
  const before = h.reads.mock.calls.length;
  await act(async () => resolve({ ok: true, conversationOk: true, transactionId: "tx-1", undoAvailable: true, deleted: ["file.txt"] }));
  expect(screen.queryByRole("button", { name: "撤销这次还原" })).toBeNull();
  expect(h.reads.mock.calls).toHaveLength(before);
  expect((screen.getByRole("combobox", { name: "任务输入" }) as HTMLTextAreaElement).value).toBe("");
});

it("does not mistake initial status hydration for leaving the known conversation", async () => {
  const h = await open(true, true);
  await restore();
  await screen.findByRole("button", { name: "撤销这次还原" });
  await act(async () => h.resolveStatus!());
  expect(screen.getByRole("button", { name: "撤销这次还原" })).toBeTruthy();
});

it("disables repeated undo clicks while the operation is in flight", async () => {
  const h = await open(); await restore();
  const button = await screen.findByRole("button", { name: "撤销这次还原" });
  let resolve!: () => void;
  h.undo.mockReturnValue(new Promise((done) => { resolve = done; }));
  fireEvent.click(button); fireEvent.click(button);
  expect((button as HTMLButtonElement).disabled).toBe(true);
  expect(h.undo).toHaveBeenCalledExactlyOnceWith("tx-1");
  await act(async () => resolve());
  expect(screen.queryByRole("button", { name: "撤销这次还原" })).toBeNull();
});

it("keeps a receipt when runtime metadata catches up to the known host session", async () => {
  const h = await open(true, false, "");
  await restore();
  await screen.findByRole("button", { name: "撤销这次还原" });
  h.view.rerender(<Pane {...props} port={h.port} />);
  expect(screen.getByRole("button", { name: "撤销这次还原" })).toBeTruthy();
});
