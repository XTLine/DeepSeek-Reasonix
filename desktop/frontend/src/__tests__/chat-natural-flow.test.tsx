import assert from "node:assert/strict";
import { act } from "react";
import { createTranscriptHarness } from "./transcript-dom-harness";
import { reconcileMountedOrder, revealEarlierMountedOrder } from "../lib/chatMountedOrder";
import type { Item } from "../lib/useController";

for (const count of [1, 5, 23, 24, 25, 48, 49]) {
  const resident = Array.from({ length: 60 }, (_, index) => `resident-${index}`);
  const prefix = Array.from({ length: count }, (_, index) => `history-${index}`);
  const full = [...prefix, ...resident];
  let mounted = reconcileMountedOrder(resident, full);
  assert.equal(mounted, resident, `${count}-node prepend keeps the resident order object before the first frame`);
  let frames = 0;
  while (mounted.length < full.length) {
    const previous = mounted;
    mounted = revealEarlierMountedOrder(mounted, full);
    frames += 1;
    assert.deepEqual(mounted.slice(-resident.length), resident, `${count}-node prepend keeps the resident suffix after frame ${frames}`);
    assert.ok(mounted.length > previous.length && mounted.length - previous.length <= 24, `${count}-node prepend reveals a bounded frame`);
  }
  assert.deepEqual(mounted, full, `${count}-node prepend converges to the source order`);
  assert.equal(frames, Math.ceil(count / 24), `${count}-node prepend uses the expected frame count`);
}

const harness = await createTranscriptHarness({ deterministic: true });
const items: Item[] = [
  { kind: "user", id: "u1", text: "hello", checkpointTurn: 1 },
  { kind: "tool", id: "t1", name: "read_file", args: "{}", output: "result", readOnly: true, status: "done" },
  { kind: "assistant", id: "a1", text: "answer", reasoning: "thought", streaming: false },
];
try {
  await harness.loadModule("/src/components/ChatToolBody.tsx");
  await harness.render(items);
  await harness.settle();
  assert.ok(harness.container.querySelector(".chat-column"));
  assert.equal(harness.container.querySelectorAll(".transcript__window-item").length, 0);
  assert.equal(harness.container.querySelectorAll(".chat-tool").length, 0, "completed process unmounts heavy rows");
  const disclosure = harness.container.querySelector<HTMLButtonElement>(".chat-process");
  await act(async () => disclosure!.click());
  const tool = harness.container.querySelector<HTMLElement>(".chat-tool [data-disclosure-row]");
  assert.ok(tool);
  await act(async () => tool.click());
  await harness.settle();
  assert.ok(harness.container.querySelector(".dsh-ToolRow-ioCard"), "tool opens an inline preview");
  await act(async () => harness.container.querySelector<HTMLButtonElement>(".dsh-ToolRow-inspectButton")!.click());
  assert.ok(harness.container.querySelector('[role="dialog"]'));
  for (let i = 0; i < 60; i++) {
    await harness.render(items.map(item => item.kind === "assistant" ? { ...item, text: `answer ${i}`, streaming: true } : item), { running: true });
    await act(async () => { harness.resizeNotifications.forEach(notify => notify()); harness.clock.advance(16); });
  }
  assert.ok(harness.container.querySelector(".chat-column"), "60 geometry changes do not trip React nested update fuse");
  await harness.render(items, { geometrySessionKey: "other" });
  assert.equal(harness.container.querySelector('[role="dialog"]'), null, "session replacement closes details");

  const restored: Item[] = [];
  for (let turn = 1; turn <= 12; turn += 1) {
    restored.push(
      { kind: "user", id: `u${turn}`, text: `prompt ${turn}`, checkpointTurn: turn },
      { kind: "assistant", id: `a${turn}`, text: `answer ${turn}`, reasoning: "", streaming: false },
    );
  }
  await harness.render(restored, { geometrySessionKey: "history-identity" });
  await harness.settle();
  const existing = new Map(Array.from(harness.container.querySelectorAll<HTMLElement>("[data-chat-anchor-key]"))
    .map(node => [node.dataset.chatAnchorKey!, node]));
  assert.equal(existing.size, 60, "fixture spans multiple legacy 24-node chunks");
  const selectedParagraph = harness.container.querySelector<HTMLElement>('[data-chat-anchor-key="a6"] .md p')!;
  const selectedText = selectedParagraph.firstChild!;
  const range = harness.dom.window.document.createRange();
  range.selectNodeContents(selectedText);
  const selection = harness.dom.window.getSelection()!;
  selection.removeAllRanges();
  selection.addRange(range);
  const selectedBefore = selection.toString();
  await harness.render([
    { kind: "user", id: "u0", text: "older prompt", checkpointTurn: 0 },
    { kind: "assistant", id: "a0", text: "older answer", reasoning: "", streaming: false },
    ...restored,
  ], { geometrySessionKey: "history-identity" });
  await harness.settle();
  for (const [key, node] of existing) {
    assert.equal(harness.container.querySelector(`[data-chat-anchor-key="${key}"]`), node, `history prepend preserves ${key} DOM identity`);
  }
  assert.equal(selection.toString(), selectedBefore, "history prepend preserves the native text selection");

  const deepHistory: Item[] = [];
  for (let turn = 1; turn <= 15; turn += 1) {
    deepHistory.push(
      { kind: "user", id: `old-u${turn}`, text: `old prompt ${turn}`, checkpointTurn: -turn },
      { kind: "assistant", id: `old-a${turn}`, text: `old answer ${turn}`, reasoning: "", streaming: false },
    );
  }
  await harness.render([
    ...deepHistory,
    { kind: "user", id: "u0", text: "older prompt", checkpointTurn: 0 },
    { kind: "assistant", id: "a0", text: "older answer", reasoning: "", streaming: false },
    ...restored,
  ], { geometrySessionKey: "history-identity" });
  assert.equal(harness.container.querySelector('[data-chat-anchor-key="old-u1"]'), null, "deep history is still mounting after two bounded frames");
  assert.equal(harness.container.querySelector('[data-nav-turn="old-u1"]'), null, "navigation omits history without a mounted target");
  await harness.settle();
  assert.ok(harness.container.querySelector('[data-chat-anchor-key="old-u1"]'), "deep history eventually mounts");
  assert.ok(harness.container.querySelector('[data-nav-turn="old-u1"]'), "navigation publishes the target after its DOM commit");
  console.log("chat natural flow: process disclosure, details, stable history identity, mounted navigation and session isolation passed");
} finally { await harness.unmount(); await harness.close(); }
