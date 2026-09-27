package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

func TestReadyJSONCarriesTrustedLifecycleScopeIntoHookClaim(t *testing.T) {
	_, admissionKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, acceptanceKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	lifecycleCfg := config.LifecycleConfig{
		AdmissionEnabled: true,
		AdmissionAuthorities: map[string]string{
			"triage": base64.StdEncoding.EncodeToString(admissionKey.Public().(ed25519.PublicKey)),
		},
		AcceptanceAuthorities: map[string]string{
			"reviewer": base64.StdEncoding.EncodeToString(acceptanceKey.Public().(ed25519.PublicKey)),
		},
	}
	workflow := "mol-polecat-work"
	cityCfg := &config.City{Lifecycle: lifecycleCfg, Agents: []config.Agent{{Name: "worker", DefaultSlingFormula: &workflow}}}
	makeCandidate := func(scope string, controllerVerified, intent, receiptPresent bool) beads.Bead {
		receipt := worklifecycle.AdmissionReceipt{
			Version:             1,
			WorkItemID:          "work-1",
			Scope:               scope,
			Route:               "worker",
			Workflow:            "mol-polecat-work",
			MergeStrategy:       "mr",
			Deliverable:         "reviewed code change",
			Verification:        "tests and merge request",
			AcceptanceAuthority: "reviewer",
			AdmittedBy:          "triage",
		}
		encoded, err := worklifecycle.SignAdmissionReceipt(receipt, admissionKey)
		if err != nil {
			t.Fatal(err)
		}
		digest, err := worklifecycle.AdmissionDigest(receipt)
		if err != nil {
			t.Fatal(err)
		}
		metadata := map[string]string{
			beadmeta.LifecycleAdmissionReceiptMetadataKey: encoded,
			beadmeta.RoutedToMetadataKey:                  "worker",
			beadmeta.ExecutionRoutedToMetadataKey:         "worker",
			beadmeta.MergeStrategyMetadataKey:             "mr",
			"workflow_id":                                 "wf-1",
		}
		if controllerVerified {
			metadata[beadmeta.LifecycleMaterializationMetadataKey], err = encodeLifecycleMaterialization(lifecycleMaterialization{
				Version: 1, State: "attached", Scope: scope, Contract: digest, Route: "worker",
				Workflow: "mol-polecat-work", MergeStrategy: "mr", Token: "controller-token", WorkflowID: "wf-1",
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		bead := beads.Bead{
			ID:       "work-1",
			Title:    "implement lifecycle fix",
			Type:     "task",
			Status:   "open",
			Labels:   nil,
			Metadata: metadata,
		}
		if intent {
			bead.Labels = []string{worklifecycle.AdmissionIntentLabel}
		}
		if !receiptPresent {
			delete(bead.Metadata, beadmeta.LifecycleAdmissionReceiptMetadataKey)
		}
		return bead
	}

	for _, tc := range []struct {
		name      string
		scope     string
		verified  bool
		intent    bool
		receipt   bool
		wantClaim bool
	}{
		{name: "local receipt with verified workflow", scope: worklifecycle.ScopeForStore("pilot", "city:pilot"), verified: true, intent: true, receipt: true, wantClaim: true},
		{name: "receipt replayed from another city", scope: worklifecycle.ScopeForStore("elsewhere", "city:elsewhere"), verified: true, intent: true, receipt: true, wantClaim: false},
		{name: "worker workflow metadata without controller verification", scope: worklifecycle.ScopeForStore("pilot", "city:pilot"), intent: true, receipt: true, wantClaim: false},
		{name: "removed intent and receipt after durable enrollment", scope: worklifecycle.ScopeForStore("pilot", "city:pilot"), verified: true, wantClaim: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &beads.MemStore{IDPrefix: "work", HonorExplicitIDs: true}
			if _, err := store.Create(makeCandidate(tc.scope, tc.verified, tc.intent, tc.receipt)); err != nil {
				t.Fatal(err)
			}
			leg := readyLeg{
				label:          "city",
				store:          store,
				ref:            "city:pilot",
				sourceStoreRef: "city:pilot",
				cityName:       "pilot",
			}
			items, owners, err := federateReadyBeadsWithOwner([]readyLeg{leg}, beads.ReadyQuery{TierMode: beads.FederatedReadTier})
			if err != nil {
				t.Fatalf("federate ready rows: %v", err)
			}
			readyJSON, err := json.Marshal(toReadyBeads(items, nil, owners))
			if err != nil {
				t.Fatal(err)
			}
			decoded, _, err := decodeHookClaimBeads(string(readyJSON))
			if err != nil {
				t.Fatalf("decode gc ready output: %v", err)
			}
			if len(decoded) != 1 || decoded[0].LifecycleScope != worklifecycle.ScopeForStore("pilot", "city:pilot") || decoded[0].SourceStoreRef != "city:pilot" {
				t.Fatalf("decoded ready row = %+v, want city-scoped provenance", decoded)
			}
			if !tc.verified {
				if decoded[0].Metadata[beadmeta.LifecycleMaterializationMetadataKey] != "" {
					t.Fatalf("unverified ready row unexpectedly has controller materialization: %+v", decoded[0].Metadata)
				}
				decision := worklifecycle.EvaluateAdmission(decoded[0], lifecycleCfg, decoded[0].LifecycleScope)
				if !decision.Requested || !decision.Admitted || lifecycleAdmissionRouteMatches(cityCfg, decoded[0], decoded[0].LifecycleScope) {
					t.Fatalf("unverified admission state: requested=%v admitted=%v routeMatches=%v reason=%q", decision.Requested, decision.Admitted, lifecycleAdmissionRouteMatches(cityCfg, decoded[0], decoded[0].LifecycleScope), decision.Reason)
				}
				filtered := filterHookLifecycleCandidates(decoded, hookClaimOptions{Lifecycle: lifecycleCfg, LifecycleCity: cityCfg, TrustedLifecycleScope: true, RouteTargets: []string{"worker"}}, io.Discard)
				if len(filtered) != 0 {
					t.Fatalf("worker-authored workflow metadata passed lifecycle filter: %+v", filtered)
				}
			}

			attempts := 0
			ops := hookClaimOps{
				Runner: func(string, string) (string, error) { return string(readyJSON), nil },
				Claim: func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
					attempts++
					row, err := store.Get(id)
					if err != nil {
						return beads.Bead{}, false, err
					}
					row.Status = "in_progress"
					row.Assignee = assignee
					return row, true, nil
				},
				DrainAck: func(io.Writer) error { return nil },
			}
			var stdout, stderr bytes.Buffer
			_ = doHookClaim("gc ready --json", "/city", hookClaimOptions{
				Assignee:              "worker-session",
				IdentityCandidates:    []string{"worker-session"},
				RouteTargets:          []string{"worker"},
				Lifecycle:             lifecycleCfg,
				LifecycleCity:         cityCfg,
				TrustedLifecycleScope: true,
				JSON:                  true,
			}, ops, &stdout, &stderr)
			if (attempts == 1) != tc.wantClaim {
				t.Fatalf("claim attempts = %d, wantClaim=%v; stdout=%q stderr=%q", attempts, tc.wantClaim, stdout.String(), stderr.String())
			}
			if !tc.wantClaim && !strings.Contains(stderr.String(), "holding lifecycle item work-1") {
				t.Fatalf("stderr = %q, want a lifecycle hold explanation", stderr.String())
			}
		})
	}
}
