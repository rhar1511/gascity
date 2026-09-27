package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

func TestLifecycleRecoverySubmitPersistsHeldIntentWithoutAction(t *testing.T) {
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
		AdmissionEnabled: true,
		RecoveryEnabled:  true,
		EscalationTarget: "attention",
		AdmissionAuthorities: map[string]string{
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
	admission, err := worklifecycle.SignAdmissionReceipt(worklifecycle.AdmissionReceipt{
		Version: 1, WorkItemID: workID, Scope: scope, Route: "worker", Workflow: "review",
		MergeStrategy: "mr", Deliverable: "reviewed patch", Verification: "focused tests",
		AcceptanceAuthority: "reviewer", AdmittedBy: "triage",
	}, admissionPrivate)
	if err != nil {
		t.Fatal(err)
	}
	var admissionReceipt worklifecycle.AdmissionReceipt
	if err := json.Unmarshal([]byte(admission), &admissionReceipt); err != nil {
		t.Fatal(err)
	}
	digest, err := worklifecycle.AdmissionDigest(admissionReceipt)
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
	}{1, "attached", scope, digest, "worker", "review", "mr", "materialization-token", "test-workflow", workID, "city:" + state.cityName, "city:" + state.cityName, admission})
	if err != nil {
		t.Fatal(err)
	}
	work, err := store.Create(beads.Bead{
		ID: workID, Title: "authorized recovery test", Type: "task", Labels: []string{worklifecycle.AdmissionIntentLabel},
		Metadata: map[string]string{
			beadmeta.LifecycleAdmissionReceiptMetadataKey: admission,
			beadmeta.LifecycleMaterializationMetadataKey:  string(materialization),
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
	if _, err := front.SetCurrentClaim(info.ID, work.ID); err != nil {
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
	server := &Server{state: state}
	input := &LifecycleRecoverySubmitInput{CityScope: CityScope{CityName: state.cityName}, Body: request}
	first, err := server.humaHandleLifecycleRecoverySubmit(context.Background(), input)
	if err != nil {
		t.Fatalf("signed request intake: %v", err)
	}
	if first.Body.Status != "accepted" || first.Body.RequestID != request.RequestID {
		t.Fatalf("intake output = %+v, want accepted request", first.Body)
	}
	intent, err := store.Get(first.Body.IntentID)
	if err != nil {
		t.Fatalf("durable intent readback: %v", err)
	}
	if intent.Type != "lifecycle-intent" || !beads.HasReadyExcludedLabel(intent) {
		t.Fatalf("intent=%+v, want held, non-routable lifecycle intent", intent)
	}
	second, err := server.humaHandleLifecycleRecoverySubmit(context.Background(), input)
	if err != nil || second.Body.IntentID != first.Body.IntentID {
		t.Fatalf("exact request replay = (%+v, %v), want same durable intent", second, err)
	}
	tampered := request
	tampered.Message += " forged"
	if _, err := server.humaHandleLifecycleRecoverySubmit(context.Background(), &LifecycleRecoverySubmitInput{
		CityScope: CityScope{CityName: state.cityName}, Body: tampered,
	}); err == nil {
		t.Fatal("tampered unsigned request was accepted")
	}
	changed, err := worklifecycle.SignRecoveryRequest(func() worklifecycle.RecoveryRequest {
		modifiedRequest := request
		modifiedRequest.Message = "different content with the same request ID"
		return modifiedRequest
	}(), recoveryPrivate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.humaHandleLifecycleRecoverySubmit(context.Background(), &LifecycleRecoverySubmitInput{
		CityScope: CityScope{CityName: state.cityName}, Body: changed,
	}); err == nil {
		t.Fatal("reused request ID with different trusted content was accepted")
	}
	if calls := state.sp.CountCalls("Nudge", info.SessionName); calls != 0 {
		t.Fatalf("request intake performed %d runtime nudges; it must only persist intent", calls)
	}
}
