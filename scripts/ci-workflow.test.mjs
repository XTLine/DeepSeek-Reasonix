import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import vm from "node:vm";
import test from "node:test";

const workflow = name => readFileSync(new URL(`../.github/workflows/${name}.yml`, import.meta.url), "utf8");
function job(source, name) {
  const body = source.match(new RegExp(`\\n  ${name}:\\n([\\s\\S]*?)(?=\\n  [a-z][a-z0-9-]*:|$)`))?.[1];
  assert.ok(body, name);
  return body;
}
function condition(body, context) {
  const expression = body.match(/^    if: (.+)$/m)[1].replace(/^\$\{\{\s*|\s*\}\}$/g, "")
    .replace(/needs\.([a-z][a-z0-9-]*)/g, 'needs["$1"]');
  return vm.runInNewContext(expression, { always: () => true, cancelled: () => false, ...context });
}
function shellStep(body, name) {
  return body.split(`      - name: ${name}\n`)[1].match(/        run: \|\n((?:          .*\n|\n)+)/)[1]
    .replace(/^          /gm, "");
}
const ci = workflow("ci");
const release = workflow("release-desktop");

test("macOS signing diagnostics require protected main and cannot publish", () => {
  const source = workflow("macos-signing-check");
  const verify = job(source, "verify");
  const github = { repository: "esengine/DeepSeek-Reasonix", ref: "refs/heads/main-v2", ref_protected: true };
  assert.equal(condition(verify, { github }), true);
  for (const changed of [{ repository: "fork/Reasonix" }, { ref: "refs/tags/v1.0.0" }, { ref_protected: false }]) {
    assert.equal(condition(verify, { github: { ...github, ...changed } }), false);
  }
  assert.match(verify, /environment: release/);
  assert.match(verify, /ref: \$\{\{ github.sha \}\}/);
  assert.match(source, /permissions:\n  contents: read\n/);
  assert.doesNotMatch(source, /: write|secrets\.(R2_|SIGNPATH_|MINISIGN_|NPM_)/);
  assert.match(verify, /HAS_APPLE_CERT: "true"/);
  assert.match(verify, /scripts\/desktop-build.sh darwin\/universal v0.0.0-signing-check stable/);
  assert.match(verify, /path: \$\{\{ runner.temp \}\}\/apple-notarization\/\*\.json/);
  assert.match(verify, /if: always\(\)/);
});

test("required desktop aggregate rejects every failed, cancelled or unexpectedly skipped child", () => {
  const script = shellStep(job(ci, "desktop"), "Verify desktop validation jobs");
  const success = { CHANGES_RESULT: "success", PREPARE_REQUIRED: "true", NATIVE_REQUIRED: "true", FRONTEND_REQUIRED: "true", BROWSER_REQUIRED: "true",
    PREPARE_RESULT: "success", GO_RESULT: "success", GO_RACE_RESULT: "success", FRONTEND_RESULT: "success", BROWSER_RESULT: "success" };
  const run = env => spawnSync("bash", ["-e", "-c", script], { env: { ...process.env, ...env } }).status;
  assert.equal(run(success), 0);
  for (const key of ["PREPARE_RESULT", "GO_RESULT", "GO_RACE_RESULT", "FRONTEND_RESULT", "BROWSER_RESULT", "CHANGES_RESULT"]) {
    for (const value of ["failure", "cancelled", "skipped", ""]) assert.notEqual(run({ ...success, [key]: value }), 0, `${key}=${value}`);
  }
  assert.equal(run({ ...success, PREPARE_REQUIRED: "false", NATIVE_REQUIRED: "false", FRONTEND_REQUIRED: "false", BROWSER_REQUIRED: "false",
    PREPARE_RESULT: "skipped", GO_RESULT: "skipped", GO_RACE_RESULT: "skipped", FRONTEND_RESULT: "skipped", BROWSER_RESULT: "skipped" }), 0);
  assert.equal(run({ ...success, FRONTEND_REQUIRED: "false", BROWSER_REQUIRED: "false", FRONTEND_RESULT: "skipped", BROWSER_RESULT: "skipped" }), 0);
});

