package orders

import (
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestScan(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/layer1/orders/digest.toml"] = []byte(`
[order]
formula = "mol-digest"
trigger = "cooldown"
interval = "24h"
pool = "dog"
`)
	fs.Files["/layer1/orders/cleanup.toml"] = []byte(`
[order]
formula = "mol-cleanup"
trigger = "cron"
schedule = "0 3 * * *"
`)

	orders, err := Scan(fs, []string{"/layer1/formulas"}, nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(orders) != 2 {
		t.Fatalf("got %d orders, want 2", len(orders))
	}
	// Names should be set from directory names.
	names := map[string]bool{}
	for _, a := range orders {
		names[a.Name] = true
	}
	if !names["digest"] || !names["cleanup"] {
		t.Errorf("expected digest and cleanup, got %v", names)
	}
}

func TestScanEmpty(t *testing.T) {
	fs := fsys.NewFake()
	fs.Dirs["/layer1/formulas"] = true

	orders, err := Scan(fs, []string{"/layer1/formulas"}, nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(orders) != 0 {
		t.Fatalf("got %d orders, want 0", len(orders))
	}
}

func TestScanLayerOverride(t *testing.T) {
	fs := fsys.NewFake()
	// Layer 1 (lower priority): digest with 24h.
	fs.Files["/layer1/orders/digest.toml"] = []byte(`
[order]
formula = "mol-digest"
trigger = "cooldown"
interval = "24h"
pool = "dog"
`)
	// Layer 2 (higher priority): digest with 8h.
	fs.Files["/layer2/orders/digest.toml"] = []byte(`
[order]
formula = "mol-digest"
trigger = "cooldown"
interval = "8h"
pool = "dog"
`)

	orders, err := Scan(fs, []string{"/layer1/formulas", "/layer2/formulas"}, nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("got %d orders, want 1", len(orders))
	}
	if orders[0].Interval != "8h" {
		t.Errorf("Interval = %q, want %q (layer 2 overrides)", orders[0].Interval, "8h")
	}
}

func TestScanSkip(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/layer1/orders/digest.toml"] = []byte(`
[order]
formula = "mol-digest"
trigger = "cooldown"
interval = "24h"
`)
	fs.Files["/layer1/orders/cleanup.toml"] = []byte(`
[order]
formula = "mol-cleanup"
trigger = "manual"
`)

	orders, err := Scan(fs, []string{"/layer1/formulas"}, []string{"digest"})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("got %d orders, want 1", len(orders))
	}
	if orders[0].Name != "cleanup" {
		t.Errorf("Name = %q, want %q", orders[0].Name, "cleanup")
	}
}

func TestScanSkipAliases(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/layer1/orders/maintenance-export.toml"] = []byte(`
[order]
exec = "scripts/export.sh"
trigger = "cooldown"
interval = "15m"
skip_aliases = ["old-export"]
`)
	fs.Files["/layer1/orders/cleanup.toml"] = []byte(`
[order]
formula = "mol-cleanup"
trigger = "manual"
`)

	orders, err := Scan(fs, []string{"/layer1/formulas"}, []string{"old-export"})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("got %d orders, want 1", len(orders))
	}
	if orders[0].Name != "cleanup" {
		t.Errorf("Name = %q, want %q", orders[0].Name, "cleanup")
	}
}

func TestScanDisabled(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/layer1/orders/digest.toml"] = []byte(`
[order]
formula = "mol-digest"
trigger = "cooldown"
interval = "24h"
enabled = false
`)

	orders, err := Scan(fs, []string{"/layer1/formulas"}, nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(orders) != 0 {
		t.Fatalf("got %d orders, want 0 (disabled)", len(orders))
	}
}

func TestScanRootsInventoryRetainsActivationDispositionAndWinningLayer(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/lower/orders/winner.toml"] = []byte(`[order]
formula = "mol-lower"
trigger = "manual"
`)
	fs.Files["/higher/orders/winner.toml"] = []byte(`[order]
formula = "mol-higher"
trigger = "manual"
enabled = false
`)
	fs.Files["/lower/orders/skipped-name.toml"] = []byte(`[order]
formula = "mol-name"
trigger = "manual"
`)
	fs.Files["/lower/orders/skipped-alias.toml"] = []byte(`[order]
formula = "mol-alias"
trigger = "manual"
skip_aliases = ["legacy-name", "legacy-name", "older-name"]
`)

	inventory, err := ScanRootsInventory(fs, []ScanRoot{
		{Dir: "/lower/orders", FormulaLayer: "/lower/formulas"},
		{Dir: "/higher/orders", FormulaLayer: "/higher/formulas"},
	}, []string{"skipped-name", "legacy-name", "older-name"})
	if err != nil {
		t.Fatalf("ScanRootsInventory: %v", err)
	}
	if len(inventory) != 3 {
		t.Fatalf("inventory = %#v, want three winning definitions", inventory)
	}
	byName := make(map[string]InventoryOrder, len(inventory))
	for _, entry := range inventory {
		byName[entry.Order.Name] = entry
	}
	winner := byName["winner"]
	if winner.Activation != ActivationDisabledBySource || winner.Order.Formula != "mol-higher" || winner.Order.FormulaLayer != "/higher/formulas" || winner.Order.Source != "/higher/orders/winner.toml" {
		t.Fatalf("higher-layer winner = %#v", winner)
	}
	byNameSkip := byName["skipped-name"]
	if byNameSkip.Activation != ActivationSkippedByName || len(byNameSkip.SkipMatches) != 1 || byNameSkip.SkipMatches[0] != "skipped-name" {
		t.Fatalf("name skip = %#v", byNameSkip)
	}
	aliasSkip := byName["skipped-alias"]
	if aliasSkip.Activation != ActivationSkippedByAlias || len(aliasSkip.SkipMatches) != 2 || aliasSkip.SkipMatches[0] != "legacy-name" || aliasSkip.SkipMatches[1] != "older-name" {
		t.Fatalf("alias skip = %#v", aliasSkip)
	}

	active, err := ScanRoots(fs, []ScanRoot{
		{Dir: "/lower/orders", FormulaLayer: "/lower/formulas"},
		{Dir: "/higher/orders", FormulaLayer: "/higher/formulas"},
	}, []string{"skipped-name", "legacy-name", "older-name"})
	if err != nil {
		t.Fatalf("ScanRoots: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("legacy active scan changed behavior: %#v", active)
	}
}

func TestScanFormulaLayer(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/pack/orders/health.toml"] = []byte(`
[order]
exec = "$PACK_DIR/scripts/health.sh"
trigger = "cooldown"
interval = "1m"
`)

	orders, err := Scan(fs, []string{"/pack/formulas"}, nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("got %d orders, want 1", len(orders))
	}
	if orders[0].FormulaLayer != "/pack/formulas" {
		t.Errorf("FormulaLayer = %q, want %q", orders[0].FormulaLayer, "/pack/formulas")
	}
}

func TestScanFormulaLayerOverride(t *testing.T) {
	fs := fsys.NewFake()
	// Layer 1: lower priority.
	fs.Files["/base/orders/health.toml"] = []byte(`
[order]
exec = "$PACK_DIR/scripts/health.sh"
trigger = "cooldown"
interval = "1h"
`)
	// Layer 2: higher priority overrides.
	fs.Files["/pack/orders/health.toml"] = []byte(`
[order]
exec = "$PACK_DIR/scripts/health.sh"
trigger = "cooldown"
interval = "5m"
`)

	orders, err := Scan(fs, []string{"/base/formulas", "/pack/formulas"}, nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("got %d orders, want 1", len(orders))
	}
	// FormulaLayer should come from the winning (higher-priority) layer.
	if orders[0].FormulaLayer != "/pack/formulas" {
		t.Errorf("FormulaLayer = %q, want %q", orders[0].FormulaLayer, "/pack/formulas")
	}
}

func TestScanSourcePath(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/layer1/orders/digest.toml"] = []byte(`
[order]
formula = "mol-digest"
trigger = "manual"
`)

	orders, err := Scan(fs, []string{"/layer1/formulas"}, nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("got %d orders, want 1", len(orders))
	}
	if orders[0].Source != "/layer1/orders/digest.toml" {
		t.Errorf("Source = %q, want %q", orders[0].Source, "/layer1/orders/digest.toml")
	}
}
