package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// poolIdleRoutedWorkCheck detects a pool template that has gc.routed_to work
// sitting open and unclaimed while a live instance of that same pool is idle
// (holds no current trigger bead) and so could pick the work up right now.
//
// An idle instance alone is not a finding: a pool's min-floor idle workers
// legitimately hold no bead while waiting for routed work to arrive. This
// check only fires when both conditions hold together — idle capacity AND
// unclaimed work already routed to it.
type poolIdleRoutedWorkCheck struct {
	cfg      *config.City
	cityPath string
	newStore func(string) (beads.Store, error)
}

func newPoolIdleRoutedWorkCheck(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) *poolIdleRoutedWorkCheck {
	return &poolIdleRoutedWorkCheck{cfg: cfg, cityPath: cityPath, newStore: newStore}
}

func (c *poolIdleRoutedWorkCheck) Name() string { return "pool-idle-routed-work" }

// CanFix returns false: deciding whether to nudge, investigate, or leave an
// idle instance alone belongs to a human or an order, not this check.
func (c *poolIdleRoutedWorkCheck) CanFix() bool { return false }

// Fix is a no-op. Detection only: it never nudges or reassigns on the
// operator's behalf.
func (c *poolIdleRoutedWorkCheck) Fix(_ *doctor.CheckContext) error { return nil }

// poolIdleRoutedWorkFinding is one pool template, in one store scope, that
// has unclaimed gc.routed_to work sitting beside at least one idle instance.
type poolIdleRoutedWorkFinding struct {
	scope         string
	template      string
	beadIDs       []string
	idleInstances []string
}

func (f poolIdleRoutedWorkFinding) describe() string {
	return fmt.Sprintf("%s pool %s has %d unclaimed routed bead(s) (%s) while %d instance(s) sit idle (%s)",
		f.scope, f.template, len(f.beadIDs), strings.Join(f.beadIDs, ", "), len(f.idleInstances), strings.Join(f.idleInstances, ", "))
}

func (c *poolIdleRoutedWorkCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	if c.cfg == nil {
		return okCheck(c.Name(), "no config available")
	}
	findings, skipped := c.collect()
	if len(findings) == 0 && len(skipped) == 0 {
		return okCheck(c.Name(), "no pool has unclaimed routed work sitting beside an idle instance")
	}
	details := make([]string, 0, len(findings)+len(skipped))
	for _, f := range findings {
		details = append(details, f.describe())
	}
	details = append(details, skipped...)
	sort.Strings(details)
	if len(findings) == 0 {
		return warnCheck(c.Name(),
			fmt.Sprintf("pool idle-routed-work check skipped %d scope(s)", len(skipped)),
			"fix bead store access, then rerun gc doctor",
			details)
	}
	msg := fmt.Sprintf("%d pool(s) have unclaimed routed work while an instance sits idle", len(findings))
	if len(skipped) > 0 {
		msg = fmt.Sprintf("%s; %d scope(s) skipped", msg, len(skipped))
	}
	return warnCheck(c.Name(),
		msg,
		"nudge the idle instance (gc session nudge <name>) or investigate why it has not claimed the routed work",
		details)
}

// collect scans every in-scope bead store (the city plus every non-suspended,
// path-bearing rig) for pool templates that have both an idle live instance
// and unclaimed gc.routed_to work. It mirrors v2RoutedToNamespaceCheck's scope
// iteration so routed-work sanity checks agree on what "in scope" means.
func (c *poolIdleRoutedWorkCheck) collect() (findings []poolIdleRoutedWorkFinding, skipped []string) {
	scopes := []struct{ label, path string }{{"city", c.cityPath}}
	suspState, _ := loadSuspensionState(fsys.OSFS{}, c.cityPath)
	for _, rig := range c.cfg.Rigs {
		if suspensionstate.EffectiveRigSuspended(suspState, rig.Name, rig.EffectiveSuspendedOnStart()) || strings.TrimSpace(rig.Path) == "" {
			continue
		}
		scopes = append(scopes, struct{ label, path string }{"rig " + rig.Name, rig.Path})
	}
	for _, sc := range scopes {
		if c.newStore == nil || strings.TrimSpace(sc.path) == "" {
			continue
		}
		store, err := c.newStore(sc.path)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: opening bead store: %v", sc.label, err))
			continue
		}
		scopeFindings, err := c.collectStoreFindings(store, sc.label)
		findings = append(findings, scopeFindings...)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: %v", sc.label, err))
		}
	}
	return findings, skipped
}

