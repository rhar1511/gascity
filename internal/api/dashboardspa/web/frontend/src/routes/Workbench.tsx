import { useCallback, useEffect, useMemo, useState } from 'react';
import { useSearchParams } from 'react-router-dom';
import { GC_EVENT_PREFIX } from 'gas-city-dashboard-shared';
import { getActiveCity } from '../api/cityBase';
import { BeadDetailModal } from '../components/BeadDetailModal';
import { Button } from '../components/Button';
import { PageHeader } from '../components/PageHeader';
import { StatusBadge, beadStatusTone } from '../components/StatusBadge';
import { useCachedData } from '../hooks/useCachedData';
import { useGcEventRefresh } from '../hooks/useGcEvents';
import { listSupervisorBeads } from '../supervisor/beadReads';
import { updateSupervisorBead } from '../supervisor/beadWrites';

// Gas City Workbench: Canvas/Kanban/Priority/Work Queue as views over the same
// Beads data.
//
// Every view resolves the same Bead identity from one shared read
// (`listSupervisorBeads`) and converges after a successful mutation: lane
// movement patches the Bead through the typed supervisor API and the SSE bead
// event path refreshes the shared read. Kanban lanes move status, Priority
// lanes move priority, and closing requires confirmation. Moving or editing a
// card NEVER starts an agent: no Execution Attempt or Session is created here —
// assignment, Sessions, worktrees and agent lifecycle stay Gas City's job.

const QUEUE_REFRESH_COALESCE_MS = 10_000;

export type WorkbenchView = 'queue' | 'kanban' | 'priority';

const KANBAN_STATUSES = ['open', 'in_progress', 'blocked', 'hooked', 'closed'] as const;
const PRIORITY_LANES = [0, 1, 2, 3, 4] as const;

type Row = Awaited<ReturnType<typeof listSupervisorBeads>>['items'][number];
type Patch = { status?: string; priority?: number };

