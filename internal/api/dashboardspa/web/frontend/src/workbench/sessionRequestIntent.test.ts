import { describe, expect, it } from 'vitest';
import {
  createSessionRequestIntent,
  readSessionRequestIntents,
  saveSessionRequestIntent,
  type SessionRequestStorage,
} from './sessionRequestIntent';

class MemoryStorage implements SessionRequestStorage {
  private readonly values = new Map<string, string>();

  get length(): number {
    return this.values.size;
  }

  getItem(key: string): string | null {
    return this.values.get(key) ?? null;
  }

  setItem(key: string, value: string): void {
    this.values.set(key, value);
  }

  key(index: number): string | null {
    return [...this.values.keys()][index] ?? null;
  }
}

describe('session request intent', () => {
  it('persists exact generation/message identity before submit and restores it for retry', () => {
    const storage = new MemoryStorage();
    const intent = createSessionRequestIntent(
      storage,
      'city-1',
      'session-1',
      7,
      '  please continue  ',
      () => 'wb-chat-request-1',
    );
    const restored = readSessionRequestIntents(storage, 'city-1', 'session-1');

    expect(intent).toMatchObject({
      requestId: 'wb-chat-request-1',
      generation: 7,
      message: 'please continue',
    });
    expect(restored).toEqual([intent]);
  });

  it('rejects missing, unsafe, zero, or negative generations before persistence', () => {
    for (const generation of [undefined, Number.MAX_SAFE_INTEGER + 1, 0, -1, 1.5]) {
      const storage = new MemoryStorage();
      expect(() =>
        createSessionRequestIntent(
          storage,
          'city-1',
          'session-1',
          generation as number,
          'hello',
          () => 'wb-chat-request-2',
        ),
      ).toThrow('execution generation is unavailable');
      expect(storage.length).toBe(0);
    }
  });

  it('persists receipts without changing request identity', () => {
    const storage = new MemoryStorage();
    const intent = createSessionRequestIntent(
      storage,
      'city-1',
      'session-1',
      7,
      'hello',
      () => 'wb-chat-request-3',
    );
    const withReceipt = {
      ...intent,
      receipt: {
        request_id: intent.requestId,
        session_id: intent.sessionId,
        generation: intent.generation,
        message_digest: 'digest',
        accepted_at: '2026-09-27T00:00:00Z',
        delivery: 'accepted',
        acknowledged_at: '2026-09-27T00:01:00Z',
        effect: 'unverified',
      },
    };

    saveSessionRequestIntent(storage, withReceipt);
    expect(readSessionRequestIntents(storage, 'city-1', 'session-1')).toEqual([withReceipt]);
  });
});
