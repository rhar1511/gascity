import type { SupervisorBead } from '../supervisor/beadReads';
import { isAllowedPreviewUrl } from './workbenchPreview';

export const WAYFINDER_REVIEW_URL_KEY = 'gc.wayfinder_review_url';
export const PROTOTYPE_URL_KEY = 'gc.prototype_url';

export interface PrototypeVariant {
  label: 'A' | 'B' | 'C';
  url: string;
}

export interface WayfinderReview {
  eligible: boolean;
  mode: 'lavish' | 'conversation';
  reviewUrl: string | null;
  reviewLinkState: 'missing' | 'ready' | 'blocked';
  variants: PrototypeVariant[];
}

// A review URL is merely a link to an already-running, operator-owned local
// Lavish session. Workbench never launches, frames, polls, or shares it.
export function resolveLocalReviewUrl(raw: string): string | null {
  try {
    const url = new URL(raw);
    if (url.protocol !== 'http:' && url.protocol !== 'https:') return null;
    if (url.username || url.password) return null;
    if (!['localhost', '127.0.0.1', '[::1]'].includes(url.hostname.toLowerCase())) return null;
    return url.toString();
  } catch {
    return null;
  }
}

export function resolveWayfinderReview(bead: SupervisorBead): WayfinderReview {
  const metadata = (bead as { metadata?: Record<string, string> }).metadata ?? {};
  const optedIn =
    /\breview\s*:\s*lavish\s+axi\b/i.test(bead.description ?? '') ||
    metadata['gc.wayfinder_review_mode'] === 'lavish' ||
    metadata[WAYFINDER_REVIEW_URL_KEY] !== undefined ||
    metadata[PROTOTYPE_URL_KEY] !== undefined;
  const eligible =
    (bead.issue_type === 'epic' && optedIn) ||
    (Array.isArray(bead.labels) && bead.labels.includes('wayfinder:prototype'));
  const mode =
    metadata['gc.wayfinder_review_mode']?.toLowerCase() === 'lavish' ||
    /\breview\s*:\s*lavish\s+axi\b/i.test(bead.description ?? '')
      ? 'lavish'
      : 'conversation';
  const rawReviewUrl = (metadata[WAYFINDER_REVIEW_URL_KEY] ?? '').trim();
  const reviewUrl = rawReviewUrl ? resolveLocalReviewUrl(rawReviewUrl) : null;
  const rawPrototypeUrl = (metadata[PROTOTYPE_URL_KEY] ?? '').trim();
  const variants: PrototypeVariant[] = [];
  if (rawPrototypeUrl && isAllowedPreviewUrl(rawPrototypeUrl)) {
    const url = new URL(rawPrototypeUrl);
    if (!url.username && !url.password) {
      for (const label of ['A', 'B', 'C'] as const) {
        url.searchParams.set('variant', label);
        variants.push({ label, url: url.toString() });
      }
    }
  }
  return {
    eligible,
    mode,
    reviewUrl,
    reviewLinkState: rawReviewUrl ? (reviewUrl ? 'ready' : 'blocked') : 'missing',
    variants,
  };
}
