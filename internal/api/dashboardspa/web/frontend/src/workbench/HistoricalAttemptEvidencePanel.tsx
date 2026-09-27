import { useEffect, useMemo, useState } from 'react';
import type {
  AttemptEvidenceRead,
  Evidence,
  Facet,
  HistoricalPrActionRecord,
  RequestReceipt,
} from 'gas-city-dashboard-shared/gc-supervisor';
import { Button } from '../components/Button';
import { useCachedData } from '../hooks/useCachedData';
import type { ExecutionAttempt } from '../lib/workbenchAttempts';
import { supervisorApi } from '../supervisor/client';
import {
  decodeCandidateCommitDiff,
  exactRequestReceiptMatchesWorkbenchEvidence,
  exactWorkbenchEvidenceMatches,
  matchingWorkbenchEvidence,
} from './attemptEvidence';

export function HistoricalAttemptEvidencePanel({
  cityName,
  workID,
  attempt,
}: {
  cityName: string | null;
  workID: string;
  attempt: ExecutionAttempt;
}) {
  const generation = attempt.executionGeneration;
  if (cityName === null) {
    return (
      <p className="text-body text-accent" role="alert">
        Archived evidence unavailable: no active city is selected.
      </p>
    );
  }
  if (generation === null || !Number.isSafeInteger(generation) || generation <= 0) {
    return (
      <p className="text-body text-fg-muted" role="status">
        Cannot match archived evidence: a valid execution generation is unavailable for this
        session.
      </p>
    );
  }
  return (
    <HistoricalAttemptEvidenceForGeneration cityName={cityName} workID={workID} attempt={attempt} />
  );
}

function HistoricalAttemptEvidenceForGeneration({
  cityName,
  workID,
  attempt,
}: {
  cityName: string;
  workID: string;
  attempt: ExecutionAttempt;
}) {
  const { data, loading, error, refresh } = useCachedData(
    `workbench:attempt-evidence:${cityName}:${workID}`,
    (signal) => supervisorApi().listAttemptEvidence(cityName, workID, signal),
  );
  const matches = useMemo(
    () => matchingWorkbenchEvidence(data ?? [], workID, attempt),
    [data, workID, attempt],
  );
  const [selection, setSelection] = useState<{ scope: string; attemptID: string } | null>(null);
  const generation = attempt.executionGeneration;
  const selectionScope = `${cityName}:${workID}:${attempt.sessionId}:${generation}`;
  const selected =
    matches.length === 1
      ? matches[0]
      : matches.find(
          (evidence) =>
            selection?.scope === selectionScope && evidence.attempt_id === selection.attemptID,
        );

  return (
    <section
      aria-label="Archived attempt evidence"
      className="mt-3 space-y-2 rounded-sm border border-rule p-3"
    >
      <div className="flex items-center justify-between gap-3">
        <h3 className="text-label font-semibold uppercase tracking-wider text-fg-muted">
          Archived attempt evidence
        </h3>
        <Button size="sm" tone="quiet" onClick={() => void refresh()} disabled={loading}>
          {loading ? 'Refreshing' : 'Refresh evidence'}
        </Button>
      </div>
      <p className="text-label text-fg-faint">
        Matching work <code>{workID}</code>, session <code>{attempt.sessionId}</code>, generation{' '}
        <code>{attempt.executionGeneration}</code>.
      </p>
      {error && (
        <p className="text-body text-accent" role="alert">
          Archived evidence could not be read: {error}
        </p>
      )}
      {loading && data === undefined && (
        <p className="text-body text-fg-muted italic">Loading archived evidence…</p>
      )}
      {data !== undefined && matches.length === 0 && (
        <p className="text-body text-fg-muted italic">
          No immutable Workbench archive matches this exact session generation.
        </p>
      )}
      {matches.length > 1 && (
        <label className="block space-y-1 text-label text-fg-muted">
          <span>Choose archived attempt</span>
          <select
            aria-label="Choose archived attempt"
            className="w-full rounded-sm border border-rule bg-surface px-2 py-1 text-body text-fg"
            value={selected?.attempt_id ?? ''}
            onChange={(event) =>
              setSelection({ scope: selectionScope, attemptID: event.target.value })
            }
          >
            <option value="">Select an immutable attempt</option>
            {matches.map((evidence) => (
              <option key={evidence.attempt_id} value={evidence.attempt_id}>
                {evidence.captured_at} · {evidence.identity.execution_bead_id} ·{' '}
                {evidence.attempt_id}
              </option>
            ))}
          </select>
        </label>
      )}
      {selected && (
        <ExactAttemptEvidence
          key={`${selectionScope}:${selected.attempt_id}`}
          cityName={cityName}
          workID={workID}
          attempt={attempt}
          listed={selected}
        />
      )}
      <p className="text-label text-fg-faint">
        Capture-time facets remain separate from later records. Missing or unavailable later records
        do not establish that no action occurred.
      </p>
    </section>
  );
}

