import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { HistoricalAttemptArtifacts } from './HistoricalAttemptArtifacts';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json' },
  });
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('HistoricalAttemptArtifacts', () => {
  it('shows explicit unavailable states and never substitutes live diff content', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        jsonResponse({
          bead_id: 'bead-1',
          session_id: 'prior-session',
          association: { state: 'unavailable', reason: 'attempt_session_link_not_recorded' },
          diff: { state: 'unavailable', reason: 'historical_diff_not_recorded' },
          pull_request: { state: 'unavailable', reason: 'attempt_pr_state_not_recorded' },
        }),
      ),
    );

    render(
      <HistoricalAttemptArtifacts
        beadId="bead-1"
        sessionId="prior-session"
        sessionLabel="worker-old"
      />,
    );

    expect(await screen.findByText('worker-old')).toBeTruthy();
    expect(
      await screen.findByText('Gas City did not save a diff snapshot for this attempt.'),
    ).toBeTruthy();
    expect(screen.getByText('Gas City did not save PR state for this attempt.')).toBeTruthy();
    expect(screen.queryByText(/current-only-change/)).toBeNull();
  });

  it('renders the diff and PR recorded for the selected historical session', async () => {
    const fetchMock = vi.fn(async () =>
      jsonResponse({
        bead_id: 'bead-1',
        session_id: 'prior-session',
        association: { state: 'available' },
        diff: { state: 'available', text: 'attempt-owned.diff\n+saved change', bytes: 19 },
        pull_request: {
          state: 'available',
          url: 'https://forge.example/pr/7',
          status: 'open',
        },
      }),
    );
    vi.stubGlobal('fetch', fetchMock);

    render(<HistoricalAttemptArtifacts beadId="bead-1" sessionId="prior-session" />);

    expect(
      await screen.findByText(
        (content, element) => element?.tagName === 'CODE' && content.includes('+saved change'),
      ),
    ).toBeTruthy();
    const link = await screen.findByRole('link', { name: 'https://forge.example/pr/7' });
    expect(link.getAttribute('href')).toBe('https://forge.example/pr/7');
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });
});
