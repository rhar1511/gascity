package compatibility

import (
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/qualification"
)

// NewMaterializationGate binds a loaded city config, controller identity, and
// exact destination store to the molecule pre-write gate. A nil authority is
// intentionally left unconfigured: formulas from packs that declare a
// controller requirement then fail closed, while ordinary formulas continue
// through the normal source-provenance check.
func NewMaterializationGate(cfg *config.City, cityID, serverID, storeRef string, build qualification.BuildIdentity, authority qualification.CompatibilityAuthority) MaterializationGate {
	return MaterializationGate{
		Config: cfg,
		Gate: Gate{
			Authority: authority,
			CityID:    cityID,
			ServerID:  serverID,
			StoreRef:  storeRef,
			Build:     build,
		},
	}
}
