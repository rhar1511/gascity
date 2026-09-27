import type { RequestReceipt } from 'gas-city-dashboard-shared/gc-supervisor';

const STORAGE_PREFIX = 'gascity:workbench:session-request:v1:';

export interface SessionRequestIntent {
  city: string;
  sessionId: string;
  requestId: string;
  generation: number;
  message: string;
  receipt?: RequestReceipt;
  submissionError?: string | undefined;
}

export interface SessionRequestStorage {
  readonly length: number;
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
  key(index: number): string | null;
}

export function createSessionRequestIntent(
  storage: SessionRequestStorage,
  city: string,
  sessionId: string,
  generation: number,
  message: string,
  createRequestId: () => string = newRequestId,
): SessionRequestIntent {
  if (!Number.isSafeInteger(generation) || generation <= 0) {
    throw new Error('The current execution generation is unavailable; the message was not sent.');
  }
  const cleanMessage = message.trim();
  if (cleanMessage.length === 0) throw new Error('Message cannot be empty.');
  const requestId = createRequestId();
  const intent: SessionRequestIntent = {
    city,
    sessionId,
    requestId,
    generation,
    message: cleanMessage,
  };
  const key = storageKey(city, sessionId, requestId);
  const encoded = JSON.stringify(intent);
  storage.setItem(key, encoded);
  if (storage.getItem(key) !== encoded) {
    throw new Error('Could not persist the exact session request; the message was not sent.');
  }
  return intent;
}

export function readSessionRequestIntents(
  storage: SessionRequestStorage,
  city: string,
  sessionId: string,
): SessionRequestIntent[] {
  const prefix =
    STORAGE_PREFIX + encodeURIComponent(city) + ':' + encodeURIComponent(sessionId) + ':';
  const intents: SessionRequestIntent[] = [];
  for (let i = 0; i < storage.length; i += 1) {
    const key = storage.key(i);
    if (key === null || !key.startsWith(prefix)) continue;
    const encoded = storage.getItem(key);
    if (encoded === null) continue;
    const value = decodeIntent(encoded);
    if (
      value.city === city &&
      value.sessionId === sessionId &&
      key === storageKey(city, sessionId, value.requestId)
    ) {
      intents.push(value);
    }
  }
  return intents.sort((a, b) => a.requestId.localeCompare(b.requestId));
}

export function saveSessionRequestIntent(
  storage: SessionRequestStorage,
  intent: SessionRequestIntent,
): void {
  const key = storageKey(intent.city, intent.sessionId, intent.requestId);
  const encoded = JSON.stringify(intent);
  storage.setItem(key, encoded);
  if (storage.getItem(key) !== encoded) {
    throw new Error('Could not persist the session request receipt.');
  }
}

function storageKey(city: string, sessionId: string, requestId: string): string {
  return (
    STORAGE_PREFIX +
    encodeURIComponent(city) +
    ':' +
    encodeURIComponent(sessionId) +
    ':' +
    encodeURIComponent(requestId)
  );
}

function decodeIntent(encoded: string): SessionRequestIntent {
  let value: unknown;
  try {
    value = JSON.parse(encoded);
  } catch {
    throw new Error('Stored session request is unreadable.');
  }
  if (
    typeof value !== 'object' ||
    value === null ||
    !('city' in value) ||
    typeof value.city !== 'string' ||
    !('sessionId' in value) ||
    typeof value.sessionId !== 'string' ||
    !('requestId' in value) ||
    typeof value.requestId !== 'string' ||
    !('generation' in value) ||
    typeof value.generation !== 'number' ||
    !('message' in value) ||
    typeof value.message !== 'string'
  ) {
    throw new Error('Stored session request is malformed.');
  }
  if (!Number.isSafeInteger(value.generation) || value.generation <= 0) {
    throw new Error('Stored session request has an unsafe execution generation.');
  }
  return value as SessionRequestIntent;
}

function newRequestId(): string {
  const uuid = globalThis.crypto?.randomUUID?.();
  if (uuid === undefined) throw new Error('Secure request identifiers are unavailable.');
  return `wb-chat-${uuid}`;
}
