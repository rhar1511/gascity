package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// hookClaimLifecycleWork submits the keyless Q54 request through the local
// controller API. The hook sends only its managed session incarnation and the
// exact source snapshot; it never receives or handles protected-mutation keys.
func hookClaimLifecycleWork(ctx context.Context, candidate beads.Bead, opts hookClaimOptions) (beads.Bead, hookLifecycleClaimResult, error) {
	if !opts.Lifecycle.AdmissionEnabled || !opts.TrustedLifecycleScope || opts.ResolveLifecycleStore == nil {
		return beads.Bead{}, hookLifecycleClaimResult{}, fmt.Errorf("enrolled lifecycle claim requires trusted controller provenance")
	}
	cityPath := strings.TrimSpace(opts.LifecycleCityPath)
	if cityPath == "" {
		return beads.Bead{}, hookLifecycleClaimResult{}, fmt.Errorf("controller city path is unavailable")
	}
	sessionID := hookClaimEnvValue(opts.Env, "GC_SESSION_ID")
	instanceToken := hookClaimEnvValue(opts.Env, "GC_INSTANCE_TOKEN")
	runtimeEpoch := hookClaimEnvValue(opts.Env, "GC_RUNTIME_EPOCH")
	if sessionID == "" || instanceToken == "" || !canonicalHookClaimEpoch(runtimeEpoch) {
		return beads.Bead{}, hookLifecycleClaimResult{}, fmt.Errorf("enrolled lifecycle claim requires GC_SESSION_ID, GC_INSTANCE_TOKEN, and canonical GC_RUNTIME_EPOCH")
	}
	store, current, ok := lifecycleAuthoritativeCandidate(candidate, opts)
	if !ok || store == nil || current.ID != candidate.ID || current.Revision <= 0 {
		return beads.Bead{}, hookLifecycleClaimResult{}, fmt.Errorf("exact admitted work source could not be re-read from its trusted store")
	}
	head := strings.TrimSpace(current.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey])
	if head == "" {
		return beads.Bead{}, hookLifecycleClaimResult{}, fmt.Errorf("admitted work has no current Q54 transition head")
	}
	client, fallbackReason := supervisorFallthroughAPIClient(cityPath)
	if client == nil {
		return beads.Bead{}, hookLifecycleClaimResult{}, fmt.Errorf("controller claim API is unavailable (%s)", fallbackReason)
	}
	claim, err := client.ClaimLifecycleWork(ctx, api.LifecycleClaimSubmitRequest{
		WorkID: current.ID, SourceStoreRef: current.SourceStoreRef,
		ExpectedRevision: current.Revision, ExpectedTransitionHead: head,
		SessionID: sessionID, InstanceToken: instanceToken, RuntimeEpoch: runtimeEpoch,
	})
	if err != nil {
		return beads.Bead{}, hookLifecycleClaimResult{}, fmt.Errorf("controller lifecycle claim failed: %w", err)
	}
	if claim.WorkID != current.ID || claim.Actor == "" || !hookClaimHasIdentity(claim.Actor, opts.IdentityCandidates) ||
		!canonicalHookClaimEpoch(claim.ClaimGeneration) || strings.TrimSpace(claim.ReceiptID) == "" {
		return beads.Bead{}, hookLifecycleClaimResult{}, fmt.Errorf("controller returned a lifecycle claim that does not match this work and session")
	}
	readback, err := store.Get(current.ID)
	if err != nil || readback.ID != current.ID {
		return beads.Bead{}, hookLifecycleClaimResult{}, fmt.Errorf("read back committed lifecycle claim: %w", err)
	}
	readback.SourceStoreRef = current.SourceStoreRef
	readback.LifecycleScope = current.LifecycleScope
	if !strings.EqualFold(strings.TrimSpace(readback.Status), "in_progress") || strings.TrimSpace(readback.Assignee) != claim.Actor ||
		readback.Metadata[beadmeta.SessionIDMetadataKey] != sessionID ||
		readback.Metadata[beadmeta.ClaimGenerationMetadataKey] != claim.ClaimGeneration ||
		strings.TrimSpace(readback.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey]) == "" {
		return beads.Bead{}, hookLifecycleClaimResult{}, fmt.Errorf("lifecycle claim source readback does not match the controller receipt")
	}
	return readback, hookLifecycleClaimResult{
		Generation: claim.ClaimGeneration, ReceiptID: claim.ReceiptID, Replayed: claim.Replayed,
	}, nil
}

func canonicalHookClaimEpoch(raw string) bool {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return false
	}
	epoch, err := strconv.ParseInt(raw, 10, 64)
	return err == nil && epoch > 0 && strconv.FormatInt(epoch, 10) == raw
}
