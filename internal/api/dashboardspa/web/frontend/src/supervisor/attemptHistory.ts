import { activeCityOrThrow } from '../api/cityBase';
import type { AttemptInspection } from 'gas-city-dashboard-shared/gc-supervisor';
import { SupervisorApiError, supervisorApi } from './client';

export type HistoricalAttemptInspection = AttemptInspection;

/**
 * Fetch artifacts recorded for this exact execution session. This read never
 * falls back to the Bead's current worktree diff or current PR state.
 */
export async function fetchHistoricalAttemptInspection(
  beadId: string,
  sessionId: string,
  signal?: AbortSignal,
): Promise<HistoricalAttemptInspection> {
  const city = activeCityOrThrow('read historical attempt artifacts');
  let inspection: AttemptInspection;
  try {
    inspection = await supervisorApi().attemptHistory(city, beadId, sessionId, signal);
  } catch (error) {
    if (error instanceof SupervisorApiError && error.status !== undefined) {
      throw new Error(`history unavailable (${error.status})`, { cause: error });
    }
    throw error;
  }
  if (inspection.bead_id !== beadId || inspection.session_id !== sessionId) {
    throw new Error('history response did not match the selected attempt');
  }
  return inspection;
}
