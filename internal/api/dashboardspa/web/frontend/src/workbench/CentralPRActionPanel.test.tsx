import { act, cleanup, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { CentralPRActionPanel } from './CentralPRActionPanel';
import { resetSupervisorApiForTests } from '../supervisor/client';

const eventRefresh = vi.hoisted(() => ({ callback: null as (() => void) | null }));

vi.mock('../hooks/useGcEvents', () => ({
  useGcEventRefresh: (_prefixes: unknown, onMatch: () => void) => {
    eventRefresh.callback = onMatch;
    return 'closed';
  },
}));

function queue(policyVersion: string) {
  return {
    availability: 'ready',
    policy_state: 'ready',
    policy_version: policyVersion,
    observed_at: new Date().toISOString(),
    fresh_until: new Date(Date.now() + 60_000).toISOString(),
    sources: [],
    items: [],
  };
}

function response(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  });
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((resolvePromise) => {
    resolve = resolvePromise;
  });
  return { promise, resolve };
}

describe('CentralPRActionPanel city fencing', () => {
  beforeEach(() => {
    resetSupervisorApiForTests();
    eventRefresh.callback = null;
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
    resetSupervisorApiForTests();
  });

  it('does not let an old city queue response replace the current city queue', async () => {
    const cityA = deferred<Response>();
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(input instanceof Request ? input.url : String(input));
      if (url.pathname === '/v0/city/city-a/pr-actions/queue') return cityA.promise;
      if (url.pathname === '/v0/city/city-b/pr-actions/queue')
        return response(queue('policy-city-b'));
      throw new Error(`unexpected request: ${url.pathname}`);
    });
    vi.stubGlobal('fetch', fetchMock);

    const panel = (cityName: string) => (
      <CentralPRActionPanel key={cityName} cityName={cityName} beadId="work-1" />
    );
    const { rerender } = render(panel('city-a'));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));

    rerender(panel('city-b'));
    expect(await screen.findByText('policy-city-b')).toBeTruthy();

    cityA.resolve(response(queue('policy-city-a')));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    expect(screen.getByText('policy-city-b')).toBeTruthy();
    expect(screen.queryByText('policy-city-a')).toBeNull();
  });

  it('keeps the newest same-city queue when an older refresh returns last', async () => {
    const olderRefresh = deferred<Response>();
    let queueRequestCount = 0;
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(input instanceof Request ? input.url : String(input));
      if (url.pathname !== '/v0/city/city-a/pr-actions/queue') {
        throw new Error(`unexpected request: ${url.pathname}`);
      }
      queueRequestCount += 1;
      if (queueRequestCount === 1) return response(queue('policy-initial'));
      if (queueRequestCount === 2) return olderRefresh.promise;
      if (queueRequestCount === 3) return response(queue('policy-newest'));
      throw new Error('unexpected extra queue refresh');
    });
    vi.stubGlobal('fetch', fetchMock);

    render(<CentralPRActionPanel cityName="city-a" beadId="work-1" />);
    expect(await screen.findByText('policy-initial')).toBeTruthy();
    await waitFor(() => expect(eventRefresh.callback).not.toBeNull());

    act(() => eventRefresh.callback?.());
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    act(() => eventRefresh.callback?.());
    expect(await screen.findByText('policy-newest')).toBeTruthy();

    await act(async () => {
      olderRefresh.resolve(response(queue('policy-older')));
      await olderRefresh.promise;
      await Promise.resolve();
    });
    expect(screen.getByText('policy-newest')).toBeTruthy();
    expect(screen.queryByText('policy-older')).toBeNull();
  });
});
