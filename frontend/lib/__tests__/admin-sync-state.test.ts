import { describe, it, expect, beforeEach } from 'vitest';
import {
  isStale,
  readSyncState,
  writeSyncState,
  clearSyncState,
  onSyncStateChange,
  SYNC_STALE_TIMEOUT_MS,
  type AdminSyncState,
} from '../admin-sync-state';

function stateAt(startedAt: number): AdminSyncState {
  return { status: 'in_progress', startedAt, updatedAt: startedAt, requestId: 'r' };
}

describe('isStale', () => {
  const now = 1_700_000_000_000;

  it('is not stale just under the timeout', () => {
    expect(isStale(stateAt(now - (SYNC_STALE_TIMEOUT_MS - 1)), now)).toBe(false);
  });

  it('is stale just past the timeout', () => {
    expect(isStale(stateAt(now - (SYNC_STALE_TIMEOUT_MS + 1)), now)).toBe(true);
  });

  it('treats a future startedAt as stale rather than never-expiring', () => {
    expect(isStale(stateAt(now + 60_000), now)).toBe(true);
  });

  it('treats NaN startedAt as stale', () => {
    expect(isStale(stateAt(NaN), now)).toBe(true);
  });

  it('treats Infinity startedAt as stale', () => {
    expect(isStale(stateAt(Infinity), now)).toBe(true);
  });

  it('treats -Infinity startedAt as stale', () => {
    expect(isStale(stateAt(-Infinity), now)).toBe(true);
  });
});

describe('clearSyncState (multi-tab ownership)', () => {
  beforeEach(() => {
    localStorage.clear();
  });

  it('clears the record when the requestId still matches', () => {
    const requestId = writeSyncState('in_progress');
    clearSyncState(requestId);
    expect(readSyncState()).toBeNull();
  });

  it('does not clear a newer record a different tab has since written', () => {
    // Tab A writes, then Tab B (a different tab/request) overwrites the slot
    // before Tab A settles -- Tab A's clear must not erase Tab B's record.
    const tabARequestId = writeSyncState('in_progress');
    const tabBRequestId = writeSyncState('in_progress');
    expect(tabBRequestId).not.toBe(tabARequestId);

    clearSyncState(tabARequestId);

    const state = readSyncState();
    expect(state).not.toBeNull();
    expect(state?.requestId).toBe(tabBRequestId);
  });

  it('is a no-op when nothing is persisted', () => {
    expect(() => clearSyncState('some-request-id')).not.toThrow();
    expect(readSyncState()).toBeNull();
  });
});

describe('onSyncStateChange', () => {
  beforeEach(() => {
    localStorage.clear();
  });

  it('notifies with the new state when the record is written', () => {
    const seen: (AdminSyncState | null)[] = [];
    const unsubscribe = onSyncStateChange((state) => seen.push(state));

    window.dispatchEvent(
      new StorageEvent('storage', {
        key: 'adminSyncState',
        newValue: JSON.stringify({ status: 'in_progress', startedAt: Date.now(), updatedAt: Date.now(), requestId: 'r1' }),
      })
    );

    expect(seen).toHaveLength(1);
    expect(seen[0]?.status).toBe('in_progress');
    unsubscribe();
  });

  it('notifies with null when the record is removed', () => {
    const seen: (AdminSyncState | null)[] = [];
    const unsubscribe = onSyncStateChange((state) => seen.push(state));

    window.dispatchEvent(new StorageEvent('storage', { key: 'adminSyncState', newValue: null }));

    expect(seen).toEqual([null]);
    unsubscribe();
  });

  it('ignores changes to unrelated storage keys', () => {
    const seen: (AdminSyncState | null)[] = [];
    const unsubscribe = onSyncStateChange((state) => seen.push(state));

    window.dispatchEvent(new StorageEvent('storage', { key: 'someOtherKey', newValue: 'x' }));

    expect(seen).toHaveLength(0);
    unsubscribe();
  });

  it('stops notifying after unsubscribe', () => {
    const seen: (AdminSyncState | null)[] = [];
    const unsubscribe = onSyncStateChange((state) => seen.push(state));
    unsubscribe();

    window.dispatchEvent(new StorageEvent('storage', { key: 'adminSyncState', newValue: null }));

    expect(seen).toHaveLength(0);
  });
});
