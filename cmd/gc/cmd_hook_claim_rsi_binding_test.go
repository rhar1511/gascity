package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rsipolicy"
)

func TestHookClaimStampsRSIExecutionBindingOnce(t *testing.T) {
	bead := beads.Bead{Metadata: map[string]string{
		beadmeta.RSIRoleMetadataKey:             beadmeta.RSIRoleImprover,
		beadmeta.RSIExecutionBindingMetadataKey: `{"actor_id":"formula-spoof","session_id":"formula-spoof"}`,
	}}
	patch := hookClaimIdentityPatch(bead, hookClaimOptions{
		Assignee: "controller-selected-actor",
		Env:      []string{"GC_SESSION_ID=controller-session"},
	}, hookClaimOps{}, "/tmp/store")
	binding, err := rsipolicy.ParseExecutionBinding(patch[beadmeta.RSIExecutionBindingMetadataKey])
	if err != nil {
		t.Fatalf("parse stamped binding: %v", err)
	}
	if binding.ActorID != "controller-selected-actor" || binding.SessionID != "controller-session" {
		t.Fatalf("binding = %+v", binding)
	}

	bead.Metadata[beadmeta.RSIExecutionBindingMetadataKey] = patch[beadmeta.RSIExecutionBindingMetadataKey]
	bead.Metadata[beadmeta.ClaimedAtMetadataKey] = "2026-01-01T00:00:00Z"
	second := hookClaimIdentityPatch(bead, hookClaimOptions{
		Assignee: "spoofed-actor",
		Env:      []string{"GC_SESSION_ID=spoofed-session"},
	}, hookClaimOps{}, "/tmp/store")
	if _, ok := second[beadmeta.RSIExecutionBindingMetadataKey]; ok {
		t.Fatal("existing RSI execution binding was mutable through a later claim")
	}
}
