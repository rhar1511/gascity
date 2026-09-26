import { useEffect, useMemo, useState } from 'react';
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

// First-slice Workbench: a read-only projection of durable Beads state.
//
// The Work Queue lists real Beads through the typed supervisor REST client
// (`listSupervisorBeads`) and refreshes through the established SSE event
// path (`useGcEventRefresh` on the bead prefix). Selecting a queue entry
// opens the corresponding real Bead in the shared `BeadDetailModal`, which
// fetches direct supervisor bead detail by id (`fetchSupervisorBead`).
//
// This slice is deliberately read-only: it imports no bead write helpers,
// renders no create/close/claim controls, and therefore cannot mint a
// second durable task record (no Vibe-native task database, scheduler,
// session/worktree ownership, or message queue lives here).

// The queue refresh is a full list refetch, so coalesce SSE-driven
// refreshes into a wide trailing window (matching the Beads board) rather
// than refetching per event under city churn.
const QUEUE_REFRESH_COALESCE_MS = 10_000;

export function WorkbenchPage() {
  const cityName = getActiveCity();
  const cityCacheKey = cityName ?? 'no-city';
  const [searchParams, setSearchParams] = useSearchParams();
  const selectedBeadParam = normalizeSelectedBeadParam(searchParams.get('bead'));
  const [selectedId, setSelectedId] = useState<string | null>(selectedBeadParam);

  const { data, loading, error, refresh } = useCachedData(
    `workbench:queue:${cityCacheKey}`,
    () => listSupervisorBeads(),
  );
  const rows = useMemo(() => data?.items ?? [], [data]);
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

  const selectedBead = useMemo(
    () => rows.find((bead) => bead.id === selectedId) ?? null,
    [rows, selectedId],
  );

  const synopsis = hasLoadedQueue
    ? rows.length === 0
      ? 'Nothing on the queue right now.'
      : `${rows.length} ${rows.length === 1 ? 'bead' : 'beads'} on the queue.`
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
            <span className="text-label uppercase tracking-wider text-fg-faint">Read-only</span>
            <Button size="sm" onClick={() => void refresh()} disabled={loading}>
              {loading && !hasLoadedQueue ? 'Loading' : loading ? 'Refreshing' : 'Refresh'}
            </Button>
          </>
        }
      />

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
      ) : (
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

function normalizeSelectedBeadParam(value: string | null): string | null {
  const clean = value?.trim();
  return clean && clean.length > 0 ? clean : null;
}