function ExactAttemptEvidence({
  cityName,
  workID,
  attempt,
  listed,
}: {
  cityName: string;
  workID: string;
  attempt: ExecutionAttempt;
  listed: Evidence;
}) {
  const [result, setResult] = useState<
    | { key: string; loading: true }
    | { key: string; loading: false; read: AttemptEvidenceRead }
    | { key: string; loading: false; error: string }
    | null
  >(null);
  const sessionID = attempt.sessionId;
  const generation = attempt.executionGeneration;
  const requestKey = `${cityName}:${workID}:${sessionID}:${generation}:${listed.attempt_id}`;

  useEffect(() => {
    const controller = new AbortController();
    let active = true;
    setResult({ key: requestKey, loading: true });
    void supervisorApi()
      .getAttemptEvidence(cityName, workID, listed.attempt_id, controller.signal)
      .then((read) => {
        if (!active) return;
        if (
          !exactWorkbenchEvidenceMatches(read, listed, workID, {
            sessionId: sessionID,
            executionGeneration: generation,
          })
        ) {
          setResult({
            key: requestKey,
            loading: false,
            error:
              'Exact archive response did not match the selected attempt identity and revisions.',
          });
          return;
        }
        setResult({ key: requestKey, loading: false, read });
      })
      .catch((cause: unknown) => {
        if (!active) return;
        setResult({
          key: requestKey,
          loading: false,
          error:
            cause instanceof Error ? cause.message : 'exact archived attempt could not be read',
        });
      });
    return () => {
      active = false;
      controller.abort();
    };
  }, [cityName, workID, requestKey, sessionID, generation, listed]);

  const current = result?.key === requestKey ? result : null;
  if (!current || current.loading) {
    return (
      <p className="text-body text-fg-muted italic" role="status">
        Loading exact archived attempt…
      </p>
    );
  }
  if ('error' in current) {
    return (
      <p className="text-body text-accent" role="alert">
        Exact archived attempt could not be verified: {current.error}
      </p>
    );
  }
  return <EvidenceDetails evidence={current.read} />;
}

