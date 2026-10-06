import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setActiveCity } from '../api/cityBase';
import { resetSupervisorApiForTests } from '../supervisor/client';
import type { ExecutionAttempt } from '../lib/workbenchAttempts';
import { AttemptChatPanel } from './AttemptChatPanel';

const attempt: ExecutionAttempt = {
  sessionId: 'session-same',
  sessionName: 'worker',
  workDir: '/work/one',
  state: 'active',
  active: true,
  startedAt: '2026-09-27T00:00:00Z',
  lastActive: '2026-09-27T00:00:00Z',
  executionGeneration: 7,
};

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((resolvePromise) => {
    resolve = resolvePromise;
  });
  return { promise, resolve };
}

describe('AttemptChatPanel city scoping', () => {
  beforeEach(() => {
    setActiveCity('city-a');
    window.localStorage.clear();
    resetSupervisorApiForTests();
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
    resetSupervisorApiForTests();
  });

  it('holds messages when the selected work has no claim generation', () => {
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
    render(<AttemptChatPanel attempt={attempt} workId="work-1" claimGeneration={null} />);
    expect(screen.getByLabelText('Message')).toHaveProperty('disabled', true);
    expect(screen.getByRole('button', { name: /send/i })).toHaveProperty('disabled', true);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('does not accept a same-session receipt attributed to a different claim', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const request = input instanceof Request ? input : new Request(input, init);
        const body = await request.clone().json();
        return new Response(
          JSON.stringify({
            request_id: body.request_id,
            session_id: attempt.sessionId,
            generation: attempt.executionGeneration,
            accepted_at: '2026-09-27T00:01:00Z',
            delivery: 'pending',
            effect: 'pending',
            message_digest: 'sha256:abc',
            attempt: { identity: { owner_bead_id: 'work-1', claim_generation: 'claim-8' } },
          }),
          { status: 202, headers: { 'Content-Type': 'application/json' } },
        );
      }),
    );
    render(<AttemptChatPanel attempt={attempt} workId="work-1" claimGeneration="claim-7" />);
    fireEvent.change(screen.getByLabelText('Message'), { target: { value: 'continue work one' } });
    fireEvent.click(screen.getByRole('button', { name: /send/i }));
    await screen.findByText(/receipt for a different session request/i);
    expect(screen.getByLabelText('Session request receipts').textContent).toContain(
      'Server acceptance: not confirmed',
    );
  });

  it('does not show a delayed same-session receipt in a different city', async () => {
    const cityAResponse = deferred<Response>();
    const acceptedAt = '2026-09-27T00:01:00Z';
    let cityARequestBody: { generation: number; message: string; request_id: string } | undefined;
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = input instanceof Request ? input : new Request(input, init);
      const url = new URL(request.url);
      if (
        url.pathname === '/v0/city/city-a/session/session-same/requests' &&
        request.method === 'POST'
      ) {
        cityARequestBody = (await request.clone().json()) as typeof cityARequestBody;
        return cityAResponse.promise;
      }
      throw new Error(`unexpected request: ${request.method} ${url.pathname}`);
    });
    vi.stubGlobal('fetch', fetchMock);

    const chat = (city: string) => {
      setActiveCity(city);
      return (
        <AttemptChatPanel
          key={`${city}:${attempt.sessionId}`}
          attempt={attempt}
          workId="work-1"
          claimGeneration="claim-7"
        />
      );
    };
    const { rerender } = render(chat('city-a'));
    fireEvent.change(screen.getByLabelText('Message'), { target: { value: 'finish the review' } });
    fireEvent.click(screen.getByRole('button', { name: /send/i }));
    await waitFor(() => expect(cityARequestBody?.message).toBe('finish the review'));
    expect(cityARequestBody).toMatchObject({ work_id: 'work-1', claim_generation: 'claim-7' });

    rerender(chat('city-b'));
    const cityBReceipts = screen.getByLabelText('Session request receipts');
    expect(cityBReceipts.textContent).not.toContain('finish the review');

    await act(async () => {
      cityAResponse.resolve(
        new Response(
          JSON.stringify({
            accepted_at: acceptedAt,
            delivery: 'pending',
            effect: 'pending',
            generation: 7,
            message_digest: 'sha256:abc',
            request_id: cityARequestBody?.request_id,
            session_id: 'session-same',
            attempt: {
              attempt_id: 'ae-1',
              identity: {
                kind: 'workbench',
                owner_bead_id: 'work-1',
                claim_generation: 'claim-7',
                session_id: 'session-same',
                session_generation: '7',
              },
              store_ref: 'rig:test',
              work_revision: '1',
            },
          }),
          { status: 202, headers: { 'Content-Type': 'application/json' } },
        ),
      );
      await cityAResponse.promise;
      await Promise.resolve();
    });

    await waitFor(() => {
      const stored = Array.from({ length: window.localStorage.length }, (_, index) =>
        window.localStorage.getItem(window.localStorage.key(index) ?? ''),
      );
      expect(stored.some((value) => value?.includes(acceptedAt))).toBe(true);
    });
    expect(cityBReceipts.textContent).not.toContain('finish the review');
    expect(cityBReceipts.textContent).not.toContain(acceptedAt);

    rerender(chat('city-a'));
    expect(await screen.findByText('finish the review')).toBeTruthy();
    expect(screen.getByLabelText('Session request receipts').textContent).toContain(acceptedAt);
  });
});
