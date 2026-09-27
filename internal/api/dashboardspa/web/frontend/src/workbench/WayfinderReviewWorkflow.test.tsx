import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { OperatorConfigProvider } from '../contexts/OperatorConfigContext';
import { setActiveCity } from '../api/cityBase';
import type { SupervisorBead } from '../supervisor/beadReads';
import type { BeadUpdateBody, OkResponseBody } from 'gas-city-dashboard-shared/gc-supervisor';
import {
  resetSupervisorApiForTests,
  setSupervisorApiForTests,
  type SupervisorApi,
} from '../supervisor/client';
import { WayfinderReviewWorkflow } from './WayfinderReviewWorkflow';

const operator = {
  operatorAlias: 'ricky',
  operatorWireAlias: 'human',
  decisionLabel: 'needs/operator',
};

const baseApi: SupervisorApi = {
  baseUrl: '/gc-supervisor',
  health: vi.fn(),
  cityHealth: vi.fn(),
  cityStatus: vi.fn(),
  cityUsage: vi.fn(),
  runCensus: vi.fn(),
  listCities: vi.fn(),
  listAgents: vi.fn(),
  listRigs: vi.fn(),
  listBeads: vi.fn(),
  listEvents: vi.fn(),
  getBead: vi.fn(),
  createBead: vi.fn(),
  updateBead: vi.fn(),
  closeBead: vi.fn(),
  sling: vi.fn(),
  formulaFeed: vi.fn(),
  listMail: vi.fn(),
  markMailRead: vi.fn(),
  markMailUnread: vi.fn(),
  archiveMail: vi.fn(),
  replyMail: vi.fn(),
  sendMail: vi.fn(),
  mailThread: vi.fn(),
  cityEventStreamUrl: vi.fn(),
  sessionStreamUrl: vi.fn(),
  listSessions: vi.fn(),
  sessionPending: vi.fn(),
  respondSession: vi.fn(),
  sessionTranscript: vi.fn(),
  workflowRun: vi.fn(),
  formulaDetail: vi.fn(),
  mutationHeaders: () => ({ 'X-GC-Request': 'dashboard' }),
};

function reviewBead(metadata: Record<string, string> = {}): SupervisorBead {
  return {
    id: 'gc-map',
    title: 'Map the review workflow',
    issue_type: 'epic',
    status: 'in_progress',
    description: '## Notes\n\nReview: Lavish AXI',
    labels: [],
    metadata,
    created_at: '2026-09-27T00:00:00Z',
  } as SupervisorBead;
}

function renderWorkflow(bead = reviewBead()) {
  return render(
    <OperatorConfigProvider operator={operator}>
      <WayfinderReviewWorkflow bead={bead} />
    </OperatorConfigProvider>,
  );
}

beforeEach(() => {
  setActiveCity('test-city');
});

afterEach(() => {
  cleanup();
  resetSupervisorApiForTests();
});

