import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { WorkbenchPage } from './Workbench';
import { setActiveCity } from '../api/cityBase';
import { invalidate } from '../api/cache';
import { NowProvider } from '../contexts/NowContext';
import type { SupervisorBead } from '../supervisor/beadReads';
import type { WorkbenchPRActionBody } from '../supervisor/client';
import { createHash } from 'node:crypto';
import { gzipSync } from 'node:zlib';
import type {
  AttemptEvidenceRead,
  Evidence,
  HistoricalPrActionRecord,
  PrActionQueue,
  PrActionResult,
  RequestReceipt,
} from 'gas-city-dashboard-shared/gc-supervisor';
import { getOrCreatePRActionIntent } from '../workbench/prActionIntent';

const PROJECT = 'gascity';

type StubMode =
  | { kind: 'ok'; beads: SupervisorBead[] }
  | { kind: 'list-error' }
  | { kind: 'pending' };

let stubMode: StubMode = { kind: 'ok', beads: [sampleBead()] };
let updateMode: 'ok' | 'reject' = 'ok';
let stubSessions: Array<Record<string, unknown>> = [];
let queueReads: unknown[] = [];
let queueReadErrors = 0;
let prActionResponses: Array<{ status: number; body: unknown }> = [];
let sessionRequestResponses: Array<{ status: number; body: unknown }> = [];
let sessionRequestReceipt: RequestReceipt | null = null;
let attemptEvidenceRows: Evidence[] = [];
type AttemptEvidenceReadStub = { status: number; body: unknown } | (() => Promise<Response>);
let attemptEvidenceReadResponses = new Map<string, AttemptEvidenceReadStub>();
let readPaths: string[] = [];
const supervisorWrites: Array<{
  method: string;
  path: string;
  body?: unknown;
  headers: Record<string, string>;
}> = [];

function setStub(mode: StubMode) {
  stubMode = mode;
}

