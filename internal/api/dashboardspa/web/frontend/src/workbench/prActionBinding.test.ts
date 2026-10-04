import { describe, expect, it } from 'vitest';
import type { ExecutionAttempt } from '../lib/workbenchAttempts';
import type { SupervisorBead } from '../supervisor/beadReads';
import type { PRActionQueue } from '../supervisor/prActions';
import { actionOptionIsAvailable, bindPRActions } from './prActionBinding';

const now = Date.parse('2026-09-29T00:00:00Z');
const headSHA = 'a'.repeat(40);
const baseSHA = 'b'.repeat(40);
const activeAttempt: ExecutionAttempt = {
  sessionId: 'session-1',
  sessionName: 'worker-1',
  workDir: '/work/one',
  state: 'active',
  active: true,
  startedAt: '2026-09-28T00:00:00Z',
  lastActive: '2026-09-29T00:00:00Z',
  executionGeneration: 7,
};
const bead = {
  id: 'work-1',
  metadata: { 'gc.work_commit': headSHA },
} as unknown as SupervisorBead;

function queue(overrides: Partial<PRActionQueue> = {}): PRActionQueue {
  return {
    availability: 'ready',
    policy_state: 'ready',
    policy_version: 'policy-v1',
    observed_at: new Date(now).toISOString(),
    fresh_until: new Date(now + 30_000).toISOString(),
    sources: [{ monitor: 'main', owner: 'acme', repo: 'widget', rig: 'app', state: 'ready' }],
    items: [
      {
        monitor: 'main',
        owner: 'acme',
        repo: 'widget',
        pull_request: 42,
        title: 'Add a feature',
        base_ref_name: 'main',
        head_ref_name: 'work-1',
        head_sha: headSHA,
        base_sha: baseSHA,
        merge_state: 'clean',
        is_draft: true,
        policy_version: 'policy-v1',
        observed_at: new Date(now).toISOString(),
        fresh_until: new Date(now + 30_000).toISOString(),
        evidence_state: 'verified',
        work_records: [
          {
            id: 'work-1',
            status: 'open',
            candidate_sha: headSHA,
            base_sha: baseSHA,
            current_revision: true,
          },
        ],
        attempt_evidence: [
          {
            store_ref: 'rig:app',
            work_id: 'work-1',
            attempt_id: 'session-1',
            base_sha: baseSHA,
            candidate_sha: headSHA,
            diff_sha256: 'c'.repeat(64),
            diff_source: 'candidate_commit_delta',
            working_tree_status: 'clean',
          },
        ],
        action_receipts: [],
        actions: [
          { action: 'prepare', available: true, requires_human_approval: false, reason: '' },
          { action: 'queue_review', available: true, requires_human_approval: false, reason: '' },
          { action: 'merge', available: true, requires_human_approval: true, reason: 'approval' },
        ],
      },
    ],
    ...overrides,
  };
}

