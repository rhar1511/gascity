import { describe, expect, it } from 'vitest';
import type { WorkbenchPRActionBody } from '../supervisor/client';
import {
  getOrCreatePRActionIntent,
  prActionIntentStorageKey,
  readPRActionIntents,
} from './prActionIntent';

class MemoryStorage {
  private readonly values = new Map<string, string>();

  get length(): number {
    return this.values.size;
  }

  key(index: number): string | null {
    return [...this.values.keys()][index] ?? null;
  }

  getItem(key: string): string | null {
    return this.values.get(key) ?? null;
  }

  setItem(key: string, value: string): void {
    this.values.set(key, value);
  }
}

const request: WorkbenchPRActionBody = {
  action: 'queue_review',
  monitor: 'monitor-1',
  owner: 'owner',
  repo: 'repo',
  pull_request: 12,
  work_id: 'work-1',
  attempt_id: 'ae-immutable-1',
  head_sha: 'a'.repeat(40),
  base_sha: 'b'.repeat(40),
  policy_version: 'policy-v1',
};

describe('getOrCreatePRActionIntent', () => {
  it('persists the exact request and reuses its key after an ambiguous retry', () => {
    const storage = new MemoryStorage();
    let generated = 0;
    const createKey = () => `wb-pr-action-${++generated}`;

    const first = getOrCreatePRActionIntent(storage, 'city-1', request, createKey);
    const retry = getOrCreatePRActionIntent(storage, 'city-1', { ...request }, createKey);

    expect(first.request).toEqual(request);
    expect(retry).toEqual(first);
    expect(generated).toBe(1);
  });

  it('creates a distinct identity when the exact revision or attempt changes', () => {
    const storage = new MemoryStorage();
    let generated = 0;
    const createKey = () => `wb-pr-action-${++generated}`;

    const first = getOrCreatePRActionIntent(storage, 'city-1', request, createKey);
    const nextAttempt = getOrCreatePRActionIntent(
      storage,
      'city-1',
      { ...request, attempt_id: 'ae-immutable-2' },
      createKey,
    );
    const nextRevision = getOrCreatePRActionIntent(
      storage,
      'city-1',
      { ...request, head_sha: 'c'.repeat(40) },
      createKey,
    );

    expect(
      new Set([first.idempotencyKey, nextAttempt.idempotencyKey, nextRevision.idempotencyKey]).size,
    ).toBe(3);
  });

  it('fails closed when stored intent contents do not match their storage identity', () => {
    const storage = new MemoryStorage();
    storage.setItem(
      prActionIntentStorageKey('city-1', request),
      JSON.stringify({
        city: 'city-1',
        request: { ...request, head_sha: 'c'.repeat(40) },
        idempotencyKey: 'wb-pr-action-1',
      }),
    );
    expect(() =>
      getOrCreatePRActionIntent(storage, 'city-1', request, () => 'wb-pr-action-2'),
    ).toThrow('Stored PR action intent does not match the exact request.');
  });

  it('restores only valid exact requests for the active city after reload', () => {
    const storage = new MemoryStorage();
    const currentCity = getOrCreatePRActionIntent(
      storage,
      'city-1',
      request,
      () => 'wb-pr-key-city1',
    );
    getOrCreatePRActionIntent(storage, 'city-2', request, () => 'wb-pr-key-city2');

    expect(readPRActionIntents(storage, 'city-1')).toEqual([currentCity]);
  });
});
