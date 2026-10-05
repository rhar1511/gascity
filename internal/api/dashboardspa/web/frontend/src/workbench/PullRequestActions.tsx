import { Button } from '../components/Button';
import type {
  PRActionKind,
  PRActionOption,
  PRActionQueueItem,
  PRActionResult,
} from '../supervisor/prActions';
import { actionOptionIsAvailable } from './prActionBinding';

export function PullRequestActions({
  item,
  prepare,
  queueReview,
  busy,
  refreshing,
  actionsDisabled,
  error,
  receipt,
  onAction,
  onRefresh,
}: {
  item: PRActionQueueItem;
  prepare: PRActionOption | null;
  queueReview: PRActionOption | null;
  busy: PRActionKind | null;
  refreshing: boolean;
  actionsDisabled: boolean;
  error: string | null;
  receipt: PRActionResult | null;
  onAction: (action: PRActionKind) => void;
  onRefresh: () => void;
}) {
  const offered = prepare !== null || queueReview !== null;
  const pullRequestURL = safePullRequestURL(item.url);
  const serverReceipt = [...(item.action_receipts ?? [])]
    .filter((candidate) => candidate.action === 'prepare' || candidate.action === 'queue_review')
    .sort((left, right) => right.created_at.localeCompare(left.created_at))[0];
  const visibleReceipt = receipt ?? serverReceipt ?? null;
  return (
    <section aria-label="Pull request actions" className="mt-3 space-y-2">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <p className="text-body text-fg">
          PR #{item.pull_request}: <span className="font-medium">{item.title}</span>
        </p>
        {pullRequestURL && (
          <a
            href={pullRequestURL}
            target="_blank"
            rel="noreferrer"
            className="text-label text-fg-muted underline decoration-rule hover:text-fg focus-mark"
          >
            Open pull request
          </a>
        )}
      </div>
      <p className="text-label text-fg-faint">
        Server verdict · {item.merge_state || 'merge state unknown'} · head{' '}
        <code>{item.head_sha.slice(0, 12)}</code> · {item.evidence_state} attempt evidence
        {item.is_draft ? ' · draft PR' : ''}
      </p>
      {offered ? (
        <div className="flex flex-wrap items-start gap-2">
          {prepare && (
            <ActionButton
              label="Prepare PR"
              action="prepare"
              option={prepare}
              busy={busy}
              refreshing={refreshing}
              actionsDisabled={actionsDisabled}
              onAction={onAction}
            />
          )}
          {queueReview && (
            <ActionButton
              label="Queue PR for review"
              action="queue_review"
              option={queueReview}
              busy={busy}
              refreshing={refreshing}
              actionsDisabled={actionsDisabled}
              onAction={onAction}
            />
          )}
        </div>
      ) : (
        <p role="status" className="text-body text-fg-muted">
          Gas City currently offers no prepare or review-queue action for this revision.
        </p>
      )}
      {prepare && !prepare.available && prepare.reason && (
        <p className="text-label text-fg-muted">Prepare: {prepare.reason}</p>
      )}
      {queueReview && !queueReview.available && queueReview.reason && (
        <p className="text-label text-fg-muted">Queue review: {queueReview.reason}</p>
      )}
      {busy && (
        <p role="status" className="text-label text-fg-muted">
          Sending {busy === 'prepare' ? 'prepare' : 'review queue'} request to Gas City…
        </p>
      )}
      {error && (
        <div role="alert" className="space-y-1 text-body text-accent">
          <p>{error}</p>
          <p className="text-label">
            If the outcome is uncertain, refresh before retrying; a retry keeps the same idempotency
            key.
          </p>
        </div>
      )}
      {visibleReceipt && (
        <p role="status" className="text-label text-fg-muted">
          Last server receipt: {visibleReceipt.action} · {visibleReceipt.status}
          {visibleReceipt.outcome ? ` · ${visibleReceipt.outcome}` : ''}
          {visibleReceipt.detail ? ` · ${visibleReceipt.detail}` : ''}
        </p>
      )}
      <Button size="sm" tone="quiet" onClick={onRefresh} disabled={busy !== null || refreshing}>
        Refresh PR verdict
      </Button>
    </section>
  );
}

function safePullRequestURL(value: string | undefined): string | null {
  if (!value) return null;
  try {
    const url = new URL(value);
    if (
      (url.protocol !== 'https:' && url.protocol !== 'http:') ||
      !url.hostname ||
      url.username ||
      url.password
    ) {
      return null;
    }
    return url.href;
  } catch {
    return null;
  }
}

function ActionButton({
  label,
  action,
  option,
  busy,
  refreshing,
  actionsDisabled,
  onAction,
}: {
  label: string;
  action: PRActionKind;
  option: PRActionOption;
  busy: PRActionKind | null;
  refreshing: boolean;
  actionsDisabled: boolean;
  onAction: (action: PRActionKind) => void;
}) {
  const available = actionOptionIsAvailable(option);
  return (
    <Button
      size="sm"
      onClick={() => onAction(action)}
      disabled={!available || busy !== null || refreshing || actionsDisabled}
      title={option.reason || undefined}
    >
      {busy === action ? 'Sending…' : label}
    </Button>
  );
}
