import { readFileSync } from "node:fs";
import { pathToFileURL } from "node:url";

// The soak runs the production frontend against browser mocks. These source
// trees cannot enter that build; native/backend behavior has separate gates.
// Paths that cannot reach the mock-hosted frontend bundle the soak measures:
// Go modules, CLI/SDK/site sources, docs and notes, repository tooling and
// workflows (app-memory.yml is claimed above), and the Electron shell,
// packaging and installer trees the browser-mocked soak never loads. The
// desktop workspace manifests (desktop/package.json, desktop/pnpm-lock.yaml)
// stay unknown on purpose: they resolve the frontend's dependencies.
const knownIndependent = /^(?:internal\/|cmd\/|sdk\/|site\/|release-notes\/|docs\/|scripts\/|tools\/|workers\/|\.github\/|go\.(?:mod|sum)$|Makefile$|\.golangci[^/]*$|desktop\/(?:[^/]+\.go$|go\.(?:mod|sum)$|cmd\/|internal\/|electron\/|packaging\/|build\/))/;
export function memoryAffected(files) {
  return files.some(file => {
    if (file.startsWith("desktop/frontend/") || file === ".github/workflows/app-memory.yml") return true;
    if (knownIndependent.test(file) || /^[^/]+\.md$/.test(file)) return false;
    return true; // Unknown dependency or workflow changes fail closed.
  });
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const files = readFileSync(process.argv[2], "utf8").split("\0").filter(Boolean);
  console.log(`run=${memoryAffected(files)}`);
}
