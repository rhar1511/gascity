import { useEffect, useMemo, useState } from 'react';
import type { Evidence, Facet } from 'gas-city-dashboard-shared/gc-supervisor';
import { Button } from '../components/Button';
import { useCachedData } from '../hooks/useCachedData';
import type { ExecutionAttempt } from '../lib/workbenchAttempts';
import { supervisorApi } from '../supervisor/client';
import { decodeCandidateCommitDiff, matchingWorkbenchEvidence } from './attemptEvidence';

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
    return <p className="text-body text-accent" role="alert">Archived evidence unavailable: no active city is selected.</p>;
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
    <HistoricalAttemptEvidenceForGeneration
      cityName={cityName}
      workID={workID}
      attempt={attempt}
    />
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
  const [selectedAttemptID, setSelectedAttemptID] = useState('');
  const selected =
    matches.length === 1
      ? matches[0]
      : matches.find((evidence) => evidence.attempt_id === selectedAttemptID);

  return (
    <section aria-label="Archived attempt evidence" className="mt-3 space-y-2 rounded-sm border border-rule p-3">
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
            onChange={(event) => setSelectedAttemptID(event.target.value)}
          >
            <option value="">Select an immutable attempt</option>
            {matches.map((evidence) => (
              <option key={evidence.attempt_id} value={evidence.attempt_id}>
                {evidence.captured_at} · {evidence.identity.execution_bead_id} · {evidence.attempt_id}
              </option>
            ))}
          </select>
        </label>
      )}
      {selected && <EvidenceDetails evidence={selected} />}
      <p className="text-label text-fg-faint">
        These facets were sealed with the attempt. Later PR receipts and session acknowledgements
        appear only when the server links them to this exact attempt.
      </p>
    </section>
  );
}

function EvidenceDetails({ evidence }: { evidence: Evidence }) {
  const [diff, setDiff] = useState<{ attemptID: string; text?: string; error?: string } | null>(null);
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
        <dd className="break-all text-fg"><code>{evidence.attempt_id}</code></dd>
        <dt className="text-fg-faint">Execution record</dt>
        <dd className="break-all text-fg"><code>{evidence.identity.execution_bead_id}</code></dd>
        <dt className="text-fg-faint">Captured</dt>
        <dd className="text-fg"><time dateTime={evidence.captured_at}>{evidence.captured_at}</time></dd>
        <dt className="text-fg-faint">Outcome</dt>
        <dd className="text-fg">{evidence.outcome || evidence.source_status}</dd>
        <dt className="text-fg-faint">Base revision</dt>
        <dd className="break-all text-fg"><code>{evidence.base_sha || evidence.base_reason || evidence.base_status}</code></dd>
        <dt className="text-fg-faint">Candidate revision</dt>
        <dd className="break-all text-fg"><code>{evidence.candidate_sha || evidence.candidate_reason || evidence.candidate_status}</code></dd>
        <dt className="text-fg-faint">Workspace at capture</dt>
        <dd className="text-fg">
          {evidence.working_tree_status}
          {evidence.working_tree_status === 'dirty' ? ' (separate from the commit diff)' : ''}
        </dd>
        <dt className="text-fg-faint">Commit diff digest</dt>
        <dd className="break-all text-fg"><code>{evidence.diff.sha256 || evidence.diff.reason || evidence.diff.status}</code></dd>
      </dl>
      <dl aria-label="Captured evidence facets" className="grid gap-x-3 gap-y-1 text-label sm:grid-cols-[max-content_1fr]">
        <FacetRow label="Policy" facet={evidence.policy} />
        <FacetRow label="Actions" facet={evidence.actions} />
        <FacetRow label="Acknowledgements" facet={evidence.acknowledgements} />
      </dl>
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
          <p className="text-body text-fg-muted italic">No committed changes between these revisions.</p>
        ) : (
          <pre aria-label="Archived commit diff" className="max-h-80 overflow-auto text-label">
            {diff?.text}
          </pre>
        )}
      </div>
    </div>
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
          <code key={reference} className="ml-1 break-all">{reference}</code>
        ))}
      </dd>
    </>
  );
}
