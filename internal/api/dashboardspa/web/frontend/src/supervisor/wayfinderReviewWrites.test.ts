import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { BeadUpdateBody, OkResponseBody } from 'gas-city-dashboard-shared/gc-supervisor';
import { setActiveCity } from '../api/cityBase';
import {
  WAYFINDER_REVIEW_EVENT_PREFIX,
  type WayfinderReviewDraft,
} from '../lib/wayfinderReviewLog';
import { resetSupervisorApiForTests, setSupervisorApiForTests, type SupervisorApi } from './client';
import { recordWayfinderReviewEntry } from './wayfinderReviewWrites';

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

beforeEach(() => {
  setActiveCity('test-city');
  vi.stubGlobal('crypto', { randomUUID: () => 'review-record-1' });
});

afterEach(() => {
  vi.unstubAllGlobals();
  resetSupervisorApiForTests();
});

describe('recordWayfinderReviewEntry', () => {
  it('adds one uniquely keyed audit entry through the typed Bead update endpoint', async () => {
    const updateBead = vi.fn(
      async (_city: string, _id: string, _body: BeadUpdateBody): Promise<OkResponseBody> => ({
        status: 'updated',
      }),
    );
    setSupervisorApiForTests({ ...baseApi, updateBead });

    const record = await recordWayfinderReviewEntry(' gc-map ', ' ricky ', {
      kind: 'annotation',
      text: '  Keep the navigation visible.  ',
    });

    expect(record).toMatchObject({
      id: 'review-record-1',
      kind: 'annotation',
      actor: 'ricky',
      text: 'Keep the navigation visible.',
    });
    expect(updateBead).toHaveBeenCalledTimes(1);
    const call = updateBead.mock.calls[0];
    expect(call).toBeDefined();
    const [city, beadId, body] = call!;
    expect(city).toBe('test-city');
    expect(beadId).toBe('gc-map');
    expect(body).toEqual({
      metadata: {
        [`${WAYFINDER_REVIEW_EVENT_PREFIX}review-record-1`]: JSON.stringify(record),
      },
    });
  });

  it('does not write an incomplete approval record', async () => {
    const updateBead = vi.fn(
      async (_city: string, _id: string, _body: BeadUpdateBody): Promise<OkResponseBody> => ({
        status: 'updated',
      }),
    );
    setSupervisorApiForTests({ ...baseApi, updateBead });
    const incompleteApproval = {
      kind: 'approval',
      scope: 'Prototype B',
      explicitly_confirmed: false,
    } as unknown as WayfinderReviewDraft;

    await expect(recordWayfinderReviewEntry('gc-map', 'ricky', incompleteApproval)).rejects.toThrow(
      /explicit confirmation is required/i,
    );
    expect(updateBead).not.toHaveBeenCalled();
  });
});
