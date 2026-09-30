package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/decisionfrontier"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func TestControllerStateDecisionFrontierDeliveryDisabledKeepsSourceHeld(t *testing.T) {
	store := frontierCompositionStore(t)
	provider := runtime.NewFake()
	state := frontierCompositionState(&config.City{Workspace: config.Workspace{Name: "test-city"}}, store, provider)

	service := state.DecisionFrontierService()
	if service.Delivery != nil || service.Verifier != nil {
		t.Fatalf("disabled decision-frontier service = verifier %T, delivery %T, want both unavailable", service.Verifier, service.Delivery)
	}
	frontier := ensureCompositionFrontier(t, service, store, "wrk-disabled")
	if frontier.Prompt.Status != "unavailable" {
		t.Fatalf("disabled prompt status = %q, want unavailable", frontier.Prompt.Status)
	}
	work, err := store.Get("wrk-disabled")
	if err != nil {
		t.Fatal(err)
	}
	if work.Metadata[beadmeta.DecisionFrontierHoldMetadataKey] == "" {
		t.Fatal("disabled prompt delivery did not preserve the durable source hold")
	}
	if len(provider.Calls) != 0 {
		t.Fatalf("disabled prompt delivery made runtime calls: %+v", provider.Calls)
	}
}

func TestControllerStateDecisionFrontierUsesConfiguredTargetAndReloadSnapshot(t *testing.T) {
	store := frontierCompositionStore(t, "mayor", "reviewer")
	provider := runtime.NewFake()
	if err := provider.Start(context.Background(), frontierCompositionSessionName("mayor"), runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	if err := provider.Start(context.Background(), frontierCompositionSessionName("reviewer"), runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	firstConfig := frontierCompositionConfig("mayor", "mayor", "reviewer")
	state := frontierCompositionState(firstConfig, store, provider)

	firstService := state.DecisionFrontierService()
	if firstService.Delivery == nil {
		t.Fatal("configured named target did not compose prompt delivery")
	}
	first := ensureCompositionFrontier(t, firstService, store, "wrk-mayor")
	if first.Prompt.Status != "accepted" {
		t.Fatalf("configured target prompt status = %q, want accepted", first.Prompt.Status)
	}
	if got := countRuntimeCalls(provider.Calls, "Nudge", frontierCompositionSessionName("mayor")); got != 1 {
		t.Fatalf("configured target sent %d prompts to Mayor, want one", got)
	}
	if got := countRuntimeCalls(provider.Calls, "Nudge", frontierCompositionSessionName("reviewer")); got != 0 {
		t.Fatalf("configured target sent %d prompts to the other session, want zero", got)
	}

	// A replay after reload keeps the durable original binding. It must not
	// send the existing prompt to the newly configured target.
	state.updateConfigAndProviderOnly(frontierCompositionConfig("reviewer", "mayor", "reviewer"), provider)
	reloadedService := state.DecisionFrontierService()
	if reloadedService.Delivery == nil {
		t.Fatal("reloaded configured named target disabled prompt delivery")
	}
	if _, err := reloadedService.Ensure(context.Background(), store, decisionfrontier.Scope{CityRef: "city:test-city", StoreRef: "city:test-city"}, "wrk-mayor", first.WorkRevision,
		decisionfrontier.Proposal{Questions: []decisionfrontier.Question{{ID: "choose", Title: "Choose", Prompt: "Choose."}}}); err != nil {
		t.Fatalf("replay old bound prompt after target reload: %v", err)
	}
	if got := countRuntimeCalls(provider.Calls, "Nudge", frontierCompositionSessionName("reviewer")); got != 0 {
		t.Fatalf("replay moved the existing prompt to the new target (%d reviewer sends)", got)
	}

	// New work uses the latest captured config and therefore reaches the
	// configured reviewer session.
	second := ensureCompositionFrontier(t, reloadedService, store, "wrk-reviewer")
	if second.Prompt.Status != "accepted" {
		t.Fatalf("reloaded target prompt status = %q, want accepted", second.Prompt.Status)
	}
	if got := countRuntimeCalls(provider.Calls, "Nudge", frontierCompositionSessionName("reviewer")); got != 1 {
		t.Fatalf("new work sent %d prompts to reloaded target, want one", got)
	}
}

func TestControllerStateDecisionFrontierRejectsAmbiguousAndInvalidTarget(t *testing.T) {
	store := frontierCompositionStore(t, "mayor")
	provider := runtime.NewFake()
	for _, tc := range []struct {
		name   string
		config *config.City
	}{
		{name: "ambiguous bare name", config: frontierAmbiguousCompositionConfig()},
		{name: "noncanonical whitespace", config: frontierCompositionConfig(" mayor ", "mayor")},
		{name: "unconfigured identity", config: frontierCompositionConfig("missing", "mayor")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := frontierCompositionState(tc.config, store, provider).DecisionFrontierService()
			if service.Delivery != nil {
				t.Fatalf("invalid prompt target composed delivery: %T", service.Delivery)
			}
		})
	}
}

func frontierCompositionState(cfg *config.City, store beads.Store, provider runtime.Provider) *controllerState {
	return &controllerState{
		cfg:           cfg,
		sp:            provider,
		cityName:      "test-city",
		cityPath:      ".",
		cityBeadStore: store,
	}
}

func frontierCompositionConfig(target string, identities ...string) *config.City {
	cfg := &config.City{
		Workspace:        config.Workspace{Name: "test-city"},
		DecisionFrontier: config.DecisionFrontierConfig{PromptTarget: target},
	}
	for _, identity := range identities {
		cfg.Agents = append(cfg.Agents, config.Agent{Name: identity})
		cfg.NamedSessions = append(cfg.NamedSessions, config.NamedSession{Name: identity, Template: identity, Scope: "city", Mode: "always"})
	}
	return cfg
}

func frontierCompositionSessionName(identity string) string {
	return config.NamedSessionRuntimeName("test-city", config.Workspace{Name: "test-city"}, identity)
}

func frontierAmbiguousCompositionConfig() *config.City {
	cfg := &config.City{
		Workspace:        config.Workspace{Name: "test-city"},
		DecisionFrontier: config.DecisionFrontierConfig{PromptTarget: "mayor"},
		Agents: []config.Agent{
			{Name: "mayor", BindingName: "one"},
			{Name: "mayor", BindingName: "two"},
		},
		NamedSessions: []config.NamedSession{
			{Name: "mayor", Template: "mayor", BindingName: "one", Scope: "city"},
			{Name: "mayor", Template: "mayor", BindingName: "two", Scope: "city"},
		},
	}
	return cfg
}

func frontierCompositionStore(t *testing.T, identities ...string) *beads.MemStore {
	t.Helper()
	store := beads.NewMemStore()
	store.HonorExplicitIDs = true
	for _, id := range append([]string{"wrk-disabled", "wrk-mayor", "wrk-reviewer"}, identities...) {
		if len(id) > 4 && id[:4] == "wrk-" {
			if _, err := store.Create(beads.Bead{ID: id, Type: "task", Title: "Resolve human decision"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, identity := range identities {
		sessionName := frontierCompositionSessionName(identity)
		if _, err := store.Create(beads.Bead{
			ID: "session-" + identity, Type: session.BeadType, Status: "open", Title: identity,
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template": identity, "agent_name": identity, "alias": identity,
				"session_name": sessionName, "state": "active", "generation": "7",
				"instance_token": "token-" + identity, "configured_named_session": "true",
				"configured_named_identity": identity,
			},
			CreatedAt: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC),
		}); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func ensureCompositionFrontier(t *testing.T, service decisionfrontier.Service, store beads.Store, workID string) decisionfrontier.Frontier {
	t.Helper()
	work, err := store.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := decisionfrontier.WorkRevision(work)
	if err != nil {
		t.Fatal(err)
	}
	frontier, err := service.Ensure(context.Background(), store, decisionfrontier.Scope{CityRef: "city:test-city", StoreRef: "city:test-city"}, workID, revision,
		decisionfrontier.Proposal{Questions: []decisionfrontier.Question{{ID: "choose", Title: "Choose", Prompt: "Choose."}}})
	if err != nil {
		t.Fatalf("ensure frontier for %s: %v", workID, err)
	}
	return frontier
}

func countRuntimeCalls(calls []runtime.Call, method, name string) int {
	count := 0
	for _, call := range calls {
		if call.Method == method && call.Name == name {
			count++
		}
	}
	return count
}
