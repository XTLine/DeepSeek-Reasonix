import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdtempSync, mkdirSync, rmSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { changedFiles, classifyPaths } from "./ci-paths.mjs";

test("explicit documentation skips build surfaces", () => {
  for (const path of ["README.md", "docs/guide.md", "desktop/AGENTS.md", "desktop/README.md", "desktop/electron/README.md"]) {
    const { flags, unknown } = classifyPaths([path]);
    assert.equal(flags.desktop, false, path);
    assert.equal(flags.memory, false, path);
    assert.deepEqual(unknown, [], path);
  }
});

test("frontend inputs select frontend, browser, memory and native validation", () => {
  for (const path of ["desktop/frontend/src/App.tsx", "desktop/frontend/public/help.md", "desktop/frontend/vite.config.ts", "desktop/pnpm-lock.yaml"]) {
    const { flags } = classifyPaths([path]);
    for (const name of ["desktop", "frontend", "browser", "memory", "native"]) assert.equal(flags[name], true, `${path}: ${name}`);
  }
});

test("Go, Electron and packaging inputs stay on their owning surfaces", () => {
  let flags = classifyPaths(["internal/control/controller.go"]).flags;
  assert.equal(flags.code, true);
  assert.equal(flags.desktop_go, true);
  assert.equal(flags.frontend, false);
  assert.equal(flags.memory, false);
  flags = classifyPaths(["desktop/electron/src/main/window.ts"]).flags;
  assert.equal(flags.electron, true);
  assert.equal(flags.frontend, false);
  flags = classifyPaths(["desktop/packaging/package.mjs"]).flags;
  assert.equal(flags.packaging, true);
  assert.equal(flags.browser, false);
});

test("mixed changes cannot hide affected work and unknown paths fail closed", () => {
  let result = classifyPaths(["README.md", "desktop/frontend/src/App.tsx"]);
  assert.equal(result.flags.memory, true);
  result = classifyPaths(["shared/new-loader.js"]);
  assert.deepEqual(result.unknown, ["shared/new-loader.js"]);
  for (const name of ["code", "desktop", "frontend", "browser", "memory", "electron", "native", "packaging"])
    assert.equal(result.flags[name], true, name);
});

test("release notes and full events are deterministic", () => {
  assert.equal(classifyPaths(["release-notes/v1.md"]).flags.notes_only, true);
  const notesPush = classifyPaths(["release-notes/v1.md"], { full: true }).flags;
  assert.equal(notesPush.notes_only, true);
  assert.equal(notesPush.desktop, false);
  assert.equal(classifyPaths([]).flags.desktop, false);
  const full = classifyPaths([], { full: true }).flags;
  assert.equal(full.notes_only, false);
  for (const name of ["code", "desktop", "frontend", "browser", "memory", "electron", "native", "packaging", "site", "sdk"])
    assert.equal(full[name], true, name);
});

test("push and merge-base diffs retain deletions and renames", t => {
  const root = mkdtempSync(path.join(os.tmpdir(), "reasonix-ci-paths-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  mkdirSync(path.join(root, "desktop/frontend/src"), { recursive: true });
  writeFileSync(path.join(root, "desktop/frontend/src/old.ts"), "old");
  writeFileSync(path.join(root, "desktop/frontend/src/delete.ts"), "delete");
  execFileSync("git", ["init"], { cwd: root });
  execFileSync("git", ["add", "."], { cwd: root });
  execFileSync("git", ["-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "base"], { cwd: root });
  const base = execFileSync("git", ["rev-parse", "HEAD"], { cwd: root, encoding: "utf8" }).trim();
  execFileSync("git", ["mv", "desktop/frontend/src/old.ts", "desktop/frontend/src/new.ts"], { cwd: root });
  execFileSync("git", ["rm", "desktop/frontend/src/delete.ts"], { cwd: root });
  execFileSync("git", ["-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-am", "change"], { cwd: root });
  const head = execFileSync("git", ["rev-parse", "HEAD"], { cwd: root, encoding: "utf8" }).trim();
  const push = changedFiles({ base, head, mode: "push", cwd: root });
  const pull = changedFiles({ base, head, mode: "pull_request", cwd: root });
  for (const files of [push, pull]) {
    assert.ok(files.includes("desktop/frontend/src/delete.ts"));
    assert.ok(files.includes("desktop/frontend/src/new.ts"));
    assert.equal(classifyPaths(files).flags.memory, true);
  }
  assert.deepEqual(changedFiles({ base: head, head, mode: "push", cwd: root }), []);
});

test("invalid diff identities fail instead of producing a skip", () => {
  assert.throws(() => changedFiles({ base: "0".repeat(40), head: "HEAD" }), /non-zero/);
  assert.throws(() => changedFiles({ base: "f".repeat(40), head: "HEAD" }));
});

test("CI routing changes exercise every routed Desktop surface", () => {
  for (const path of [".github/workflows/ci.yml", ".github/workflows/app-memory.yml", "scripts/ci-paths.mjs"]) {
    const flags = classifyPaths([path]).flags;
    for (const name of ["desktop", "desktop_go", "frontend", "browser", "memory", "electron", "native", "packaging"])
      assert.equal(flags[name], true, `${path}: ${name}`);
  }
});