describe('WayfinderReviewWorkflow', () => {
  it('shows published prototype and local review availability with the recorded trail', () => {
    const bead = reviewBead({
      'gc.prototype_url': 'http://localhost:3000/prototype?bead=gc-map',
      'gc.wayfinder_review_url': 'http://127.0.0.1:4173/session/map',
      'gc.wayfinder_review.event.note-1':
        '{"version":1,"id":"note-1","kind":"annotation","actor":"ricky","recorded_at":"2026-09-27T01:00:00.000Z","text":"Tighten the hierarchy"}',
    });

    renderWorkflow(bead);

    expect(screen.getByRole('link', { name: 'Prototype A' }).getAttribute('href')).toBe(
      'http://localhost:3000/prototype?bead=gc-map&variant=A',
    );
    expect(screen.getByRole('link', { name: /open lavish review/i }).getAttribute('href')).toBe(
      'http://127.0.0.1:4173/session/map',
    );
    expect(screen.getByText('Tighten the hierarchy')).toBeTruthy();
    expect(screen.getByText(/nothing is recorded until you submit/i)).toBeTruthy();
    expect(baseApi.updateBead).not.toHaveBeenCalled();
  });

  it('records an annotation on the Bead only after the operator submits it', async () => {
    const updateBead = vi.fn(
      async (_city: string, _id: string, _body: BeadUpdateBody): Promise<OkResponseBody> => ({
        status: 'updated',
      }),
    );
    setSupervisorApiForTests({ ...baseApi, updateBead });
    renderWorkflow();

    fireEvent.change(screen.getByLabelText('Annotation'), {
      target: { value: 'Use a clearer sequence for the review.' },
    });
    expect(updateBead).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole('button', { name: 'Record annotation' }));

    await waitFor(() => expect(updateBead).toHaveBeenCalledTimes(1));
    const call = updateBead.mock.calls[0];
    expect(call).toBeDefined();
    const [city, beadId, body] = call!;
    expect(city).toBe('test-city');
    expect(beadId).toBe('gc-map');
    expect(Object.keys(body)).toEqual(['metadata']);
    const [key, value] = Object.entries(body.metadata ?? {})[0]!;
    expect(key).toMatch(/^gc\.wayfinder_review\.event\./);
    expect(JSON.parse(value!)).toMatchObject({
      kind: 'annotation',
      actor: 'ricky',
      text: 'Use a clearer sequence for the review.',
    });
    expect(screen.getByRole('status').textContent).toMatch(/recorded on this bead/i);
  });

  it('discards unsaved drafts when the selected Bead changes', () => {
    const { rerender } = renderWorkflow();
    fireEvent.change(screen.getByLabelText('Annotation'), {
      target: { value: 'This belongs to the first Bead.' },
    });

    rerender(
      <OperatorConfigProvider operator={operator}>
        <WayfinderReviewWorkflow bead={{ ...reviewBead(), id: 'second-bead' }} />
      </OperatorConfigProvider>,
    );

    expect((screen.getByLabelText('Annotation') as HTMLTextAreaElement).value).toBe('');
  });

  it('requires a named scope and explicit confirmation before recording approval', async () => {
    const updateBead = vi.fn(
      async (_city: string, _id: string, _body: BeadUpdateBody): Promise<OkResponseBody> => ({
        status: 'updated',
      }),
    );
    setSupervisorApiForTests({ ...baseApi, updateBead });
    renderWorkflow();

    fireEvent.click(screen.getByRole('button', { name: 'Explicit approval' }));
    const submit = screen.getByRole('button', { name: 'Record explicit approval' });
    expect(submit.hasAttribute('disabled')).toBe(true);

    fireEvent.change(screen.getByLabelText('Approval scope'), {
      target: { value: 'Prototype B for issues GC-12 and GC-13' },
    });
    expect(submit.hasAttribute('disabled')).toBe(true);

    fireEvent.change(screen.getByLabelText('Artifact target'), {
      target: { value: 'Prototype B' },
    });
    expect(submit.hasAttribute('disabled')).toBe(true);
    fireEvent.change(screen.getByLabelText('Revision'), { target: { value: 'rev-42' } });
    fireEvent.click(screen.getByRole('checkbox', { name: /i explicitly approve this scope/i }));
    expect(submit.hasAttribute('disabled')).toBe(false);
    fireEvent.click(submit);

    await waitFor(() => expect(updateBead).toHaveBeenCalledTimes(1));
    const call = updateBead.mock.calls[0];
    expect(call).toBeDefined();
    const record = JSON.parse(Object.values(call![2].metadata ?? {})[0]!);
    expect(record).toMatchObject({
      kind: 'approval',
      actor: 'ricky',
      target: 'Prototype B',
      revision: 'rev-42',
      scope: 'Prototype B for issues GC-12 and GC-13',
      explicitly_confirmed: true,
    });
    expect(
      screen.getByText(/does not approve a PR, publish the artifact, or start rollout/i),
    ).toBeTruthy();
  });

  it('records the prompt beside its answer', async () => {
    const updateBead = vi.fn(
      async (_city: string, _id: string, _body: BeadUpdateBody): Promise<OkResponseBody> => ({
        status: 'updated',
      }),
    );
    setSupervisorApiForTests({ ...baseApi, updateBead });
    renderWorkflow();

    fireEvent.click(screen.getByRole('button', { name: 'Answer a prompt' }));
    fireEvent.change(screen.getByLabelText('Prompt'), { target: { value: 'Which layout?' } });
    fireEvent.change(screen.getByLabelText('Answer'), {
      target: { value: 'B, with a smaller header.' },
    });
    fireEvent.click(screen.getByRole('button', { name: 'Record answer' }));

    await waitFor(() => expect(updateBead).toHaveBeenCalledTimes(1));
    const call = updateBead.mock.calls[0];
    expect(call).toBeDefined();
    const record = JSON.parse(Object.values(call![2].metadata ?? {})[0]!);
    expect(record).toMatchObject({
      kind: 'answer',
      prompt: 'Which layout?',
      answer: 'B, with a smaller header.',
    });
  });

  it('opens a safe Lavish link in a tab and never embeds the document', () => {
    const updateBead = vi.fn();
    setSupervisorApiForTests({ ...baseApi, updateBead });
    const { container } = renderWorkflow();

    fireEvent.change(screen.getByLabelText('Local Lavish URL'), {
      target: { value: 'http://127.0.0.1:4173/session/local-review' },
    });

    const link = screen.getByRole('link', { name: /open lavish review/i });
    expect(link.getAttribute('target')).toBe('_blank');
    expect(link.getAttribute('rel')).toBe('noopener noreferrer');
    expect(container.querySelector('iframe')).toBeNull();
    expect(updateBead).not.toHaveBeenCalled();
  });

  it('keeps the draft visible if Gas City rejects the review write', async () => {
    const updateBead = vi.fn(
      async (_city: string, _id: string, _body: BeadUpdateBody): Promise<OkResponseBody> => {
        throw new Error('supervisor unavailable');
      },
    );
    setSupervisorApiForTests({ ...baseApi, updateBead });
    renderWorkflow();

    fireEvent.change(screen.getByLabelText('Annotation'), {
      target: { value: 'Keep this note until the server is back.' },
    });
    fireEvent.click(screen.getByRole('button', { name: 'Record annotation' }));

    expect((await screen.findByRole('alert')).textContent).toContain('supervisor unavailable');
    expect((screen.getByLabelText('Annotation') as HTMLTextAreaElement).value).toBe(
      'Keep this note until the server is back.',
    );
    expect(screen.queryByText(/annotation recorded on this bead/i)).toBeNull();
  });
});
