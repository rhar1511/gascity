import { activeCityOrThrow } from '../api/cityBase';
import {
  WAYFINDER_REVIEW_EVENT_PREFIX,
  createWayfinderReviewRecord,
  type WayfinderReviewDraft,
  type WayfinderReviewRecord,
} from '../lib/wayfinderReviewLog';
import { supervisorApi } from './client';

/** Persists one review entry as a new, typed metadata key on the selected Bead. */
export async function recordWayfinderReviewEntry(
  beadId: string,
  actor: string,
  draft: WayfinderReviewDraft,
): Promise<WayfinderReviewRecord> {
  const normalizedBeadId = beadId.trim();
  if (normalizedBeadId.length === 0) throw new Error('a Bead ID is required to record review');

  const record = createWayfinderReviewRecord(actor, draft);
  const key = `${WAYFINDER_REVIEW_EVENT_PREFIX}${record.id}`;
  await supervisorApi().updateBead(activeCityOrThrow('record Wayfinder review'), normalizedBeadId, {
    metadata: { [key]: JSON.stringify(record) },
  });
  return record;
}
