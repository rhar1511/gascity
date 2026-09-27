export const WAYFINDER_REVIEW_EVENT_PREFIX = 'gc.wayfinder_review.event.';

interface WayfinderReviewRecordBase {
  version: 1;
  id: string;
  actor: string;
  recorded_at: string;
}

export type WayfinderReviewRecord =
  | (WayfinderReviewRecordBase & { kind: 'annotation'; text: string })
  | (WayfinderReviewRecordBase & { kind: 'answer'; prompt: string; answer: string })
  | (WayfinderReviewRecordBase & {
      kind: 'approval';
      target: string;
      revision: string;
      scope: string;
      explicitly_confirmed: true;
    });

export interface WayfinderReviewHistory {
  records: WayfinderReviewRecord[];
  unreadableCount: number;
}

export type WayfinderReviewDraft =
  | { kind: 'annotation'; text: string }
  | { kind: 'answer'; prompt: string; answer: string }
  | {
      kind: 'approval';
      target: string;
      revision: string;
      scope: string;
      explicitly_confirmed: true;
    };

export interface WayfinderReviewRecordOptions {
  id?: string;
  recordedAt?: string;
}

/** Reads the typed review records stored as individual entries in Bead metadata. */
export function readWayfinderReviewHistory(
  metadata: Readonly<Record<string, string>> | undefined,
): WayfinderReviewHistory {
  const records: WayfinderReviewRecord[] = [];
  let unreadableCount = 0;

  for (const [key, value] of Object.entries(metadata ?? {})) {
    if (!key.startsWith(WAYFINDER_REVIEW_EVENT_PREFIX)) continue;
    const record = parseRecord(value);
    if (record === null || key !== `${WAYFINDER_REVIEW_EVENT_PREFIX}${record.id}`) {
      unreadableCount += 1;
      continue;
    }
    records.push(record);
  }

  records.sort((left, right) => {
    const byDate = right.recorded_at.localeCompare(left.recorded_at);
    return byDate === 0 ? right.id.localeCompare(left.id) : byDate;
  });
  return { records, unreadableCount };
}

/** Creates a validated review record with an operator and timestamp for Bead metadata. */
export function createWayfinderReviewRecord(
  actor: string,
  draft: WayfinderReviewDraft,
  options: WayfinderReviewRecordOptions = {},
): WayfinderReviewRecord {
  const normalizedActor = requiredText(actor, 'operator', 256);
  const id = options.id ?? globalThis.crypto.randomUUID();
  const recordedAt = options.recordedAt ?? new Date().toISOString();
  if (!/^[A-Za-z0-9_-]+$/.test(id)) throw new Error('review record ID is invalid');
  if (!Number.isFinite(Date.parse(recordedAt))) throw new Error('review record time is invalid');

  const base: WayfinderReviewRecordBase = {
    version: 1,
    id,
    actor: normalizedActor,
    recorded_at: new Date(recordedAt).toISOString(),
  };
  if (draft.kind === 'annotation') {
    return { ...base, kind: 'annotation', text: requiredText(draft.text, 'annotation', 4000) };
  }
  if (draft.kind === 'answer') {
    return {
      ...base,
      kind: 'answer',
      prompt: requiredText(draft.prompt, 'prompt', 2000),
      answer: requiredText(draft.answer, 'answer', 4000),
    };
  }
  if (draft.kind === 'approval') {
    if (draft.explicitly_confirmed !== true) {
      throw new Error('explicit confirmation is required to record approval');
    }
    const target = requiredText(draft.target, 'artifact target', 1000);
    const revision = requiredText(draft.revision, 'artifact revision', 256);
    const scope = requiredText(draft.scope, 'approval scope', 2000);
    if (containsLocalReviewSessionUrl(`${target}\n${revision}\n${scope}`)) {
      throw new Error('approval must not include a local review session URL');
    }
    return {
      ...base,
      kind: 'approval',
      target,
      revision,
      scope,
      explicitly_confirmed: true,
    };
  }
  throw new Error('review record type is invalid');
}

function parseRecord(value: string): WayfinderReviewRecord | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(value);
  } catch {
    return null;
  }
  if (!isRecord(parsed) || parsed.version !== 1) return null;
  if (!isNonEmptyString(parsed.id) || !isNonEmptyString(parsed.actor)) return null;
  if (!isNonEmptyString(parsed.recorded_at) || !Number.isFinite(Date.parse(parsed.recorded_at))) {
    return null;
  }

  const base: WayfinderReviewRecordBase = {
    version: 1,
    id: parsed.id,
    actor: parsed.actor,
    recorded_at: parsed.recorded_at,
  };
  if (parsed.kind === 'annotation' && isNonEmptyString(parsed.text)) {
    return { ...base, kind: 'annotation', text: parsed.text };
  }
  if (
    parsed.kind === 'answer' &&
    isNonEmptyString(parsed.prompt) &&
    isNonEmptyString(parsed.answer)
  ) {
    return { ...base, kind: 'answer', prompt: parsed.prompt, answer: parsed.answer };
  }
  if (
    parsed.kind === 'approval' &&
    isNonEmptyString(parsed.target) &&
    isNonEmptyString(parsed.revision) &&
    isNonEmptyString(parsed.scope) &&
    parsed.explicitly_confirmed === true
  ) {
    return {
      ...base,
      kind: 'approval',
      target: parsed.target,
      revision: parsed.revision,
      scope: parsed.scope,
      explicitly_confirmed: true,
    };
  }
  return null;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function isNonEmptyString(value: unknown): value is string {
  return typeof value === 'string' && value.trim().length > 0;
}

function requiredText(value: string, label: string, maxLength: number): string {
  const trimmed = value.trim();
  if (trimmed.length === 0) throw new Error(`${label} is required`);
  if (trimmed.length > maxLength)
    throw new Error(`${label} must be ${maxLength} characters or fewer`);
  return trimmed;
}

function containsLocalReviewSessionUrl(value: string): boolean {
  const urls = value.match(/https?:\/\/[^\s<>()]+/gi) ?? [];
  return urls.some((candidate) => {
    try {
      const hostname = new URL(candidate).hostname.toLowerCase().replace(/^\[|\]$/g, '');
      return (
        hostname === 'localhost' ||
        hostname.endsWith('.localhost') ||
        hostname === '::1' ||
        /^127(?:\.\d{1,3}){3}$/.test(hostname)
      );
    } catch {
      return false;
    }
  });
}
