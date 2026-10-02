package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

type lifecycleRecoveryAdmissionTestState struct {
	State
	verify func(context.Context, LifecycleRecoveryAdmissionRequest) error
}

func (s *lifecycleRecoveryAdmissionTestState) VerifyLifecycleRecoveryAdmission(ctx context.Context, request LifecycleRecoveryAdmissionRequest) error {
	return s.verify(ctx, request)
}

func TestLifecycleRecoverySubmitRejectsV2WithoutAttachmentAndPolicyProof(t *testing.T) {
	state := newFakeState(t)
	state.cfg.Rigs = nil
	state.cfg.Workspace.Prefix = "test"
	store := &beads.MemStore{IDPrefix: "test", HonorExplicitIDs: true}
	state.cityBeadStore = store
	state.sessionsBeadStore = store
	state.stores = map[string]beads.Store{}
	const workID = "test-recovery-work"
	scope := worklifecycle.ScopeForStore(state.cityName, "city:"+state.cityName)

	admissionPublic, admissionPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	acceptancePublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	recoveryPublic, recoveryPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	state.cfg.Lifecycle = config.LifecycleConfig{
		AdmissionEnabled:            true,
		RecoveryEnabled:             true,
		EscalationTarget:            "attention",
		AdmissionV2PrimaryAuthority: "triage",
		AdmissionV2Authorities: map[string]string{
			"triage": base64.StdEncoding.EncodeToString(admissionPublic),
		},
		AcceptanceAuthorities: map[string]string{"reviewer": base64.StdEncoding.EncodeToString(acceptancePublic)},
		RecoveryAuthorities: map[string]config.LifecycleRecoveryAuthority{
			"recovery": {
				PublicKey: base64.StdEncoding.EncodeToString(recoveryPublic),
				Actions:   []string{"nudge"},
				Scopes:    []string{scope},
			},
		},
	}
	admission, err := worklifecycle.SignAdmissionReceiptV2(worklifecycle.AdmissionReceiptV2{
		Version: 2, WorkItemID: workID, Scope: scope, ExpectedWorkRevision: 1, Route: "test-rig/worker", Workflow: "review",
		RoutingPolicyDigest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		MergeStrategy:       "mr", Deliverable: "reviewed patch", Verification: "focused tests",
		AcceptanceAuthority: "reviewer", AdmittedBy: "triage",
	}, admissionPrivate)
	if err != nil {
		t.Fatal(err)
	}
	var admissionReceipt worklifecycle.AdmissionReceiptV2
	if err := json.Unmarshal([]byte(admission), &admissionReceipt); err != nil {
		t.Fatal(err)
	}
	digest, err := worklifecycle.AdmissionDigestV2(admissionReceipt)
	if err != nil {
		t.Fatal(err)
	}
	materialization, err := json.Marshal(struct {
		Version          int    `json:"version"`
		State            string `json:"state"`
		Scope            string `json:"scope"`
		Contract         string `json:"contract"`
		Route            string `json:"route"`
		Workflow         string `json:"workflow"`
		MergeStrategy    string `json:"merge_strategy"`
		Token            string `json:"token"`
		WorkflowID       string `json:"workflow_id"`
		SourceID         string `json:"source_id"`
		SourceStoreRef   string `json:"source_store_ref"`
		WorkflowStoreRef string `json:"workflow_store_ref"`
		AdmissionReceipt string `json:"admission_receipt"`
	}{1, "attached", scope, digest, "test-rig/worker", "review", "mr", "materialization-token", "test-workflow", workID, "city:" + state.cityName, "city:" + state.cityName, admission})
	if err != nil {
		t.Fatal(err)
	}
	work, err := store.Create(beads.Bead{
		ID: workID, Title: "authorized recovery test", Type: "task", Labels: []string{worklifecycle.AdmissionIntentLabel},
		Metadata: map[string]string{
			beadmeta.LifecycleAdmissionReceiptV2MetadataKey: admission,
			beadmeta.LifecycleMaterializationMetadataKey:    string(materialization),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager := session.NewManagerWithOptions(store, state.sp)
	info, err := manager.CreateSession(context.Background(), session.CreateOptions{
		Template: "worker", Command: "claude", WorkDir: t.TempDir(), Provider: "claude",
	})
	if err != nil {
		t.Fatal(err)
	}
	owner := info.ID
	status := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{
		Status: &status, Assignee: &owner,
		Metadata: map[string]string{
			beadmeta.ClaimGenerationMetadataKey: "claim-1",
			beadmeta.SessionIDMetadataKey:       info.ID,
		},
	}); err != nil {
		t.Fatal(err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	front := session.NewStore(beads.SessionStore{Store: store})
	if _, err := front.SetCurrentClaimForGeneration(info.ID, work.ID, "claim-1"); err != nil {
		t.Fatal(err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	request, err := worklifecycle.SignRecoveryRequest(worklifecycle.RecoveryRequest{
		Version: 1, RequestID: "recovery-api-1", Action: "nudge", Scope: scope,
		WorkItemID: work.ID, ExpectedRevision: work.Revision, Owner: work.Assignee,
		ClaimGeneration: work.Metadata[beadmeta.ClaimGenerationMetadataKey],
		SessionID:       info.ID, SessionGeneration: info.Generation,
		Message:  "Please report the current state and verifiable progress.",
		IssuedAt: now.Add(-time.Second).Format(time.RFC3339Nano), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339Nano),
		AuthorizedBy: "recovery",
	}, recoveryPrivate)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{state: &lifecycleRecoveryAdmissionTestState{
		State: state,
		verify: func(_ context.Context, request LifecycleRecoveryAdmissionRequest) error {
			adapter, err := worklifecycle.NewAdmissionAttachmentAdapter(request.WorkStore)
			if err != nil {
				return err
			}
			_, _, err = adapter.VerifyForTransition(request.Work.ID, state.cfg.Lifecycle, request.Scope)
			return err
		},
	}}
	input := &LifecycleRecoverySubmitInput{CityScope: CityScope{CityName: state.cityName}, Body: request}
	if _, err := server.humaHandleLifecycleRecoverySubmit(context.Background(), input); err == nil || !strings.Contains(err.Error(), "current, attached lifecycle admission") {
		t.Fatalf("recovery intake error = %v, want v2 attachment/policy proof hold", err)
	}
	intents, err := store.List(beads.ListQuery{Type: "lifecycle-intent", AllowScan: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(intents) != 0 {
		t.Fatalf("unproved v2 source created durable recovery intent(s): %+v", intents)
	}
	if calls := state.sp.CountCalls("Nudge", info.SessionName); calls != 0 {
		t.Fatalf("unproved recovery intake performed %d runtime nudges", calls)
	}
}
