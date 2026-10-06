import { describe, expect, it } from 'vitest';
import type { PrActionQueueItem } from 'gas-city-dashboard-shared/gc-supervisor';
import {
  actionOption,
  isVerifiedActionReceipt,
  receiptMatchesRequest,
  verifiedEvidenceForWork,
  workRecordForBead,
} from './prActionQueue';

const current = new Date(Date.now() + 60_000).toISOString();
const digest = 'a'.repeat(64);

function item(over: Partial<PrActionQueueItem> = {}): PrActionQueueItem {
  return {
    monitor: 'monitor-1',
    owner: 'owner',
    repo: 'repo',
    pull_request: 12,
    title: 'Fix it',
    base_ref_name: 'main',
    head_sha: 'b'.repeat(40),
    base_sha: 'c'.repeat(40),
    merge_state: 'CLEAN',
    is_draft: false,
    policy_version: 'policy-v1',
    observed_at: new Date().toISOString(),
    fresh_until: current,
    work_records: [
      {
        id: 'work-1',
        status: 'open',
        candidate_sha: 'b'.repeat(40),
        base_sha: 'c'.repeat(40),
        current_revision: true,
      },
    ],
    evidence_state: 'verified',
    attempt_evidence: [
      {
        store_ref: 'rig:repo',
        work_id: 'work-1',
        attempt_id: 'ae-immutable-1',
        candidate_sha: 'b'.repeat(40),
        base_sha: 'c'.repeat(40),
        diff_sha256: digest,
        diff_source: 'candidate_commit_delta',
        working_tree_status: 'dirty',
      },
    ],
    action_receipts: [],
    actions: [
      {
        action: 'prepare',
        available: false,
        reason: 'work already exists for this revision',
        requires_human_approval: false,
      },
      {
        action: 'queue_review',
        available: true,
        reason: 'verified server evidence is current',
        requires_human_approval: false,
      },
      {
        action: 'merge',
        available: true,
        reason: 'test fixture must never authorize this in Workbench',
        requires_human_approval: true,
      },
    ],
    ...over,
  };
}

function queue(itemValue: PrActionQueueItem, over: Record<string, unknown> = {}) {
  return {
    availability: 'ready',
    policy_state: 'ready',
    policy_version: 'policy-v1',
    observed_at: new Date().toISOString(),
    fresh_until: current,
    sources: [
      {
        monitor: 'monitor-1',
        owner: 'owner',
        repo: 'repo',
        rig: 'repo',
        state: 'ready',
      },
    ],
    items: [itemValue],
    ...over,
  };
}

describe('central PR queue projection', () => {
  it('joins exact work IDs and preserves immutable attempt IDs separately from session IDs', () => {
    const row = item();
    const work = workRecordForBead(row, 'work-1');
    const refs = verifiedEvidenceForWork(row, 'work-1');

    expect(work?.id).toBe('work-1');
    expect(refs).toHaveLength(1);
    expect(refs[0]?.attempt_id).toBe('ae-immutable-1');
    expect(refs[0]?.attempt_id).not.toBe('session-1');
  });

  it('does not use a stale work record or mismatched base/head evidence', () => {
    expect(workRecordForBead(item(), 'work-2')).toBeUndefined();
    expect(
      verifiedEvidenceForWork(
        item({
          attempt_evidence: [
            {
              ...item().attempt_evidence![0]!,
              candidate_sha: 'd'.repeat(40),
            },
          ],
        }),
        'work-1',
      ),
    ).toEqual([]);
  });

  it('returns every exact server reference without choosing a latest one', () => {
    const second = {
      ...item().attempt_evidence![0]!,
      attempt_id: 'ae-immutable-2',
      diff_sha256: 'e'.repeat(64),
    };
    const refs = verifiedEvidenceForWork(
      item({ attempt_evidence: [second, item().attempt_evidence![0]!] }),
      'work-1',
    );
    expect(refs.map((ref) => ref.attempt_id)).toEqual(['ae-immutable-2', 'ae-immutable-1']);
  });

  it('uses server action availability and refuses stale/unavailable sources', () => {
    const row = item();
    const ready = queue(row);
    expect(actionOption(ready, row, 'queue_review')).toMatchObject({
      available: true,
      reason: 'verified server evidence is current',
    });
    expect(actionOption(ready, row, 'merge').available).toBe(false);
    expect(
      actionOption(queue(row, { availability: 'unavailable' }), row, 'queue_review').available,
    ).toBe(false);
    const stale = item({ fresh_until: new Date(Date.now() - 1).toISOString() });
    expect(actionOption(queue(stale), stale, 'queue_review').available).toBe(false);
  });

  it('accepts only a verified receipt bound to the exact request and policy revision', () => {
    const receipt = {
      id: 'gc-pr-action-1',
      action: 'queue_review',
      status: 'verified',
      outcome: 'review_queued',
      idempotency_key: 'wb-pr-action-123',
      monitor: 'monitor-1',
      owner: 'owner',
      repo: 'repo',
      pull_request: 12,
      work_id: 'work-1',
      attempt_id: 'ae-immutable-1',
      head_sha: 'b'.repeat(40),
      base_sha: 'c'.repeat(40),
      policy_version: 'policy-v1',
      actor_key_id: 'worker-key',
      created_at: new Date().toISOString(),
    };
    const request = {
      action: 'queue_review' as const,
      monitor: 'monitor-1',
      owner: 'owner',
      repo: 'repo',
      pull_request: 12,
      work_id: 'work-1',
      attempt_id: 'ae-immutable-1',
      head_sha: 'b'.repeat(40),
      base_sha: 'c'.repeat(40),
      policy_version: 'policy-v1',
    };

    expect(receiptMatchesRequest(receipt, request, 'wb-pr-action-123')).toBe(true);
    expect(isVerifiedActionReceipt(receipt, request, 'wb-pr-action-123')).toBe(true);
    expect(
      isVerifiedActionReceipt(
        receipt,
        { ...request, head_sha: 'd'.repeat(40) },
        'wb-pr-action-123',
      ),
    ).toBe(false);
    expect(
      isVerifiedActionReceipt({ ...receipt, status: 'unknown' }, request, 'wb-pr-action-123'),
    ).toBe(false);
  });
});
