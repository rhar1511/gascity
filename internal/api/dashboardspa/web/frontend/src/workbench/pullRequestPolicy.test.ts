import { describe, expect, it } from 'vitest';
import { pullRequestPolicy, type PullRequestContext } from './pullRequestPolicy';
import type { ExecutionAttempt } from '../lib/workbenchAttempts';
import type { SupervisorBead } from '../supervisor/beadReads';

const attempt: ExecutionAttempt = {
  sessionId: 's-1',
  sessionName: 'worker-1',
  workDir: '/wt/s-1',
  state: 'active',
  active: true,
  startedAt: '2026-01-01T00:00:00Z',
  lastActive: '2026-01-01T00:00:00Z',
};

function ctx(over: Partial<PullRequestContext> = {}): PullRequestContext {
  const revision = 'a'.repeat(40);
  return {
    bead: { id: 'gascity-1' } as SupervisorBead,
    attempt,
    attemptRevision: revision,
    policyRevision: revision,
    queueAvailable: true,
    policyRejected: false,
    hasConflict: false,
    ...over,
  };
}

describe('pullRequestPolicy', () => {
  it('offers prepare and queue for a live attempt at the exact revision', () => {
    expect(pullRequestPolicy(ctx())).toEqual({ allowed: ['prepare', 'queue'], blocked: null });
  });

  it('blocks with no active attempt', () => {
    expect(pullRequestPolicy(ctx({ attempt: null })).blocked).toBe('no_active_attempt');
  });

  it('blocks for a completed attempt even when a queue is available', () => {
    expect(pullRequestPolicy(ctx({ attempt: { ...attempt, active: false } })).blocked).toBe(
      'no_active_attempt',
    );
  });

  it('blocks on policy rejection', () => {
    expect(pullRequestPolicy(ctx({ policyRejected: true })).blocked).toBe('policy_rejected');
  });

  it('blocks on conflict', () => {
    expect(pullRequestPolicy(ctx({ hasConflict: true })).blocked).toBe('conflict');
  });

  it('blocks when the queue is unavailable', () => {
    expect(pullRequestPolicy(ctx({ queueAvailable: false })).blocked).toBe('queue_unavailable');
  });

  it('blocks on a stale revision', () => {
    expect(pullRequestPolicy(ctx({ policyRevision: 'b'.repeat(40) })).blocked).toBe(
      'stale_revision',
    );
  });

  it('does not treat a mutable branch name as an exact attempt revision', () => {
    expect(
      pullRequestPolicy(ctx({ attemptRevision: 'feature/workbench', policyRevision: 'feature/workbench' }))
        .blocked,
    ).toBe('missing_revision');
  });

  it('does not accept a branch-name policy verdict', () => {
    expect(pullRequestPolicy(ctx({ policyRevision: 'feature/workbench' })).blocked).toBe(
      'missing_policy_revision',
    );
  });

  it('does not fall back to a mutable work branch when attempt commit is absent', () => {
    expect(
      pullRequestPolicy(
        ctx({
          bead: {
            id: 'gascity-1',
            metadata: { 'gc.work_branch': 'feature/workbench' },
          } as unknown as SupervisorBead,
          attemptRevision: null,
        }),
      ).blocked,
    ).toBe('missing_revision');
  });

  it('blocks when the attempt revision is missing, even if the queue reports available', () => {
    expect(pullRequestPolicy(ctx({ attemptRevision: null })).blocked).toBe('missing_revision');
  });

  it('blocks when Gas City did not provide a policy revision', () => {
    expect(pullRequestPolicy(ctx({ policyRevision: null })).blocked).toBe(
      'missing_policy_revision',
    );
  });

  it('never offers a merge or bypass action', () => {
    const { allowed } = pullRequestPolicy(ctx());
    expect(allowed).not.toContain('merge');
    expect(allowed).not.toContain('force-push');
  });
});