beforeEach(() => {
  setActiveCity('test-city');
  supervisorWrites.length = 0;
  updateMode = 'ok';
  stubSessions = [];
  queueReads = [unavailableQueue()];
  queueReadErrors = 0;
  prActionResponses = [];
  sessionRequestResponses = [];
  sessionRequestReceipt = null;
  attemptEvidenceRows = [];
  attemptEvidenceReadResponses = new Map();
  readPaths = [];
  window.localStorage.clear();
  setStub({ kind: 'ok', beads: [sampleBead()] });
  invalidate('workbench:queue:');
  invalidate('workbench:attempt-evidence:');
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = parsedUrl(input);
      const method = requestMethod(input, init);
      readPaths.push(url.pathname);
      const beadMatch = /^\/v0\/city\/test-city\/bead\/([^/]+)$/.exec(url.pathname);
      if (url.pathname === '/v0/city/test-city/pr-actions/queue' && method === 'GET') {
        if (queueReadErrors > 0) {
          queueReadErrors -= 1;
          return jsonResponse({ error: 'central queue unavailable' }, { status: 503 });
        }
        const next = queueReads.length > 1 ? queueReads.shift() : queueReads[0];
        return jsonResponse(next ?? unavailableQueue());
      }
      if (
        /^\/v0\/city\/test-city\/session\/[^/]+\/requests\/[^/]+$/.test(url.pathname) &&
        method === 'GET'
      ) {
        if (sessionRequestReceipt) return jsonResponse(sessionRequestReceipt);
        return jsonResponse({ error: 'not found' }, { status: 404 });
      }
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
        supervisorWrites.push({
          method,
          path: url.pathname,
          body: capturedBody,
          headers: requestHeaders(input, init),
        });
        if (url.pathname === '/v0/city/test-city/pr-actions') {
          const next = prActionResponses.shift();
          if (!next) return jsonResponse({ error: 'unexpected PR action' }, { status: 500 });
          if (next.status < 400 && typeof next.body !== 'function') {
            const exactRequest = (capturedBody ?? {}) as Record<string, unknown>;
            const result: Record<string, unknown> = {
              ...(next.body as Record<string, unknown>),
              ...exactRequest,
              idempotency_key: requestHeaders(input, init)['idempotency-key'],
              status: 'verified',
              outcome: exactRequest.action === 'queue_review' ? 'review_queued' : 'work_prepared',
            };
            if (exactRequest.attempt_id === undefined) delete result.attempt_id;
            return jsonResponse(result, { status: next.status });
          }
          const payload =
            typeof next.body === 'function'
              ? next.body(capturedBody, requestHeaders(input, init))
              : next.body;
          return jsonResponse(payload, { status: next.status });
        }
        if (/^\/v0\/city\/test-city\/session\/[^/]+\/requests$/.test(url.pathname)) {
          const next = sessionRequestResponses.shift();
          if (next) {
            if (next.status < 400) {
              const body = (capturedBody ?? {}) as { request_id?: string; generation?: number };
              const sessionId = url.pathname.split('/').at(-2) ?? '';
              const receipt = {
                ...(next.body as Record<string, unknown>),
                request_id: body.request_id,
                generation: body.generation,
                session_id: sessionId,
              } as RequestReceipt;
              sessionRequestReceipt = receipt;
              return jsonResponse(receipt, { status: next.status });
            }
            return jsonResponse(next.body, { status: next.status });
          }
          return jsonResponse({ error: 'unexpected session request' }, { status: 500 });
        }
        if (/\/mail$/.test(url.pathname)) return jsonResponse({ id: 'm-1' });
        if (/\/sling$/.test(url.pathname)) return jsonResponse({ ok: true });
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
        return jsonResponse({ items: stubSessions, total: stubSessions.length });
      }
      const transcriptMatch = /^\/v0\/city\/test-city\/session\/([^/]+)\/transcript$/.exec(
        url.pathname,
      );
      if (transcriptMatch && method === 'GET') {
        return jsonResponse({
          session_id: decodeURIComponent(transcriptMatch[1] ?? ''),
          format: 'conversation',
          turns: [],
        });
      }
      const exactEvidenceMatch =
        /^\/v0\/city\/test-city\/bead\/([^/]+)\/attempt-evidence\/([^/]+)$/.exec(url.pathname);
      if (exactEvidenceMatch && method === 'GET') {
        const workID = decodeURIComponent(exactEvidenceMatch[1] ?? '');
        const attemptID = decodeURIComponent(exactEvidenceMatch[2] ?? '');
        const stub = attemptEvidenceReadResponses.get(attemptID);
        if (typeof stub === 'function') return stub();
        if (stub) return jsonResponse(stub.body, { status: stub.status });
        const row = attemptEvidenceRows.find(
          (candidate) =>
            candidate.attempt_id === attemptID && candidate.identity.owner_bead_id === workID,
        );
        if (row) return jsonResponse(attemptEvidenceRead(row));
        return jsonResponse({ error: 'attempt evidence not found' }, { status: 404 });
      }
      if (
        /^\/v0\/city\/test-city\/bead\/[^/]+\/attempt-evidence$/.test(url.pathname) &&
        method === 'GET'
      ) {
        return jsonResponse(attemptEvidenceRows);
      }
      if (/\/attempts\/diff$/.test(url.pathname) && method === 'GET') {
        return jsonResponse({
          body: {
            worktree: '/wt/s-active',
            state: 'ok',
            text: 'diff --git a/a.txt b/a.txt\n-old\n+new\n',
            truncated: false,
            binary: false,
            bytes: 42,
          },
        });
      }
      if (beadMatch) {
        const id = decodeURIComponent(beadMatch[1] ?? '');
        const bead =
          stubMode.kind === 'ok'
            ? stubMode.beads.find((candidate) => candidate.id === id)
            : undefined;
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

  it('keeps bead details and attempt controls together without covering them in a modal', async () => {
    renderPage();

    await screen.findByText('Sample bead');
    fireEvent.click(screen.getByTitle('Open gascity-0001'));

    const detail = await screen.findByRole('region', { name: /selected bead/i });
    expect(within(detail).getByText(`${PROJECT}-0001`)).toBeTruthy();
    expect(within(detail).getByText('Sample bead description.')).toBeTruthy();
    expect(within(detail).getByLabelText('Execution attempt')).toBeTruthy();
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  it('is read-only: renders no create, close, or claim controls and performs no writes', async () => {
    renderPage('/workbench?bead=gascity-0001');

    expect((await screen.findAllByText('Sample bead')).length).toBeGreaterThan(0);
    const detail = await screen.findByRole('region', { name: /selected bead/i });

    expect(screen.queryByRole('button', { name: /new bead/i })).toBeNull();
    expect(within(detail).queryByRole('button', { name: /^close$/i })).toBeNull();
    expect(within(detail).queryByRole('button', { name: /^claim$/i })).toBeNull();
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

    const detail = await screen.findByRole('region', { name: /selected bead/i });
    expect(await within(detail).findByText(/resolved or removed/i)).toBeTruthy();
  });

  it('filters and orders the queue without changing the shared bead read', async () => {
    setStub({
      kind: 'ok',
      beads: [
        sampleBead(),
        {
          ...sampleBead(),
          id: 'gascity-0002',
          title: 'Other bead',
          priority: 0,
          status: 'closed',
        } as SupervisorBead,
      ],
    });
    renderPage();
    await screen.findByText('Other bead');
    fireEvent.change(screen.getByRole('searchbox', { name: /search workbench/i }), {
      target: { value: 'Sample' },
    });
    const queue = screen.getByRole('list', { name: /work queue/i });
    expect(within(queue).getByText('Sample bead')).toBeTruthy();
    expect(within(queue).queryByText('Other bead')).toBeNull();
    fireEvent.change(screen.getByRole('searchbox', { name: /search workbench/i }), {
      target: { value: '' },
    });
    fireEvent.change(screen.getByLabelText('Filter status'), { target: { value: 'closed' } });
    expect(within(queue).getByText('Other bead')).toBeTruthy();
    expect(within(queue).queryByText('Sample bead')).toBeNull();
  });

  it('groups work by the rig of its linked session without inventing an owner', async () => {
    setStub({
      kind: 'ok',
      beads: [sampleBead(), { ...sampleBead(), id: 'gascity-0002', title: 'Unlinked bead' }],
    });
    stubSessions = [
      {
        id: 's-old',
        session_name: 'worker-old',
        title: 'worker-old',
        template: 'worker',
        state: 'stopped',
        running: false,
        attached: false,
        provider: 'opencode',
        created_at: '2026-01-01T00:00:00Z',
        active_bead: 'gascity-0001',
        rig: 'legacy',
      },
      {
        id: 's-1',
        session_name: 'worker-1',
        title: 'worker-1',
        template: 'worker',
        state: 'active',
        running: true,
        attached: false,
        provider: 'opencode',
        created_at: '2026-01-02T00:00:00Z',
        active_bead: 'gascity-0001',
        rig: 'frontend',
      },
    ];
    renderPage();
    await screen.findByText('Sample bead');
    fireEvent.click(screen.getByRole('button', { name: /^rigs$/i }));
    expect(
      within(screen.getByRole('region', { name: 'Rig frontend' })).getByText('Sample bead'),
    ).toBeTruthy();
    expect(screen.queryByRole('region', { name: 'Rig legacy' })).toBeNull();
    expect(
      within(screen.getByRole('region', { name: 'No rig-linked session' })).getByText(
        'Unlinked bead',
      ),
    ).toBeTruthy();
  });

  it('shows only blocked or stale-session beads in Needs attention, respecting shared search', async () => {
    setStub({
      kind: 'ok',
      beads: [
        sampleBead(),
        { ...sampleBead(), id: 'gascity-0002', title: 'Blocked bead', status: 'blocked' },
        {
          ...sampleBead(),
          id: 'gascity-0003',
          title: 'Stale bead',
          metadata: { 'gc.session_id': 'missing' },
        },
      ],
    });
    renderPage();
    await screen.findByText('Sample bead');
    fireEvent.click(screen.getByRole('button', { name: /needs attention/i }));
    const attention = screen.getByRole('list', { name: /needs attention/i });
    expect(within(attention).getByText('Blocked bead')).toBeTruthy();
    expect(within(attention).getByText('Stale bead')).toBeTruthy();
    expect(within(attention).queryByText('Sample bead')).toBeNull();
    fireEvent.change(screen.getByRole('searchbox', { name: /search workbench/i }), {
      target: { value: 'Stale' },
    });
    expect(within(attention).queryByText('Blocked bead')).toBeNull();
    expect(within(attention).getByText('Stale bead')).toBeTruthy();
  });

  it('offers a keyboard and touch-friendly move control for board cards', async () => {
    renderPage();
    await screen.findByText('Sample bead');
    fireEvent.click(screen.getByRole('button', { name: /kanban/i }));
    fireEvent.change(screen.getByLabelText('Move gascity-0001 to status'), {
      target: { value: 'in_progress' },
    });
    await waitFor(() => expect(supervisorWrites[0]?.body).toEqual({ status: 'in_progress' }));
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

  it('shows the active Execution Attempt terminal for the selected Bead', async () => {
    stubSessions = [
      {
        id: 's-active',
        template: 'worker',
        session_name: 'worker-1',
        title: 'worker-1',
        state: 'active',
        running: true,
        attached: false,
        provider: 'opencode',
        created_at: '2026-01-02T00:00:00Z',
        active_bead: `${PROJECT}-0001`,
        work_dir: '/wt/s-active',
      },
    ];
    renderPage('/workbench?bead=gascity-0001');

    expect(await screen.findByText(/current attempt/i)).toBeTruthy();
    expect(screen.getByText('worker-1')).toBeTruthy();
  });

  it('shows the read-only worktree diff for the attempt (gp-466)', async () => {
    stubSessions = [
      {
        id: 's-active',
        template: 'worker',
        session_name: 'worker-1',
        title: 'worker-1',
        state: 'active',
        running: true,
        attached: false,
        provider: 'opencode',
        created_at: '2026-01-02T00:00:00Z',
        active_bead: `${PROJECT}-0001`,
        work_dir: '/wt/s-active',
      },
    ];
    renderPage('/workbench?bead=gascity-0001');

    const diff = await screen.findByLabelText('Attempt diff');
    expect(diff.textContent).toContain('+new');
    // Viewing the diff mutates nothing: no bead/worktree write is issued.
    expect(supervisorWrites.filter((w) => w.path.includes('/bead/'))).toEqual([]);
  });

  it('lets the operator inspect a prior attempt without offering live controls on it', async () => {
    stubSessions = [
      {
        id: 's-active',
        session_name: 'worker-1',
        state: 'active',
        running: true,
        active_bead: 'gascity-0001',
        created_at: '2026-01-02T00:00:00Z',
        execution_generation: 7,
      },
      {
        id: 's-old',
        session_name: 'worker-old',
        state: 'completed',
        running: false,
        active_bead: 'gascity-0001',
        created_at: '2026-01-01T00:00:00Z',
        execution_generation: 3,
      },
    ];
    attemptEvidenceRows = [
      workbenchEvidence({
        attemptId: 'ae-old-attempt',
        sessionId: 's-old',
        sessionGeneration: '3',
        baseSha: 'base-old',
        candidateSha: 'candidate-old',
        patch: 'diff --git a/old.txt b/old.txt\n+archived old revision\n',
        workspacePatch: 'mutable workspace edit must stay separate',
      }),
      workbenchEvidence({
        attemptId: 'ae-current-attempt',
        sessionId: 's-active',
        sessionGeneration: '7',
        baseSha: 'base-current',
        candidateSha: 'candidate-current',
        patch: 'diff --git a/current.txt b/current.txt\n+current revision\n',
      }),
    ];
    renderPage('/workbench?bead=gascity-0001');
    fireEvent.click(await screen.findByRole('button', { name: /inspect worker-old/i }));
    const panel = screen.getByLabelText('Execution attempt');
    expect(within(panel).getAllByText(/worker-old/).length).toBeGreaterThan(0);
    expect(within(panel).queryByRole('button', { name: /send/i })).toBeNull();
    expect(screen.queryByLabelText('Pull request actions')).toBeNull();
    const archive = await within(panel).findByLabelText('Archived attempt evidence');
    expect(readPaths).toContain('/v0/city/test-city/bead/gascity-0001/attempt-evidence');
    expect(archive.textContent).toContain('ae-old-attempt');
    expect(archive.textContent).toContain('base-old');
    expect(archive.textContent).toContain('candidate-old');
    expect(await within(archive).findByText(/\+archived old revision/)).toBeTruthy();
    expect(archive.textContent).not.toContain('mutable workspace edit must stay separate');
    const facets = within(archive).getByLabelText('Captured evidence facets');
    expect(facets.textContent).toContain('Acknowledgements');
    expect(facets.textContent).toContain('unavailable');
    expect(archive.textContent).toContain('no_server_proven_attempt_attribution');
    expect(readPaths.filter((path) => path.endsWith('/attempts/diff'))).toHaveLength(1);
  });

  it('requires an explicit choice when multiple archives match one exact session generation', async () => {
    stubSessions = [
      {
        id: 's-old',
        session_name: 'worker-old',
        state: 'completed',
        running: false,
        active_bead: 'gascity-0001',
        created_at: '2026-01-01T00:00:00Z',
        execution_generation: 3,
      },
    ];
    attemptEvidenceRows = [
      workbenchEvidence({
        attemptId: 'ae-choice-one',
        executionBeadId: 'attempt-row-one',
        sessionId: 's-old',
        sessionGeneration: '3',
        patch: 'first exact archive',
      }),
      workbenchEvidence({
        attemptId: 'ae-choice-two',
        executionBeadId: 'attempt-row-two',
        sessionId: 's-old',
        sessionGeneration: '3',
        patch: 'second exact archive',
      }),
    ];
    const selectedRow = attemptEvidenceRows[1];
    if (!selectedRow) throw new Error('selected archive fixture is missing');
    const mismatchedReceipt = historicalRequestReceipt(selectedRow, 'wrong-attempt-request');
    mismatchedReceipt.attempt = {
      ...mismatchedReceipt.attempt!,
      attempt_id: 'ae-choice-one',
    };
    attemptEvidenceReadResponses.set('ae-choice-two', {
      status: 200,
      body: attemptEvidenceRead(selectedRow, {
        actions: {
          status: 'available',
          records: [historicalActionRecord('ae-choice-two', 'selected-action-only')],
        },
        acknowledgements: {
          status: 'available',
          records: [
            historicalRequestReceipt(selectedRow, 'selected-attempt-request'),
            mismatchedReceipt,
          ],
          unattributed_requests: 1,
        },
      }),
    });
    renderPage('/workbench?bead=gascity-0001');
    fireEvent.click(await screen.findByRole('button', { name: /inspect worker-old/i }));
    const selector = await screen.findByLabelText('Choose archived attempt');
    expect(selector).toHaveProperty('value', '');
    expect(screen.queryByText('first exact archive')).toBeNull();
    fireEvent.change(selector, { target: { value: 'ae-choice-two' } });
    expect(await screen.findByText(/second exact archive/)).toBeTruthy();
    expect(screen.queryByText('first exact archive')).toBeNull();
    expect(await screen.findByText(/selected-action-only/)).toBeTruthy();
    const laterRecords = screen.getByLabelText('Later attempt records');
    expect(laterRecords.textContent).toContain('verified');
    expect(laterRecords.textContent).toContain('selected-action-only');
    expect(laterRecords.textContent).toContain('policy_verdict_not_captured');
    expect(laterRecords.textContent).toContain('selected-attempt-request');
    expect(laterRecords.textContent).toContain('Accepted at');
    expect(laterRecords.textContent).toContain('Delivery attempted at');
    expect(laterRecords.textContent).toContain('Provider result recorded at');
    expect(laterRecords.textContent).toContain('Acknowledged at');
    expect(laterRecords.textContent).toContain('Delivery: accepted');
    expect(laterRecords.textContent).toContain('Recorded effect: unverified');
    expect(laterRecords.textContent).toContain('work revision 15');
    expect(laterRecords.textContent).toContain('Unattributed requests: 1');
    expect(laterRecords.textContent).toContain('withheld because their exact attempt binding');
    expect(
      within(laterRecords).queryByLabelText('Session request receipt wrong-attempt-request'),
    ).toBeNull();
    expect(laterRecords.textContent).not.toContain('request body');
    expect(readPaths).toContain(
      '/v0/city/test-city/bead/gascity-0001/attempt-evidence/ae-choice-two',
    );
    expect(readPaths).not.toContain(
      '/v0/city/test-city/bead/gascity-0001/attempt-evidence/ae-choice-one',
    );
  });

  it('ignores an exact-read reply after switching to a different historical attempt', async () => {
    stubSessions = [
      {
        id: 's-active',
        session_name: 'worker-current',
        state: 'active',
        running: true,
        active_bead: 'gascity-0001',
        created_at: '2026-01-03T00:00:00Z',
        execution_generation: 7,
      },
      {
        id: 's-first',
        session_name: 'worker-first',
        state: 'completed',
        running: false,
        active_bead: 'gascity-0001',
        created_at: '2026-01-02T00:00:00Z',
        execution_generation: 3,
      },
      {
        id: 's-second',
        session_name: 'worker-second',
        state: 'completed',
        running: false,
        active_bead: 'gascity-0001',
        created_at: '2026-01-01T00:00:00Z',
        execution_generation: 2,
      },
    ];
    attemptEvidenceRows = [
      workbenchEvidence({
        attemptId: 'ae-first',
        sessionId: 's-first',
        sessionGeneration: '3',
        patch: 'first attempt archived diff',
      }),
      workbenchEvidence({
        attemptId: 'ae-second',
        sessionId: 's-second',
        sessionGeneration: '2',
        patch: 'second attempt archived diff',
      }),
    ];
    let resolveFirst!: (response: Response) => void;
    const delayedFirstRead = new Promise<Response>((resolve) => {
      resolveFirst = resolve;
    });
    const secondRow = attemptEvidenceRows[1];
    if (!secondRow) throw new Error('second archive fixture is missing');
    attemptEvidenceReadResponses.set('ae-first', () => delayedFirstRead);
    attemptEvidenceReadResponses.set('ae-second', {
      status: 200,
      body: attemptEvidenceRead(secondRow, {
        actions: {
          status: 'available',
          records: [historicalActionRecord('ae-second', 'second-attempt-action')],
        },
        acknowledgements: {
          status: 'available',
          records: [historicalRequestReceipt(secondRow, 'second-attempt-request')],
          unattributed_requests: 0,
        },
      }),
    });

    renderPage('/workbench?bead=gascity-0001');
    fireEvent.click(await screen.findByRole('button', { name: /inspect worker-first/i }));
    await waitFor(() => {
      expect(readPaths).toContain('/v0/city/test-city/bead/gascity-0001/attempt-evidence/ae-first');
    });
    fireEvent.click(screen.getByRole('button', { name: /inspect worker-second/i }));
    expect(await screen.findByText(/second-attempt-action/)).toBeTruthy();
    await act(async () => {
      resolveFirst(
        jsonResponse(
          attemptEvidenceRead(attemptEvidenceRows[0]!, {
            actions: {
              status: 'available',
              records: [historicalActionRecord('ae-first', 'stale-first-attempt-action')],
            },
            acknowledgements: {
              status: 'available',
              records: [
                historicalRequestReceipt(attemptEvidenceRows[0]!, 'stale-first-attempt-request'),
              ],
              unattributed_requests: 0,
            },
          }),
        ),
      );
      await delayedFirstRead;
    });

    expect(screen.getByText(/second-attempt-action/)).toBeTruthy();
    expect(screen.getByText(/second-attempt-request/)).toBeTruthy();
    expect(screen.queryByText('stale-first-attempt-action')).toBeNull();
    expect(screen.queryByText('stale-first-attempt-request')).toBeNull();
    expect(await screen.findByText(/second attempt archived diff/)).toBeTruthy();
  });

  it('keeps sealed capture visible while later action records are unreadable', async () => {
    stubSessions = [
      {
        id: 's-old',
        session_name: 'worker-old',
        state: 'completed',
        running: false,
        active_bead: 'gascity-0001',
        created_at: '2026-01-01T00:00:00Z',
        execution_generation: 3,
      },
    ];
    attemptEvidenceRows = [
      workbenchEvidence({
        attemptId: 'ae-ledger-unavailable',
        sessionId: 's-old',
        sessionGeneration: '3',
        patch: 'sealed diff remains available',
      }),
    ];
    const row = attemptEvidenceRows[0];
    if (!row) throw new Error('archive fixture is missing');
    attemptEvidenceReadResponses.set('ae-ledger-unavailable', {
      status: 200,
      body: attemptEvidenceRead(row, {
        actions: {
          status: 'unavailable',
          reason: 'action_ledger_validation_failed',
          records: [],
        },
        acknowledgements: {
          status: 'unavailable',
          reason: 'session_requests_lack_verified_work_attempt_attribution',
          records: [historicalRequestReceipt(row, 'receipt-with-unavailable-status')],
          unattributed_requests: 1,
        },
      }),
    });

    renderPage('/workbench?bead=gascity-0001');
    fireEvent.click(await screen.findByRole('button', { name: /inspect worker-old/i }));
    const archive = await screen.findByLabelText('Archived attempt evidence');
    expect(await within(archive).findByText(/sealed diff remains available/)).toBeTruthy();
    const laterRecords = within(archive).getByLabelText('Later attempt records');
    expect(laterRecords.textContent).toContain('unavailable');
    expect(laterRecords.textContent).toContain('action_ledger_validation_failed');
    expect(laterRecords.textContent).toContain(
      'session_requests_lack_verified_work_attempt_attribution',
    );
    expect(laterRecords.textContent).not.toContain('No PR action records');
    expect(laterRecords.textContent).not.toContain('No attributed session request receipts');
    expect(laterRecords.textContent).toContain(
      'were withheld because acknowledgement records are unavailable',
    );
    expect(laterRecords.textContent).not.toContain('receipt-with-unavailable-status');
    expect(laterRecords.textContent).not.toContain('Unattributed requests: 1');
  });

  it('keeps acknowledgement availability explicit for legacy exact reads without later-record fields', async () => {
    stubSessions = [
      {
        id: 's-legacy',
        session_name: 'worker-legacy',
        state: 'completed',
        running: false,
        active_bead: 'gascity-0001',
        created_at: '2026-01-01T00:00:00Z',
        execution_generation: 2,
      },
    ];
    attemptEvidenceRows = [
      workbenchEvidence({
        attemptId: 'ae-legacy-read',
        sessionId: 's-legacy',
        sessionGeneration: '2',
        patch: 'legacy exact archive remains visible',
      }),
    ];
    const row = attemptEvidenceRows[0];
    if (!row) throw new Error('legacy archive fixture is missing');
    attemptEvidenceReadResponses.set('ae-legacy-read', {
      status: 200,
      body: { ...row },
    });

    renderPage('/workbench?bead=gascity-0001');
    fireEvent.click(await screen.findByRole('button', { name: /inspect worker-legacy/i }));
    const archive = await screen.findByLabelText('Archived attempt evidence');
    expect(await within(archive).findByText(/legacy exact archive remains visible/)).toBeTruthy();
    const laterRecords = within(archive).getByLabelText('Later attempt records');
    expect(laterRecords.textContent).toContain('related_acknowledgements_not_returned');
    expect(laterRecords.textContent).not.toContain('No attributed session request receipts');
  });

  it('does not infer a missing session generation when locating historical evidence', async () => {
    stubSessions = [
      {
        id: 's-old',
        session_name: 'worker-old',
        state: 'completed',
        running: false,
        active_bead: 'gascity-0001',
        created_at: '2026-01-01T00:00:00Z',
      },
    ];
    attemptEvidenceRows = [
      workbenchEvidence({
        attemptId: 'ae-generation-required',
        sessionId: 's-old',
        sessionGeneration: '1',
        patch: 'must not be selected by a guessed generation',
      }),
    ];
    renderPage('/workbench?bead=gascity-0001');
    fireEvent.click(await screen.findByRole('button', { name: /inspect worker-old/i }));
    expect(await screen.findByText(/valid execution generation is unavailable/i)).toBeTruthy();
    expect(screen.queryByText('must not be selected by a guessed generation')).toBeNull();
  });

  it('does not offer PR actions without a Gas City queue, policy and conflict verdict', async () => {
    stubSessions = [
      {
        id: 's-active',
        session_name: 'worker-1',
        state: 'active',
        running: true,
        active_bead: 'gascity-0001',
        created_at: '2026-01-02T00:00:00Z',
      },
    ];
    renderPage('/workbench?bead=gascity-0001');
    const actions = await screen.findByLabelText('Pull request actions');
    expect(actions.textContent).toMatch(/unavailable/i);
    expect(within(actions).queryByRole('button', { name: /prepare pr|queue pr/i })).toBeNull();
    expect(supervisorWrites.some((write) => write.path.endsWith('/mail'))).toBe(false);
  });

  it('sends chat to the exact session generation and renders receipt facets separately', async () => {
    stubSessions = [
      {
        id: 's-active',
        template: 'worker',
        session_name: 'worker-1',
        title: 'worker-1',
        state: 'active',
        running: true,
        attached: false,
        provider: 'opencode',
        created_at: '2026-01-02T00:00:00Z',
        execution_generation: 7,
        active_bead: `${PROJECT}-0001`,
        work_dir: '/wt/s-active',
      },
    ];
    const receipt: RequestReceipt = {
      accepted_at: '2026-01-02T00:01:00Z',
      delivery: 'pending',
      effect: 'pending',
      generation: 7,
      message_digest: 'sha256:abcd',
      request_id: 'pending',
      session_id: 's-active',
    };
    sessionRequestResponses = [{ status: 202, body: receipt }];
    renderPage('/workbench?bead=gascity-0001');

    const input = await screen.findByLabelText('Message');
    expect(input).toHaveProperty('disabled', false);
    fireEvent.change(input, { target: { value: 'please continue' } });
    fireEvent.click(screen.getByRole('button', { name: /send/i }));

    const queue = await screen.findByLabelText('Session request receipts');
    await waitFor(() => expect(queue.textContent).toContain('Provider delivery'));
    const request = supervisorWrites.find((write) =>
      /\/session\/s-active\/requests$/.test(write.path),
    );
    expect(request?.body).toMatchObject({ generation: 7, message: 'please continue' });
    expect((request?.body as { request_id?: string })?.request_id).toMatch(/^wb-chat-/);
    expect(request?.path).toContain('/session/s-active/requests');
    expect(supervisorWrites.some((write) => write.path.includes('/ack'))).toBe(false);
    expect(supervisorWrites.some((write) => write.path.endsWith('/mail'))).toBe(false);
    expect(queue.textContent).toContain('Server acceptance');
    expect(queue.textContent).toContain('2026-01-02T00:01:00Z');
    expect(queue.textContent).toContain('Provider delivery');
    expect(queue.textContent).toContain('Session acknowledgement');
    expect(queue.textContent).toContain('Verified effect');
    expect(queue.textContent).toContain('pending');
    sessionRequestReceipt = {
      ...sessionRequestReceipt!,
      delivery: 'delivered',
      provider_result_at: '2026-01-02T00:01:01Z',
      acknowledged_at: '2026-01-02T00:01:02Z',
      effect: 'verified',
    };
    fireEvent.click(screen.getByRole('button', { name: /refresh receipt/i }));
    await waitFor(() => expect(queue.textContent).toContain('2026-01-02T00:01:01Z'));
    expect(queue.textContent).toContain('2026-01-02T00:01:02Z');
    expect(queue.textContent).toContain('verified');
  });

  it('does not submit chat when the server has no safe execution generation', async () => {
    stubSessions = [
      {
        id: 's-active',
        template: 'worker',
        session_name: 'worker-1',
        state: 'active',
        running: true,
        active_bead: `${PROJECT}-0001`,
        created_at: '2026-01-02T00:00:00Z',
      },
    ];
    renderPage('/workbench?bead=gascity-0001');
    const input = await screen.findByLabelText('Message');
    expect(input).toHaveProperty('disabled', true);
    expect(screen.getByRole('button', { name: /send/i })).toHaveProperty('disabled', true);
    expect(screen.getByText(/safe current execution generation/i)).toBeTruthy();
    expect(supervisorWrites.some((write) => write.path.includes('/requests'))).toBe(false);
  });

  it('retries an ambiguous session request with the same persisted request identity', async () => {
    stubSessions = [
      {
        id: 's-active',
        template: 'worker',
        session_name: 'worker-1',
        state: 'active',
        running: true,
        active_bead: `${PROJECT}-0001`,
        created_at: '2026-01-02T00:00:00Z',
        execution_generation: 7,
      },
    ];
    sessionRequestResponses = [
      { status: 503, body: { error: 'connection lost' } },
      {
        status: 202,
        body: {
          accepted_at: '2026-01-02T00:01:00Z',
          delivery: 'pending',
          effect: 'pending',
          generation: 7,
          message_digest: 'sha256:abcd',
          request_id: 'pending',
          session_id: 's-active',
        } satisfies RequestReceipt,
      },
    ];
    renderPage('/workbench?bead=gascity-0001');
    fireEvent.change(await screen.findByLabelText('Message'), {
      target: { value: 'retry this request' },
    });
    fireEvent.click(screen.getByRole('button', { name: /send/i }));
    await screen.findByText(/connection lost/i);
    await waitFor(() =>
      expect(supervisorWrites.filter((write) => write.path.endsWith('/requests'))).toHaveLength(1),
    );
    fireEvent.click(await screen.findByRole('button', { name: /retry exact request/i }));
    await waitFor(() =>
      expect(supervisorWrites.filter((write) => write.path.endsWith('/requests'))).toHaveLength(2),
    );
    const writes = supervisorWrites.filter((write) => write.path.endsWith('/requests'));
    expect(writes[0]?.body).toEqual(writes[1]?.body);
    expect(writes[0]?.body).toMatchObject({ generation: 7, message: 'retry this request' });
    expect((writes[0]?.body as { request_id?: string })?.request_id).toMatch(/^wb-chat-/);
    expect(supervisorWrites.some((write) => write.path.includes('/ack'))).toBe(false);
  });

  it('queues an exact server evidence ref and retries an ambiguous action with the same durable key', async () => {
    const multipleEvidence = readyQueue();
    multipleEvidence.items?.[0]?.attempt_evidence?.push({
      attempt_id: 'ae-other-immutable-attempt',
      base_sha: 'base-a',
      candidate_sha: 'candidate-a',
      diff_sha256: 'b'.repeat(64),
      diff_source: 'candidate_commit_delta',
      store_ref: 'rig:gascity',
      work_id: `${PROJECT}-0001`,
      working_tree_status: 'clean',
    });
    queueReads = [multipleEvidence];
    stubSessions = [
      {
        id: 's-active',
        template: 'worker',
        session_name: 'worker-1',
        state: 'active',
        running: true,
        active_bead: `${PROJECT}-0001`,
        created_at: '2026-01-02T00:00:00Z',
      },
    ];
    const result = reviewQueuedResult();
    prActionResponses = [
      { status: 503, body: { error: 'response lost' } },
      { status: 200, body: result },
    ];
    renderPage('/workbench?bead=gascity-0001');
    const attemptChoice = await screen.findByLabelText('Verified attempt for ricky/gascity#42');
    expect(screen.getByRole('button', { name: /queue exact revision for review/i })).toHaveProperty(
      'disabled',
      true,
    );
    fireEvent.change(attemptChoice, { target: { value: 'ae-immutable-server-attempt' } });
    const action = await screen.findByRole('button', { name: /queue exact revision for review/i });
    fireEvent.click(action);
    await waitFor(() =>
      expect(screen.getByLabelText('Pull request actions').textContent).toMatch(
        /outcome is not confirmed/i,
      ),
    );
    fireEvent.click(screen.getByRole('button', { name: /retry exact action/i }));
    await waitFor(() =>
      expect(screen.getByLabelText('Pull request actions').textContent).toMatch(
        /Exact queue_review for candidate-a against base-a: verified · review_queued/i,
      ),
    );
    const writes = supervisorWrites.filter(
      (write) => write.path === '/v0/city/test-city/pr-actions',
    );
    expect(writes).toHaveLength(2);
    expect(writes[0]?.body).toEqual(writes[1]?.body);
    expect(writes[0]?.headers['idempotency-key']).toMatch(/^wb-pr-/);
    expect(writes[0]?.headers['idempotency-key']).toBe(writes[1]?.headers['idempotency-key']);
    expect(writes[0]?.body).toMatchObject({
      action: 'queue_review',
      work_id: `${PROJECT}-0001`,
      attempt_id: 'ae-immutable-server-attempt',
      head_sha: 'candidate-a',
      base_sha: 'base-a',
    });
    expect((writes[0]?.body as { attempt_id: string }).attempt_id).not.toBe('s-active');
    expect(screen.queryByRole('button', { name: /merge/i })).toBeNull();
  });

  it('offers repair preparation only when the central queue authorizes it', async () => {
    const queue = readyQueue();
    const item = queue.items?.[0];
    if (!item) throw new Error('ready queue fixture has no item');
    item.work_records = [];
    item.attempt_evidence = [];
    item.actions = [
      {
        action: 'prepare',
        available: true,
        reason: 'trusted policy permits prepare',
        requires_human_approval: false,
      },
      {
        action: 'queue_review',
        available: false,
        reason: 'no prepared work record',
        requires_human_approval: false,
      },
    ];
    queueReads = [queue];
    prActionResponses = [{ status: 200, body: reviewQueuedResult() }];
    renderPage('/workbench?bead=gascity-0001');
    fireEvent.click(await screen.findByRole('button', { name: /prepare repair task/i }));
    await waitFor(() =>
      expect(screen.getByLabelText('Pull request actions').textContent).toMatch(/work_prepared/i),
    );
    const write = supervisorWrites.find(
      (candidate) => candidate.path === '/v0/city/test-city/pr-actions',
    );
    expect(write?.body).toEqual({
      action: 'prepare',
      monitor: 'monitor-a',
      owner: 'ricky',
      repo: 'gascity',
      pull_request: 42,
      head_sha: 'candidate-a',
      base_sha: 'base-a',
      policy_version: 'policy-7',
    });
  });

  it('refreshes a stale PR verdict without automatically submitting the newer revision', async () => {
    queueReads = [readyQueue(), readyQueue({ head: 'candidate-b', base: 'base-b' })];
    prActionResponses = [{ status: 409, body: { error: 'stale revision' } }];
    renderPage('/workbench?bead=gascity-0001');
    fireEvent.click(
      await screen.findByRole('button', { name: /queue exact revision for review/i }),
    );
    await waitFor(() =>
      expect(
        supervisorWrites.filter((write) => write.path === '/v0/city/test-city/pr-actions'),
      ).toHaveLength(1),
    );
    await waitFor(() =>
      expect(screen.getByLabelText('Pull request actions').textContent).toMatch(
        /review the new revision and submit it explicitly/i,
      ),
    );
    await waitFor(() => expect(screen.getByText('candidate-b')).toBeTruthy());
    const writes = supervisorWrites.filter(
      (write) => write.path === '/v0/city/test-city/pr-actions',
    );
    expect(writes).toHaveLength(1);
    expect(writes[0]?.body).toMatchObject({ head_sha: 'candidate-a', base_sha: 'base-a' });
  });

  it('keeps a saved exact action retry visible when the queue read fails after reload', async () => {
    const request: WorkbenchPRActionBody = {
      action: 'queue_review',
      monitor: 'monitor-a',
      owner: 'ricky',
      repo: 'gascity',
      pull_request: 42,
      work_id: `${PROJECT}-0001`,
      attempt_id: 'ae-immutable-server-attempt',
      head_sha: 'candidate-a',
      base_sha: 'base-a',
      policy_version: 'policy-7',
    };
    getOrCreatePRActionIntent(window.localStorage, 'test-city', request, () => 'wb-pr-reload-key');
    queueReadErrors = 1;
    prActionResponses = [{ status: 200, body: reviewQueuedResult() }];
    renderPage('/workbench?bead=gascity-0001');
    fireEvent.click(await screen.findByRole('button', { name: /retry exact action/i }));
    await waitFor(() =>
      expect(screen.getByLabelText('Pull request actions').textContent).toMatch(
        /Exact queue_review for candidate-a against base-a: verified · review_queued/i,
      ),
    );
    const write = supervisorWrites.find(
      (candidate) => candidate.path === '/v0/city/test-city/pr-actions',
    );
    expect(write?.body).toEqual(request);
    expect(write?.headers['idempotency-key']).toBe('wb-pr-reload-key');
  });

  it('offers an explicit start action when a Bead has no active attempt (gp-w3q)', async () => {
    setStub({
      kind: 'ok',
      beads: [
        {
          ...sampleBead(),
          status: 'open',
          metadata: { 'gc.routed_to': 'worker' },
        } as unknown as SupervisorBead,
      ],
    });
    renderPage('/workbench?bead=gascity-0001');

    const start = await screen.findByRole('button', { name: /start new attempt/i });
    fireEvent.click(start);
    fireEvent.click(start); // second click while in flight is ignored (idempotent)
    await waitFor(() => expect(supervisorWrites.some((w) => w.path.endsWith('/sling'))).toBe(true));
    await waitFor(() =>
      expect(supervisorWrites.filter((w) => w.path.endsWith('/sling'))).toHaveLength(1),
    );
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

function workbenchEvidence(options: {
  attemptId: string;
  sessionId: string;
  sessionGeneration: string;
  patch: string;
  workspacePatch?: string;
  baseSha?: string;
  candidateSha?: string;
  executionBeadId?: string;
}): Evidence {
  const compressPatch = (patch: string) => {
    const bundle = Buffer.from(
      JSON.stringify({ tracked_patch: Buffer.from(patch).toString('base64') }),
    );
    const compressed = gzipSync(bundle);
    return {
      encoding: 'gzip+json',
      payload: compressed.toString('base64'),
      sha256: createHash('sha256').update(compressed).digest('hex'),
      source: 'candidate_commit_delta',
      status: 'available',
      uncompressed_bytes: bundle.length,
    };
  };
  const baseSha = options.baseSha ?? 'base-commit-sha';
  const candidateSha = options.candidateSha ?? 'candidate-commit-sha';
  return {
    acknowledgements: {
      status: 'unavailable',
      reason: 'no_server_proven_attempt_attribution',
    },
    actions: { status: 'unavailable', reason: 'action_receipt_not_linked' },
    attempt_id: options.attemptId,
    base_sha: baseSha,
    base_status: 'available',
    candidate_sha: candidateSha,
    candidate_status: 'available',
    captured_at: '2026-01-03T00:00:00Z',
    diff: compressPatch(options.patch),
    identity: {
      kind: 'workbench',
      owner_bead_id: 'gascity-0001',
      execution_bead_id: options.executionBeadId ?? 'execution-row',
      session_id: options.sessionId,
      session_generation: options.sessionGeneration,
      claim_generation: `claim-${options.sessionGeneration}`,
    },
    outcome: 'completed',
    permission_scope: {
      store_ref: 'rig:gascity',
      work_id: 'gascity-0001',
      repository_root: '/repo',
      workspace_root: '/repo/worktree',
    },
    policy: { status: 'unavailable', reason: 'policy_verdict_not_linked_to_attempt' },
    redaction: { status: 'unavailable', reason: 'redaction_not_performed' },
    schema_version: 1,
    source_status: 'available',
    store_ref: 'rig:gascity',
    working_tree_status: options.workspacePatch === undefined ? 'clean' : 'dirty',
    workspace_diff:
      options.workspacePatch === undefined
        ? { status: 'unavailable', source: 'working_tree', reason: 'no_mutable_workspace_diff' }
        : {
            ...compressPatch(options.workspacePatch),
            source: 'working_tree',
          },
  };
}

function attemptEvidenceRead(
  evidence: Evidence,
  relatedRecords: AttemptEvidenceRead['related_records'] = {
    actions: { status: 'missing', reason: 'no_attributed_pr_action_records', records: [] },
    acknowledgements: {
      status: 'unavailable',
      reason: 'session_requests_lack_verified_work_attempt_attribution',
    },
  },
): AttemptEvidenceRead {
  return { ...evidence, related_records: relatedRecords };
}

function historicalActionRecord(attemptID: string, marker: string): HistoricalPrActionRecord {
  const receipt: PrActionResult = {
    action: 'queue_review',
    actor_key_id: 'human-grant-key',
    attempt_id: attemptID,
    base_sha: 'base-commit-sha',
    created_at: '2026-01-03T00:00:00Z',
    head_sha: 'candidate-commit-sha',
    id: marker,
    idempotency_key: `idempotency-${attemptID}`,
    monitor: 'pull-request-monitor',
    outcome: marker,
    owner: 'owner',
    policy_version: 'policy-v1',
    pull_request: 42,
    repo: 'repo',
    status: 'verified',
    work_id: 'gascity-0001',
  };
  return {
    receipt,
    admission_policy: { status: 'missing', reason: 'policy_verdict_not_captured' },
    execution_policy: { status: 'missing', reason: 'policy_verdict_not_captured' },
  };
}

function historicalRequestReceipt(evidence: Evidence, requestID: string): RequestReceipt {
  const identity = evidence.identity;
  return {
    accepted_at: '2026-01-03T01:00:00Z',
    acknowledged_at: '2026-01-03T01:00:04Z',
    attempt: {
      attempt_id: evidence.attempt_id,
      identity,
      store_ref: evidence.store_ref ?? '',
      work_revision: '15',
    },
    delivery: 'accepted',
    delivery_attempted_at: '2026-01-03T01:00:01Z',
    effect: 'unverified',
    generation: Number(identity.session_generation),
    message_digest: 'sha256:request-body-digest',
    provider_result_at: '2026-01-03T01:00:02Z',
    request_id: requestID,
    session_id: identity.session_id ?? '',
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

function requestHeaders(input: RequestInfo | URL, init?: RequestInit): Record<string, string> {
  const headers = new Headers(input instanceof Request ? input.headers : undefined);
  if (init?.headers) new Headers(init.headers).forEach((value, key) => headers.set(key, value));
  return Object.fromEntries(headers.entries());
}

function unavailableQueue(): PrActionQueue {
  return {
    availability: 'unavailable',
    fresh_until: new Date(Date.now() + 60_000).toISOString(),
    items: [],
    observed_at: new Date().toISOString(),
    policy_state: 'unavailable',
    policy_version: '',
    sources: [],
  };
}

function readyQueue(revisions: { head?: string; base?: string } = {}): PrActionQueue {
  const head = revisions.head ?? 'candidate-a';
  const base = revisions.base ?? 'base-a';
  const fresh = new Date(Date.now() + 60_000).toISOString();
  return {
    availability: 'ready',
    fresh_until: fresh,
    observed_at: new Date().toISOString(),
    policy_state: 'ready',
    policy_version: 'policy-7',
    sources: [
      { monitor: 'monitor-a', owner: 'ricky', repo: 'gascity', rig: 'gascity', state: 'ready' },
    ],
    items: [
      {
        action_receipts: [],
        actions: [
          { action: 'prepare', available: true, reason: '', requires_human_approval: false },
          { action: 'queue_review', available: true, reason: '', requires_human_approval: false },
          {
            action: 'merge',
            available: true,
            reason: 'not enabled',
            requires_human_approval: true,
          },
        ],
        attempt_evidence: [
          {
            attempt_id: 'ae-immutable-server-attempt',
            base_sha: base,
            candidate_sha: head,
            diff_sha256: 'a'.repeat(64),
            diff_source: 'candidate_commit_delta',
            store_ref: 'rig:gascity',
            work_id: `${PROJECT}-0001`,
            working_tree_status: 'dirty',
          },
        ],
        base_ref_name: 'main',
        base_sha: base,
        evidence_state: 'verified',
        fresh_until: fresh,
        head_sha: head,
        is_draft: false,
        merge_state: 'clean',
        monitor: 'monitor-a',
        observed_at: new Date().toISOString(),
        owner: 'ricky',
        policy_version: 'policy-7',
        pull_request: 42,
        repo: 'gascity',
        title: 'Central queue test',
        work_records: [
          {
            assignee: 'worker',
            base_sha: base,
            candidate_sha: head,
            current_revision: true,
            id: `${PROJECT}-0001`,
            status: 'closed',
          },
        ],
      },
    ],
  };
}

function reviewQueuedResult(): PrActionResult {
  return {
    action: 'queue_review',
    actor_key_id: 'key-1',
    attempt_id: 'ae-immutable-server-attempt',
    base_sha: 'base-a',
    created_at: new Date().toISOString(),
    head_sha: 'candidate-a',
    id: 'receipt-1',
    idempotency_key: 'pending',
    monitor: 'monitor-a',
    outcome: 'review_queued',
    owner: 'ricky',
    policy_version: 'policy-7',
    pull_request: 42,
    repo: 'gascity',
    status: 'verified',
    verified_at: new Date().toISOString(),
    work_id: `${PROJECT}-0001`,
  };
}