// collectStoreFindings checks every generic-ephemeral pool template in cfg
// against one store with at most TWO reads: one session-class enumeration for
// idle live instances, and one authoritative ready read. Both are whole-scope
// reads bucketed in Go, replacing the previous per-template session Ledger scan
// and per-template gc.routed_to metadata List (O(templates) round-trips each).
// On remote Dolt that per-template fan-out made the check exceed its budget and
// time out; two reads are O(1) in template count.
func (c *poolIdleRoutedWorkCheck) collectStoreFindings(store beads.Store, label string) ([]poolIdleRoutedWorkFinding, error) {
	sessStore := cliSessionFrontDoor(store, c.cfg, c.cityPath)

	// Build the generic pool-template set once.
	genericTemplates := make(map[string]struct{})
	for i := range c.cfg.Agents {
		agent := &c.cfg.Agents[i]
		if agent.Suspended || !agent.SupportsGenericEphemeralSessions() {
			continue
		}
		if template := agent.QualifiedName(); template != "" {
			genericTemplates[template] = struct{}{}
		}
	}
	if len(genericTemplates) == 0 {
		return nil, nil
	}

	// One session-class enumeration, bucketed into idle instances per template.
	sessions, err := sessStore.List("", "")
	if err != nil {
		return nil, fmt.Errorf("listing sessions: %w", err)
	}
	idleByTemplate := make(map[string][]string)
	for _, info := range sessions {
		template := strings.TrimSpace(info.Template)
		if _, ok := genericTemplates[template]; !ok {
			continue
		}
		if !poolSessionIsLiveInfo(info) || strings.TrimSpace(info.TriggerBeadID) != "" {
			continue
		}
		name := strings.TrimSpace(info.SessionName)
		if name == "" {
			name = info.ID
		}
		idleByTemplate[template] = append(idleByTemplate[template], name)
	}
	if len(idleByTemplate) == 0 {
		// No idle instance in this scope: no finding is possible, so skip the
		// ready read entirely.
		return nil, nil
	}

	// Per-template routed lookup, but ONLY for templates that actually have an
	// idle instance. The previous shape ran both reads for every generic pool
	// template; idle templates are typically 1-2, so this is O(idle) not
	// O(templates) round-trips against remote Dolt.
	//
	// Live so bd's raw --status=open filter drops blocked/deferred rows before
	// mapBdStatus collapses them into "open" and the check reports work the
	// instance is correct to leave alone (same tradeoff as
	// listOpenForControllerDemandLive). FederatedReadTier because a relocated
	// class leg answers at exactly the tier asked.
	var findings []poolIdleRoutedWorkFinding
	for template, idle := range idleByTemplate {
		items, err := beads.HandlesFor(store).Live.List(beads.ListQuery{
			Status:   "open",
			TierMode: beads.FederatedReadTier,
			Metadata: map[string]string{beadmeta.RoutedToMetadataKey: template},
		})
		if err != nil {
			return findings, fmt.Errorf("listing routed work for %s: %w", template, err)
		}
		var beadIDs []string
		for _, b := range items {
			if strings.TrimSpace(b.Assignee) != "" || b.Status != "open" {
				continue
			}
			beadIDs = append(beadIDs, b.ID)
		}
		if len(beadIDs) == 0 {
			continue
		}
		sort.Strings(beadIDs)
		sort.Strings(idle)
		findings = append(findings, poolIdleRoutedWorkFinding{
			scope:         label,
			template:      template,
			beadIDs:       beadIDs,
			idleInstances: idle,
		})
	}
	return findings, nil
}
