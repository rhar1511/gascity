package worklifecycle

import "strings"

// ScopeForStore namespaces a canonical store ref with its owning city for
// receipt signing. Census refs such as rig:worker and class:gmnos are unique
// within one city, but not across cities. The city prefix prevents a trusted
// receipt from being replayed into another city that happens to use the same
// bead ID, rig name, and storage layout.
func ScopeForStore(cityName, storeRef string) string {
	cityName = strings.TrimSpace(cityName)
	storeRef = strings.TrimSpace(storeRef)
	if cityName == "" || storeRef == "" {
		return ""
	}
	return "city:" + cityName + "/" + storeRef
}
