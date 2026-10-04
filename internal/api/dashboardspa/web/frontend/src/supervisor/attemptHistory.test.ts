import { afterEach, describe, expect, it, vi } from 'vitest';
import { fetchHistoricalAttemptInspection } from './attemptHistory';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json' },
  });
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('fetchHistoricalAttemptInspection', () => {
  it('requests the selected bead and session and returns only the attempt-scoped record', async () => {
    const inspection = {
      bead_id: 'bead/one',
      session_id: 'session two',
      association: { state: 'unavailable', reason: 'attempt_session_link_not_recorded' },
      diff: { state: 'unavailable', reason: 'historical_diff_not_recorded' },
      pull_request: { state: 'unavailable', reason: 'attempt_pr_state_not_recorded' },
    };
    const fetchMock = vi.fn(async () => jsonResponse(inspection));
    vi.stubGlobal('fetch', fetchMock);

    await expect(fetchHistoricalAttemptInspection('bead/one', 'session two')).resolves.toEqual(
      inspection,
    );
    expect(fetchMock).toHaveBeenCalledWith(
      'http://127.0.0.1/v0/city/test-city/bead/bead%2Fone/attempts/session%20two/history',
      expect.objectContaining({ headers: { Accept: 'application/json' } }),
    );
  });

  it('surfaces an API failure instead of falling back to the current attempt diff', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => jsonResponse({ error: 'unavailable' }, 503)),
    );

    await expect(fetchHistoricalAttemptInspection('bead-1', 'session-1')).rejects.toThrow(
      'history unavailable (503)',
    );
  });

  it('rejects an artifact response for a different bead or session', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        jsonResponse({
          bead_id: 'bead-1',
          session_id: 'current-session',
          association: { state: 'available' },
          diff: { state: 'available', text: 'wrong attempt' },
          pull_request: { state: 'unavailable' },
        }),
      ),
    );

    await expect(fetchHistoricalAttemptInspection('bead-1', 'prior-session')).rejects.toThrow(
      'history response did not match the selected attempt',
    );
  });
});
