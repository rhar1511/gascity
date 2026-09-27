import type {
  AttemptEvidenceRead,
  DiffSnapshot,
  Evidence,
  RequestReceipt,
} from 'gas-city-dashboard-shared/gc-supervisor';
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
 * Require the exact read to match both the selected immutable list row and
 * the Workbench session incarnation before displaying its later records.
 */
export function exactWorkbenchEvidenceMatches(
  read: AttemptEvidenceRead,
  listed: Evidence,
  workID: string,
  attempt: Pick<ExecutionAttempt, 'sessionId' | 'executionGeneration'>,
): boolean {
  const generation = attempt.executionGeneration;
  if (
    generation === null ||
    !Number.isSafeInteger(generation) ||
    generation <= 0 ||
    workID.trim() === '' ||
    attempt.sessionId.trim() === ''
  ) {
    return false;
  }
  const selectedIdentity = listed.identity;
  const readIdentity = read.identity;
  return (
    read.attempt_id === listed.attempt_id &&
    read.attempt_id.trim() !== '' &&
    readIdentity.kind === 'workbench' &&
    readIdentity.kind === selectedIdentity.kind &&
    readIdentity.owner_bead_id === workID &&
    readIdentity.owner_bead_id === selectedIdentity.owner_bead_id &&
    readIdentity.execution_bead_id === selectedIdentity.execution_bead_id &&
    readIdentity.session_id === attempt.sessionId &&
    readIdentity.session_id === selectedIdentity.session_id &&
    readIdentity.session_generation === String(generation) &&
    readIdentity.session_generation === selectedIdentity.session_generation &&
    readIdentity.claim_generation === selectedIdentity.claim_generation &&
    read.store_ref === listed.store_ref &&
    read.permission_scope.work_id === workID &&
    read.permission_scope.store_ref === read.store_ref &&
    read.base_sha === listed.base_sha &&
    read.candidate_sha === listed.candidate_sha &&
    read.diff.sha256 === listed.diff.sha256 &&
    read.diff.source === listed.diff.source &&
    read.working_tree_status === listed.working_tree_status
  );
}

/**
 * Return true only when a request receipt carries the same immutable execution
 * binding as the selected archive. The server performs the authoritative join;
 * this check keeps a malformed or mismatched response from being displayed as
 * an acknowledgement for the selected attempt.
 */
export function exactRequestReceiptMatchesWorkbenchEvidence(
  receipt: RequestReceipt,
  evidence: Evidence,
): boolean {
  const binding = receipt.attempt;
  if (!binding || !isCanonicalPositiveDecimal(binding.work_revision)) return false;

  const archived = evidence.identity;
  const bound = binding.identity;
  const generation = archived.session_generation;
  const claimGeneration = archived.claim_generation;
  if (
    archived.kind !== 'workbench' ||
    archived.owner_bead_id.trim() === '' ||
    archived.execution_bead_id.trim() === '' ||
    archived.session_id?.trim() === '' ||
    generation === undefined ||
    !isCanonicalPositiveDecimal(generation) ||
    claimGeneration === undefined ||
    claimGeneration.trim() === '' ||
    !Number.isSafeInteger(receipt.generation) ||
    receipt.generation <= 0 ||
    String(receipt.generation) !== generation
  ) {
    return false;
  }

  return (
    receipt.request_id.trim() !== '' &&
    receipt.session_id === archived.session_id &&
    binding.attempt_id === evidence.attempt_id &&
    binding.store_ref === evidence.store_ref &&
    binding.store_ref === evidence.permission_scope.store_ref &&
    bound.kind === archived.kind &&
    bound.owner_bead_id === archived.owner_bead_id &&
    bound.owner_bead_id === evidence.permission_scope.work_id &&
    bound.execution_bead_id === archived.execution_bead_id &&
    bound.session_id === archived.session_id &&
    bound.session_generation === archived.session_generation &&
    bound.claim_generation === archived.claim_generation
  );
}

function isCanonicalPositiveDecimal(value: string): boolean {
  if (!/^[1-9][0-9]*$/.test(value)) return false;
  const maxInt64 = '9223372036854775807';
  return value.length < maxInt64.length || (value.length === maxInt64.length && value <= maxInt64);
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
