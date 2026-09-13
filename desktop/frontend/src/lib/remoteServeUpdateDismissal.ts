// Per-serve dismissal memory for the remote Serve update banner. A dismissal
// is keyed by host, workspace, and the serve version it was shown for, so a
// replaced serve (new version) makes the banner eligible again.
const STORAGE_KEY = "remote.serveUpdate.dismissed";
const MAX_ENTRIES = 200;

function keyFor(hostId: string, workspace: string, serveVersion: string): string {
  return `${hostId}|${workspace}|${serveVersion}`;
}

function readSet(): Set<string> {
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY);
    const list = raw ? (JSON.parse(raw) as unknown) : [];
    return new Set(Array.isArray(list) ? list.map(String) : []);
  } catch {
    return new Set();
  }
}

export function isRemoteServeUpdateDismissed(hostId: string, workspace: string, serveVersion: string): boolean {
  return readSet().has(keyFor(hostId, workspace, serveVersion));
}

export function dismissRemoteServeUpdate(hostId: string, workspace: string, serveVersion: string): void {
  const set = readSet();
  set.add(keyFor(hostId, workspace, serveVersion));
  try {
    window.localStorage.setItem(STORAGE_KEY, JSON.stringify([...set].slice(-MAX_ENTRIES)));
  } catch {
    // Storage unavailable (private mode): the dismissal lasts this session only.
  }
}
