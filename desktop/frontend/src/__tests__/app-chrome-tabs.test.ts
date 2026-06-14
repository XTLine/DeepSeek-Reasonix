// Run: tsx src/__tests__/app-chrome-tabs.test.ts

import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const testDir = dirname(fileURLToPath(import.meta.url));
const appChromeSource = readFileSync(resolve(testDir, "../components/AppChrome.tsx"), "utf8");

let passed = 0;
let failed = 0;

function ok(value: unknown, label: string) {
  if (value) {
    process.stdout.write(`  PASS  ${label}\n`);
    passed += 1;
  } else {
    process.stdout.write(`  FAIL  ${label}\n`);
    failed += 1;
  }
}

console.log("\napp chrome tabs");

ok(
  !/\bTabBar\b/.test(appChromeSource),
  "AppChrome does not render the old top session tab strip",
);

for (const propName of ["onTabChange", "onTabClose", "onTabsClose", "onTabsReorder", "onNewTab"]) {
  ok(
    !new RegExp(`\\b${propName}\\b`).test(appChromeSource),
    `AppChrome does not expose ${propName}`,
  );
}

ok(
  !/app-chrome__tab-strip/.test(appChromeSource),
  "AppChrome markup does not include a tab strip container",
);

console.log(`\n${passed} passed, ${failed} failed`);
if (failed > 0) process.exit(1);
