import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { Dispatch, SetStateAction } from 'react';
import { GC_EVENT_PREFIX } from 'gas-city-dashboard-shared';
import type {
  PrActionQueue,
  PrActionQueueItem,
  PrActionResult,
} from 'gas-city-dashboard-shared/gc-supervisor';
import { Button } from '../components/Button';
import { useGcEventRefresh } from '../hooks/useGcEvents';
import {
  SupervisorApiError,
  supervisorApi,
  type WorkbenchPRActionBody,
} from '../supervisor/client';
import {
  actionOption,
  isVerifiedActionReceipt,
  receiptMatchesRequest,
  verifiedEvidenceForWork,
  workRecordForBead,
} from './prActionQueue';
import {
  getOrCreatePRActionIntent,
  prActionIntentStorageKey,
  readPRActionIntents,
} from './prActionIntent';

interface ActionState {
  city: string;
  idempotencyKey: string;
  request: WorkbenchPRActionBody;
  result?: PrActionResult;
  message?: string | undefined;
  kind: 'verified' | 'rejected' | 'unknown' | 'stale';
}

export function CentralPRActionPanel({
  beadId,
  cityName,
}: {
  beadId: string;
  cityName: string | null;
}) {
  const latestQueueRequest = useRef(0);
  const [queue, setQueue] = useState<PrActionQueue | null>(null);
  const [queueError, setQueueError] = useState<string | null>(null);
  const [intentError, setIntentError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [nowMs, setNowMs] = useState(Date.now());
  const [selectedAttempts, setSelectedAttempts] = useState<Record<string, string>>({});
  const [actionStates, setActionStates] = useState<Record<string, ActionState>>({});
  const [busyKey, setBusyKey] = useState<string | null>(null);

  const refreshQueue = useCallback(async (): Promise<PrActionQueue | null> => {
    if (cityName === null) {
      setQueueError('No active city is selected.');
      return null;
    }
    const requestId = ++latestQueueRequest.current;
    setLoading(true);
    try {
      const fresh = await supervisorApi().prActionQueue(cityName);
      if (requestId !== latestQueueRequest.current) return null;
      setQueue(fresh);
      setQueueError(null);
      setNowMs(Date.now());
      try {
        const intents = readPRActionIntents(window.localStorage, cityName);
        for (const intent of intents) {
          reconcileFromQueue(
            fresh,
            intent.request,
            intent.idempotencyKey,
            prActionIntentStorageKey(cityName, intent.request),
            cityName,
            setActionStates,
          );
        }
        setIntentError(null);
      } catch (cause) {
        setIntentError(
          cause instanceof Error ? cause.message : 'Saved PR action intents are unreadable.',
        );
      }
      return fresh;
    } catch (cause) {
      if (requestId !== latestQueueRequest.current) return null;
      setQueueError(cause instanceof Error ? cause.message : 'central PR queue unavailable');
      return null;
    } finally {
      if (requestId === latestQueueRequest.current) setLoading(false);
    }
  }, [cityName]);

  useEffect(() => {
    void refreshQueue();
  }, [refreshQueue]);

  useEffect(
    () => () => {
      latestQueueRequest.current += 1;
    },
    [],
  );

  useEffect(() => {
    if (cityName === null) return;
    try {
      const intents = readPRActionIntents(window.localStorage, cityName);
      setActionStates((current) => {
        const next = Object.fromEntries(
          Object.entries(current).filter(([, state]) => state.city !== cityName),
        );
        for (const intent of intents) {
          const identity = prActionIntentStorageKey(cityName, intent.request);
          next[identity] = {
            city: cityName,
            idempotencyKey: intent.idempotencyKey,
            request: intent.request,
            kind: 'unknown',
            message: 'Saved exact request; checking its durable server receipt.',
          };
        }
        return next;
      });
      setIntentError(null);
    } catch (cause) {
      setIntentError(
        cause instanceof Error ? cause.message : 'Saved PR action intents are unreadable.',
      );
    }
  }, [cityName]);

  useGcEventRefresh([GC_EVENT_PREFIX.bead], () => void refreshQueue(), {
    coalesceMs: 10_000,
  });

  useEffect(() => {
    if (queue === null) return;
    const expiresAt = Date.parse(queue.fresh_until);
    if (!Number.isFinite(expiresAt)) {
      setNowMs(Date.now());
      return;
    }
    const delay = Math.max(0, expiresAt - Date.now());
    const timer = window.setTimeout(() => {
      setNowMs(Date.now());
      void refreshQueue();
    }, delay + 1);
    return () => window.clearTimeout(timer);
  }, [queue, refreshQueue]);

  const items = useMemo(() => {
    if (queue === null) return [];
    const rows = queue?.items ?? [];
    return rows.filter((item) => {
      const hasPrepare = actionOption(queue, item, 'prepare', nowMs).available;
      return hasPrepare || workRecordForBead(item, beadId) !== undefined;
    });
  }, [queue, beadId, nowMs]);

  const submit = useCallback(
    async (body: WorkbenchPRActionBody) => {
      if (cityName === null || busyKey !== null) return;
      const localIdentity = prActionIntentStorageKey(cityName, body);
      let idempotencyKey: string | undefined;
      setBusyKey(localIdentity);
      setActionStates((current) => ({
        ...current,
        [localIdentity]: {
          city: cityName,
          idempotencyKey: '',
          request: body,
          kind: 'unknown',
          message: 'Submitting exact request…',
        },
      }));
      try {
        const intent = getOrCreatePRActionIntent(window.localStorage, cityName, body);
        idempotencyKey = intent.idempotencyKey;
        setActionStates((current) => ({
          ...current,
          [localIdentity]: {
            city: cityName,
            idempotencyKey: intent.idempotencyKey,
            request: intent.request,
            kind: 'unknown',
            message: 'Request submitted; verifying the durable receipt…',
          },
        }));
        const result = await supervisorApi().executePRAction(
          cityName,
          intent.request,
          intent.idempotencyKey,
        );
        if (isVerifiedActionReceipt(result, intent.request, intent.idempotencyKey)) {
          setActionStates((current) => ({
            ...current,
            [localIdentity]: {
              city: cityName,
              idempotencyKey: intent.idempotencyKey,
              request: intent.request,
              result,
              kind: 'verified',
            },
          }));
        } else if (receiptMatchesRequest(result, intent.request, intent.idempotencyKey)) {
          setActionStates((current) => ({
            ...current,
            [localIdentity]: {
              city: cityName,
              idempotencyKey: intent.idempotencyKey,
              request: intent.request,
              result,
              kind:
                result.status === 'rejected' || result.status === 'failed' ? 'rejected' : 'unknown',
              message: result.detail || 'The exact action has no verified completion receipt yet.',
            },
          }));
        } else {
          setActionStates((current) => ({
            ...current,
            [localIdentity]: {
              city: cityName,
              idempotencyKey: intent.idempotencyKey,
              request: intent.request,
              kind: 'unknown',
              message: 'The returned receipt does not match this exact request.',
            },
          }));
        }
        const fresh = await refreshQueue();
        reconcileFromQueue(
          fresh,
          intent.request,
          intent.idempotencyKey,
          localIdentity,
          cityName,
          setActionStates,
        );
      } catch (cause) {
        const fresh = await refreshQueue();
        if (idempotencyKey) {
          const reconciled = reconcileFromQueue(
            fresh,
            body,
            idempotencyKey,
            localIdentity,
            cityName,
            setActionStates,
          );
          if (reconciled) return;
        }
        const stale = cause instanceof SupervisorApiError && cause.status === 409;
        setActionStates((current) => ({
          ...current,
          [localIdentity]: {
            city: cityName,
            idempotencyKey: idempotencyKey ?? '',
            request: body,
            kind: stale ? 'stale' : 'unknown',
            message: stale
              ? 'The server rejected this stale request. The queue was refreshed; review the new revision and submit it explicitly.'
              : `Outcome is not confirmed. The exact saved request can be retried safely. ${errorText(cause)}`,
          },
        }));
      } finally {
        setBusyKey(null);
      }
    },
    [cityName, busyKey, refreshQueue],
  );

  return (
    <section aria-label="Pull request actions" className="mt-4 space-y-3">
      <div className="flex items-center justify-between gap-3">
        <h3 className="text-label font-semibold uppercase tracking-wider text-fg-muted">
          Central pull request queue
        </h3>
        <Button size="sm" tone="quiet" onClick={() => void refreshQueue()} disabled={loading}>
          {loading ? 'Refreshing' : 'Refresh PR queue'}
        </Button>
      </div>
      {intentError ? (
        <p role="alert" className="text-label text-accent">
          {intentError}
        </p>
      ) : null}
      {Object.entries(actionStates)
        .filter(([, state]) => state.city === cityName)
        .map(([identity, state]) => (
          <div
            key={identity}
            role={state.kind === 'unknown' || state.kind === 'stale' ? 'status' : undefined}
            className="text-label text-fg-faint"
          >
            <span>
              Exact {state.request.action} for <code>{state.request.head_sha}</code> against{' '}
              <code>{state.request.base_sha}</code>:{' '}
              {state.result
                ? `${state.result.status}${state.result.outcome ? ` · ${state.result.outcome}` : ''}`
                : state.message || state.kind}
              {state.result?.work_id ? (
                <>
                  {' · work '}
                  <code>{state.result.work_id}</code>
                </>
              ) : null}
            </span>
            {state.kind === 'unknown' ? (
              <Button
                size="sm"
                tone="quiet"
                disabled={busyKey !== null || cityName === null || state.idempotencyKey === ''}
                onClick={() => void submit(state.request)}
              >
                Retry exact action
              </Button>
            ) : null}
          </div>
        ))}
      {queue === null ? (
        <p role="status" className="text-body text-fg-muted">
          PR actions unavailable{queueError ? `: ${queueError}` : ' while the central queue loads'}.
        </p>
      ) : (
        <>
          <p role="status" className="text-label text-fg-faint">
            Queue {queue.availability}; policy {queue.policy_state}; version{' '}
            <code>{queue.policy_version || 'unavailable'}</code>
            {queueError ? ` · Refresh failed: ${queueError}` : ''}
          </p>
          {items.length === 0 ? (
            <p className="text-body text-fg-muted">
              No centrally actionable PRs are linked to this work.
            </p>
          ) : (
            <ul aria-label="Monitored pull requests" className="space-y-3">
              {items.map((item) => (
                <PRQueueItemCard
                  key={`${item.monitor}:${item.owner}/${item.repo}#${item.pull_request}`}
                  queue={queue}
                  item={item}
                  beadId={beadId}
                  nowMs={nowMs}
                  selectedAttemptId={selectedAttempts[itemKey(item)] ?? ''}
                  busyKey={busyKey}
                  onSelectAttempt={(attemptId) =>
                    setSelectedAttempts((current) => ({ ...current, [itemKey(item)]: attemptId }))
                  }
                  onAction={submit}
                />
              ))}
            </ul>
          )}
        </>
      )}
    </section>
  );
}

function PRQueueItemCard({
  queue,
  item,
  beadId,
  nowMs,
  selectedAttemptId,
  busyKey,
  onSelectAttempt,
  onAction,
}: {
  queue: PrActionQueue;
  item: PrActionQueueItem;
  beadId: string;
  nowMs: number;
  selectedAttemptId: string;
  busyKey: string | null;
  onSelectAttempt: (attemptId: string) => void;
  onAction: (body: WorkbenchPRActionBody) => Promise<void>;
}) {
  const work = workRecordForBead(item, beadId);
  const refs = work ? verifiedEvidenceForWork(item, work.id) : [];
  const selectedRef =
    refs.find((ref) => ref.attempt_id === selectedAttemptId) ??
    (refs.length === 1 ? refs[0] : undefined);
  const prepareVerdict = actionOption(queue, item, 'prepare', nowMs);
  const queueVerdict = actionOption(queue, item, 'queue_review', nowMs);
  const prepareBody = actionBody(item, 'prepare');
  const reviewBody = selectedRef
    ? actionBody(item, 'queue_review', work?.id, selectedRef.attempt_id)
    : null;
  const currentReceipts = (item.action_receipts ?? []).filter(
    (receipt) =>
      receipt.head_sha === item.head_sha &&
      receipt.base_sha === item.base_sha &&
      receipt.policy_version === item.policy_version,
  );

  return (
    <li className="rounded-sm border border-rule p-3 space-y-2">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <p className="text-body font-medium text-fg">
          {item.owner}/{item.repo}#{item.pull_request} · {item.title}
        </p>
        <span className="text-label text-fg-faint">{item.merge_state || 'state unavailable'}</span>
      </div>
      <p className="text-label text-fg-faint">
        <code>{item.head_sha}</code> against <code>{item.base_sha}</code> · policy{' '}
        <code>{item.policy_version}</code>
      </p>
      {work ? (
        <p className="text-label text-fg-faint">
          Exact selected work <code>{work.id}</code> · {item.evidence_state} evidence
        </p>
      ) : (
        <p className="text-label text-fg-faint">
          This PR has no current server work record matching selected bead <code>{beadId}</code>.
        </p>
      )}
      {refs.length > 1 && (
        <label className="block text-label text-fg-muted">
          Verified immutable attempt
          <select
            aria-label={`Verified attempt for ${item.owner}/${item.repo}#${item.pull_request}`}
            value={selectedAttemptId}
            onChange={(event) => onSelectAttempt(event.target.value)}
            className="ml-2 rounded-sm border border-rule bg-surface px-2 py-1"
          >
            <option value="">Choose exact server evidence</option>
            {refs.map((ref) => (
              <option key={ref.attempt_id} value={ref.attempt_id}>
                {ref.attempt_id} · {ref.diff_sha256.slice(0, 12)}
              </option>
            ))}
          </select>
        </label>
      )}
      {refs.length === 1 && (
        <p className="text-label text-fg-faint">
          Verified immutable attempt <code>{refs[0]?.attempt_id}</code>
        </p>
      )}
      <div className="flex flex-wrap items-center gap-2">
        {prepareVerdict.available && (
          <Button
            size="sm"
            onClick={() => void onAction(prepareBody)}
            disabled={busyKey !== null || !queueFresh(queue, item, nowMs)}
          >
            Prepare repair task
          </Button>
        )}
        {queueVerdict.available && work !== undefined && refs.length > 0 && (
          <Button
            size="sm"
            onClick={() => reviewBody && void onAction(reviewBody)}
            disabled={
              busyKey !== null ||
              !queueFresh(queue, item, nowMs) ||
              selectedRef === undefined ||
              (refs.length > 1 && selectedAttemptId === '')
            }
          >
            Queue exact revision for review
          </Button>
        )}
        {refs.length > 0 && !selectedRef && (
          <span className="text-label text-fg-faint">Choose one immutable evidence reference.</span>
        )}
        {!prepareVerdict.available && !queueVerdict.available && (
          <span className="text-label text-fg-faint">
            {prepareVerdict.reason || queueVerdict.reason}
          </span>
        )}
      </div>
      {currentReceipts.length > 0 && (
        <ul aria-label={`PR action receipts for ${item.owner}/${item.repo}#${item.pull_request}`}>
          {currentReceipts.map((receipt) => (
            <li key={receipt.id} className="text-label text-fg-faint">
              {receipt.action} receipt: {receipt.status}
              {receipt.outcome ? ` · ${receipt.outcome}` : ''}
              {receipt.attempt_id ? ` · attempt ${receipt.attempt_id}` : ''}
              {receipt.detail ? ` · ${receipt.detail}` : ''}
            </li>
          ))}
        </ul>
      )}
    </li>
  );
}

function actionBody(
  item: PrActionQueueItem,
  action: 'prepare' | 'queue_review',
  workId?: string,
  attemptId?: string,
): WorkbenchPRActionBody {
  return {
    action,
    monitor: item.monitor,
    owner: item.owner,
    repo: item.repo,
    pull_request: item.pull_request,
    head_sha: item.head_sha,
    base_sha: item.base_sha,
    policy_version: item.policy_version,
    ...(workId === undefined ? {} : { work_id: workId }),
    ...(attemptId === undefined ? {} : { attempt_id: attemptId }),
  };
}

function queueFresh(queue: PrActionQueue, item: PrActionQueueItem, nowMs: number): boolean {
  const queueExpiresAt = Date.parse(queue.fresh_until);
  const itemExpiresAt = Date.parse(item.fresh_until);
  return (
    Number.isFinite(queueExpiresAt) &&
    Number.isFinite(itemExpiresAt) &&
    nowMs < queueExpiresAt &&
    nowMs < itemExpiresAt
  );
}

function itemKey(item: PrActionQueueItem): string {
  return `${item.monitor}:${item.owner}/${item.repo}#${item.pull_request}`;
}

function reconcileFromQueue(
  queue: PrActionQueue | null,
  request: WorkbenchPRActionBody,
  idempotencyKey: string,
  identity: string,
  city: string,
  setStates: Dispatch<SetStateAction<Record<string, ActionState>>>,
): boolean {
  if (queue === null) return false;
  const receipt = (queue.items ?? [])
    .flatMap((item) => item.action_receipts ?? [])
    .find((candidate) => receiptMatchesRequest(candidate, request, idempotencyKey));
  if (!receipt) return false;
  const kind = isVerifiedActionReceipt(receipt, request, idempotencyKey)
    ? 'verified'
    : receipt.status === 'rejected' || receipt.status === 'failed'
      ? 'rejected'
      : 'unknown';
  setStates((current) => ({
    ...current,
    [identity]: { city, idempotencyKey, request, result: receipt, kind, message: receipt.detail },
  }));
  return true;
}

function errorText(cause: unknown): string {
  return cause instanceof Error ? cause.message : 'The supervisor request failed.';
}
