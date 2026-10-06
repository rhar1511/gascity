package beads

import (
	"errors"
	"fmt"
)

// DependencyBatchLister reads the DOWN edges of many anchors in one round trip.
//
// It is the batch shape of Store.DepList's DOWN direction, and it is a separate
// optional capability for the same reason BatchDeleter is: not every backing
// store can answer it, and the ones that can answer it in ONE read rather than
// N. A walk over a few hundred beads that asks per anchor costs one read
// transaction per bead — and, against a served store whose pool has gone cold or
// whose link drops handshakes, one CONNECT per bead.
type DependencyBatchLister interface {
	// DepListBatch returns each anchor's DOWN edges, keyed by the anchor id as
	// the caller spelled it.
	//
	// A MISSING ANCHOR GETS NO ENTRY. DepList answers ErrNotFound for an anchor
	// that is not there; a batch cannot, because failing the call for one absent
	// id would discard the answers for every id that was found. An anchor that
	// IS held and has no edges gets an entry carrying an empty slice, so the two
	// cases stay distinguishable — but presence in this map is not a portable
	// existence check across store types.
	//
	// A FAILED READ RETURNS NO MAP. Implementations return a nil map alongside
	// their error rather than the partial answer, because a caller that walks a
	// partial map reads the anchors the failure cost it as beads with no edges,
	// and a dropped link presenting as a clean graph is the one answer no
	// dependency walk can survive.
	DepListBatch(ids []string) (map[string][]Dep, error)
}

// ErrDepListBatchUnsupported signals that a Store wrapper implements
// DepListBatch only to forward the capability, but its backing store does not
// implement DependencyBatchLister.
//
// A wrapper embeds the plain Store interface, which does not promote optional
// capabilities, so a wrapper that stays silent makes the batch INVISIBLE to
// every caller that asks the wrapped store for it — the assertion simply stops
// matching and the caller takes the per-anchor path with no diagnostic. That is
// what hid this capability behind the policy wrapper on a live hosted city and
// left `gc storage recover-stranded` paying a round trip per bead over a link
// that drops handshakes (ga-50tsx). Wrappers therefore forward the method and
// report this sentinel, which callers treat as the cue to fall back to per-anchor
// reads — exactly as they would if the wrapper did not advertise it at all.
var ErrDepListBatchUnsupported = errors.New("batched dep-edge read unsupported by backing store")

// DepListBatchFor returns the batched dep-edge reader behind store, if any.
//
// It exists so callers do not each re-invent the assertion, and so a wrapper
// that forwards the capability but is backed by a store without it reports
// "unsupported" through one path rather than by returning an empty answer that
// reads as a graph with no edges.
func DepListBatchFor(store Store) (DependencyBatchLister, bool) {
	batch, ok := store.(DependencyBatchLister)
	return batch, ok
}

// DependencySafeDeleteOrder orders selected sources before their targets, so
// reference cleanup cannot invalidate another selected row's checked revision.
// Cycles require an atomic multi-row delete; refuse before any caller mutation
// rather than refreshing a stale revision and accepting unrelated changes.
func DependencySafeDeleteOrder(store Store, ids []string) ([]string, error) {
	selected := make(map[string]bool, len(ids))
	orderedIDs := make([]string, 0, len(ids))
	for _, id := range ids {
		if !selected[id] {
			selected[id] = true
			orderedIDs = append(orderedIDs, id)
		}
	}
	indegree := make(map[string]int, len(ids))
	edges := make(map[string][]string, len(ids))
	for _, id := range orderedIDs {
		row, err := store.Get(id)
		if err != nil {
			return nil, fmt.Errorf("plan deletion parent for %s: %w", id, err)
		}
		deps, err := store.DepList(id, "down")
		if err != nil {
			return nil, fmt.Errorf("plan deletion dependencies for %s: %w", id, err)
		}
		// Some stores retain ancestry as a row field without an edge projection.
		if row.ParentID != "" {
			deps = append(deps, Dep{DependsOnID: row.ParentID})
		}
		seen := make(map[string]bool, len(deps))
		for _, dep := range deps {
			target := dep.DependsOnID
			if target != id && selected[target] && !seen[target] {
				seen[target] = true
				edges[id] = append(edges[id], target)
				indegree[target]++
			}
		}
	}
	queue := make([]string, 0, len(ids))
	for _, id := range orderedIDs {
		if indegree[id] == 0 {
			queue = append(queue, id)
		}
	}
	result := make([]string, 0, len(ids))
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		result = append(result, id)
		for _, target := range edges[id] {
			indegree[target]--
			if indegree[target] == 0 {
				queue = append(queue, target)
			}
		}
	}
	if len(result) != len(orderedIDs) {
		return nil, errors.New("deletion dependency cycle requires atomic multi-row deletion")
	}
	return result, nil
}
