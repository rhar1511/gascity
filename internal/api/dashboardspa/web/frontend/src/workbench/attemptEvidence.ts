import type { DiffSnapshot, Evidence } from 'gas-city-dashboard-shared/gc-supervisor';
import type { ExecutionAttempt } from '../lib/workbenchAttempts';

const MAX_ARCHIVED_DIFF_BYTES = 16 * 1024 * 1024;

/** Return only archives bound to this exact Workbench session incarnation. */
export function matchingWorkbenchEvidence(
  rows: readonly Evidence[],
  workID: string,
  attempt: ExecutionAttempt,
): Evidence[] {
  const generation = attempt.executionGeneration;
  if (
    workID.trim() === '' ||
    attempt.sessionId.trim() === '' ||
    generation === null ||
    !Number.isSafeInteger(generation) ||
    generation <= 0
  ) {
    return [];
  }
  const exactGeneration = String(generation);
  return rows.filter(
    (evidence) =>
      evidence.identity.kind === 'workbench' &&
      evidence.identity.owner_bead_id === workID &&
      evidence.identity.session_id === attempt.sessionId &&
      evidence.identity.session_generation === exactGeneration,
  );
}

/**
 * Decode only the sealed base-to-candidate commit delta. The mutable workspace
 * diff is a separate snapshot and is never used as reviewed revision evidence.
 */
export async function decodeCandidateCommitDiff(snapshot: DiffSnapshot): Promise<string> {
  if (snapshot.status !== 'available') {
    throw new Error(snapshot.reason || `archived commit diff is ${snapshot.status}`);
  }
  if (snapshot.source !== 'candidate_commit_delta') {
    throw new Error('archived diff is not a base-to-candidate commit delta');
  }
  if (snapshot.encoding !== 'gzip+json' || !snapshot.payload || !snapshot.sha256) {
    throw new Error('archived commit diff is missing its encoded payload or digest');
  }
  const expectedBytes = snapshot.uncompressed_bytes;
  if (
    expectedBytes === undefined ||
    !Number.isSafeInteger(expectedBytes) ||
    expectedBytes < 0 ||
    expectedBytes > MAX_ARCHIVED_DIFF_BYTES
  ) {
    throw new Error('archived commit diff has an invalid uncompressed size');
  }

  const compressed = decodeBase64(snapshot.payload);
  const digest = await globalThis.crypto.subtle.digest('SHA-256', compressed.buffer as ArrayBuffer);
  if (toHex(new Uint8Array(digest)) !== snapshot.sha256) {
    throw new Error('archived commit diff digest does not match its payload');
  }

  const compressedBody = new Response(compressed.buffer as ArrayBuffer).body;
  if (!compressedBody) throw new Error('archived commit diff could not be opened');
  const reader = compressedBody.pipeThrough(new DecompressionStream('gzip')).getReader();
  const chunks: Uint8Array[] = [];
  let byteCount = 0;
  while (true) {
    const next = await reader.read();
    if (next.done) break;
    byteCount += next.value.byteLength;
    if (byteCount > MAX_ARCHIVED_DIFF_BYTES) {
      await reader.cancel();
      throw new Error('archived commit diff exceeds the display size limit');
    }
    chunks.push(next.value);
  }
  if (byteCount !== expectedBytes) {
    throw new Error('archived commit diff size does not match its sealed record');
  }

  const decoded = new Uint8Array(byteCount);
  let offset = 0;
  for (const chunk of chunks) {
    decoded.set(chunk, offset);
    offset += chunk.byteLength;
  }
  let bundle: unknown;
  try {
    bundle = JSON.parse(new TextDecoder().decode(decoded));
  } catch {
    throw new Error('archived commit diff bundle is malformed');
  }
  if (
    typeof bundle !== 'object' ||
    bundle === null ||
    !('tracked_patch' in bundle) ||
    typeof bundle.tracked_patch !== 'string'
  ) {
    throw new Error('archived commit diff bundle has no tracked patch');
  }
  return new TextDecoder().decode(decodeBase64(bundle.tracked_patch));
}

function decodeBase64(value: string): Uint8Array {
  const binary = atob(value);
  return Uint8Array.from(binary, (character) => character.charCodeAt(0));
}

function toHex(bytes: Uint8Array): string {
  return Array.from(bytes, (byte) => byte.toString(16).padStart(2, '0')).join('');
}
