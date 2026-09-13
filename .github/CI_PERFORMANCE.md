# CI and release execution

[中文](CI_PERFORMANCE.zh-CN.md)

## Desktop PR checks

`scripts/ci-paths.mjs` is the shared path classifier for normal and memory CI.
It distinguishes frontend, Go, generated protocol, Electron, native and
packaging inputs. Explicit documentation such as `desktop/AGENTS.md` skips
build and soak work, while Markdown inside the frontend remains a build input.
Unknown paths and unavailable diffs fail closed. Pull requests use merge-base
diffs; pushes use `before..sha`; normal `main-v2` pushes keep the complete
qualification matrix after the existing release-notes-only exception.

`desktop-prepare` regenerates the desktop host contract (failing on drift) and
produces the required `electron/stable` and `electron/canary` frontend variants
once on Linux. Each artifact carries a versioned manifest with checkout,
workflow attempt, variant, build inputs, toolchain and every `dist` file hash.
Linux, macOS and Windows consumers verify it before compilation or packaging.
Explicit reuse fails on a missing, stale or damaged manifest and never falls
back to a hidden rebuild. Static frontend files are portable; dependencies,
native modules and Electron binaries are not shared. Build-input verification
streams every committed blob through one Git batch process instead of starting
one process per file; the version-one digest remains byte-for-byte compatible.

The protected `lint` job aggregates `lint-code` and, when selected, the
complete `desktop-frontend` result. Motion unit tests remain in that frontend
plan and run once. `desktop-browser-group` runs application/settings/motion and
Transcript as two groups with `max-parallel: 2`; the `desktop-browser` summary
rejects failed, cancelled or unexpected skips. Go-only changes retain protocol
and native validation without launching browser or memory work.

`node desktop/frontend/scripts/run-ci-tests.mjs --list` prints the unit test
plan. It expands the existing dedicated scripts and lifecycle hooks, discovers
new tests, and schedules each TypeScript suite once with its original loader.
Unsupported script syntax and conflicting explicit invocations fail closed.
CI runs two isolated processes at a time; the history performance benchmark
runs alone after them. Local dedicated `pnpm test:*` commands remain available.

## Timing reports

The CI and memory workflow summaries report stage execution without queue time,
workflow wall time, recorded job queue time and the sum of runner execution.
Frontend builds, dependency and browser installation, each browser group and
each memory shard are listed separately. These measurements describe a single
run; comparisons should use the same candidate and report the median and range
of three runs so runner variance is visible. The Windows Desktop Go step keeps
native non-verbose output because Go's JSON mode made Windows spend several
minutes finalizing verbose test-cache output; the central report records its
step execution time from the Actions API without wrapping the test process.

## Memory screening

Protocol v2 still requires three independent processes, each completing
128 full, 128 windowed, 128 safety and 512 mixed round trips, with the same
checkpoints, five heap snapshots, GC, frame settling and screening thresholds.
Only the explicit mock memory-soak URL removes the fixture's artificial
1.5-second hydration latency. Hydration still crosses an asynchronous timer
task. Default browser and native geometry fixtures retain the delayed path.

The pointer rests outside topic rows and the warmed baseline follows a complete
round trip after layout switching, avoiding samples of temporary menu state.
Reports declare the protocol, and aggregation rejects older protocols.
`timings.json` records host-side counts, total time and maximum time for
navigation, frame settling, GC and heap capture/analysis. Short profiling runs
are diagnostic only and cannot pass the complete screening gate. A green gate
still does not prove offline heap-retainer attribution.

## Signed release artifacts

The successful stable SignPath preflight hands its full native matrix to the
desktop publisher. The publisher revalidates authorization, candidate identity,
signing contract and cache/docs guards, then verifies and publishes the same
signed bytes without rebuilding or signing them again. CLI and npm publication
still wait for the entire preflight; no public surface starts early.

Each platform bundle binds file sizes and SHA-256 hashes to candidate/control
SHAs, version, tag, channel, signing fingerprint, run, invocation and producer
attempt. Missing/extra platforms, conflicting identities, symlinks, duplicate
filenames or modified bytes stop publication. These transport hashes supplement
the existing Authenticode/minisign checks; they are not signature verification.

A failed-job retry may reuse earlier successful platforms from the same run
and invocation. A rebuilt platform replaces only its own fully verified bundle.
New workflow runs prepare a new artifact set. Standalone recovery still builds
and validates its own full matrix. pnpm dependencies are cached by lockfile;
signed release artifacts are transported as artifacts, never dependency caches.