export function WorkbenchPage() {
  const cityName = getActiveCity();
  const cityCacheKey = cityName ?? 'no-city';
  const [searchParams, setSearchParams] = useSearchParams();
  const selectedBeadParam = normalizeSelectedBeadParam(searchParams.get('bead'));
  const [selectedId, setSelectedId] = useState<string | null>(selectedBeadParam);
  const [view, setView] = useState<WorkbenchView>('queue');
  // Optimistic overlays keyed by bead id; cleared once the shared read catches up.
  const [overrides, setOverrides] = useState<Record<string, Patch>>({});
  const [mutationError, setMutationError] = useState<string | null>(null);
  const [confirmCloseId, setConfirmCloseId] = useState<string | null>(null);

  const { data, loading, error, refresh } = useCachedData(
    `workbench:queue:${cityCacheKey}`,
    () => listSupervisorBeads(),
  );
  const rows = useMemo(() => applyOverrides(data?.items ?? [], overrides), [data, overrides]);
  const hasLoadedQueue = data !== undefined;

  useGcEventRefresh([GC_EVENT_PREFIX.bead], () => void refresh(), {
    coalesceMs: QUEUE_REFRESH_COALESCE_MS,
  });

  useEffect(() => {
    if (selectedBeadParam !== null) setSelectedId(selectedBeadParam);
  }, [selectedBeadParam]);

  const openBead = (beadId: string) => {
    setSelectedId(beadId);
    setSearchParams(beadId.length > 0 ? { bead: beadId } : {}, { replace: true });
  };
  const closeBead = () => {
    setSelectedId(null);
    setSearchParams({}, { replace: true });
  };

  const mutate = useCallback(
    async (id: string, patch: Patch) => {
      setMutationError(null);
      setOverrides((current) => ({ ...current, [id]: { ...current[id], ...patch } }));
      try {
        await updateSupervisorBead(id, patch);
        await refresh();
        setOverrides((current) => {
          const next = { ...current };
          delete next[id];
          return next;
        });
      } catch (cause) {
        // Server rejection / reconnect: drop the optimistic overlay so the view
        // reconverges on the authoritative read, and surface the failure.
        setOverrides((current) => {
          const next = { ...current };
          delete next[id];
          return next;
        });
        setMutationError(cause instanceof Error ? cause.message : 'Update rejected');
      }
    },
    [refresh],
  );

  const confirmClose = useCallback(async () => {
    const id = confirmCloseId;
    setConfirmCloseId(null);
    if (id === null) return;
    await mutate(id, { status: 'closed' });
  }, [confirmCloseId, mutate]);

  const selectedBead = useMemo(
    () => rows.find((bead) => bead.id === selectedId) ?? null,
    [rows, selectedId],
  );

  const synopsis = hasLoadedQueue
    ? rows.length === 0
      ? 'Nothing on the queue right now.'
      : `${rows.length} ${rows.length === 1 ? 'bead' : 'beads'} across ${viewLabel(view)}.`
    : 'Loading work queue.';

  return (
    <section>
      <PageHeader
        title="Workbench"
        synopsis={synopsis}
        meta={
          <>
            {error && hasLoadedQueue && (
              <span className="normal-case text-body text-accent" role="alert">
                {error}
              </span>
            )}
            {mutationError && (
              <span className="normal-case text-body text-accent" role="alert">
                {mutationError}
              </span>
            )}
            <span className="text-label uppercase tracking-wider text-fg-faint">Beads-backed</span>
            <Button size="sm" onClick={() => void refresh()} disabled={loading}>
              {loading && !hasLoadedQueue ? 'Loading' : loading ? 'Refreshing' : 'Refresh'}
            </Button>
          </>
        }
      />

      <nav aria-label="Workbench views" className="mb-4 flex gap-2">
        {(['queue', 'kanban', 'priority'] as const).map((candidate) => (
          <Button
            key={candidate}
            size="sm"
            onClick={() => setView(candidate)}
            aria-pressed={view === candidate}
            tone={view === candidate ? 'accent' : 'quiet'}
          >
            {viewLabel(candidate)}
          </Button>
        ))}
      </nav>

      {!hasLoadedQueue && loading ? (
        <p className="text-body text-fg-muted italic">Loading work queue.</p>
      ) : error && !hasLoadedQueue ? (
        <div className="space-y-3">
          <p className="text-body text-accent" role="alert">
            Work queue unavailable: {error}
          </p>
          <Button size="sm" onClick={() => void refresh()} disabled={loading}>
            Retry
          </Button>
        </div>
      ) : rows.length === 0 ? (
        <p className="text-body text-fg-muted italic">Nothing on the queue right now.</p>
      ) : view === 'queue' ? (
        <ul aria-label="Work queue" className="space-y-2 max-w-prose">
          {rows.map((bead) => (
            <li key={bead.id}>
              <button
                type="button"
                onClick={() => openBead(bead.id)}
                aria-pressed={selectedId === bead.id}
                title={`Open ${bead.id}`}
                className="w-full text-left rounded-sm border border-rule px-4 py-3 hover:border-fg-faint focus-mark transition-colors duration-150 ease-out-quart"
              >
                <span className="flex items-baseline justify-between gap-3">
                  <span className="text-body text-fg font-medium truncate">{bead.title}</span>
                  <StatusBadge tone={beadStatusTone(bead.status)} label={bead.status} />
                </span>
                <span className="text-label uppercase tracking-wider text-fg-faint">
                  <code>{bead.id}</code>
                  {' · '}
                  {bead.issue_type}
                </span>
              </button>
            </li>
          ))}
        </ul>
      ) : (
        <LaneBoard
          rows={rows}
          view={view}
          selectedId={selectedId}
          onOpen={openBead}
          onMove={mutate}
          onRequestClose={(id) => setConfirmCloseId(id)}
        />
      )}

      {confirmCloseId !== null && (
        <div
          role="alertdialog"
          aria-label="Confirm close"
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/40"
        >
          <div className="rounded-sm border border-rule bg-surface p-5 max-w-sm space-y-3">
            <p className="text-body text-fg">
              Close <code>{confirmCloseId}</code>? Closing changes the Bead status and cannot be
              undone from here.
            </p>
            <div className="flex justify-end gap-2">
              <Button size="sm" tone="quiet" onClick={() => setConfirmCloseId(null)}>
                Cancel
              </Button>
              <Button size="sm" onClick={() => void confirmClose()}>
                Close bead
              </Button>
            </div>
          </div>
        </div>
      )}

      <BeadDetailModal
        open={selectedId !== null}
        onClose={closeBead}
        beadId={selectedId}
        initialBead={selectedBead}
        onOpenBead={openBead}
      />
    </section>
  );
}