test("required lint aggregates code lint and the deduplicated frontend suite", () => {
  const body = job(ci, "lint");
  const script = shellStep(body, "Verify lint and frontend validation jobs");
  const success = { CHANGES_RESULT: "success", LINT_CODE_RESULT: "success", LINT_CODE_REQUIRED: "true",
    PREPARE_RESULT: "success", FRONTEND_RESULT: "success", FRONTEND_REQUIRED: "true" };
  const run = env => spawnSync("bash", ["-e", "-c", script], { env: { ...process.env, ...env } }).status;
  assert.equal(run(success), 0);
  for (const key of ["CHANGES_RESULT", "LINT_CODE_RESULT", "PREPARE_RESULT", "FRONTEND_RESULT"])
    for (const value of ["failure", "cancelled", "skipped", ""]) assert.notEqual(run({ ...success, [key]: value }), 0, `${key}=${value}`);
  assert.equal(run({ ...success, LINT_CODE_REQUIRED: "false", LINT_CODE_RESULT: "skipped",
    FRONTEND_REQUIRED: "false", PREPARE_RESULT: "skipped", FRONTEND_RESULT: "skipped" }), 0);
  assert.equal(run({ ...success, FRONTEND_REQUIRED: "false", PREPARE_RESULT: "success", FRONTEND_RESULT: "skipped" }), 0);
  assert.doesNotMatch(job(ci, "lint-code"), /test:motion/);
  assert.match(body, /needs: \[changes, lint-code, desktop-prepare, desktop-frontend\]/);
});

test("reuse skips only build work and still gates every publisher on validation", () => {
  const context = {
    inputs: { preflight_artifact_prefix: "desktop-123-1-preflight", orchestrated: true, signing_preflight_verified: true, signing_preflight: false, production_signing_smoke: false },
    needs: { resolve: { result: "success" }, "cache-guard": { result: "success" }, "signing-contract": { result: "success" }, "mac-universal-intel": { result: "skipped" }, build: { result: "skipped" } },
  };
  assert.equal(condition(job(release, "build"), context), false);
  assert.equal(condition(job(release, "publish"), context), true);
  for (const key of ["resolve", "cache-guard", "signing-contract", "mac-universal-intel", "build"]) {
    for (const result of ["failure", "cancelled"]) {
      const changed = structuredClone(context);
      changed.needs[key].result = result;
      assert.equal(condition(job(release, "publish"), changed), false, `${key}=${result}`);
    }
  }
  for (const key of ["orchestrated", "signing_preflight_verified"]) {
    assert.equal(condition(job(release, "publish"), { ...context, inputs: { ...context.inputs, [key]: false } }), false);
  }
  for (const key of ["signing_preflight", "production_signing_smoke"]) {
    assert.equal(condition(job(release, "publish"), { ...context, inputs: { ...context.inputs, [key]: true } }), false);
  }
  const fresh = structuredClone(context);
  fresh.inputs.preflight_artifact_prefix = "";
  assert.equal(condition(job(release, "build"), fresh), true);
  assert.equal(condition(job(release, "publish"), fresh), false);
  fresh.needs.build.result = "success";
  fresh.needs["mac-universal-intel"].result = "success";
  assert.equal(condition(job(release, "publish"), fresh), true);
});

test("reuse never moves artifact verification past public mutation or trusts candidate scripts", () => {
  const publisher = job(release, "publish");
  assert.ok(publisher.indexOf("Verify complete signed artifact handoff") < publisher.indexOf("name: Publish GitHub release"));
  assert.ok(publisher.includes("node release-control/scripts/desktop-release-artifacts.mjs collect"));
  assert.ok(publisher.includes("ref: ${{ github.workflow_sha }}"));
  assert.ok(!publisher.includes("merge-multiple: true"));
  const stable = workflow("release-stable");
  assert.ok(job(stable, "desktop").includes("preflight_artifact_prefix: ${{ needs.signpath-preflight.outputs.artifact_prefix }}"));
  for (const name of ["desktop", "cli", "npm"]) assert.ok(job(stable, name).includes("needs: [authorize, signpath-preflight]"));
});

