import { useEffect, useState, type FormEvent } from 'react';
import type { SupervisorBead } from '../supervisor/beadReads';
import { resolveLocalReviewUrl, resolveWayfinderReview } from '../lib/wayfinderReview';
import {
  readWayfinderReviewHistory,
  type WayfinderReviewDraft,
  type WayfinderReviewRecord,
} from '../lib/wayfinderReviewLog';
import { useOperatorConfig } from '../contexts/OperatorConfigContext';
import { recordWayfinderReviewEntry } from '../supervisor/wayfinderReviewWrites';

/**
 * Presents prototype availability and auditable review controls for the selected Bead.
 * Mount beside its attempt details; entries reach Gas City only after explicit submission.
 */
export function WayfinderReviewWorkflow({ bead }: { bead: SupervisorBead }) {
  return <WayfinderReviewWorkflowForBead key={bead.id} bead={bead} />;
}

function WayfinderReviewWorkflowForBead({ bead }: { bead: SupervisorBead }) {
  const review = resolveWayfinderReview(bead);
  const { operatorAlias } = useOperatorConfig();
  const [localUrl, setLocalUrl] = useState('');
  const [history, setHistory] = useState(() => readWayfinderReviewHistory(bead.metadata));
  const [entryKind, setEntryKind] = useState<WayfinderReviewDraft['kind']>('annotation');
  const [annotation, setAnnotation] = useState('');
  const [prompt, setPrompt] = useState('');
  const [answer, setAnswer] = useState('');
  const [approvalScope, setApprovalScope] = useState('');
  const [approvalConfirmed, setApprovalConfirmed] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  useEffect(() => {
    setHistory(readWayfinderReviewHistory(bead.metadata));
  }, [bead.id, bead.metadata]);

  if (!review.eligible) return null;

  const localReviewUrl = review.reviewUrl ?? resolveLocalReviewUrl(localUrl.trim());
  const canRecord =
    !saving &&
    (entryKind === 'annotation'
      ? annotation.trim().length > 0
      : entryKind === 'answer'
        ? prompt.trim().length > 0 && answer.trim().length > 0
        : approvalScope.trim().length > 0 && approvalConfirmed);
  const submitLabel =
    entryKind === 'annotation'
      ? 'Record annotation'
      : entryKind === 'answer'
        ? 'Record answer'
        : 'Record explicit approval';

  const recordDraft = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!canRecord) return;
    const draft: WayfinderReviewDraft =
      entryKind === 'annotation'
        ? { kind: 'annotation', text: annotation }
        : entryKind === 'answer'
          ? { kind: 'answer', prompt, answer }
          : { kind: 'approval', scope: approvalScope, explicitly_confirmed: true };

    setSaving(true);
    setError(null);
    setNotice(null);
    try {
      const record = await recordWayfinderReviewEntry(bead.id, operatorAlias, draft);
      setHistory((current) => ({
        ...current,
        records: [...current.records, record].sort((left, right) =>
          right.recorded_at.localeCompare(left.recorded_at),
        ),
      }));
      setAnnotation('');
      setPrompt('');
      setAnswer('');
      setApprovalScope('');
      setApprovalConfirmed(false);
      setNotice(`${submitLabel.replace('Record ', '')} recorded on this Bead.`);
    } catch (cause) {
      setError(
        cause instanceof Error ? cause.message : 'Gas City could not record this review entry.',
      );
    } finally {
      setSaving(false);
    }
  };

  const selectEntryKind = (kind: WayfinderReviewDraft['kind']) => {
    setEntryKind(kind);
    setApprovalConfirmed(false);
    setError(null);
    setNotice(null);
  };

  return (
    <section aria-label="Wayfinder review" className="mt-5 border-t border-rule pt-4">
      <div className="mb-3 flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1">
        <h3 className="text-label font-semibold uppercase tracking-wider text-fg">
          Wayfinder review
        </h3>
        <span className="text-label text-fg-muted">
          {review.mode === 'lavish' ? 'Lavish AXI · local document review' : 'Conversation review'}
        </span>
      </div>

      <ol className="space-y-3 text-body text-fg-muted">
        <li>
          <span className="text-fg">01 · Compare prototypes</span>
          {review.variants.length > 0 ? (
            <div className="mt-1 flex flex-wrap gap-3 text-label">
              {review.variants.map((variant) => (
                <a
                  key={variant.label}
                  href={variant.url}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="text-accent underline decoration-rule underline-offset-4 focus-mark"
                >
                  Prototype {variant.label}
                </a>
              ))}
            </div>
          ) : (
            <p className="text-label text-fg-faint">
              No prototype variants are published on this Bead. Review the running app route when
              live data or navigation matters.
            </p>
          )}
        </li>
        <li>
          <span className="text-fg">02 · Annotate the review document</span>
          {review.mode === 'lavish' ? (
            <div className="mt-1 space-y-2 text-label">
              {review.reviewLinkState === 'blocked' && (
                <p className="text-accent" role="alert">
                  The linked review URL is not a safe local address, so it cannot be opened here.
                </p>
              )}
              {review.reviewLinkState === 'missing' && (
                <p className="text-fg-faint">
                  No Lavish session is linked. Open the review document in your local Lavish app,
                  then paste its loopback session URL below. This link stays in this browser until
                  you leave the Bead.
                </p>
              )}
              {review.reviewUrl === null && (
                <input
                  type="url"
                  aria-label="Local Lavish URL"
                  placeholder="http://127.0.0.1:…/session/…"
                  value={localUrl}
                  onChange={(event) => setLocalUrl(event.target.value)}
                  className="w-full rounded-sm border border-rule bg-surface px-2 py-1 text-body text-fg"
                />
              )}
              {localUrl.trim() && localReviewUrl === null && (
                <p className="text-accent" role="alert">
                  Enter a local HTTP(S) URL without credentials.
                </p>
              )}
              {localReviewUrl && (
                <p>
                  <a
                    href={localReviewUrl}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="text-accent underline decoration-rule underline-offset-4 focus-mark"
                  >
                    Open Lavish review ↗
                  </a>{' '}
                  in a separate tab. Workbench does not embed or read the document.
                </p>
              )}
            </div>
          ) : (
            <p className="text-label text-fg-faint">
              Review the map and its prompts with the owning session. Record annotations and answers
              below after discussing them.
            </p>
          )}
        </li>
        <li>
          <span className="text-fg">03 · Critique and iterate</span>
          <p className="text-label text-fg-faint">
            Ask the design agent for an Impeccable /critique, revise the prototype and document,
            then compare again. Nothing is sent or published from this panel.
          </p>
        </li>
      </ol>

      <div className="mt-4 border-t border-rule pt-3">
        <div className="flex flex-wrap items-baseline justify-between gap-2">
          <h4 className="text-label font-semibold text-fg">Review record</h4>
          <span className="text-label text-fg-faint">Saved on Bead {bead.id}</span>
        </div>
        <p className="mt-1 text-label text-fg-faint">
          Drafts are lost if you leave or reload. Nothing is recorded until you submit it; an
          approval record does not publish artifacts or start rollout.
        </p>
        <form className="mt-3 space-y-3" onSubmit={(event) => void recordDraft(event)}>
          <div role="group" aria-label="Review entry type" className="flex flex-wrap gap-2">
            <EntryKindButton
              selected={entryKind === 'annotation'}
              onClick={() => selectEntryKind('annotation')}
            >
              Annotation
            </EntryKindButton>
            <EntryKindButton
              selected={entryKind === 'answer'}
              onClick={() => selectEntryKind('answer')}
            >
              Answer a prompt
            </EntryKindButton>
            <EntryKindButton
              selected={entryKind === 'approval'}
              onClick={() => selectEntryKind('approval')}
            >
              Explicit approval
            </EntryKindButton>
          </div>

          {entryKind === 'annotation' && (
            <label className="block text-label text-fg-muted" htmlFor="wayfinder-annotation">
              Annotation
              <textarea
                id="wayfinder-annotation"
                aria-label="Annotation"
                autoComplete="off"
                maxLength={4000}
                required
                rows={3}
                value={annotation}
                onChange={(event) => setAnnotation(event.target.value)}
                className="mt-1 block w-full rounded-sm border border-rule bg-surface px-2 py-1 text-body text-fg focus-mark"
              />
            </label>
          )}

          {entryKind === 'answer' && (
            <div className="space-y-3">
              <label className="block text-label text-fg-muted" htmlFor="wayfinder-prompt">
                Prompt
                <textarea
                  id="wayfinder-prompt"
                  aria-label="Prompt"
                  maxLength={2000}
                  required
                  rows={2}
                  value={prompt}
                  onChange={(event) => setPrompt(event.target.value)}
                  className="mt-1 block w-full rounded-sm border border-rule bg-surface px-2 py-1 text-body text-fg focus-mark"
                />
              </label>
              <label className="block text-label text-fg-muted" htmlFor="wayfinder-answer">
                Answer
                <textarea
                  id="wayfinder-answer"
                  aria-label="Answer"
                  maxLength={4000}
                  required
                  rows={3}
                  value={answer}
                  onChange={(event) => setAnswer(event.target.value)}
                  className="mt-1 block w-full rounded-sm border border-rule bg-surface px-2 py-1 text-body text-fg focus-mark"
                />
              </label>
            </div>
          )}

          {entryKind === 'approval' && (
            <div className="space-y-2">
              <label className="block text-label text-fg-muted" htmlFor="wayfinder-approval-scope">
                Approval scope
                <textarea
                  id="wayfinder-approval-scope"
                  aria-label="Approval scope"
                  maxLength={2000}
                  required
                  rows={2}
                  value={approvalScope}
                  onChange={(event) => setApprovalScope(event.target.value)}
                  className="mt-1 block w-full rounded-sm border border-rule bg-surface px-2 py-1 text-body text-fg focus-mark"
                />
              </label>
              <label className="flex items-start gap-2 text-label text-fg-muted">
                <input
                  type="checkbox"
                  checked={approvalConfirmed}
                  onChange={(event) => setApprovalConfirmed(event.target.checked)}
                  className="mt-0.5 accent-current"
                />
                I explicitly approve this scope.
              </label>
              <p className="text-label text-fg-faint">
                This records review approval only; it does not approve a PR, publish the artifact,
                or start rollout.
              </p>
            </div>
          )}

          <div className="flex flex-wrap items-center gap-3">
            <button
              type="submit"
              disabled={!canRecord}
              className="rounded-sm border border-accent px-3 py-1 text-label font-semibold text-accent hover:bg-accent/10 focus-mark disabled:cursor-not-allowed disabled:opacity-50"
            >
              {saving ? 'Recording…' : submitLabel}
            </button>
            {notice && (
              <span role="status" className="text-label text-fg-muted">
                {notice}
              </span>
            )}
            {error && (
              <span role="alert" className="text-label text-accent">
                {error}
              </span>
            )}
          </div>
        </form>
        {history.unreadableCount > 0 && (
          <p role="alert" className="mt-2 text-label text-accent">
            {history.unreadableCount} existing review record
            {history.unreadableCount === 1 ? ' is' : 's are'} unreadable. The visible history may be
            incomplete.
          </p>
        )}
        {history.records.length > 0 ? (
          <ol aria-label="Wayfinder review records" className="mt-3 space-y-3">
            {history.records.map((record) => (
              <li key={record.id} className="border-l border-rule pl-3 text-label text-fg-muted">
                <ReviewRecord record={record} />
              </li>
            ))}
          </ol>
        ) : (
          <p className="mt-3 text-label text-fg-faint">
            No annotations, answers, or approvals recorded.
          </p>
        )}
      </div>
    </section>
  );
}