function viewLabel(view: WorkbenchView): string {
  switch (view) {
    case 'queue':
      return 'Work Queue';
    case 'kanban':
      return 'Kanban';
    case 'priority':
      return 'Priority';
  }
}

function applyOverrides(rows: Row[], overrides: Record<string, Patch>): Row[] {
  if (Object.keys(overrides).length === 0) return rows;
  return rows.map((row) => {
    const patch = overrides[row.id];
    return patch ? ({ ...row, ...patch } as Row) : row;
  });
}

interface LaneBoardProps {
  rows: Row[];
  view: Exclude<WorkbenchView, 'queue'>;
  selectedId: string | null;
  onOpen: (id: string) => void;
  onMove: (id: string, patch: Patch) => void | Promise<void>;
  onRequestClose: (id: string) => void;
}

// LaneBoard renders Kanban (lanes = status) or Priority (lanes = priority) over
// the SAME rows the Work Queue shows. Dropping a card on a lane patches exactly
// the one field that view owns: Kanban -> status, Priority -> priority.
function LaneBoard({ rows, view, selectedId, onOpen, onMove, onRequestClose }: LaneBoardProps) {
  const lanes =
    view === 'kanban'
      ? KANBAN_STATUSES.map((status) => ({ key: `status:${status}`, label: status, patch: { status } as Patch }))
      : PRIORITY_LANES.map((priority) => ({
          key: `priority:${priority}`,
          label: `P${priority}`,
          patch: { priority } as Patch,
        }));

  const laneKeyFor = (row: Row): string =>
    view === 'kanban' ? `status:${row.status}` : `priority:${row.priority}`;

  return (
    <div aria-label={`${viewLabel(view)} board`} className="flex gap-3 overflow-x-auto">
      {lanes.map((lane) => {
        const laneRows = rows.filter((row) => laneKeyFor(row) === lane.key);
        return (
          <div
            key={lane.key}
            data-testid={`lane-${lane.key}`}
            onDragOver={(event) => event.preventDefault()}
            onDrop={(event) => {
              event.preventDefault();
              const id = event.dataTransfer.getData('text/bead-id');
              if (id) void onMove(id, lane.patch);
            }}
            className="min-w-[14rem] flex-1 rounded-sm border border-rule p-2"
          >
            <div className="mb-2 flex items-center justify-between">
              <span className="text-label uppercase tracking-wider text-fg-faint">{lane.label}</span>
              <span className="text-label text-fg-faint">{laneRows.length}</span>
            </div>
            <ul className="space-y-2">
              {laneRows.map((row) => (
                <li
                  key={row.id}
                  draggable
                  onDragStart={(event) => event.dataTransfer.setData('text/bead-id', row.id)}
                  className="rounded-sm border border-rule px-3 py-2"
                >
                  <button
                    type="button"
                    onClick={() => onOpen(row.id)}
                    aria-pressed={selectedId === row.id}
                    className="w-full text-left"
                  >
                    <span className="text-body text-fg font-medium truncate">{row.title}</span>
                    <span className="block text-label text-fg-faint">
                      <code>{row.id}</code>
                    </span>
                  </button>
                  <Button size="sm" tone="quiet" onClick={() => onRequestClose(row.id)}>
                    Close
                  </Button>
                </li>
              ))}
            </ul>
          </div>
        );
      })}
    </div>
  );
}

function normalizeSelectedBeadParam(value: string | null): string | null {
  const clean = value?.trim();
  return clean && clean.length > 0 ? clean : null;
}
