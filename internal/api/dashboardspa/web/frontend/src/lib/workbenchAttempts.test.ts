import { describe, expect, it } from 'vitest';
import type { SupervisorSession } from '../supervisor/sessionReads';
import { attemptWorktree, resolveAttempts } from './workbenchAttempts';
import type { SupervisorBead } from '../supervisor/beadReads';

function bead(id: string, metadata: Record<string, string> = {}): SupervisorBead {
  return {
    id,
    title: id,
    description: '',
    status: 'in_progress',
    priority: 0,
    issue_type: 'task',
    labels: [],
    created_at: '2026-01-01T00:00:00Z',
    metadata,
  } as unknown as SupervisorBead;
}

function session(over: Partial<SupervisorSession>): SupervisorSession {
  return {
    id: 's-1',
    template: 'worker',
    session_name: 'worker-1',
    state: 'active',
    running: true,
    created_at: '2026-01-01T00:00:00Z',
    ...over,
  } as SupervisorSession;
}

describe('resolveAttempts', () => {
  it('resolves the newest active attempt when one exists', () => {
    const b = bead('gascity-1', { 'gc.session_id': 's-1' });
    const resolved = resolveAttempts(b, [
      session({ id: 's-1', running: true, work_dir: '/wt/s-1', execution_generation: 7 }),
    ]);
    expect(resolved.current?.sessionId).toBe('s-1');
    expect(resolved.current?.executionGeneration).toBe(7);
    expect(resolved.staleReference).toBe(false);
    expect(attemptWorktree(b, resolved)).toBe('/wt/s-1');
  });

  it('accepts the largest exact JavaScript generation', () => {
    const resolved = resolveAttempts(bead('gascity-1'), [
      session({
        id: 's-1',
        running: true,
        active_bead: 'gascity-1',
        execution_generation: Number.MAX_SAFE_INTEGER,
      }),
    ]);
    expect(resolved.current?.executionGeneration).toBe(Number.MAX_SAFE_INTEGER);
  });

  it('rejects absent, non-positive, and unsafe execution generations in the browser', () => {
    for (const generation of [undefined, 0, -1, 1.5, Number.MAX_SAFE_INTEGER + 1]) {
      const sessionInput: Partial<SupervisorSession> = {
        id: 's-1',
        running: true,
        active_bead: 'gascity-1',
      };
      if (generation !== undefined) sessionInput.execution_generation = generation;
      const resolved = resolveAttempts(bead('gascity-1'), [session(sessionInput)]);
      expect(resolved.current?.executionGeneration).toBeNull();
    }
  });

  it('keeps earlier completed/failed attempts as chronological history', () => {
    const b = bead('gascity-1', { 'gc.session_id': 's-2' });
    const resolved = resolveAttempts(b, [
      session({
        id: 's-1',
        running: false,
        state: 'exited',
        created_at: '2026-01-01T00:00:00Z',
        active_bead: 'gascity-1',
      }),
      session({
        id: 's-2',
        running: true,
        state: 'active',
        created_at: '2026-01-02T00:00:00Z',
        active_bead: 'gascity-1',
      }),
      session({
        id: 's-3',
        running: false,
        state: 'failed',
        created_at: '2026-01-03T00:00:00Z',
        active_bead: 'gascity-1',
      }),
    ]);
    expect(resolved.current?.sessionId).toBe('s-2');
    expect(resolved.history.map((a) => a.sessionId)).toEqual(['s-3', 's-1']);
  });

  it('reports an explicit empty state for a Bead with no attempt', () => {
    const resolved = resolveAttempts(bead('gascity-1'), []);
    expect(resolved.current).toBeNull();
    expect(resolved.history).toEqual([]);
    expect(resolved.staleReference).toBe(false);
  });

  it('flags a stale Session reference the read cannot resolve', () => {
    const b = bead('gascity-1', { 'gc.session_id': 's-gone' });
    const resolved = resolveAttempts(b, [session({ id: 's-other' })]);
    expect(resolved.current).toBeNull();
    expect(resolved.staleReference).toBe(true);
    expect(resolved.referencedSessionId).toBe('s-gone');
  });
});
