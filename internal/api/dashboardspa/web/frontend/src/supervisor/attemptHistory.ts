import { activeCityOrThrow } from '../api/cityBase';
import { resolveSupervisorBaseUrl, supervisorUrl } from './url';

export type HistoricalArtifactState = 'available' | 'unavailable';

export interface HistoricalArtifactAvailability {
  state: HistoricalArtifactState;
  reason?: string;
}

export interface HistoricalAttemptDiff extends HistoricalArtifactAvailability {
  text?: string;
  truncated?: boolean;
  binary?: boolean;
  bytes?: number;
}

export interface HistoricalAttemptPullRequest extends HistoricalArtifactAvailability {
  url?: string;
  status?: string;
}

export interface HistoricalAttemptInspection {
  bead_id: string;
  session_id: string;
  association: HistoricalArtifactAvailability;
  diff: HistoricalAttemptDiff;
  pull_request: HistoricalAttemptPullRequest;
}

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
  const url = supervisorUrl(
    resolveSupervisorBaseUrl(),
    `/v0/city/${encodeURIComponent(city)}/bead/${encodeURIComponent(beadId)}/attempts/${encodeURIComponent(sessionId)}/history`,
  );
  const response = await fetch(url, {
    headers: { Accept: 'application/json' },
    ...(signal === undefined ? {} : { signal }),
  });
  if (!response.ok) {
    throw new Error(`history unavailable (${response.status})`);
  }
  const payload = (await response.json()) as
    | HistoricalAttemptInspection
    | { body?: HistoricalAttemptInspection };
  const inspection =
    'body' in payload && payload.body !== undefined
      ? payload.body
      : (payload as HistoricalAttemptInspection);
  if (inspection.bead_id !== beadId || inspection.session_id !== sessionId) {
    throw new Error('history response did not match the selected attempt');
  }
  return inspection;
}
