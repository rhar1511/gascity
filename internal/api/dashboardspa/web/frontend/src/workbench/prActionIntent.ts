import type { WorkbenchPRActionBody } from '../supervisor/client';

const STORAGE_PREFIX = 'gascity:workbench:pr-action:v1:';
const IDEMPOTENCY_KEY_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._:-]{7,199}$/;

export interface PRActionIntent {
  city: string;
  request: WorkbenchPRActionBody;
  idempotencyKey: string;
}

export interface IntentStorage {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
}

export interface PRActionIntentStorage extends IntentStorage {
  readonly length: number;
  key(index: number): string | null;
}

/**
 * Reads or durably creates the exact request identity. The storage key binds
 * city plus every revision/evidence field, so retries after a lost response
 * reuse the original key and request. A changed revision is a different
 * intent, never an automatic retry of the old one.
 */
export function getOrCreatePRActionIntent(
  storage: IntentStorage,
  city: string,
  request: WorkbenchPRActionBody,
  createKey: () => string = newRequestKey,
): PRActionIntent {
  const storageKey = prActionIntentStorageKey(city, request);
  const encoded = storage.getItem(storageKey);
  if (encoded !== null) {
    const prior = decodeIntent(encoded);
    if (
      prior.city !== city ||
      canonicalRequest(prior.request) !== canonicalRequest(request) ||
      !IDEMPOTENCY_KEY_PATTERN.test(prior.idempotencyKey)
    ) {
      throw new Error('Stored PR action intent does not match the exact request.');
    }
    return prior;
  }

  const intent: PRActionIntent = {
    city,
    request: { ...request },
    idempotencyKey: createKey(),
  };
  if (!IDEMPOTENCY_KEY_PATTERN.test(intent.idempotencyKey)) {
    throw new Error('Unable to create a valid PR action idempotency key.');
  }
  const serialized = JSON.stringify(intent);
  storage.setItem(storageKey, serialized);
  const persisted = storage.getItem(storageKey);
  if (persisted !== serialized) {
    throw new Error('Could not persist the exact PR action intent; the request was not sent.');
  }
  return intent;
}

export function prActionIntentStorageKey(city: string, request: WorkbenchPRActionBody): string {
  return STORAGE_PREFIX + encodeURIComponent(JSON.stringify([city, canonicalRequest(request)]));
}

/** Restores saved exact requests for explicit retry and receipt reconciliation. */
export function readPRActionIntents(
  storage: PRActionIntentStorage,
  city: string,
): PRActionIntent[] {
  const intents: PRActionIntent[] = [];
  for (let index = 0; index < storage.length; index += 1) {
    const key = storage.key(index);
    if (key === null || !key.startsWith(STORAGE_PREFIX)) continue;
    const encoded = storage.getItem(key);
    if (encoded === null) continue;
    const intent = decodeIntent(encoded);
    if (intent.city === city && prActionIntentStorageKey(intent.city, intent.request) === key) {
      intents.push(intent);
    }
  }
  return intents.sort(
    (a, b) =>
      a.request.head_sha.localeCompare(b.request.head_sha) ||
      a.request.base_sha.localeCompare(b.request.base_sha) ||
      a.idempotencyKey.localeCompare(b.idempotencyKey),
  );
}

function canonicalRequest(request: WorkbenchPRActionBody): string {
  return JSON.stringify([
    request.action,
    request.monitor,
    request.owner,
    request.repo,
    request.pull_request,
    request.head_sha,
    request.base_sha,
    request.policy_version,
    request.work_id ?? '',
    request.attempt_id ?? '',
  ]);
}

function decodeIntent(encoded: string): PRActionIntent {
  let value: unknown;
  try {
    value = JSON.parse(encoded);
  } catch {
    throw new Error('Stored PR action intent is unreadable.');
  }
  if (
    typeof value !== 'object' ||
    value === null ||
    !('city' in value) ||
    typeof value.city !== 'string' ||
    !('request' in value) ||
    typeof value.request !== 'object' ||
    value.request === null ||
    !('idempotencyKey' in value) ||
    typeof value.idempotencyKey !== 'string'
  ) {
    throw new Error('Stored PR action intent is malformed.');
  }
  const intent = value as PRActionIntent;
  if (
    (intent.request.action !== 'prepare' && intent.request.action !== 'queue_review') ||
    typeof intent.request.monitor !== 'string' ||
    typeof intent.request.owner !== 'string' ||
    typeof intent.request.repo !== 'string' ||
    !Number.isSafeInteger(intent.request.pull_request) ||
    intent.request.pull_request <= 0 ||
    typeof intent.request.head_sha !== 'string' ||
    typeof intent.request.base_sha !== 'string' ||
    typeof intent.request.policy_version !== 'string' ||
    !IDEMPOTENCY_KEY_PATTERN.test(intent.idempotencyKey)
  ) {
    throw new Error('Stored PR action intent is malformed.');
  }
  if (
    intent.request.action === 'queue_review' &&
    (typeof intent.request.work_id !== 'string' || typeof intent.request.attempt_id !== 'string')
  ) {
    throw new Error('Stored PR review intent is missing its exact work or attempt reference.');
  }
  return intent;
}

function newRequestKey(): string {
  const uuid = globalThis.crypto?.randomUUID?.();
  if (uuid === undefined) {
    throw new Error('Secure request identifiers are unavailable; the PR action was not sent.');
  }
  return `wb-pr-${uuid}`;
}
