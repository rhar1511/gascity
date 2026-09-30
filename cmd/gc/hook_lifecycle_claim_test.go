package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestEnrolledHookClaimUsesOnlyLifecycleTransitionPath(t *testing.T) {
	candidate := beads.Bead{
		ID: "work-1", Type: "task", Status: "open",
		Metadata: map[string]string{
			beadmeta.LifecycleAdmissionReceiptV2MetadataKey: "v2-receipt",
			beadmeta.LifecycleTransitionHeadMetadataKey:     "head-1",
			beadmeta.RoutedToMetadataKey:                    "worker",
			beadmeta.RootBeadIDMetadataKey:                  "root-1",
			beadmeta.ContinuationGroupMetadataKey:           "review",
		},
	}
	opts := hookClaimOptions{
		Assignee: "worker", SessionID: "session-1", IdentityCandidates: []string{"worker"},
		RouteTargets: []string{"worker"}, Env: []string{"GC_SESSION_ID=session-1"}, JSON: true,
		Lifecycle: config.LifecycleConfig{AdmissionEnabled: true},
	}
	var lifecycleCalls, genericClaims, workIdentityWrites, sessionClaimWrites int
	var continuationQueries, continuationAssignments, releases int
	ops := hookClaimOps{
		ClaimLifecycle: func(_ context.Context, bead beads.Bead, _ hookClaimOptions) (beads.Bead, hookLifecycleClaimResult, error) {
			lifecycleCalls++
			bead.Status = "in_progress"
			bead.Assignee = "worker"
			bead.Metadata[beadmeta.SessionIDMetadataKey] = "session-1"
			bead.Metadata[beadmeta.ClaimGenerationMetadataKey] = "1"
			bead.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] = "claim-head-1"
			return bead, hookLifecycleClaimResult{Generation: "1", ReceiptID: "claim-receipt-1"}, nil
		},
		Claim: func(context.Context, string, []string, string, string) (beads.Bead, bool, error) {
			genericClaims++
			return beads.Bead{}, false, nil
		},
		StampWorkMeta: func(context.Context, string, []string, string, string, map[string]string) error {
			workIdentityWrites++
			return nil
		},
		StampSessionClaim: func(string, string, string) error {
			sessionClaimWrites++
			return nil
		},
		ListContinuation: func(context.Context, string, []string, string, string) ([]beads.Bead, error) {
			continuationQueries++
			return nil, nil
		},
		AssignContinuation: func(context.Context, string, []string, string, string) error {
			continuationAssignments++
			return nil
		},
		Release: func(context.Context, string, []string, string, string) (bool, error) {
			releases++
			return true, nil
		},
		PublishRunMap:            func(string, string, ...string) error { return nil },
		EmitExecutionStepStarted: func(beads.Bead, string, []string, string) {},
	}
	var stdout, stderr bytes.Buffer
	result := claimFirstEligibleHookCandidate([]beads.Bead{candidate}, opts, ops, "/work", &stdout, &stderr)
	if !result.terminal || result.code != 0 {
		t.Fatalf("claim result = %+v; stderr %q", result, stderr.String())
	}
	if lifecycleCalls != 1 || genericClaims != 0 || workIdentityWrites != 0 || sessionClaimWrites != 0 ||
		continuationQueries != 0 || continuationAssignments != 0 || releases != 0 {
		t.Fatalf("claim calls lifecycle=%d generic=%d work identity writes=%d session claim writes=%d continuation queries=%d assignments=%d releases=%d",
			lifecycleCalls, genericClaims, workIdentityWrites, sessionClaimWrites, continuationQueries, continuationAssignments, releases)
	}
	if stdout.Len() == 0 {
		t.Fatal("lifecycle claim result was not written")
	}
}
