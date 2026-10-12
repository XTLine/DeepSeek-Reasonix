// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, it, vi } from "vitest";
import { MockPort } from "../port/mock";
import { WorkbenchPanel } from "./WorkbenchPanel";
import { Composer } from "./Composer";
import "./testkit";

const props = (port = new MockPort(), workspaceRoot = "/old") => ({
  port, workspaceRoot, tabs: [], manual: false, shown: true, scheme: "light" as const,
  changes: [], onCloseManual: vi.fn(), onSurfaces: vi.fn(), onExternal: vi.fn(),
});
const deferred = () => {
  let resolve!: (value: { files: string[]; directories: string[] }) => void;
  let reject!: (error: Error) => void;
  const promise = new Promise<{ files: string[]; directories: string[] }>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
};
afterEach(() => { cleanup(); vi.restoreAllMocks(); });

it.each([
  ["space name.txt", false, "@space\\ name.txt"],
  ["trailing ", true, "@trailing\\ /"],
  ["trailing\t", true, "@trailing\\\t/"],
  ['name"', false, '@name"'],
  [String.raw`literal\name.txt`, false, String.raw`@literal\name.txt`],
  [String.raw`before\ space.txt`, false, String.raw`@before\\ space.txt`],
  [String.raw`repeat\\ space.txt`, false, String.raw`@repeat\\\ space.txt`],
  ["before\\\ttab.txt", false, "@before\\\\\ttab.txt"],
  ["folder\\", true, "@folder\\/"],
])("copies %j into the real Composer without changing its reference", async (path, folder, reference) => {
  const user = userEvent.setup();
  const write = vi.spyOn(navigator.clipboard, "writeText").mockResolvedValue();
  const port = new MockPort();
  vi.spyOn(port, "workspaceFiles").mockResolvedValue({ files: folder ? [] : [path], directories: folder ? [path] : [] });
  const onSubmit = vi.fn().mockResolvedValue(true);
  render(<><WorkbenchPanel {...props(port)} /><Composer port={port} status={null} running={false} onSubmit={onSubmit} onChanged={vi.fn()} onError={vi.fn()} /></>);
  await waitFor(() => expect(document.querySelector(".workbench-tree-row")).toBeTruthy());
  fireEvent.contextMenu(document.querySelector(".workbench-tree-row")!);
  await user.click(screen.getByRole("menuitem", { name: "复制引用" }));
  expect(write).toHaveBeenCalledExactlyOnceWith(reference);
  const box = screen.getByRole("combobox", { name: "任务输入" });
  await user.click(box);
  await user.paste(write.mock.calls[0][0]);
  await user.click(screen.getByRole("button", { name: /^发送$/ }));
  await waitFor(() => expect(onSubmit.mock.calls[0]?.[0]).toBe(reference));
});

it.each(["trailing ", "trailing\t", "trailing\u00a0", "trailing\ufeff", "trailing\\"])("keeps only path copying for a file whose suffix cannot safely form a reference: %j", async (path) => {
  const user = userEvent.setup();
  const write = vi.spyOn(navigator.clipboard, "writeText").mockResolvedValue();
  const port = new MockPort();
  vi.spyOn(port, "workspaceFiles").mockResolvedValue({ files: [path], directories: [] });
  render(<WorkbenchPanel {...props(port)} />);
  await waitFor(() => expect(document.querySelector(".workbench-tree-row")).toBeTruthy());
  fireEvent.contextMenu(document.querySelector(".workbench-tree-row")!);
  expect((screen.getByRole("menuitem", { name: "复制引用" }) as HTMLButtonElement).disabled).toBe(true);
  await user.click(screen.getByRole("menuitem", { name: "复制路径" }));
  expect(write).toHaveBeenCalledWith(`/old/${path}`);
});

it.each([[false, false], [false, true], [true, false], [true, true]])("ignores a folder completion after replacing the workspace (failure: %s, port: %s)", async (failure, replacePort) => {
  const user = userEvent.setup();
  const port = new MockPort(), pending = deferred();
  vi.spyOn(port, "workspaceFiles").mockImplementation((path) => path === "old-folder" ? pending.promise : Promise.resolve({ files: [], directories: ["old-folder"] }));
  const first = props(port);
  const { rerender } = render(<WorkbenchPanel {...first} />);
  await user.click(await screen.findByRole("button", { name: "old-folder" }));
  vi.mocked(port.workspaceFiles).mockResolvedValue({ files: ["new.txt"], directories: [] });
  const nextPort = replacePort ? new MockPort() : port;
  if (replacePort) vi.spyOn(nextPort, "workspaceFiles").mockResolvedValue({ files: ["new.txt"], directories: [] });
  rerender(<WorkbenchPanel {...first} port={nextPort} workspaceRoot={replacePort ? "/old" : "/new"} />);
  await screen.findByRole("button", { name: "new.txt" });
  await act(async () => failure ? pending.reject(new Error("old root refused")) : pending.resolve({ files: ["old-folder/secret.txt"], directories: [] }));
  expect(screen.queryByRole("button", { name: "secret.txt" })).toBeNull();
  expect(screen.queryByRole("alert")).toBeNull();
});

it.each([false, true])("keeps the latest same-folder result over an older completion (failure: %s)", async (failure) => {
  const user = userEvent.setup();
  const port = new MockPort(), older = deferred(), newer = deferred();
  const list = vi.spyOn(port, "workspaceFiles").mockResolvedValue({ files: [], directories: ["docs"] });
  render(<WorkbenchPanel {...props(port)} />);
  const row = await screen.findByRole("button", { name: "docs" });
  list.mockImplementationOnce(() => older.promise).mockImplementationOnce(() => newer.promise);
  await user.click(row);
  await user.click(row);
  await act(async () => newer.resolve({ files: ["docs/current.txt"], directories: [] }));
  await act(async () => failure ? older.reject(new Error("old read refused")) : older.resolve({ files: ["docs/stale.txt"], directories: [] }));
  expect(screen.getByRole("button", { name: "current.txt" })).toBeTruthy();
  expect(screen.queryByRole("button", { name: "stale.txt" })).toBeNull();
  expect(screen.queryByRole("alert")).toBeNull();
});

it("reports the current folder failure and permits retry", async () => {
  const user = userEvent.setup();
  const port = new MockPort();
  const list = vi.spyOn(port, "workspaceFiles").mockResolvedValue({ files: [], directories: ["docs"] });
  render(<WorkbenchPanel {...props(port)} />);
  const row = await screen.findByRole("button", { name: "docs" });
  list.mockRejectedValueOnce(new Error("read refused")).mockResolvedValueOnce({ files: ["docs/current.txt"], directories: [] });
  await user.click(row);
  expect((await screen.findByRole("alert")).textContent).toBe("read refused");
  await user.click(row);
  expect(await screen.findByRole("button", { name: "current.txt" })).toBeTruthy();
  expect(screen.queryByRole("alert")).toBeNull();
});