describe('bindPRActions', () => {
  it('offers only server-authorized prepare/review actions bound to exact attempt evidence', () => {
    const result = bindPRActions(queue(), bead, activeAttempt, now, {
      attempt_id: 'session-1',
      store_ref: 'rig:app',
      base_sha: baseSHA,
      candidate_sha: headSHA,
      identity: {
        kind: 'workbench',
        owner_bead_id: bead.id,
        execution_bead_id: 'execution-1',
        session_id: activeAttempt.sessionId,
        session_generation: '7',
        claim_generation: 'claim-7',
      },
    });
    expect(result.state).toBe('ready');
    if (result.state !== 'ready') return;
    expect(result.actions.item.pull_request).toBe(42);
    expect(result.actions.prepare?.available).toBe(true);
    expect(result.actions.queueReview?.available).toBe(true);
    expect(result.actions.workId).toBe('work-1');
    expect(result.actions.attemptId).toBe('session-1');
    expect(result.actions.item.actions?.some((option) => option.action === 'merge')).toBe(true);
    expect(
      actionOptionIsAvailable(
        result.actions.item.actions?.find((option) => option.action === 'merge') ?? null,
      ),
    ).toBe(false);
  });

  it('fails closed when policy, queue freshness, commit, or evidence is missing', () => {
    expect(
      bindPRActions(queue({ availability: 'partial' }), bead, activeAttempt, now),
    ).toMatchObject({
      state: 'unavailable',
    });
    expect(
      bindPRActions(
        queue({ fresh_until: new Date(now - 1).toISOString() }),
        bead,
        activeAttempt,
        now,
      ),
    ).toMatchObject({ state: 'unavailable' });
    expect(
      bindPRActions(queue(), { ...bead, metadata: {} } as SupervisorBead, activeAttempt, now),
    ).toMatchObject({ state: 'unavailable' });
    const missingEvidence = queue();
    const item = missingEvidence.items?.[0];
    if (item) item.attempt_evidence = [];
    const result = bindPRActions(missingEvidence, bead, activeAttempt, now);
    expect(result.state).toBe('ready');
    if (result.state === 'ready') {
      expect(result.actions.queueReview?.available).toBe(false);
      expect(result.actions.queueReview?.reason).toMatch(/exact work and attempt evidence/i);
    }
  });

  it('does not turn a session ID into an immutable attempt ID without archive attribution', () => {
    const result = bindPRActions(queue(), bead, activeAttempt, now);
    expect(result.state).toBe('ready');
    if (result.state !== 'ready') throw new Error('expected a fresh queue fixture');
    expect(result.actions.prepare?.available).toBe(true);
    expect(result.actions.queueReview?.available).toBe(false);
    expect(result.actions.attemptId).toBeNull();
  });

  it('uses the archived attempt ID rather than the session ID', () => {
    const snapshot = queue();
    snapshot.items![0]!.attempt_evidence![0]!.attempt_id = 'immutable-attempt-9';
    const result = bindPRActions(snapshot, bead, activeAttempt, now, {
      attempt_id: 'immutable-attempt-9',
      store_ref: 'rig:app',
      base_sha: baseSHA,
      candidate_sha: headSHA,
      identity: {
        kind: 'workbench',
        owner_bead_id: bead.id,
        execution_bead_id: 'execution-1',
        session_id: activeAttempt.sessionId,
        session_generation: '7',
        claim_generation: 'claim-7',
      },
    });
    expect(result.state).toBe('ready');
    if (result.state !== 'ready') throw new Error('expected a fresh queue fixture');
    expect(result.actions.queueReview?.available).toBe(true);
    expect(result.actions.attemptId).toBe('immutable-attempt-9');
  });

  it.each([
    { owner_bead_id: 'foreign-work' },
    { session_id: 'foreign-session' },
    { session_generation: '8' },
    { execution_bead_id: '' },
    { claim_generation: '' },
  ])('refuses archive attribution with a mismatched incarnation: %j', (changed) => {
    const result = bindPRActions(queue(), bead, activeAttempt, now, {
      attempt_id: 'session-1',
      store_ref: 'rig:app',
      base_sha: baseSHA,
      candidate_sha: headSHA,
      identity: {
        kind: 'workbench',
        owner_bead_id: bead.id,
        execution_bead_id: 'execution-1',
        session_id: activeAttempt.sessionId,
        session_generation: '7',
        claim_generation: 'claim-7',
        ...changed,
      },
    });
    expect(result.state).toBe('ready');
    if (result.state !== 'ready') throw new Error('expected a fresh queue fixture');
    expect(result.actions.prepare?.available).toBe(true);
    expect(result.actions.queueReview?.available).toBe(false);
    expect(result.actions.attemptId).toBeNull();
  });

  it('fails closed on ambiguous pull requests for the same revision', () => {
    const snapshot = queue();
    const item = snapshot.items?.[0];
    if (item) snapshot.items = [item, { ...item, owner: 'another-owner' }];
    expect(bindPRActions(snapshot, bead, activeAttempt, now)).toMatchObject({
      state: 'unavailable',
    });
  });

  it('refuses archive attribution from a different physical store', () => {
    const result = bindPRActions(queue(), bead, activeAttempt, now, {
      attempt_id: 'session-1',
      store_ref: 'rig:foreign',
      base_sha: baseSHA,
      candidate_sha: headSHA,
      identity: {
        kind: 'workbench',
        owner_bead_id: bead.id,
        execution_bead_id: 'execution-1',
        session_id: activeAttempt.sessionId,
        session_generation: '7',
        claim_generation: 'claim-7',
      },
    });
    expect(result.state).toBe('ready');
    if (result.state !== 'ready') throw new Error('expected a fresh queue fixture');
    expect(result.actions.prepare?.available).toBe(true);
    expect(result.actions.queueReview?.available).toBe(false);
    expect(result.actions.attemptId).toBeNull();
  });

  it('fails closed when the server execute endpoint cannot accept the commit SHA length', () => {
    const sha256Head = 'd'.repeat(64);
    const sha256Bead = {
      ...bead,
      metadata: { 'gc.work_commit': sha256Head },
    } as SupervisorBead;
    const snapshot = queue();
    const item = snapshot.items?.[0];
    if (item) {
      item.head_sha = sha256Head;
      const workRecord = item.work_records?.[0];
      if (workRecord) workRecord.candidate_sha = sha256Head;
      const attemptEvidence = item.attempt_evidence?.[0];
      if (attemptEvidence) attemptEvidence.candidate_sha = sha256Head;
    }

    expect(bindPRActions(snapshot, sha256Bead, activeAttempt, now)).toMatchObject({
      state: 'unavailable',
      reason: 'This attempt has no verified commit revision.',
    });
  });
});
