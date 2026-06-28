// Run: tsx src/__tests__/doctor-steer-bypass.test.ts

import { bypassesSteerWhenRunning } from "../lib/commandRouting";

let passed = 0;
let failed = 0;

function eq(a: unknown, b: unknown, label: string) {
  if (a === b) {
    process.stdout.write(`  PASS  ${label}\n`);
    passed += 1;
  } else {
    process.stdout.write(`  FAIL  ${label}: expected ${JSON.stringify(b)}, got ${JSON.stringify(a)}\n`);
    failed += 1;
  }
}

console.log("\ndoctor steer bypass");

eq(bypassesSteerWhenRunning("/doctor"), true, "/doctor bypasses steer");
eq(bypassesSteerWhenRunning(" /doctor ".trim()), true, "trimmed /doctor bypasses steer");
eq(bypassesSteerWhenRunning("/doctor now"), false, "/doctor with args does not bypass steer");
eq(bypassesSteerWhenRunning("/model"), false, "other management commands keep existing routing");
eq(bypassesSteerWhenRunning("hello"), false, "normal text does not bypass steer");

if (failed > 0) process.exit(1);
process.stdout.write(`doctor steer bypass: ${passed} passed\n`);
