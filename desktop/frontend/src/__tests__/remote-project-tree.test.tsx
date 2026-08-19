// Run: tsx src/__tests__/remote-project-tree.test.tsx
// Source-contract test: the remote project group's tree behavior — session
// rows, the + action, the remote context menu, the connection badge, and
// the local-action guards — is wired exactly once and in the remote shape.

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

ok(
  /void openRemoteProject\(node\.remote, \{ sessionName: row\.name \}\)/.test(source),
  "session rows open by sessionName",
);
ok(
  /remoteRows\.map\(\(row\) =>/.test(source),
  "remote group children render from the fetched session list",
);
ok(
  /app\.RemoteProjectSessions\(hostId, workspace\)/.test(source),
  "sessions are fetched through the bridge",
);
ok(
  /state !== "connected" && state !== "degraded"[\s\S]{0,200}continue;/.test(source),
  "session fetch waits for a connected host",
);
ok(
  /if \(node\.remote\) \{\s*void openRemoteProject\(node\.remote, \{ newSession: true \}\);\s*return;\s*\}\s*void handleCreateTopic/.test(source),
  "the + action on a remote group opens a remote session, never a local topic",
);
ok(
  /key: "remote-new-session"[\s\S]*?key: "remote-open-window"[\s\S]*?key: "remote-stop-server"[\s\S]*?key: "remote-unpin"/.test(source),
  "the remote context menu offers new session, remote window, stop server, and unpin",
);
ok(
  /items=\{node\.remote \? remoteProjectMenuItems :/.test(source),
  "remote groups swap out the local project menu",
);
ok(
  /app\.OpenRemoteWorkspace\(node\.remote!\.hostId, node\.remote!\.workspace\)/.test(source),
  "remote window action passes host and workspace",
);
ok(
  /app\.RemoveRemoteProject\(node\.remote!\.hostId, node\.remote!\.workspace\)/.test(source) && /void refresh\(\);/.test(source),
  "unpin removes the registration and refreshes the tree",
);
ok(
  /project-tree__remote-badge--\$\{remoteStatuses\[node\.remote\.hostId\]\?\.state/.test(source),
  "the group row badge reflects the live host status",
);

const appSource = readFileSync(resolve(here, "../App.tsx"), "utf8");
ok(
  /onSend=\{remoteSurfaceActive \? remoteSend : handleSend\}/.test(appSource),
  "the shared Composer sends through the remote session on remote tabs",
);
ok(
  /running=\{remoteSurfaceActive \? remoteSession\.running :/.test(appSource),
  "the shared Composer's running flag comes from the remote session",
);
ok(
  /onCancel=\{remoteSurfaceActive \? remoteCancel : cancel\}/.test(appSource),
  "the shared Composer cancels the remote turn on remote tabs",
);

process.stdout.write(`\n${passed} passed, ${failed} failed\n`);
if (failed > 0) process.exit(1);
