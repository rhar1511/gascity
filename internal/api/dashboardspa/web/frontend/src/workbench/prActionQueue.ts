import type {
  PrActionQueue,
  PrActionQueueItem,
  PrActionWorkRecord,
  PrActionAttemptReference,
  PrActionResult,
} from 'gas-city-dashboard-shared/gc-supervisor';

export type WorkbenchPRAction = 'prepare' | 'queue_review' | 'merge';

export interface ServerActionOption {
  available: boolean;
  reason: string;
}

/** Returns only the selected bead's exact current-revision work record. */
export function workRecordForBead(
  item: PrActionQueueItem,
  beadId: string,
): PrActionWorkRecord | undefined {
  return item.work_records?.find(
    (work) =>
      work.id === beadId &&
      work.current_revision &&
      work.candidate_sha === item.head_sha &&
      work.base_sha === item.base_sha,
  );
}

/**
 * Filters immutable server references for one exact current work revision.
 * It intentionally returns every matching reference in server order; callers
 * must not silently turn that order into a "latest attempt" choice.
 */
export function verifiedEvidenceForWork(
  item: PrActionQueueItem,
  workId: string,
): PrActionAttemptReference[] {
  if (item.evidence_state !== 'verified' || !workRecordForBead(item, workId)) return [];
  return (item.attempt_evidence ?? []).filter(
    (ref) =>
      ref.work_id === workId &&
      ref.base_sha === item.base_sha &&
      ref.candidate_sha === item.head_sha &&
      ref.diff_source === 'candidate_commit_delta' &&
      /^[a-f0-9]{64}$/i.test(ref.diff_sha256),
  );
}

/**
 * Projects the server option without deriving PR policy in the browser.
 * Queue/source freshness only determines whether that server verdict is still
 * usable. Workbench never exposes merge, even if a future DTO reports it.
 */
export function actionOption(
  queue: PrActionQueue,
  item: PrActionQueueItem,
  action: WorkbenchPRAction,
  nowMs = Date.now(),
): ServerActionOption {
  if (action === 'merge') {
    return { available: false, reason: 'Merge is unavailable in Workbench.' };
  }
  if (
    (queue.availability !== 'ready' && queue.availability !== 'partial') ||
    queue.policy_state !== 'ready'
  ) {
    return {
      available: false,
      reason: queue.policy_detail || 'The central PR queue is unavailable.',
    };
  }
  const source = queue.sources?.find(
    (candidate) =>
      candidate.monitor === item.monitor &&
      candidate.owner === item.owner &&
      candidate.repo === item.repo,
  );
  if (source?.state !== 'ready') {
    return { available: false, reason: source?.detail || 'This PR source is unavailable.' };
  }
  if (!isFresh(queue.fresh_until, nowMs) || !isFresh(item.fresh_until, nowMs)) {
    return { available: false, reason: 'The central PR verdict is stale. Refresh the queue.' };
  }
  const option = item.actions?.find((candidate) => candidate.action === action);
  if (option?.available !== true) {
    return {
      available: false,
      reason: option?.reason || 'The central policy does not offer this action.',
    };
  }
  return { available: true, reason: option.reason };
}

/** Matches a durable result to the entire submitted request identity. */
export function receiptMatchesRequest(
  receipt: PrActionResult,
  request: {
    action: 'prepare' | 'queue_review';
    monitor: string;
    owner: string;
    repo: string;
    pull_request: number;
    head_sha: string;
    base_sha: string;
    policy_version: string;
    work_id?: string;
    attempt_id?: string;
  },
  idempotencyKey: string,
): boolean {
  return (
    receipt.idempotency_key === idempotencyKey &&
    receipt.action === request.action &&
    receipt.monitor === request.monitor &&
    receipt.owner === request.owner &&
    receipt.repo === request.repo &&
    receipt.pull_request === request.pull_request &&
    receipt.head_sha === request.head_sha &&
    receipt.base_sha === request.base_sha &&
    receipt.policy_version === request.policy_version &&
    (request.work_id === undefined || receipt.work_id === request.work_id) &&
    receipt.attempt_id === request.attempt_id
  );
}

export function isVerifiedActionReceipt(
  receipt: PrActionResult,
  request: {
    action: 'prepare' | 'queue_review';
    monitor: string;
    owner: string;
    repo: string;
    pull_request: number;
    head_sha: string;
    base_sha: string;
    policy_version: string;
    work_id?: string;
    attempt_id?: string;
  },
  idempotencyKey: string,
): boolean {
  if (!receiptMatchesRequest(receipt, request, idempotencyKey) || receipt.status !== 'verified') {
    return false;
  }
  if (request.action === 'queue_review') return receipt.outcome === 'review_queued';
  return receipt.outcome === 'work_prepared' && (receipt.work_id?.length ?? 0) > 0;
}

function isFresh(raw: string, nowMs: number): boolean {
  const expiresAt = Date.parse(raw);
  return Number.isFinite(expiresAt) && nowMs < expiresAt;
}
