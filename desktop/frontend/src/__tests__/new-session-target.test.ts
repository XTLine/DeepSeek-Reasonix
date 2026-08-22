// Run: node --import tsx src/__tests__/new-session-target.test.ts

import { newSessionTarget } from "../lib/newSessionTarget";

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
function eq(actual: unknown, expected: unknown, label: string) {
  if (JSON.stringify(actual) === JSON.stringify(expected)) ok(true, label);
  else ok(false, `${label}: expected ${JSON.stringify(expected)}, got ${JSON.stringify(actual)}`);
}

// A remote tab (scope "project" + remote-only workspace path) must route to
// the remote new-session path, never into the local EnsureBlankTab pipeline.
eq(
  newSessionTarget({ scope: "project", workspaceRoot: "~/app", remote: { hostId: "gpu-box", workspace: "~/app" } }),
  { kind: "remote", hostId: "gpu-box", workspace: "~/app" },
  "remote tab routes to remote new-session",
);

// Local project tab keeps the project blank-session behavior.
eq(
  newSessionTarget({ scope: "project", workspaceRoot: "/repo/local", remote: null }),
  { kind: "blank", scope: "project", workspaceRoot: "/repo/local" },
  "local project tab routes to project blank session",
);

// Global tab falls back to a global blank session.
eq(
  newSessionTarget({ scope: "global", workspaceRoot: "", remote: null }),
  { kind: "blank", scope: "global", workspaceRoot: "" },
  "global tab routes to global blank session",
);

// No tab at all behaves like global.
eq(
  newSessionTarget(null),
  { kind: "blank", scope: "global", workspaceRoot: "" },
  "missing tab routes to global blank session",
);

// A project-scoped tab without a workspace root degrades to global.
eq(
  newSessionTarget({ scope: "project", workspaceRoot: "" }),
  { kind: "blank", scope: "global", workspaceRoot: "" },
  "project tab without root routes to global",
);

process.stdout.write(`\n${passed} passed, ${failed} failed\n`);
if (failed > 0) process.exit(1);
