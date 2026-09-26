import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { WorkbenchPage } from './Workbench';
import { setActiveCity } from '../api/cityBase';
import { invalidate } from '../api/cache';
import { NowProvider } from '../contexts/NowContext';
import type { SupervisorBead } from '../supervisor/beadReads';

const PROJECT = 'gascity';

type StubMode =
  | { kind: 'ok'; beads: SupervisorBead[] }
  | { kind: 'list-error' }
  | { kind: 'pending' };

let stubMode: StubMode = { kind: 'ok', beads: [sampleBead()] };
let updateMode: 'ok' | 'reject' = 'ok';
const supervisorWrites: Array<{ method: string; path: string; body?: unknown }> = [];

function setStub(mode: StubMode) {
  stubMode = mode;
}

beforeEach(() => {
  setActiveCity('test-city');
  supervisorWrites.length = 0;
  updateMode = 'ok';
  setStub({ kind: 'ok', beads: [sampleBead()] });
  invalidate('workbench:queue:');
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = parsedUrl(input);
      const method = requestMethod(input, init);
      const beadMatch = /^\/v0\/city\/test-city\/bead\/([^/]+)$/.exec(url.pathname);
      if (method !== 'GET') {
        let capturedBody: unknown;
        if (input instanceof Request) {
          try {
            capturedBody = JSON.parse(await input.clone().text());
          } catch {
            capturedBody = undefined;
          }
        } else {
          capturedBody = parseBody(init?.body);
        }
        supervisorWrites.push({ method, path: url.pathname, body: capturedBody });
        if (beadMatch && updateMode === 'ok') return jsonResponse({ ok: true });
        if (beadMatch && updateMode === 'reject') {
          return jsonResponse({ error: 'update rejected' }, { status: 409 });
        }
        return jsonResponse({ error: 'unexpected write' }, { status: 405 });
      }
      if (url.pathname === '/v0/city/test-city/beads' && method === 'GET') {
        if (stubMode.kind === 'pending') return new Promise<Response>(() => {});
        if (stubMode.kind === 'list-error') {
          return jsonResponse({ error: 'store unavailable' }, { status: 500 });
        }
        return jsonResponse(beadListPayload(stubMode.beads));
      }
      if (url.pathname === '/v0/city/test-city/sessions' && method === 'GET') {
        return jsonResponse({ items: [], total: 0 });
      }
      if (beadMatch) {
        const id = decodeURIComponent(beadMatch[1] ?? '');
        const bead =
          stubMode.kind === 'ok' ? stubMode.beads.find((candidate) => candidate.id === id) : undefined;
        if (bead) return jsonResponse(bead);
        return jsonResponse({ error: 'not found' }, { status: 404 });
      }
      throw new Error(`unexpected fetch: ${url.pathname}${url.search}`);
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('WorkbenchPage', () => {
  it('renders the work queue from real beads through the typed supervisor API', async () => {
    renderPage();

    await screen.findByRole('heading', { name: /^workbench$/i });
    expect(await screen.findByRole('list', { name: /work queue/i })).toBeTruthy();
    expect(await screen.findByText('Sample bead')).toBeTruthy();
  });

  it('opens the corresponding real bead when a work queue entry is selected', async () => {
    renderPage();

    await screen.findByText('Sample bead');
    fireEvent.click(screen.getByTitle('Open gascity-0001'));

    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText(`${PROJECT}-0001`)).toBeTruthy();
    expect(within(dialog).getByText('Sample bead description.')).toBeTruthy();
  });

  it('is read-only: renders no create, close, or claim controls and performs no writes', async () => {
    renderPage('/workbench?bead=gascity-0001');

    expect((await screen.findAllByText('Sample bead')).length).toBeGreaterThan(0);
    const dialog = await screen.findByRole('dialog');

    expect(screen.queryByRole('button', { name: /new bead/i })).toBeNull();
    expect(
      within(dialog)
        .getAllByRole('button', { name: /^close$/i })
        .filter((button) => button.textContent?.trim() === 'Close'),
    ).toHaveLength(0);
    expect(within(dialog).queryByRole('button', { name: /^claim$/i })).toBeNull();
    expect(supervisorWrites).toEqual([]);
  });

  it('covers the loading state while the queue fetch is in flight', async () => {
    setStub({ kind: 'pending' });
    renderPage();

    expect((await screen.findAllByText('Loading work queue.')).length).toBeGreaterThan(0);
  });

  it('covers the unavailable state when the bead store cannot be reached', async () => {
    setStub({ kind: 'list-error' });
    renderPage();

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toMatch(/unavailable/i);
    expect(screen.getByRole('button', { name: /retry/i })).toBeTruthy();
  });

  it('covers the unknown-bead state for a deep link to a bead that no longer exists', async () => {
    setStub({ kind: 'ok', beads: [] });
    renderPage('/workbench?bead=gascity-9999');

    const dialog = await screen.findByRole('dialog');
    expect(
      await within(dialog).findByText(/resolved or removed/i),
    ).toBeTruthy();
  });

  it('Kanban drag changes only the supported Bead status', async () => {
    renderPage();
    await screen.findByText('Sample bead');
    fireEvent.click(screen.getByRole('button', { name: /kanban/i }));

    const lane = await screen.findByTestId('lane-status:in_progress');
    fireEvent.drop(lane, { dataTransfer: dt(`${PROJECT}-0001`) });

    await waitFor(() => expect(supervisorWrites).toHaveLength(1));
    expect(supervisorWrites[0]?.method).not.toBe('GET');
    expect(supervisorWrites[0]?.path).toContain(`${PROJECT}-0001`);
    expect(supervisorWrites[0]?.body).toEqual({ status: 'in_progress' });
  });

  it('Priority drag changes only the supported Bead priority', async () => {
    renderPage();
    await screen.findByText('Sample bead');
    fireEvent.click(screen.getByRole('button', { name: /priority/i }));

    const lane = await screen.findByTestId('lane-priority:3');
    fireEvent.drop(lane, { dataTransfer: dt(`${PROJECT}-0001`) });

    await waitFor(() => expect(supervisorWrites).toHaveLength(1));
    expect(supervisorWrites[0]?.body).toEqual({ priority: 3 });
  });

  it('closing requires confirmation and cancellation leaves the Bead unchanged', async () => {
    renderPage();
    await screen.findByText('Sample bead');
    fireEvent.click(screen.getByRole('button', { name: /kanban/i }));

    fireEvent.click(await screen.findByRole('button', { name: /^close$/i }));
    const dialog = await screen.findByRole('alertdialog');
    expect(dialog).toBeTruthy();

    fireEvent.click(within(dialog).getByRole('button', { name: /cancel/i }));
    expect(supervisorWrites).toEqual([]);

    fireEvent.click(screen.getByRole('button', { name: /^close$/i }));
    fireEvent.click(
      within(await screen.findByRole('alertdialog')).getByRole('button', { name: /close bead/i }),
    );
    await waitFor(() => expect(supervisorWrites).toHaveLength(1));
    expect(supervisorWrites[0]?.body).toEqual({ status: 'closed' });
  });

  it('reverts the optimistic move and surfaces a server rejection', async () => {
    updateMode = 'reject';
    renderPage();
    await screen.findByText('Sample bead');
    fireEvent.click(screen.getByRole('button', { name: /kanban/i }));

    const lane = await screen.findByTestId('lane-status:blocked');
    fireEvent.drop(lane, { dataTransfer: dt(`${PROJECT}-0001`) });

    expect(await screen.findByText(/update rejected/i)).toBeTruthy();
    // The Bead reconverges on the authoritative read: it stays in its open lane.
    await waitFor(() =>
      expect(screen.getByTestId('lane-status:open').textContent).toContain('Sample bead'),
    );
  });
});

function dt(id: string): DataTransfer {
  const store = new Map<string, string>([['text/bead-id', id]]);
  return {
    getData: (key: string) => store.get(key) ?? '',
    setData: (key: string, value: string) => {
      store.set(key, value);
    },
  } as unknown as DataTransfer;
}

function parseBody(body: BodyInit | null | undefined): unknown {
  if (typeof body !== 'string') return undefined;
  try {
    return JSON.parse(body);
  } catch {
    return body;
  }
}

function renderPage(path = '/workbench') {
  return render(
    <MemoryRouter
      initialEntries={[path]}
      future={{ v7_relativeSplatPath: true, v7_startTransition: true }}
    >
      <NowProvider intervalMs={1_000_000}>
        <WorkbenchPage />
      </NowProvider>
    </MemoryRouter>,
  );
}

function jsonResponse(payload: unknown, init: ResponseInit = {}): Response {
  return new Response(JSON.stringify(payload), {
    status: init.status ?? 200,
    headers: { 'content-type': 'application/json' },
  });
}

function beadListPayload(items: ReadonlyArray<SupervisorBead>): {
  items: ReadonlyArray<SupervisorBead>;
  total: number;
} {
  return { items, total: items.length };
}

function sampleBead(): SupervisorBead {
  return {
    id: `${PROJECT}-0001`,
    title: 'Sample bead',
    description: 'Sample bead description.',
    status: 'open',
    priority: 0,
    issue_type: 'task',
    assignee: 'mayor',
    labels: [],
    created_at: '2026-01-01T00:00:00Z',
  };
}

function parsedUrl(input: RequestInfo | URL): URL {
  const value =
    input instanceof Request ? input.url : input instanceof URL ? input.toString() : String(input);
  return new URL(value, window.location.origin);
}

function requestMethod(input: RequestInfo | URL, init?: RequestInit): string {
  if (input instanceof Request) return input.method;
  return init?.method ?? 'GET';
}
