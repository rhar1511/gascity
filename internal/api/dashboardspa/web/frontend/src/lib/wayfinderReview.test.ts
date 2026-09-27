import { describe, expect, it } from 'vitest';
import type { SupervisorBead } from '../supervisor/beadReads';
import { resolveWayfinderReview } from './wayfinderReview';

const epic = {
  id: 'gc-map',
  issue_type: 'epic',
  description: '## Notes\n\nReview: Lavish AXI',
  labels: [],
} as unknown as SupervisorBead;

describe('resolveWayfinderReview', () => {
  it('recognizes an epic opting into Lavish without inventing a live session', () => {
    expect(resolveWayfinderReview(epic)).toEqual({
      eligible: true,
      mode: 'lavish',
      reviewUrl: null,
      reviewLinkState: 'missing',
      variants: [],
    });
  });

  it('exposes explicitly published A/B/C prototype variants and a local review link', () => {
    const result = resolveWayfinderReview({
      ...epic,
      metadata: {
        'gc.prototype_url': 'http://localhost:3000/prototype?bead=gc-map',
        'gc.wayfinder_review_url': 'http://127.0.0.1:4173/session/abc',
      },
    } as SupervisorBead);
    expect(result.variants.map((variant) => variant.label)).toEqual(['A', 'B', 'C']);
    expect(result.variants[0]?.url).toBe('http://localhost:3000/prototype?bead=gc-map&variant=A');
    expect(result.reviewLinkState).toBe('ready');
    expect(result.reviewUrl).toBe('http://127.0.0.1:4173/session/abc');
  });

  it('treats a published local review URL as Lavish mode without a separate flag', () => {
    const result = resolveWayfinderReview({
      ...epic,
      description: 'A Wayfinder map without a review-mode note yet.',
      metadata: { 'gc.wayfinder_review_url': 'http://localhost:4173/session/map' },
    } as SupervisorBead);
    expect(result.eligible).toBe(true);
    expect(result.mode).toBe('lavish');
    expect(result.reviewLinkState).toBe('ready');
  });

  it('rejects remote and credentialed review links and unsafe prototype URLs', () => {
    const result = resolveWayfinderReview({
      ...epic,
      metadata: {
        'gc.prototype_url': 'javascript:alert(1)',
        'gc.wayfinder_review_url': 'http://example.com/session/abc',
      },
    } as SupervisorBead);
    expect(result.variants).toEqual([]);
    expect(result.reviewLinkState).toBe('blocked');
    expect(result.reviewUrl).toBeNull();
  });

  it('does not turn ordinary tasks into a second review workflow', () => {
    expect(resolveWayfinderReview({ ...epic, issue_type: 'task' } as SupervisorBead).eligible).toBe(
      false,
    );
    expect(
      resolveWayfinderReview({
        ...epic,
        description: 'A regular epic without a Wayfinder review mode.',
      } as SupervisorBead).eligible,
    ).toBe(false);
  });
});
