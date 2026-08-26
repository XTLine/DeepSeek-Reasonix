import type { RemoteSessionView } from "./types";

const REMOTE_SESSIONS_CACHE_PREFIX = "projectTree:remoteSessions:";
const REMOTE_SESSION_CACHE_VERSION = 1;
export const REMOTE_SESSION_CACHE_TTL_MS = 5 * 60 * 1000;

interface RemoteSessionCacheEnvelope {
  version: typeof REMOTE_SESSION_CACHE_VERSION;
  savedAt: number;
  rows: RemoteSessionView[];
}

function cacheKey(groupKey: string): string {
  return REMOTE_SESSIONS_CACHE_PREFIX + groupKey;
}

function validRows(value: unknown): value is RemoteSessionView[] {
  return Array.isArray(value) && value.every((row) => {
    if (!row || typeof row !== "object") return false;
    const candidate = row as Partial<RemoteSessionView>;
    return typeof candidate.name === "string"
      && typeof candidate.title === "string"
      && typeof candidate.turns === "number"
      && Number.isFinite(candidate.turns)
      && (candidate.path === undefined || typeof candidate.path === "string")
      && (candidate.current === undefined || typeof candidate.current === "boolean")
      && (candidate.running === undefined || typeof candidate.running === "boolean")
      && (candidate.lastActivityAt === undefined
        || typeof candidate.lastActivityAt === "number" && Number.isFinite(candidate.lastActivityAt))
      && (candidate.pinned === undefined || typeof candidate.pinned === "boolean");
  });
}

export function removeRemoteSessionCache(groupKey: string): void {
  try {
    if (typeof localStorage !== "undefined") localStorage.removeItem(cacheKey(groupKey));
  } catch {
    // localStorage is an optional optimistic-paint cache.
  }
}

export function saveRemoteSessionCache(groupKey: string, rows: RemoteSessionView[], now = Date.now()): void {
  try {
    if (typeof localStorage === "undefined") return;
    const envelope: RemoteSessionCacheEnvelope = {
      version: REMOTE_SESSION_CACHE_VERSION,
      savedAt: now,
      rows,
    };
    localStorage.setItem(cacheKey(groupKey), JSON.stringify(envelope));
  } catch {
    // localStorage can be unavailable or full; live state remains authoritative.
  }
}

export function loadRemoteSessionCache(groupKey: string, now = Date.now()): RemoteSessionView[] {
  try {
    if (typeof localStorage === "undefined") return [];
    const raw = localStorage.getItem(cacheKey(groupKey));
    if (!raw) return [];
    const parsed = JSON.parse(raw) as Partial<RemoteSessionCacheEnvelope> | null;
    const fresh = parsed?.version === REMOTE_SESSION_CACHE_VERSION
      && typeof parsed.savedAt === "number"
      && parsed.savedAt <= now
      && now - parsed.savedAt <= REMOTE_SESSION_CACHE_TTL_MS
      && validRows(parsed.rows);
    if (!fresh) {
      removeRemoteSessionCache(groupKey);
      return [];
    }
    return parsed.rows!;
  } catch {
    removeRemoteSessionCache(groupKey);
    return [];
  }
}
