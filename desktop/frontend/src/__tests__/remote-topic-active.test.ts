// Run: tsx src/__tests__/remote-topic-active.test.ts

import { topicIsActive } from "../components/ProjectTree";
import { mergeRemoteSessionsIntoTree } from "../components/ProjectTreeRemoteGroups";
import type { Translator } from "../lib/i18n";
import type { ProjectNode } from "../lib/types";

const remoteProject: ProjectNode = {
  key: "project_remote_box_app", kind: "project", label: "app",
  root: "remote-project:box:~/app", remote: { hostId: "box", workspace: "~/app" },
};
const remoteTopic = mergeRemoteSessionsIntoTree([remoteProject], {
  "box\u0000~/app": [{ name: "s1", title: "First chat", turns: 1, path: "/remote/sessions/s1.jsonl" }],
}, ((key: string) => key) as Translator)[0]?.children?.[0];
if (!remoteTopic) throw new Error("remote topic was not synthesized");

function eq(actual: unknown, expected: unknown, label: string) {
  if (actual !== expected) throw new Error(`${label}: expected ${String(expected)}, got ${String(actual)}`);
  process.stdout.write(`  PASS  ${label}\n`);
}

eq(topicIsActive(remoteTopic, "project", "~/app", remoteTopic.topicId, remoteTopic.sessionPath), true,
  "matching remote identity is active");
eq(topicIsActive(remoteTopic, "project", "~/app", "box\u0000~/app\u0000s2", "/remote/sessions/s2.jsonl"), false,
  "another remote identity stays inactive");
eq(topicIsActive(remoteTopic, "project", "~/app", "", remoteTopic.sessionPath), true,
  "session path bridges the pre-hydration identity gap");
