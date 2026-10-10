// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { boot, STORAGE } from "../i18n";
import { RestoreNotice } from "./RestoreNotice";
import css from "./RestoreNotice.css?raw";

afterEach(() => { cleanup(); localStorage.setItem(STORAGE, "zh"); boot(); });
it.each(["zh", "en"])("renders named undo and dismiss buttons in %s", (language) => {
  localStorage.setItem(STORAGE, language); boot();
  render(<RestoreNotice receipt={{ tx: "tx-1", files: 1, working: false, error: "" }} onUndo={vi.fn(async () => {})} onDismiss={vi.fn()} />);
  expect(screen.getByRole("button", { name: language === "zh" ? "撤销这次还原" : "Undo this restore" })).toBeTruthy();
  expect(screen.getAllByRole("button").every((b) => !!b.textContent?.trim())).toBe(true);
});
it("uses wrapping inline layout without mount-triggered motion", () => {
  expect(css).toContain(".restore-notice");
  expect(css).toMatch(/flex-wrap:\s*wrap/);
  expect(css).toMatch(/overflow-wrap:\s*anywhere/);
  expect(css).not.toMatch(/animation|transition|position:\s*(fixed|absolute)/);
});
