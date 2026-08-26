// Run: tsx src/__tests__/remote-topic-active.test.ts

import { topicIsActive } from "../components/ProjectTree";
import type { ProjectNode } from "../lib/types";

const remoteTopic: ProjectNode = {
  key: "remote-session-box-s1",
  kind: "topic",
  label: "First chat",
  root: "~/app",
  topicId: "box\u0000~/app\u0000s1",
  sessionPath: "/remote/sessions/s1.jsonl",
  remoteSession: { hostId: "box", workspace: "~/app", name: "s1", path: "/remote/sessions/s1.jsonl", title: "First chat" },
};

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