function EntryKindButton({
  children,
  selected,
  onClick,
}: {
  children: string;
  selected: boolean;
  onClick: () => void;
}) {
  return (
    <button
      type="button"
      aria-pressed={selected}
      onClick={onClick}
      className={`rounded-sm border px-2 py-1 text-label focus-mark ${
        selected ? 'border-accent text-accent' : 'border-rule text-fg-muted hover:text-fg'
      }`}
    >
      {children}
    </button>
  );
}

function ReviewRecord({ record }: { record: WayfinderReviewRecord }) {
  const label =
    record.kind === 'annotation'
      ? 'Annotation'
      : record.kind === 'answer'
        ? 'Answer'
        : 'Explicit approval';

  return (
    <article>
      <div className="flex flex-wrap items-baseline gap-x-2">
        <span className="font-semibold text-fg">{label}</span>
        <span>{record.actor}</span>
        <time dateTime={record.recorded_at}>{formatRecordedAt(record.recorded_at)}</time>
      </div>
      {record.kind === 'annotation' && <p className="mt-1 whitespace-pre-wrap">{record.text}</p>}
      {record.kind === 'answer' && (
        <div className="mt-1 space-y-1">
          <p>
            <span className="text-fg">Prompt: </span>
            {record.prompt}
          </p>
          <p>
            <span className="text-fg">Answer: </span>
            {record.answer}
          </p>
        </div>
      )}
      {record.kind === 'approval' && (
        <p className="mt-1">
          Explicitly approved: <span className="text-fg">{record.scope}</span>
        </p>
      )}
    </article>
  );
}

function formatRecordedAt(value: string): string {
  return new Intl.DateTimeFormat(undefined, {
    dateStyle: 'medium',
    timeStyle: 'short',
  }).format(new Date(value));
}
