import { describe, expect, it } from 'vitest';
import {
  WAYFINDER_REVIEW_EVENT_PREFIX,
  createWayfinderReviewRecord,
  readWayfinderReviewHistory,
  type WayfinderReviewDraft,
} from './wayfinderReviewLog';

describe('Wayfinder review record metadata', () => {
  it('reads valid records newest first and reports malformed or miskeyed history', () => {
    const history = readWayfinderReviewHistory({
      [`${WAYFINDER_REVIEW_EVENT_PREFIX}newer`]:
        '{"version":1,"id":"newer","kind":"approval","actor":"ricky","recorded_at":"2026-09-27T02:00:00.000Z","scope":"Prototype B","explicitly_confirmed":true}',
      [`${WAYFINDER_REVIEW_EVENT_PREFIX}older`]:
        '{"version":1,"id":"older","kind":"answer","actor":"operator","recorded_at":"2026-09-27T01:00:00.000Z","prompt":"Which?","answer":"B"}',
      [`${WAYFINDER_REVIEW_EVENT_PREFIX}broken`]: '{not json',
      [`${WAYFINDER_REVIEW_EVENT_PREFIX}wrong-key`]:
        '{"version":1,"id":"other-id","kind":"annotation","actor":"ricky","recorded_at":"2026-09-27T00:00:00.000Z","text":"Note"}',
      'gc.prototype_url': 'http://localhost:3000/prototype',
    });

    expect(history.records.map((record) => record.id)).toEqual(['newer', 'older']);
    expect(history.records[0]).toMatchObject({ kind: 'approval', explicitly_confirmed: true });
    expect(history.unreadableCount).toBe(2);
  });

  it('trims and timestamps the operator’s explicit approval scope', () => {
    const record = createWayfinderReviewRecord(
      ' ricky ',
      {
        kind: 'approval',
        scope: '  Prototype B for GC-12  ',
        explicitly_confirmed: true,
      },
      { id: 'approval-1', recordedAt: '2026-09-27T03:00:00.000Z' },
    );

    expect(record).toEqual({
      version: 1,
      id: 'approval-1',
      kind: 'approval',
      actor: 'ricky',
      recorded_at: '2026-09-27T03:00:00.000Z',
      scope: 'Prototype B for GC-12',
      explicitly_confirmed: true,
    });
  });

  it('rejects blank answers and approval drafts without explicit confirmation', () => {
    expect(() =>
      createWayfinderReviewRecord(
        'operator',
        { kind: 'answer', prompt: '  ', answer: 'B' },
        {
          id: 'answer-1',
        },
      ),
    ).toThrow(/prompt is required/i);

    const unconfirmed = {
      kind: 'approval',
      scope: 'Prototype B',
      explicitly_confirmed: false,
    } as unknown as WayfinderReviewDraft;
    expect(() =>
      createWayfinderReviewRecord('operator', unconfirmed, { id: 'approval-2' }),
    ).toThrow(/explicit confirmation is required/i);
  });
});