test("all desktop consumers verify the prepared build and reject a failed preparation", () => {
  const context = { github: { event_name: "pull_request" },
    needs: { changes: { outputs: { desktop: "true" } }, "desktop-prepare": { result: "success" } } };
  const aggregate = job(ci, "desktop");
  for (const [name, variant] of [
    ["desktop-go", "stable"], ["desktop-frontend", "stable"], ["desktop-browser-group", "stable"],
    ["desktop-macos", "stable"], ["desktop-windows", "canary"], ["desktop-windows-go", "stable"],
  ]) {
    const body = job(ci, name);
    if (["desktop-go", "desktop-frontend"].includes(name)) assert.ok(aggregate.includes(name));
    assert.ok(body.includes("needs: [changes, desktop-prepare]"));
    assert.ok(body.includes(`name: \${{ needs.desktop-prepare.outputs.${variant}_artifact_name }}`));
    assert.ok(body.includes(`--shell electron --channel ${variant}`));
    assert.ok(!body.includes("pnpm --dir frontend build"));
    assert.equal(condition(body, context), true);
    assert.equal(condition(body, { ...context, needs: { ...context.needs, "desktop-prepare": { result: "failure" } } }), false);
  }
  for (const name of ["desktop-windows", "desktop-windows-package"]) {
    const body = job(ci, name);
    assert.match(body, /REASONIX_PACKAGE_REUSE_FRONTEND: "1"/);
    assert.match(body, /REASONIX_FRONTEND_PNPM_VERSION="\$\(pnpm --version\)"\n\s+export REASONIX_FRONTEND_PNPM_VERSION/);
    assert.match(body, /canary_artifact_name/);
  }
  assert.match(job(ci, "desktop-macos"), /REASONIX_FRONTEND_PNPM_VERSION="\$\(pnpm --version\)"\n\s+export REASONIX_FRONTEND_PNPM_VERSION/);
});

test("browser matrix preserves five entry points and fails closed through desktop-browser", () => {
  const groups = job(ci, "desktop-browser-group");
  assert.match(groups, /max-parallel: 2/);
  assert.match(groups, /fail-fast: false/);
  assert.match(groups, /group: \[app-settings-motion, transcript\]/);
  assert.doesNotMatch(groups, /group: \[app-settings, motion, transcript\]/);
  for (const command of ["test:app-browser", "test:settings-browser", "test:motion-browser", "test:transcript-browser", "test:transcript-reader-browser"])
    assert.equal(ci.match(new RegExp(`pnpm --dir frontend ${command}(?:\\s|$)`, "g"))?.length, 1, command);
  const summary = job(ci, "desktop-browser");
  assert.match(summary, /needs: \[changes, desktop-prepare, desktop-browser-group\]/);
  const script = shellStep(summary, "Verify desktop browser groups");
  const run = env => spawnSync("bash", ["-e", "-c", script], { env: { ...process.env, ...env } }).status;
  assert.equal(run({ CHANGES_RESULT: "success", SHOULD_RUN: "true", PREPARE_RESULT: "success", GROUP_RESULT: "success" }), 0);
  for (const result of ["failure", "cancelled", "skipped", ""])
    assert.notEqual(run({ CHANGES_RESULT: "success", SHOULD_RUN: "true", PREPARE_RESULT: "success", GROUP_RESULT: result }), 0);
  assert.equal(run({ CHANGES_RESULT: "success", SHOULD_RUN: "false", PREPARE_RESULT: "success", GROUP_RESULT: "skipped" }), 0);
});

test("Windows desktop Go runs once without verbose JSON cache overhead", () => {
  const windowsGo = job(ci, "desktop-windows-go");
  assert.equal(windowsGo.match(/go test \.\/\.\.\./g)?.length, 1);
  assert.doesNotMatch(windowsGo, /go test -json/);
  assert.doesNotMatch(windowsGo, /go-test-timing/);
  assert.doesNotMatch(windowsGo, /go test -run ['"]?\^\$/);
});
