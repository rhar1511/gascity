package worklifecycle

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

func TestNextClaimGeneration(t *testing.T) {
	for _, tc := range []struct {
		previous string
		want     string
		bad      bool
	}{
		{previous: "", want: "1"},
		{previous: "1", want: "2"},
		{previous: "41", want: "42"},
		{previous: "0", bad: true},
		{previous: "-1", bad: true},
		{previous: "+1", bad: true},
		{previous: "01", bad: true},
		{previous: " 1", bad: true},
		{previous: "9223372036854775808", bad: true},
		{previous: "9223372036854775807", bad: true},
	} {
		got, err := NextClaimGeneration(tc.previous)
		if tc.bad {
			if !errors.Is(err, ErrTransitionChainInvalid) {
				t.Errorf("NextClaimGeneration(%q) = %q, %v; want invalid", tc.previous, got, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("NextClaimGeneration(%q) = %q, %v; want %q", tc.previous, got, err, tc.want)
		}
	}
}

func TestClaimIdentityOperationIDBindsIncarnationAndGeneration(t *testing.T) {
	first, err := ClaimIdentityOperationID("session-1", "secret-token", "7", "3")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ token, epoch, generation string }{
		{"different-token", "7", "3"},
		{"secret-token", "8", "3"},
		{"secret-token", "7", "4"},
	} {
		other, err := ClaimIdentityOperationID("session-1", tc.token, tc.epoch, tc.generation)
		if err != nil || other == first {
			t.Fatalf("operation ID for changed incarnation/generation = %q, %v; want a different ID", other, err)
		}
	}
	if strings.Contains(first, "secret-token") {
		t.Fatalf("operation ID exposed instance token: %q", first)
	}
}

func TestTransitionChainClaimIdentityAtomicAndIdempotent(t *testing.T) {
	fixture := newTransitionChainFixture(t)
	attached := applyTestAttachment(t, fixture, applyTestReservation(t, fixture))
	request := claimIdentityRequest(t, fixture, attached.Receipt.ReceiptID)
	result, err := fixture.chain.ClaimIdentity(request)
	if err != nil {
		t.Fatalf("ClaimIdentity: %v", err)
	}
	if result.ClaimGeneration != "1" || result.Receipt.Kind != transitionKindClaimIdentity || result.Replayed || result.Recovered {
		t.Fatalf("claim result = %+v, want fresh generation 1 transition", result)
	}
	if len(fixture.store.patchRequests) != 3 || len(fixture.permits.requests) != 3 {
		t.Fatalf("typed writes/permits = %d/%d, want reservation, attachment, and one atomic claim", len(fixture.store.patchRequests), len(fixture.permits.requests))
	}
	current, err := fixture.store.DecisionFrontierSourceSnapshot(fixture.work.ID)
	if err != nil || current.Status != "in_progress" || current.Assignee != "controller" ||
		current.Metadata[beadmeta.SessionIDMetadataKey] != "session-1" ||
		current.Metadata[beadmeta.ClaimGenerationMetadataKey] != "1" ||
		current.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] != result.Receipt.ReceiptID {
		t.Fatalf("claimed source = %+v, err %v", current, err)
	}
	if len(fixture.store.patchRequests[2].Patch.Metadata) != 7 || fixture.store.patchRequests[2].Patch.Status == nil || fixture.store.patchRequests[2].Patch.Assignee == nil {
		t.Fatalf("claim writer request = %+v, want one typed patch carrying all claim metadata and status/assignee", fixture.store.patchRequests[2])
	}

	// A lost response can retry the original request. It reuses the exact claim
	// receipt and performs no second work write, allowing the caller to repair
	// the session's reciprocal stamp safely.
	replayed, err := fixture.chain.ClaimIdentity(request)
	if err != nil || !replayed.Replayed || replayed.ClaimGeneration != "1" || replayed.Receipt.ReceiptID != result.Receipt.ReceiptID || len(fixture.store.patchRequests) != 3 {
		t.Fatalf("ClaimIdentity replay = %+v err %v writes %d, want exact receipt without another patch", replayed, err, len(fixture.store.patchRequests))
	}
	currentRequest := request
	currentRequest.ExpectedRevision = result.Receipt.ToVersion
	currentRequest.ExpectedTransitionHead = result.Receipt.ReceiptID
	replayed, err = fixture.chain.ClaimIdentity(currentRequest)
	if err != nil || !replayed.Replayed || replayed.Receipt.ReceiptID != result.Receipt.ReceiptID {
		t.Fatalf("current-head claim repair = %+v err %v, want exact replay", replayed, err)
	}
}

