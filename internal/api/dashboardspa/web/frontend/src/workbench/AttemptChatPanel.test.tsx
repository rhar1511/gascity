import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setActiveCity } from '../api/cityBase';
import { resetSupervisorApiForTests } from '../supervisor/client';
import type { ExecutionAttempt } from '../lib/workbenchAttempts';
import { AttemptChatPanel } from './AttemptChatPanel';
import { createSessionRequestIntent } from './sessionRequestIntent';

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

  it('does not restore saved requests from a previous claim or execution generation', () => {
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
    createSessionRequestIntent(
      window.localStorage,
      'city-a',
      attempt.sessionId,
      7,
      'old claim message',
      () => 'old-claim',
      { workId: 'work-1', claimGeneration: 'claim-6' },
    );
    createSessionRequestIntent(
      window.localStorage,
      'city-a',
      attempt.sessionId,
      6,
      'old execution message',
      () => 'old-execution',
      { workId: 'work-1', claimGeneration: 'claim-7' },
    );
    createSessionRequestIntent(
      window.localStorage,
      'city-a',
      attempt.sessionId,
      7,
      'current message',
      () => 'current',
      { workId: 'work-1', claimGeneration: 'claim-7' },
    );
    const { rerender } = render(
      <AttemptChatPanel attempt={attempt} workId="work-1" claimGeneration="claim-7" />,
    );
    const receipts = screen.getByLabelText('Session request receipts');
    expect(receipts.textContent).toContain('current message');
    expect(receipts.textContent).not.toContain('old claim message');
    expect(receipts.textContent).not.toContain('old execution message');
    rerender(<AttemptChatPanel attempt={attempt} workId="work-1" claimGeneration="claim-8" />);
    expect(receipts.textContent).not.toContain('current message');
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

  it('does not accept a receipt with the right owner but a different execution Bead', async () => {
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
            attempt: {
              attempt_id: 'ae-exact',
              store_ref: 'city:city-a',
              work_revision: '1',
              identity: {
                kind: 'workbench',
                owner_bead_id: 'work-1',
                execution_bead_id: 'work-2',
                session_id: attempt.sessionId,
                session_generation: '7',
                claim_generation: 'claim-7',
              },
            },
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

  it('keeps a delayed receipt saved without displaying it under a newer claim', async () => {
    const response = deferred<Response>();
    let requestId: string | undefined;
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const request = input instanceof Request ? input : new Request(input, init);
        requestId = (await request.clone().json()).request_id;
        return response.promise;
      }),
    );
    const { rerender } = render(
      <AttemptChatPanel attempt={attempt} workId="work-1" claimGeneration="claim-7" />,
    );
    fireEvent.change(screen.getByLabelText('Message'), { target: { value: 'old in flight' } });
    fireEvent.click(screen.getByRole('button', { name: /send/i }));
    await waitFor(() => expect(requestId).toBeDefined());
    rerender(<AttemptChatPanel attempt={attempt} workId="work-1" claimGeneration="claim-8" />);
    await act(async () => {
      response.resolve(
        new Response(
          JSON.stringify({
            request_id: requestId,
            session_id: attempt.sessionId,
            generation: 7,
            accepted_at: '2026-09-27T00:01:00Z',
            delivery: 'pending',
            effect: 'pending',
            message_digest: 'sha256:abc',
            attempt: {
              attempt_id: 'ae-exact',
              store_ref: 'city:city-a',
              work_revision: '1',
              identity: {
                kind: 'workbench',
                owner_bead_id: 'work-1',
                execution_bead_id: 'work-1',
                session_id: attempt.sessionId,
                session_generation: '7',
                claim_generation: 'claim-7',
              },
            },
          }),
          { status: 202, headers: { 'Content-Type': 'application/json' } },
        ),
      );
      await response.promise;
    });
    await waitFor(() =>
      expect(window.localStorage.getItem(window.localStorage.key(0)!)).toContain(
        '2026-09-27T00:01:00Z',
      ),
    );
    expect(screen.getByLabelText('Session request receipts').textContent).not.toContain(
      'old in flight',
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
                execution_bead_id: 'work-1',
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
