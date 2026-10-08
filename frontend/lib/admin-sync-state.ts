/**
 * Persists Sync Now state across tab switches, SPA navigation, and browser
 * tabs open to the same admin page at once.
 *
 * The record lives in one shared localStorage slot, so every write carries a
 * requestId and clearSyncState only removes the slot when it still holds
 * that exact requestId. Without this, a tab that mounted (or last checked)
 * before a different tab started a sync would still believe nothing was
 * running, and its own settle/timeout logic could overwrite or clear the
 * other tab's genuinely in-progress record purely by timing.
 *
 * onSyncStateChange complements this: the browser never fires `storage` in
 * the tab that made the change, so it is how a tab that mounted before a
 * sync started learns about it (and about it finishing) in real time,
 * disabling its own button instead of only finding out on its next remount.
 *
 * Hard reloads abort the synchronous request, so stale state is cleared
 * after the timeout to avoid locking out admins.
 */

const STORAGE_KEY = "adminSyncState";
export const SYNC_STALE_TIMEOUT_MS = 60_000;

export type AdminSyncStatus = "in_progress" | "completed" | "failed";

export interface AdminSyncState {
  status: AdminSyncStatus;
  startedAt: number;
  updatedAt: number;
  /** Identifies the write that produced this record, so a clear can be scoped to it. */
  requestId: string;
}

function createRequestId(): string {
  return `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`;
}

function parseSyncState(raw: string | null): AdminSyncState | null {
  if (!raw) return null;
  try {
    const parsed = JSON.parse(raw);
    if (
      !parsed ||
      typeof parsed.startedAt !== "number" ||
      typeof parsed.updatedAt !== "number" ||
      typeof parsed.requestId !== "string" ||
      (parsed.status !== "in_progress" && parsed.status !== "completed" && parsed.status !== "failed")
    ) {
      return null;
    }
    return parsed as AdminSyncState;
  } catch {
    return null;
  }
}

export function readSyncState(): AdminSyncState | null {
  try {
    return parseSyncState(localStorage.getItem(STORAGE_KEY));
  } catch {
    // Corrupt or inaccessible storage is treated the same as "no state".
    return null;
  }
}

/** Writes a new record and returns its requestId, so the caller can later
 * scope a clearSyncState call to exactly this write. */
export function writeSyncState(status: AdminSyncStatus, startedAt: number = Date.now()): string {
  const requestId = createRequestId();
  try {
    const state: AdminSyncState = { status, startedAt, updatedAt: Date.now(), requestId };
    localStorage.setItem(STORAGE_KEY, JSON.stringify(state));
  } catch {
    // Persistence is best effort
  }
  return requestId;
}

/**
 * Removes the persisted record, but only if it is still the exact record
 * identified by requestId. If a different tab has since written a newer
 * record (a different requestId), this is a no-op: clearing here would
 * otherwise erase a sync that is still genuinely in progress elsewhere.
 */
export function clearSyncState(requestId: string): void {
  try {
    const current = parseSyncState(localStorage.getItem(STORAGE_KEY));
    if (current && current.requestId !== requestId) {
      return;
    }
    localStorage.removeItem(STORAGE_KEY);
  } catch {
    // See writeSyncState.
  }
}

export function isStale(state: AdminSyncState, now: number = Date.now()): boolean {
  // A non-finite (NaN/Infinity) or future startedAt can never age past the
  // timeout through subtraction alone, which would lock the Sync Now button
  // forever instead of recovering. Treat both as stale so recovery still runs.
  if (!Number.isFinite(state.startedAt) || state.startedAt > now) {
    return true;
  }
  return now - state.startedAt > SYNC_STALE_TIMEOUT_MS;
}

/**
 * Subscribes to the persisted record changing from another tab (the browser
 * never fires `storage` in the tab that made the change). Returns an
 * unsubscribe function. Calls back with the new state, or null if the
 * record was removed or a full localStorage.clear() ran.
 */
export function onSyncStateChange(callback: (state: AdminSyncState | null) => void): () => void {
  if (typeof window === "undefined") {
    return () => {};
  }
  const handler = (event: StorageEvent) => {
    if (event.key !== null && event.key !== STORAGE_KEY) return;
    callback(parseSyncState(event.newValue));
  };
  window.addEventListener("storage", handler);
  return () => window.removeEventListener("storage", handler);
}

/**
 * Tracks the active request so remounted components can reattach to it.
 * A hard reload clears this module state.
 */
let activeSyncPromise: Promise<unknown> | null = null;
let activeRequestId: string | null = null;

export function getActiveSyncPromise(): Promise<unknown> | null {
  return activeSyncPromise;
}

export function setActiveSyncPromise(promise: Promise<unknown> | null): void {
  activeSyncPromise = promise;
}

/** The requestId of the write behind getActiveSyncPromise(), so a remounted
 * component that reattaches to it can still scope its eventual clear. */
export function getActiveRequestId(): string | null {
  return activeRequestId;
}

export function setActiveRequestId(requestId: string | null): void {
  activeRequestId = requestId;
}
