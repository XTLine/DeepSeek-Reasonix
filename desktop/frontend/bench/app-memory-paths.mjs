import { classifyPaths } from "../../../scripts/ci-paths.mjs";

export function memoryAffected(files) {
  return classifyPaths(files).flags.memory;
}
