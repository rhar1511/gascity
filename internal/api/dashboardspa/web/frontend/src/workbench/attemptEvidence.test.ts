import { describe, expect, it } from 'vitest';
import type {
  AttemptEvidenceRead,
  Evidence,
  RequestReceipt,
} from 'gas-city-dashboard-shared/gc-supervisor';
import type { ExecutionAttempt } from '../lib/workbenchAttempts';
import {
  exactRequestReceiptMatchesWorkbenchEvidence,
  exactWorkbenchEvidenceMatches,
} from './attemptEvidence';

const attempt: ExecutionAttempt = {
  sessionId: 'session-7',
  sessionName: 'worker-7',
  workDir: '/work/7',
  state: 'completed',
  active: false,
  startedAt: '2026-01-01T00:00:00Z',
  lastActive: '2026-01-01T01:00:00Z',
  executionGeneration: 7,
};

function evidence(): Evidence {
  return {
    attempt_id: 'ae-exact-7',
    identity: {
      kind: 'workbench',
      owner_bead_id: 'work-1',
      execution_bead_id: 'attempt-row-7',
      session_id: 'session-7',
      session_generation: '7',
      claim_generation: 'claim-7',
    },
    store_ref: 'rig:pilot',
    permission_scope: {
      store_ref: 'rig:pilot',
      work_id: 'work-1',
      repository_root: '/repo',
      workspace_root: '/repo/worktree',
    },
    base_sha: 'base-7',
    candidate_sha: 'candidate-7',
    diff: { status: 'available', source: 'candidate_commit_delta', sha256: 'digest-7' },
    working_tree_status: 'clean',
  } as Evidence;
}

function exactRead(): AttemptEvidenceRead {
  return {
    ...evidence(),
    related_records: {
      actions: { status: 'missing', reason: 'no_attributed_pr_action_records', records: [] },
      acknowledgements: { status: 'unavailable', reason: 'no_verified_attribution' },
    },
  } as AttemptEvidenceRead;
}

function requestReceipt(overrides: Partial<RequestReceipt> = {}): RequestReceipt {
  const row = evidence();
  return {
    accepted_at: '2026-01-01T01:00:00Z',
    acknowledged_at: '2026-01-01T01:00:03Z',
    attempt: {
      attempt_id: row.attempt_id,
      identity: row.identity,
      store_ref: row.store_ref ?? '',
      work_revision: '9',
    },
    delivery: 'accepted',
    delivery_attempted_at: '2026-01-01T01:00:01Z',
    effect: 'unverified',
    generation: 7,
    message_digest: 'sha256:message',
    provider_result_at: '2026-01-01T01:00:02Z',
    request_id: 'request-7',
    session_id: 'session-7',
    ...overrides,
  };
}

describe('exact Workbench archive identity', () => {
  it('accepts the exact listed attempt and its full execution identity', () => {
    expect(exactWorkbenchEvidenceMatches(exactRead(), evidence(), 'work-1', attempt)).toBe(true);
  });

  it.each([
    ['attempt id', (read: AttemptEvidenceRead) => ({ ...read, attempt_id: 'ae-other' })],
    [
      'execution row',
      (read: AttemptEvidenceRead) => ({
        ...read,
        identity: { ...read.identity, execution_bead_id: 'attempt-row-other' },
      }),
    ],
    [
      'claim generation',
      (read: AttemptEvidenceRead) => ({
        ...read,
        identity: { ...read.identity, claim_generation: 'claim-other' },
      }),
    ],
    ['candidate revision', (read: AttemptEvidenceRead) => ({ ...read, candidate_sha: 'other' })],
    [
      'owner work',
      (read: AttemptEvidenceRead) => ({
        ...read,
        identity: { ...read.identity, owner_bead_id: 'other' },
      }),
    ],
    [
      'session',
      (read: AttemptEvidenceRead) => ({
        ...read,
        identity: { ...read.identity, session_id: 'other' },
      }),
    ],
    [
      'session generation',
      (read: AttemptEvidenceRead) => ({
        ...read,
        identity: { ...read.identity, session_generation: '8' },
      }),
    ],
  ])('rejects a mismatched %s', (_label, alter) => {
    expect(exactWorkbenchEvidenceMatches(alter(exactRead()), evidence(), 'work-1', attempt)).toBe(
      false,
    );
  });

  it('accepts only request receipts bound to this immutable attempt, store, and execution identity', () => {
    expect(exactRequestReceiptMatchesWorkbenchEvidence(requestReceipt(), evidence())).toBe(true);
    expect(
      exactRequestReceiptMatchesWorkbenchEvidence(
        requestReceipt({
          attempt: { ...requestReceipt().attempt!, work_revision: '9223372036854775807' },
        }),
        evidence(),
      ),
    ).toBe(true);
    expect(
      exactRequestReceiptMatchesWorkbenchEvidence(
        requestReceipt({
          attempt: { ...requestReceipt().attempt!, attempt_id: 'ae-other' },
        }),
        evidence(),
      ),
    ).toBe(false);
    expect(
      exactRequestReceiptMatchesWorkbenchEvidence(
        requestReceipt({
          attempt: { ...requestReceipt().attempt!, store_ref: 'city:other' },
        }),
        evidence(),
      ),
    ).toBe(false);
    expect(
      exactRequestReceiptMatchesWorkbenchEvidence(
        requestReceipt({
          attempt: { ...requestReceipt().attempt!, work_revision: '09' },
        }),
        evidence(),
      ),
    ).toBe(false);
    expect(
      exactRequestReceiptMatchesWorkbenchEvidence(
        requestReceipt({
          attempt: { ...requestReceipt().attempt!, work_revision: '9223372036854775808' },
        }),
        evidence(),
      ),
    ).toBe(false);
    const exact = requestReceipt();
    const exactBinding = exact.attempt;
    if (!exactBinding) throw new Error('request receipt fixture has no attempt binding');
    expect(
      exactRequestReceiptMatchesWorkbenchEvidence(
        {
          ...exact,
          attempt: {
            ...exactBinding,
            identity: { ...exactBinding.identity, execution_bead_id: 'attempt-row-other' },
          },
        },
        evidence(),
      ),
    ).toBe(false);
    expect(
      exactRequestReceiptMatchesWorkbenchEvidence(
        {
          ...exact,
          attempt: {
            ...exactBinding,
            identity: { ...exactBinding.identity, session_generation: '8' },
          },
        },
        evidence(),
      ),
    ).toBe(false);
    expect(
      exactRequestReceiptMatchesWorkbenchEvidence(
        {
          ...exact,
          attempt: {
            ...exactBinding,
            identity: { ...exactBinding.identity, claim_generation: 'claim-other' },
          },
        },
        evidence(),
      ),
    ).toBe(false);
    expect(
      exactRequestReceiptMatchesWorkbenchEvidence(
        { ...exact, generation: Number.MAX_SAFE_INTEGER + 1 },
        evidence(),
      ),
    ).toBe(false);
  });
});
