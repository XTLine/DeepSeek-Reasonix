// Run: tsx src/__tests__/goal-lifecycle-actions.test.tsx

import { JSDOM } from "jsdom";
import React from "react";
import { act } from "react";
import { createRoot } from "react-dom/client";
import { GoalLifecycleActions } from "../components/GoalLifecycleActions";
import { LocaleProvider } from "../lib/i18n";

const dom = new JSDOM("<!doctype html><html><body><div id=\"root\"></div></body></html>", { url: "http://localhost/" });
(globalThis as typeof globalThis & { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
globalThis.window = dom.window as unknown as Window & typeof globalThis;
globalThis.document = dom.window.document;
globalThis.HTMLElement = dom.window.HTMLElement;
globalThis.Event = dom.window.Event;

let edit: { objective: string; maxGoalRounds: number | null } | undefined;
let paused = 0;
const prompts = ["finish the migration safely", "12"];
window.prompt = () => prompts.shift() ?? null;

await act(async () => {
  createRoot(document.getElementById("root")!).render(
    <LocaleProvider>
      <GoalLifecycleActions
        goalView={{
          id: "goal-1", revision: 3, objective: "finish the migration", phase: "active",
          maxGoalRounds: null, roundsStarted: 2, createdAt: "2026-01-01T00:00:00Z",
          updatedAt: "2026-01-01T00:00:00Z", activation: "armed",
        }}
        goalStatus="running"
        running
        onEditGoal={(objective, maxGoalRounds) => { edit = { objective, maxGoalRounds }; }}
        onPauseGoal={() => { paused += 1; }}
        onResumeGoal={() => {}}
        onStopGoal={() => {}}
      />
    </LocaleProvider>,
  );
});

const buttons = Array.from(document.querySelectorAll<HTMLButtonElement>("button"));
const editButton = buttons.find((button) => button.textContent === "Edit goal");
const pauseButton = buttons.find((button) => button.textContent === "Pause goal");
if (!editButton || !pauseButton) throw new Error("active goal lifecycle actions did not render");
if (pauseButton.disabled) throw new Error("running automatic Goal round is not pausable");

await act(async () => { editButton.click(); });
if (edit?.objective !== "finish the migration safely" || edit.maxGoalRounds !== 12) {
  throw new Error(`edit action returned ${JSON.stringify(edit)}`);
}
await act(async () => { pauseButton.click(); });
if (paused !== 1) throw new Error("pause action was not delivered");
console.log("goal lifecycle actions: PASS");