function EvidenceDetails({ evidence }: { evidence: AttemptEvidenceRead }) {
  const [diff, setDiff] = useState<{ attemptID: string; text?: string; error?: string } | null>(
    null,
  );
  useEffect(() => {
    let active = true;
    setDiff({ attemptID: evidence.attempt_id });
    void decodeCandidateCommitDiff(evidence.diff).then(
      (text) => {
        if (active) setDiff({ attemptID: evidence.attempt_id, text });
      },
      (cause: unknown) => {
        if (active) {
          setDiff({
            attemptID: evidence.attempt_id,
            error: cause instanceof Error ? cause.message : 'archived diff could not be decoded',
          });
        }
      },
    );
    return () => {
      active = false;
    };
  }, [evidence]);

  const diffPending = diff?.attemptID !== evidence.attempt_id;
  const diffLoading = !diffPending && diff?.text === undefined && diff?.error === undefined;
  return (
    <div className="space-y-3">
      <dl className="grid gap-x-3 gap-y-1 text-label sm:grid-cols-[max-content_1fr]">
        <dt className="text-fg-faint">Immutable attempt</dt>
        <dd className="break-all text-fg">
          <code>{evidence.attempt_id}</code>
        </dd>
        <dt className="text-fg-faint">Execution record</dt>
        <dd className="break-all text-fg">
          <code>{evidence.identity.execution_bead_id}</code>
        </dd>
        <dt className="text-fg-faint">Captured</dt>
        <dd className="text-fg">
          <time dateTime={evidence.captured_at}>{evidence.captured_at}</time>
        </dd>
        <dt className="text-fg-faint">Outcome</dt>
        <dd className="text-fg">{evidence.outcome || evidence.source_status}</dd>
        <dt className="text-fg-faint">Base revision</dt>
        <dd className="break-all text-fg">
          <code>{evidence.base_sha || evidence.base_reason || evidence.base_status}</code>
        </dd>
        <dt className="text-fg-faint">Candidate revision</dt>
        <dd className="break-all text-fg">
          <code>
            {evidence.candidate_sha || evidence.candidate_reason || evidence.candidate_status}
          </code>
        </dd>
        <dt className="text-fg-faint">Workspace at capture</dt>
        <dd className="text-fg">
          {evidence.working_tree_status}
          {evidence.working_tree_status === 'dirty' ? ' (separate from the commit diff)' : ''}
        </dd>
        <dt className="text-fg-faint">Commit diff digest</dt>
        <dd className="break-all text-fg">
          <code>{evidence.diff.sha256 || evidence.diff.reason || evidence.diff.status}</code>
        </dd>
      </dl>
      <dl
        aria-label="Captured evidence facets"
        className="grid gap-x-3 gap-y-1 text-label sm:grid-cols-[max-content_1fr]"
      >
        <FacetRow label="Policy" facet={evidence.policy} />
        <FacetRow label="Actions" facet={evidence.actions} />
        <FacetRow label="Acknowledgements" facet={evidence.acknowledgements} />
      </dl>
      <LaterAttemptRecords evidence={evidence} />
      <div aria-label="Archived candidate commit diff" className="space-y-1">
        <p className="text-label font-semibold uppercase tracking-wider text-fg-muted">
          Base-to-candidate commit diff
        </p>
        {diffPending || diffLoading ? (
          <p className="text-body text-fg-muted italic">Decoding archived commit diff…</p>
        ) : diff?.error ? (
          <p className="text-body text-accent" role="alert">
            Archived commit diff unavailable: {diff.error}
          </p>
        ) : diff?.text === '' ? (
          <p className="text-body text-fg-muted italic">
            No committed changes between these revisions.
          </p>
        ) : (
          <pre aria-label="Archived commit diff" className="max-h-80 overflow-auto text-label">
            {diff?.text}
          </pre>
        )}
      </div>
    </div>
  );
}

function LaterAttemptRecords({ evidence }: { evidence: AttemptEvidenceRead }) {
  const actions = evidence.related_records?.actions;
  const acknowledgements = evidence.related_records?.acknowledgements;
  const returnedReceipts = Array.isArray(acknowledgements?.records) ? acknowledgements.records : [];
  const requestReceipts = acknowledgements?.status === 'available' ? returnedReceipts : [];
  const matchedReceipts = requestReceipts.filter((receipt) =>
    exactRequestReceiptMatchesWorkbenchEvidence(receipt, evidence),
  );
  const withheldByAvailability =
    acknowledgements?.status !== 'available' ? returnedReceipts.length : 0;
  const withheldByIdentity = requestReceipts.length - matchedReceipts.length;
  return (
    <section aria-label="Later attempt records" className="space-y-2 border-t border-rule pt-2">
      <h4 className="text-label font-semibold uppercase tracking-wider text-fg-muted">
        Later records linked to this exact attempt
      </h4>
      <p className="text-label text-fg-faint">
        These records were read after capture. They do not change the sealed capture-time facets.
      </p>
      <dl
        aria-label="Related record availability"
        className="grid gap-x-3 gap-y-1 text-label sm:grid-cols-[max-content_1fr]"
      >
        <FacetRow
          label="PR action ledger"
          facet={
            actions ?? { status: 'unavailable', reason: 'related_action_records_not_returned' }
          }
        />
        <FacetRow
          label="Session acknowledgements"
          facet={
            acknowledgements ?? {
              status: 'unavailable',
              reason: 'related_acknowledgements_not_returned',
            }
          }
        />
      </dl>
      {actions?.records?.map((record) => (
        <HistoricalPRAction key={record.receipt.id} record={record} />
      ))}
      <div aria-label="Session acknowledgement receipts" className="space-y-2">
        <p className="text-label text-fg-faint">
          Acceptance, delivery, acknowledgement, and effect are separate server-recorded stages.
        </p>
        {matchedReceipts.map((receipt) => (
          <HistoricalSessionRequestReceipt
            key={`${receipt.session_id}:${receipt.request_id}:${receipt.generation}`}
            receipt={receipt}
          />
        ))}
        {withheldByAvailability > 0 && (
          <p className="text-body text-accent" role="alert">
            {withheldByAvailability} request receipt(s) were withheld because acknowledgement
            records are {acknowledgements?.status ?? 'unavailable'}.
          </p>
        )}
        {withheldByIdentity > 0 && (
          <p className="text-body text-accent" role="alert">
            {withheldByIdentity} request receipt(s) were withheld because their exact attempt
            binding did not match this archive.
          </p>
        )}
        {acknowledgements?.status === 'available' &&
          Array.isArray(acknowledgements.records) &&
          acknowledgements.records.length === 0 && (
            <p className="text-body text-fg-muted italic">
              No attributed session request receipts were returned for this exact attempt.
            </p>
          )}
        {acknowledgements?.status === 'available' &&
          acknowledgements.unattributed_requests !== undefined && (
            <p className="text-label text-fg-faint">
              Unattributed requests: {acknowledgements.unattributed_requests}
            </p>
          )}
      </div>
    </section>
  );
}

