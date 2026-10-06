package main

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/sling"
)

// TestBeadPolicyStoreResolvesConditionalWritesThroughWrapper pins the stage-3
// wiring hazard: every factory store is policy-wrapped, and interface
// embedding hides the factory's conditional-writes stamp — without the
// wrapper's declared resolution target, a require deployment would silently
// resolve unset→legacy through the wrapper on every consumer.
func TestBeadPolicyStoreResolvesConditionalWritesThroughWrapper(t *testing.T) {
	result, err := beads.OpenStoreAtForCity(context.Background(), beads.StoreOpenOptions{
		ScopeRoot:         t.TempDir(),
		Provider:          "file",
		ConditionalWrites: gate.Require,
		OpenFileStore:     func() (beads.Store, error) { return beads.NewMemStore(), nil },
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity: %v", err)
	}

	wrapped := wrapStoreWithBeadPolicies(result.Store, nil)
	if _, _, ok := unwrapBeadPolicyStore(wrapped); !ok {
		t.Fatalf("test premise: store %T is not policy-wrapped", wrapped)
	}

	writer, diag, resolveErr := beads.ResolveConditionalWriter(wrapped)
	if resolveErr != nil || diag != nil {
		t.Fatalf("resolve through policy wrapper = diag %v err %v, want the stamped store's writer", diag, resolveErr)
	}
	if writer == nil {
		t.Fatal("resolve through policy wrapper returned no writer: the require stamp was hidden by interface embedding")
	}
}

func TestCliBeadRouterRoutesThroughPolicyWrappedStoreCAS(t *testing.T) {
	base := beads.NewMemStore()
	created, err := base.Create(beads.Bead{Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("seed bead: %v", err)
	}
	wrapped := wrapStoreWithBeadPolicies(base, &config.City{})
	router := cliBeadRouter{deps: &slingDeps{Store: wrapped}}
	if err := router.Route(context.Background(), sling.RouteRequest{BeadID: created.ID, Target: "pool/worker"}); err != nil {
		t.Fatalf("route through policy wrapper: %v", err)
	}
	got, err := base.Get(created.ID)
	if err != nil {
		t.Fatalf("read routed bead: %v", err)
	}
	if got.Metadata[beadmeta.RoutedToMetadataKey] != "pool/worker" {
		t.Fatalf("gc.routed_to = %q, want pool/worker", got.Metadata[beadmeta.RoutedToMetadataKey])
	}
}
