import { useState } from 'react';
import type { SupervisorBead } from '../supervisor/beadReads';
import { resolveLocalReviewUrl, resolveWayfinderReview } from '../lib/wayfinderReview';

// This panel is a read-only projection of the Bead's chosen review mode and
// published links. The HTML document and Lavish transcript are working
// material; Beads remains the map/decision authority.
export function WayfinderReviewPanel({ bead }: { bead: SupervisorBead }) {
  const [localUrl, setLocalUrl] = useState('');
  const review = resolveWayfinderReview(bead);
  const reviewUrl = review.reviewUrl ?? resolveLocalReviewUrl(localUrl.trim());
  if (!review.eligible) return null;

  return (
    <section aria-label="Wayfinder review" className="mt-5 border-t border-rule pt-4">
      <div className="mb-3 flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1">
        <h3 className="text-label font-semibold uppercase tracking-wider text-fg">
          Wayfinder review
        </h3>
        <span className="text-label text-fg-muted">
          {review.mode === 'lavish' ? 'Lavish selected · local review' : 'Conversation review'}
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
              No prototype variants published for this Bead. Review the real app route if the design
              depends on live data or navigation.
            </p>
          )}
        </li>
        <li>
          <span className="text-fg">02 · Annotate the document</span>
          {review.mode === 'lavish' ? (
            <div className="mt-1 space-y-2 text-label">
              {review.reviewLinkState === 'blocked' && (
                <p className="text-accent" role="alert">
                  Published review link is not a safe local URL; it cannot be opened here.
                </p>
              )}
              {review.reviewLinkState === 'missing' && (
                <p className="text-fg-faint">
                  No local review link published. Ask the owning agent to open the HTML with Lavish
                  on loopback, then paste its session URL here or publish it on this Bead.
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
              {localUrl.trim() && reviewUrl === null && (
                <p className="text-accent" role="alert">
                  Use a local HTTP(S) URL without credentials.
                </p>
              )}
              {reviewUrl && (
                <p>
                  <a
                    href={reviewUrl}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="text-accent underline decoration-rule underline-offset-4 focus-mark"
                  >
                    Open Lavish review ↗
                  </a>{' '}
                  in a separate tab. Feedback is read by the owning agent session, not this panel.
                </p>
              )}
            </div>
          ) : (
            <p className="text-label text-fg-faint">
              Review the map and answer its prompts in the active conversation.
            </p>
          )}
        </li>
        <li>
          <span className="text-fg">03 · Critique and iterate</span>
          <p className="text-label text-fg-faint">
            Ask for an Impeccable /critique of hierarchy, clarity, interaction, and responsive
            behavior. Revise the prototype and review document, then inspect them again.
          </p>
        </li>
        <li>
          <span className="text-fg">04 · Decide</span>
          <p className="text-label text-fg-faint">
            An annotation is feedback, not explicit approval. Confirm the named tickets before
            rollout; record answers and approval on the Beads map.
          </p>
        </li>
      </ol>
    </section>
  );
}
