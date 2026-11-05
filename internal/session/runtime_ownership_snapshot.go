package session

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
)

// ListRuntimeOwnershipInfos reads the complete open type/label ownership union
// from the live store. Damaged labeled rows remain visible conservatively;
// partial or unavailable history cannot establish an empty ownership set.
func (s *Store) ListRuntimeOwnershipInfos() ([]Info, error) {
	if s == nil || s.store.Store == nil {
		return nil, fmt.Errorf("runtime ownership store unavailable")
	}
	live := beads.HandlesFor(s.store.Store).Live
	seen := make(map[string]bool)
	var infos []Info
	for _, query := range []beads.ListQuery{{Type: BeadType}, {Label: LabelSession}} {
		rows, err := live.List(query)
		if err != nil {
			return nil, fmt.Errorf("reading runtime ownership: %w", err)
		}
		for _, row := range rows {
			if row.Status == "closed" || seen[row.ID] {
				continue
			}
			seen[row.ID] = true
			infos = append(infos, infoFromPersistedBead(row))
		}
	}
	return infos, nil
}