function HistoricalSessionRequestReceipt({ receipt }: { receipt: RequestReceipt }) {
  const binding = receipt.attempt;
  if (!binding) return null;
  const identity = binding.identity;
  return (
    <article
      aria-label={`Session request receipt ${receipt.request_id}`}
      className="space-y-1 rounded-sm border border-rule p-2 text-label"
    >
      <p className="font-semibold text-fg">
        Session request <code>{receipt.request_id}</code>
      </p>
      <p className="text-fg-muted">
        Session <code>{receipt.session_id}</code> · generation {receipt.generation}
      </p>
      <p className="text-fg-muted">
        Accepted at: <time dateTime={receipt.accepted_at}>{receipt.accepted_at}</time>
      </p>
      <p className="text-fg-muted">Delivery: {receipt.delivery}</p>
      {receipt.delivery_attempted_at && (
        <p className="text-fg-muted">
          Delivery attempted at:{' '}
          <time dateTime={receipt.delivery_attempted_at}>{receipt.delivery_attempted_at}</time>
        </p>
      )}
      {receipt.provider_result_at && (
        <p className="text-fg-muted">
          Provider result recorded at:{' '}
          <time dateTime={receipt.provider_result_at}>{receipt.provider_result_at}</time>
        </p>
      )}
      {receipt.acknowledged_at && (
        <p className="text-fg-muted">
          Acknowledged at: <time dateTime={receipt.acknowledged_at}>{receipt.acknowledged_at}</time>
        </p>
      )}
      <p className="text-fg-muted">Recorded effect: {receipt.effect}</p>
      <p className="break-all text-fg-faint">
        Exact attempt binding: <code>{binding.attempt_id}</code> · store{' '}
        <code>{binding.store_ref}</code> · work revision {binding.work_revision}
      </p>
      <p className="break-all text-fg-faint">
        Execution <code>{identity.execution_bead_id}</code> · session{' '}
        <code>{identity.session_id}</code> · generation {identity.session_generation}
        {identity.claim_generation ? ` · claim ${identity.claim_generation}` : ''}
      </p>
    </article>
  );
}

function HistoricalPRAction({ record }: { record: HistoricalPrActionRecord }) {
  const { receipt } = record;
  return (
    <article
      aria-label={`PR action ${receipt.id}`}
      className="space-y-1 rounded-sm border border-rule p-2 text-label"
    >
      <p className="font-semibold text-fg">
        {receipt.action}: {receipt.status}
      </p>
      {receipt.outcome && <p className="text-fg-muted">Outcome: {receipt.outcome}</p>}
      <p className="text-fg-muted">
        {receipt.owner}/{receipt.repo}#{receipt.pull_request} · policy {receipt.policy_version}
      </p>
      <p className="break-all text-fg-faint">
        Base <code>{receipt.base_sha}</code> · head <code>{receipt.head_sha}</code>
      </p>
      <dl
        aria-label="Captured action policy facets"
        className="grid gap-x-3 gap-y-1 sm:grid-cols-[max-content_1fr]"
      >
        <FacetRow label="Admission policy" facet={record.admission_policy} />
        <FacetRow label="Execution policy" facet={record.execution_policy} />
      </dl>
    </article>
  );
}

function FacetRow({ label, facet }: { label: string; facet: Facet }) {
  return (
    <>
      <dt className="text-fg-faint">{label}</dt>
      <dd className="space-y-0.5 text-fg">
        <span>{facet.status}</span>
        {facet.reason && <span className="ml-1 text-fg-faint">({facet.reason})</span>}
        {facet.refs?.map((reference) => (
          <code key={reference} className="ml-1 break-all">
            {reference}
          </code>
        ))}
      </dd>
    </>
  );
}
