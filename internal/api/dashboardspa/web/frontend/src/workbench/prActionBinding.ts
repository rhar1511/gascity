import type { SupervisorBead } from '../supervisor/beadReads';
import type { ExecutionAttempt } from '../lib/workbenchAttempts';
import type { PRActionOption, PRActionQueue, PRActionQueueItem } from '../supervisor/prActions';

export interface BoundPRActions {
  item: PRActionQueueItem;
  prepare: PRActionOption | null;
  queueReview: PRActionOption | null;
  workId: string | null;
  attemptId: string | null;
}

export type PRActionBinding =
  | { state: 'unavailable'; reason: string }
  | { state: 'ready'; actions: BoundPRActions };

// The execute endpoint currently accepts Git's 40-character object IDs only.
// Fail closed for SHA-256 repositories instead of rendering an action the API
// will reject at request validation.
const COMMIT_SHA = /^[0-9a-f]{40}$/i;

/**
 * Bind the selected active attempt to one fresh, authoritative server queue
 * item. A branch name, stale item, partial queue, or client-inferred policy
 * must never enable an action.
 */
export function bindPRActions(
  queue: PRActionQueue,
  bead: SupervisorBead,
  attempt: ExecutionAttempt,
  now = Date.now(),
): PRActionBinding {
  if (!attempt.active) return unavailable('No active attempt is available.');
  if (queue.availability !== 'ready' || queue.policy_state !== 'ready') {
    return unavailable(
      queue.policy_detail || `Gas City PR queue is ${queue.availability || 'unavailable'}.`,
    );
  }
  if (!isFresh(queue.fresh_until, now)) {
    return unavailable('Gas City PR verdict expired. Refresh the queue before acting.');
  }

  const metadata = (bead as { metadata?: Record<string, string> }).metadata ?? {};
  const revision = (metadata['gc.work_commit'] ?? '').trim();
  if (!COMMIT_SHA.test(revision)) {
    return unavailable('This attempt has no verified commit revision.');
  }

  const matchingItems = (queue.items ?? []).filter((item) => item.head_sha === revision);
  if (matchingItems.length !== 1) {
    return unavailable(
      matchingItems.length === 0
        ? 'No monitored pull request matches this attempt’s exact commit.'
        : 'More than one monitored pull request matches this commit; refusing an ambiguous action.',
    );
  }

  const item = matchingItems[0];
  if (!item || !isFresh(item.fresh_until, now)) {
    return unavailable('The pull-request verdict expired. Refresh the queue before acting.');
  }
  if (item.policy_version.length === 0 || item.policy_version !== queue.policy_version) {
    return unavailable('The pull-request policy revision does not match the queue snapshot.');
  }

  const source = (queue.sources ?? []).find(
    (candidate) =>
      candidate.monitor === item.monitor &&
      candidate.owner === item.owner &&
      candidate.repo === item.repo,
  );
  if (source?.state !== 'ready') {
    return unavailable(source?.detail || 'The pull-request source is unavailable.');
  }

  const work = (item.work_records ?? []).find(
    (record) =>
      record.id === bead.id &&
      record.current_revision &&
      record.candidate_sha === item.head_sha &&
      record.base_sha === item.base_sha,
  );
  const evidence = (item.attempt_evidence ?? []).find(
    (reference) =>
      reference.work_id === bead.id &&
      reference.attempt_id === attempt.sessionId &&
      reference.base_sha === item.base_sha &&
      reference.candidate_sha === item.head_sha,
  );
  const options = item.actions ?? [];
  const prepare = options.find((option) => option.action === 'prepare') ?? null;
  const queueReviewOption = options.find((option) => option.action === 'queue_review') ?? null;
  const queueReview =
    work !== undefined && evidence !== undefined
      ? queueReviewOption
      : queueReviewOption === null
        ? null
        : { ...queueReviewOption, available: false, reason: 'No exact work and attempt evidence.' };

  return {
    state: 'ready',
    actions: {
      item,
      prepare,
      queueReview,
      workId: work?.id ?? null,
      attemptId: evidence?.attempt_id ?? null,
    },
  };
}

export function actionOptionIsAvailable(option: PRActionOption | null): boolean {
  return option?.available === true && option.requires_human_approval === false;
}

function isFresh(value: string, now: number): boolean {
  const expiresAt = Date.parse(value);
  return Number.isFinite(expiresAt) && expiresAt > now;
}

function unavailable(reason: string): PRActionBinding {
  return { state: 'unavailable', reason };
}
