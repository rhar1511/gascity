import { useEffect, useState } from 'react';
import {
  fetchHistoricalAttemptInspection,
  type HistoricalAttemptInspection as Inspection,
} from '../supervisor/attemptHistory';

type LoadState =
  | { kind: 'loading' }
  | { kind: 'error'; message: string }
  | { kind: 'ready'; inspection: Inspection };

/** Read-only historical artifacts for one selected session attempt. */
export function HistoricalAttemptArtifacts({
  beadId,
  sessionId,
  sessionLabel,
}: {
  beadId: string;
  sessionId: string;
  sessionLabel?: string;
}) {
  const [state, setState] = useState<LoadState>({ kind: 'loading' });

  useEffect(() => {
    const controller = new AbortController();
    setState({ kind: 'loading' });
    void fetchHistoricalAttemptInspection(beadId, sessionId, controller.signal).then(
      (inspection) => {
        if (!controller.signal.aborted) setState({ kind: 'ready', inspection });
      },
      (cause: unknown) => {
        if (!controller.signal.aborted) {
          setState({
            kind: 'error',
            message: cause instanceof Error ? cause.message : 'Could not load attempt artifacts.',
          });
        }
      },
    );
    return () => controller.abort();
  }, [beadId, sessionId]);

  return (
    <section
      aria-label="Historical attempt artifacts"
      className="space-y-3 rounded-sm border border-rule p-3"
    >
      <header>
        <h3 className="text-heading">Saved attempt artifacts</h3>
        <p className="text-label text-fg-muted">{sessionLabel ?? sessionId}</p>
      </header>

      {state.kind === 'loading' && <p role="status">Loading saved artifacts…</p>}
      {state.kind === 'error' && <p role="alert">{state.message}</p>}
      {state.kind === 'ready' && <ArtifactDetails inspection={state.inspection} />}
    </section>
  );
}

function ArtifactDetails({ inspection }: { inspection: Inspection }) {
  const pullRequestUrl = safeHttpUrl(inspection.pull_request.url);

  return (
    <div className="space-y-4">
      {inspection.association.state === 'unavailable' && (
        <p className="text-body text-fg-muted">
          Gas City could not verify a saved link between this session and the Bead.
        </p>
      )}

      <section aria-labelledby="historical-diff-heading" className="space-y-1">
        <h4 id="historical-diff-heading" className="text-label uppercase tracking-wider">
          Attempt diff
        </h4>
        {inspection.diff.state === 'unavailable' ? (
          <p className="text-body text-fg-muted">
            {diffUnavailableMessage(inspection.diff.reason)}
          </p>
        ) : (
          <>
            <pre className="max-h-80 overflow-auto rounded-sm bg-surface-tint p-3 text-body">
              <code>{inspection.diff.text ?? ''}</code>
            </pre>
            {inspection.diff.truncated === true && (
              <p className="text-label text-fg-muted">Saved diff is truncated.</p>
            )}
            {inspection.diff.binary === true && (
              <p className="text-label text-fg-muted">This attempt changed binary files.</p>
            )}
          </>
        )}
      </section>

      <section aria-labelledby="historical-pr-heading" className="space-y-1">
        <h4 id="historical-pr-heading" className="text-label uppercase tracking-wider">
          Pull request
        </h4>
        {inspection.pull_request.state === 'unavailable' ? (
          <p className="text-body text-fg-muted">
            {pullRequestUnavailableMessage(inspection.pull_request.reason)}
          </p>
        ) : (
          <div className="flex flex-wrap items-center gap-2 text-body">
            {inspection.pull_request.status && <span>{inspection.pull_request.status}</span>}
            {pullRequestUrl && (
              <a
                className="text-accent underline underline-offset-2"
                href={pullRequestUrl}
                target="_blank"
                rel="noreferrer"
              >
                {inspection.pull_request.url}
              </a>
            )}
            {!inspection.pull_request.status && !pullRequestUrl && (
              <span className="text-fg-muted">No PR details recorded.</span>
            )}
          </div>
        )}
      </section>
    </div>
  );
}

function diffUnavailableMessage(reason?: string): string {
  if (reason === 'historical_diff_not_recorded') {
    return 'Gas City did not save a diff snapshot for this attempt.';
  }
  return 'A historical diff is not available for this attempt.';
}

function pullRequestUnavailableMessage(reason?: string): string {
  if (reason === 'attempt_pr_state_not_recorded') {
    return 'Gas City did not save PR state for this attempt.';
  }
  return 'Historical PR state is not available for this attempt.';
}

function safeHttpUrl(value?: string): string | null {
  if (!value) return null;
  try {
    const url = new URL(value);
    return url.protocol === 'http:' || url.protocol === 'https:' ? url.toString() : null;
  } catch {
    return null;
  }
}