func TestTransitionChainClaimIdentityRejectsStaleInputsAndAssignedPredecessor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*ClaimIdentityRequest, *transitionChainFixture)
		assign string
	}{
		{name: "stale revision", mutate: func(request *ClaimIdentityRequest, _ *transitionChainFixture) { request.ExpectedRevision++ }},
		{name: "stale head", mutate: func(request *ClaimIdentityRequest, _ *transitionChainFixture) {
			request.ExpectedTransitionHead = "old-head"
		}},
		{name: "already assigned predecessor", assign: "someone-else"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newTransitionChainFixture(t)
			attached := applyTestAttachment(t, fixture, applyTestReservation(t, fixture))
			request := claimIdentityRequest(t, fixture, attached.Receipt.ReceiptID)
			if tc.mutate != nil {
				tc.mutate(&request, fixture)
			}
			if tc.assign != "" {
				fixture.store.mu.Lock()
				fixture.store.source.Assignee = tc.assign
				fixture.store.mu.Unlock()
			}
			beforeWrites, beforePermits := len(fixture.store.patchRequests), len(fixture.permits.requests)
			if _, err := fixture.chain.ClaimIdentity(request); !errors.Is(err, ErrTransitionChainStale) {
				t.Fatalf("ClaimIdentity error = %v, want stale refusal", err)
			}
			if len(fixture.store.patchRequests) != beforeWrites || len(fixture.permits.requests) != beforePermits {
				t.Fatalf("stale/assigned claim reached typed writer or permit: %d->%d writes, %d->%d permits", beforeWrites, len(fixture.store.patchRequests), beforePermits, len(fixture.permits.requests))
			}
		})
	}
}

func TestTransitionChainClaimIdentityRejectsMalformedPriorGeneration(t *testing.T) {
	for _, previous := range []string{"bad", "01", "-1", "9223372036854775807"} {
		t.Run(previous, func(t *testing.T) {
			fixture := newTransitionChainFixture(t)
			attached := applyTestAttachment(t, fixture, applyTestReservation(t, fixture))
			fixture.store.mu.Lock()
			fixture.store.source.Metadata[beadmeta.ClaimGenerationMetadataKey] = previous
			fixture.store.mu.Unlock()
			request := claimIdentityRequest(t, fixture, attached.Receipt.ReceiptID)
			beforeWrites, beforePermits := len(fixture.store.patchRequests), len(fixture.permits.requests)
			if _, err := fixture.chain.ClaimIdentity(request); !errors.Is(err, ErrTransitionChainInvalid) {
				t.Fatalf("ClaimIdentity with generation %q = %v, want invalid", previous, err)
			}
			if len(fixture.store.patchRequests) != beforeWrites || len(fixture.permits.requests) != beforePermits {
				t.Fatalf("malformed generation reached typed writer or permit")
			}
		})
	}
}

func claimIdentityRequest(t *testing.T, fixture *transitionChainFixture, parentID string) ClaimIdentityRequest {
	t.Helper()
	current, err := fixture.store.DecisionFrontierSourceSnapshot(fixture.work.ID)
	if err != nil {
		t.Fatal(err)
	}
	return ClaimIdentityRequest{
		IssueID: current.ID, SessionID: "session-1", InstanceToken: "instance-token-1", RuntimeEpoch: "7",
		ExpectedRevision: current.Revision, ExpectedTransitionHead: parentID,
		SessionName: "worker-session", WorkDir: "/tmp/work", WorkBranch: "claim-branch",
		Evidence: fixture.evidence,
	}
}
