import { useCallback, useEffect, useState } from 'react';
import type { RequestReceipt } from 'gas-city-dashboard-shared/gc-supervisor';
import { getActiveCity } from '../api/cityBase';
import { Button } from '../components/Button';
import { supervisorApi } from '../supervisor/client';
import type { ExecutionAttempt } from '../lib/workbenchAttempts';
import {
  createSessionRequestIntent,
  readSessionRequestIntents,
  saveSessionRequestIntent,
  type SessionRequestIntent,
} from './sessionRequestIntent';

export function AttemptChatPanel({ attempt }: { attempt: ExecutionAttempt }) {
  const cityName = getActiveCity();
  const [draft, setDraft] = useState('');
  const [intents, setIntents] = useState<SessionRequestIntent[]>([]);
  const [busyRequestId, setBusyRequestId] = useState<string | null>(null);
  const [storageError, setStorageError] = useState<string | null>(null);

  useEffect(() => {
    if (cityName === null) return;
    try {
      setIntents(readSessionRequestIntents(window.localStorage, cityName, attempt.sessionId));
      setStorageError(null);
    } catch (cause) {
      setStorageError(errorText(cause));
    }
  }, [cityName, attempt.sessionId]);

  const updateIntent = useCallback((updated: SessionRequestIntent) => {
    try {
      saveSessionRequestIntent(window.localStorage, updated);
      setIntents((current) => {
        const index = current.findIndex((intent) => intent.requestId === updated.requestId);
        if (index < 0) return [...current, updated];
        return current.map((intent) => (intent.requestId === updated.requestId ? updated : intent));
      });
      setStorageError(null);
      return true;
    } catch (cause) {
      setStorageError(errorText(cause));
      return false;
    }
  }, []);

  const submitIntent = useCallback(
    async (intent: SessionRequestIntent) => {
      if (cityName === null || busyRequestId !== null) return;
      setBusyRequestId(intent.requestId);
      const submitting = { ...intent, submissionError: undefined };
      if (!updateIntent(submitting)) {
        setBusyRequestId(null);
        return;
      }
      try {
        const receipt = await supervisorApi().submitSessionRequest(cityName, attempt.sessionId, {
          request_id: intent.requestId,
          generation: intent.generation,
          message: intent.message,
        });
        if (!receiptMatchesIntent(receipt, intent)) {
          throw new Error('The supervisor returned a receipt for a different session request.');
        }
        updateIntent({ ...submitting, receipt, submissionError: undefined });
      } catch (cause) {
        updateIntent({ ...submitting, submissionError: errorText(cause) });
      } finally {
        setBusyRequestId(null);
      }
    },
    [attempt.sessionId, busyRequestId, cityName, updateIntent],
  );

  const refreshReceipt = useCallback(
    async (intent: SessionRequestIntent) => {
      if (cityName === null || busyRequestId !== null) return;
      setBusyRequestId(intent.requestId);
      try {
        const receipt = await supervisorApi().getSessionRequest(
          cityName,
          attempt.sessionId,
          intent.requestId,
        );
        if (!receiptMatchesIntent(receipt, intent)) {
          throw new Error('The supervisor returned a receipt for a different session request.');
        }
        updateIntent({ ...intent, receipt, submissionError: undefined });
      } catch (cause) {
        updateIntent({ ...intent, submissionError: `Receipt refresh failed: ${errorText(cause)}` });
      } finally {
        setBusyRequestId(null);
      }
    },
    [attempt.sessionId, busyRequestId, cityName, updateIntent],
  );

  const send = () => {
    const text = draft.trim();
    if (cityName === null || attempt.executionGeneration === null || text.length === 0) return;
    try {
      const intent = createSessionRequestIntent(
        window.localStorage,
        cityName,
        attempt.sessionId,
        attempt.executionGeneration,
        text,
      );
      setDraft('');
      setIntents((current) => [...current, intent]);
      setStorageError(null);
      void submitIntent(intent);
    } catch (cause) {
      setStorageError(errorText(cause));
    }
  };

  const canCompose = cityName !== null && attempt.executionGeneration !== null;
  const canSend = canCompose && draft.trim().length > 0;
  return (
    <section aria-label="Attempt chat" className="mt-3 space-y-2">
      {attempt.executionGeneration === null ? (
        <p role="status" className="text-label text-fg-faint">
          Session messages are unavailable because the supervisor did not provide a safe current
          execution generation.
        </p>
      ) : null}
      {cityName === null ? (
        <p role="status" className="text-label text-fg-faint">
          Session messages are unavailable without an active city.
        </p>
      ) : null}
      {storageError ? (
        <p role="alert" className="text-label text-accent">
          {storageError}
        </p>
      ) : null}
      <ul aria-label="Session request receipts" className="space-y-2">
        {intents.map((intent) => (
          <li key={intent.requestId} className="rounded-sm border border-rule p-2 space-y-1">
            <p className="text-label text-fg">{intent.message}</p>
            <p className="text-label text-fg-faint">
              Request <code>{intent.requestId}</code> · generation {intent.generation}
            </p>
            {intent.receipt ? (
              <ReceiptFacets receipt={intent.receipt} />
            ) : (
              <p className="text-label text-fg-faint">Server acceptance: not confirmed</p>
            )}
            {intent.submissionError ? (
              <p role="status" className="text-label text-accent">
                {intent.submissionError}
              </p>
            ) : null}
            <div className="flex flex-wrap gap-2">
              {!intent.receipt ? (
                <Button
                  size="sm"
                  tone="quiet"
                  disabled={busyRequestId !== null || cityName === null}
                  onClick={() => void submitIntent(intent)}
                >
                  Retry exact request
                </Button>
              ) : null}
              <Button
                size="sm"
                tone="quiet"
                disabled={busyRequestId !== null || cityName === null}
                onClick={() => void refreshReceipt(intent)}
              >
                Refresh receipt
              </Button>
            </div>
          </li>
        ))}
      </ul>
      <div className="flex gap-2">
        <input
          aria-label="Message"
          value={draft}
          onChange={(event) => setDraft(event.target.value)}
          className="flex-1 rounded-sm border border-rule px-2 py-1 text-body"
          disabled={!canCompose || busyRequestId !== null}
        />
        <Button size="sm" onClick={send} disabled={!canSend || busyRequestId !== null}>
          Send
        </Button>
      </div>
      <p className="text-label text-fg-faint">
        Provider delivery, session acknowledgement, and verified effect are reported separately.
        Workbench does not acknowledge delivery for the session.
      </p>
    </section>
  );
}

function ReceiptFacets({ receipt }: { receipt: RequestReceipt }) {
  return (
    <dl className="grid gap-x-2 text-label text-fg-faint sm:grid-cols-[max-content_1fr]">
      <dt>Server acceptance</dt>
      <dd>{receipt.accepted_at || 'not confirmed'}</dd>
      <dt>Provider delivery</dt>
      <dd>
        {receipt.delivery || 'not reported'}
        {receipt.provider_result_at ? ` · ${receipt.provider_result_at}` : ''}
      </dd>
      <dt>Session acknowledgement</dt>
      <dd>{receipt.acknowledged_at || 'not acknowledged'}</dd>
      <dt>Verified effect</dt>
      <dd>{receipt.effect || 'not verified'}</dd>
    </dl>
  );
}

function receiptMatchesIntent(receipt: RequestReceipt, intent: SessionRequestIntent): boolean {
  return (
    receipt.request_id === intent.requestId &&
    receipt.session_id === intent.sessionId &&
    receipt.generation === intent.generation
  );
}

function errorText(cause: unknown): string {
  return cause instanceof Error ? cause.message : 'The supervisor request failed.';
}
