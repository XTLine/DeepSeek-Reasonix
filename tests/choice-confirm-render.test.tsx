import React from "react";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { ChoiceConfirm } from "../src/cli/ui/ChoiceConfirm.js";
import {
  type KeystrokeHandler,
  KeystrokeProvider,
  makeKeyEvent,
} from "../src/cli/ui/keystroke-context.js";
import { setLanguageRuntime } from "../src/i18n/index.js";
import { render } from "./helpers/ink-test.js";

const OPTIONS = [
  { id: "A", title: "Use the default", summary: "Keep the normal path" },
  { id: "B", title: "Try another route", summary: "Use the fallback" },
];

async function nextFrame(): Promise<void> {
  await new Promise((resolve) => setTimeout(resolve, 0));
}

class FakeReader {
  private handlers = new Set<KeystrokeHandler>();

  start(): void {}

  subscribe(handler: KeystrokeHandler): () => void {
    this.handlers.add(handler);
    return () => this.handlers.delete(handler);
  }

  emit(overrides: Parameters<typeof makeKeyEvent>[0]): void {
    const ev = makeKeyEvent(overrides);
    for (const handler of [...this.handlers]) handler(ev);
  }
}

describe("ChoiceConfirm", () => {
  beforeEach(() => setLanguageRuntime("EN"));
  afterEach(() => setLanguageRuntime("EN"));

  it("renders custom answer input inline when allowCustom is true", () => {
    const { lastFrame, unmount } = render(
      <ChoiceConfirm
        question="Pick one"
        options={OPTIONS}
        allowCustom={true}
        onChoose={() => {}}
      />,
    );
    const out = lastFrame() ?? "";
    unmount();

    expect(out).toContain("Free-form reply");
    expect(out).toContain("Let me type my own answer");
    expect(out).toContain("›");
  });

  it("submits the highlighted option even when custom text is typed", async () => {
    let picked: unknown = null;
    const { stdin, unmount } = render(
      <ChoiceConfirm
        question="Pick one"
        options={OPTIONS}
        allowCustom={true}
        onChoose={(choice) => {
          picked = choice;
        }}
      />,
    );

    stdin.write("custom route");
    await nextFrame();
    stdin.write("\r");
    await nextFrame();
    unmount();

    expect(picked).toEqual({ kind: "pick", optionId: "A" });
  });

  it("submits typed custom text when the custom answer row is highlighted", async () => {
    let picked: unknown = null;
    const { stdin, unmount } = render(
      <ChoiceConfirm
        question="Pick one"
        options={OPTIONS}
        allowCustom={true}
        onChoose={(choice) => {
          picked = choice;
        }}
      />,
    );

    stdin.write("custom route");
    await nextFrame();
    stdin.write("\x1b[B\x1b[B");
    await nextFrame();
    stdin.write("\r");
    await nextFrame();
    unmount();

    expect(picked).toEqual({ kind: "custom", text: "custom route" });
  });

  it("submits latest custom text when input and enter arrive in the same chunk", async () => {
    let picked: unknown = null;
    const reader = new FakeReader();
    const { unmount } = render(
      <KeystrokeProvider reader={reader}>
        <ChoiceConfirm
          question="Pick one"
          options={OPTIONS}
          allowCustom={true}
          onChoose={(choice) => {
            picked = choice;
          }}
        />
      </KeystrokeProvider>,
    );

    await nextFrame();
    reader.emit({ downArrow: true });
    reader.emit({ downArrow: true });
    await nextFrame();
    reader.emit({ input: "custom route" });
    reader.emit({ return: true });
    await nextFrame();
    unmount();

    expect(picked).toEqual({ kind: "custom", text: "custom route" });
  });

  it.each([
    ["backspace", { backspace: true }],
    ["delete", { delete: true }],
  ] as const)("applies same-batch %s before submitting custom text", async (_name, eraseKey) => {
    let picked: unknown = null;
    const reader = new FakeReader();
    const { unmount } = render(
      <KeystrokeProvider reader={reader}>
        <ChoiceConfirm
          question="Pick one"
          options={OPTIONS}
          allowCustom={true}
          onChoose={(choice) => {
            picked = choice;
          }}
        />
      </KeystrokeProvider>,
    );

    await nextFrame();
    reader.emit({ downArrow: true });
    reader.emit({ downArrow: true });
    await nextFrame();
    reader.emit({ input: "abc" });
    reader.emit(eraseKey);
    reader.emit({ return: true });
    await nextFrame();
    unmount();

    expect(picked).toEqual({ kind: "custom", text: "ab" });
  });
});
