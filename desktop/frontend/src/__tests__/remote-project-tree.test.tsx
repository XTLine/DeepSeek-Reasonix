// Run: tsx src/__tests__/remote-project-tree.test.tsx
// Source-contract test: remote sessions ride the LOCAL tree renderer and
// menu templates — synthesized topic nodes, remote-routed actions, and
// structure-aligned project menus.

import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

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

console.log("\nRemote project tree wiring");
const here = dirname(fileURLToPath(import.meta.url));
const source = readFileSync(resolve(here, "../components/ProjectTree.tsx"), "utf8");
const appSource = readFileSync(resolve(here, "../App.tsx"), "utf8");
const bridgeSource = readFileSync(resolve(here, "../lib/bridge.ts"), "utf8");

ok(
  /const treeWithRemoteSessions = useMemo\(\(\) => \{[\s\S]*?kind: "topic" as const,[\s\S]*?remoteSession: \{ hostId: node\.remote!\.hostId/.test(source),
  "remote sessions join the tree data as synthetic topic nodes",
);
ok(
  /treeWithRemoteSessions\s*\.map\(filterNode\)/.test(source),
  "the synthesized rows flow through the local filter/arrange/pinned pipeline",
);
ok(
  /if \(remote && remote\.name\) \{[\s\S]*?\{ sessionName: remote\.name \}[\s\S]*?\} else if \(remote\) \{[\s\S]*?\{ focus: true \}/.test(source),
  "row clicks resume named sessions and focus the blank one",
);
ok(
  /const remote = remoteFromTopicId\(topicId\);[\s\S]*?await app\.RenameRemoteProjectSession/.test(source),
  "row rename routes to RenameRemoteProjectSession",
);
ok(
  /await app\.SetRemoteSessionPinned\(remote\.hostId, remote\.workspace, remote\.name, pinned\)/.test(source),
  "row pin routes to SetRemoteSessionPinned",
);
ok(
  /await app\.DeleteRemoteProjectSession\(remote\.hostId, remote\.workspace, remote\.name\)/.test(source),
  "row archive routes to DeleteRemoteProjectSession",
);
ok(
  /key: "new-session",[\s\S]*?key: "rename",[\s\S]*?key: "reveal",[\s\S]*?key: "copy-path",[\s\S]*?key: "remove",/.test(source),
  "the remote classic menu mirrors the local structure (new/rename/reveal/copy/remove)",
);
ok(
  /label: t\("projectTree\.revealRemoteWorkspace"\)/.test(source),
  "reveal maps to the remote file manager entry",
);
ok(
  /root\.startsWith\("remote-project:"\)/.test(source) && /await app\.SetRemoteProjectTitle/.test(source),
  "project rename routes to SetRemoteProjectTitle",
);
ok(
  /disabled: aiRenamingTopic !== null \|\| Boolean\(node\.remoteSession\)/.test(source),
  "AI rename stays disabled for remote rows (the serve owns generated titles)",
);
ok(
  /!\(node\.remoteSession && !node\.remoteSession\.name\) && \(/.test(source),
  "hover actions hide on the pending blank row",
);
ok(
  /await app\.OpenRemoteProjectTab\(ref\.hostId, ref\.workspace, \{ newSession: true \}\);\s*bumpRemoteSessionsRef\.current\(\);/.test(source),
  "a new-session open refreshes the session listing",
);
ok(
  !/remoteBlankGroups|markRemoteBlank|handleRemoteNewSession/.test(source),
  "the frontend blank state machine is gone — the listing is the only source",
);
ok(
  /void openRemoteProject\(node\.remote(!)?, \{ newSession: true \}\);/.test(source),
  "+ routes straight to the ensure-open; blank reuse is backend-decided",
);

ok(
  /project-tree__remote-badge--\$\{remoteStatuses\[node\.remote\.hostId\]\?\.state/.test(source),
  "the group row badge reflects the live host status",
);
ok(
  /onSend=\{remoteSurfaceActive \? remoteSend : handleSend\}/.test(appSource),
  "the shared Composer sends through the remote session on remote tabs",
);
ok(
  /mockRemoteProjects: RemoteProjectView\[\] = \[\s*\/\/ Demo preseed[\s\S]*?hostId: "demo"/.test(bridgeSource),
  "the mock preseeds a demo remote project",
);
ok(
  /const remoteDemoTabId = `remote-mock-demo-~\/app`\.replace\(/.test(bridgeSource) && /id: remoteDemoTabId/.test(bridgeSource),
  "the mock preseeds a demo remote tab under the computed ensure-reuse id",
);

process.stdout.write(`\n${passed} passed, ${failed} failed\n`);
if (failed > 0) process.exit(1);
