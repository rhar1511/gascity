package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storeref"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

type lifecycleClaimProviderFunc func(context.Context, LifecycleClaimTransitionRequest) (LifecycleClaimTransitionResult, error)

type lifecycleClaimTestState struct {
	*fakeState
	claim lifecycleClaimProviderFunc
}

func (s *lifecycleClaimTestState) ClaimLifecycleWork(ctx context.Context, request LifecycleClaimTransitionRequest) (LifecycleClaimTransitionResult, error) {
	return s.claim(ctx, request)
}

func TestLifecycleClaimSubmitAuthenticatesSessionAndStampsReciprocalClaim(t *testing.T) {
	for _, revision := range []int64{1, -7} {
		t.Run(fmt.Sprint(revision), func(t *testing.T) {
			state, store, info, work := newLifecycleClaimTestStateWithRevision(t, revision)
			called := false
			state.claim = func(_ context.Context, request LifecycleClaimTransitionRequest) (LifecycleClaimTransitionResult, error) {
				called = true
				if request.Work.ID != work.ID || request.WorkStore != store || request.WorkStoreRef != storeref.WorkRef ||
					request.Scope != worklifecycle.ScopeForStore("test-city", "city:test-city") ||
					request.ExpectedRevision != work.Revision || request.ExpectedTransitionHead != "head-1" ||
					request.Session.ID != info.ID || request.Session.Template != info.Template ||
					request.Session.AgentName != info.AgentName || request.Session.CommonName != info.CommonName ||
					request.Actor != session.AssigneeIdentifier(info) {
					t.Fatalf("provider request = %+v; want exact work/session snapshot", request)
				}
				return LifecycleClaimTransitionResult{ClaimGeneration: "1", ReceiptID: "claim-receipt-1"}, nil
			}

			body, err := json.Marshal(lifecycleClaimInput(info, work).Body)
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			newTestCityHandler(t, state).ServeHTTP(response, newPostRequest(cityURL(state, "/lifecycle/claims"), bytes.NewReader(body)))
			if response.Code != http.StatusOK {
				t.Fatalf("claim submit HTTP %d: %s", response.Code, response.Body.String())
			}
			var output LifecycleClaimSubmitOutput
			if err := json.Unmarshal(response.Body.Bytes(), &output.Body); err != nil {
				t.Fatal(err)
			}
			if !called || output.Body.WorkID != work.ID || output.Body.Actor != session.AssigneeIdentifier(info) ||
				output.Body.ClaimGeneration != "1" || output.Body.ReceiptID != "claim-receipt-1" || output.Body.Replayed {
				t.Fatalf("claim result = %+v called=%v", output.Body, called)
			}
			claimID, generation, err := session.NewStore(state.SessionsBeadStore()).CurrentClaim(info.ID)
			if err != nil || claimID != work.ID || generation != "1" {
				t.Fatalf("reciprocal session claim = %q/%q, %v; want %q/1", claimID, generation, err, work.ID)
			}
		})
	}
}

func TestLifecycleClaimSubmitRejectsStaleSessionOrSourceIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*LifecycleClaimSubmitRequest)
	}{
		{name: "instance token", mutate: func(request *LifecycleClaimSubmitRequest) { request.InstanceToken = "stale-token" }},
		{name: "zero revision", mutate: func(request *LifecycleClaimSubmitRequest) { request.ExpectedRevision = 0 }},
		{name: "runtime epoch", mutate: func(request *LifecycleClaimSubmitRequest) { request.RuntimeEpoch = "2" }},
		{name: "source store", mutate: func(request *LifecycleClaimSubmitRequest) { request.SourceStoreRef = "rig:stale" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, _, info, work := newLifecycleClaimTestState(t)
			called := false
			state.claim = func(context.Context, LifecycleClaimTransitionRequest) (LifecycleClaimTransitionResult, error) {
				called = true
				return LifecycleClaimTransitionResult{ClaimGeneration: "1", ReceiptID: "claim-receipt-1"}, nil
			}
			input := lifecycleClaimInput(info, work)
			tc.mutate(&input.Body)
			if _, err := (&Server{state: state}).humaHandleLifecycleClaimSubmit(context.Background(), input); err == nil {
				t.Fatal("claim submit succeeded with stale session or source identity")
			}
			if called {
				t.Fatal("transition provider ran before managed incarnation validation")
			}
			claimID, _, err := session.NewStore(state.SessionsBeadStore()).CurrentClaim(info.ID)
			if err != nil || claimID != "" {
				t.Fatalf("mismatched incarnation stamped reciprocal claim %q, %v", claimID, err)
			}
		})
	}
}

func TestLifecycleClaimSubmitRepairsReciprocalAfterLostTransitionResponse(t *testing.T) {
	state, _, info, work := newLifecycleClaimTestState(t)
	providerCalls := 0
	committed := false
	state.claim = func(_ context.Context, request LifecycleClaimTransitionRequest) (LifecycleClaimTransitionResult, error) {
		providerCalls++
		if request.ExpectedRevision != work.Revision || request.ExpectedTransitionHead != "head-1" {
			t.Fatalf("retry changed original transition precondition: %+v", request)
		}
		if providerCalls == 1 {
			// Model a committed transition whose response was lost. The retry
			// returns the same durable receipt without a second transition.
			committed = true
			return LifecycleClaimTransitionResult{}, errors.New("transition response lost after commit")
		}
		if !committed {
			t.Fatal("replay ran before a simulated durable transition")
		}
		return LifecycleClaimTransitionResult{ClaimGeneration: "4", ReceiptID: "claim-receipt-4", Replayed: true}, nil
	}
	server := &Server{state: state}
	if _, err := server.humaHandleLifecycleClaimSubmit(context.Background(), lifecycleClaimInput(info, work)); err == nil {
		t.Fatal("first call returned success despite its simulated lost transition response")
	}
	claimID, generation, err := session.NewStore(state.SessionsBeadStore()).CurrentClaim(info.ID)
	if err != nil || claimID != "" || generation != "" {
		t.Fatalf("lost-response call stamped reciprocal claim %q/%q, %v", claimID, generation, err)
	}
	output, err := server.humaHandleLifecycleClaimSubmit(context.Background(), lifecycleClaimInput(info, work))
	if err != nil {
		t.Fatalf("replayed claim submit: %v", err)
	}
	if !output.Body.Replayed || output.Body.ClaimGeneration != "4" || output.Body.ReceiptID != "claim-receipt-4" || providerCalls != 2 {
		t.Fatalf("retry result = %+v, provider calls %d", output.Body, providerCalls)
	}
	claimID, generation, err = session.NewStore(state.SessionsBeadStore()).CurrentClaim(info.ID)
	if err != nil || claimID != work.ID || generation != "4" {
		t.Fatalf("repaired reciprocal claim = %q/%q, %v", claimID, generation, err)
	}
}

func newLifecycleClaimTestState(t *testing.T) (*lifecycleClaimTestState, *beads.MemStore, session.Info, beads.Bead) {
	t.Helper()
	return newLifecycleClaimTestStateWithRevision(t, 1)
}

func newLifecycleClaimTestStateWithRevision(t *testing.T, revision int64) (*lifecycleClaimTestState, *beads.MemStore, session.Info, beads.Bead) {
	t.Helper()
	base := newFakeState(t)
	base.cfg.Rigs = nil
	base.cfg.Workspace.Prefix = "test"
	base.cfg.Lifecycle = config.LifecycleConfig{AdmissionEnabled: true}
	store := beads.NewMemStoreFrom(1, []beads.Bead{{
		ID: "test-claim-work", Title: "admitted work", Type: "task", Status: "open", Revision: revision,
		Metadata: map[string]string{
			beadmeta.LifecycleAdmissionReceiptV2MetadataKey: "v2-admission",
			beadmeta.LifecycleTransitionHeadMetadataKey:     "head-1",
		},
	}}, nil)
	store.IDPrefix = "test"
	store.HonorExplicitIDs = true
	// Protected reciprocal claims require the same factory-enabled conditional
	// writer as production; a raw unstamped MemStore deliberately defaults off.
	_, err := beads.OpenStoreAtForCity(context.Background(), beads.StoreOpenOptions{
		ScopeRoot: t.TempDir(), Provider: "file", ConditionalWrites: gate.Require,
		OpenFileStore: func() (beads.Store, error) { return store, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	base.cityBeadStore = store
	base.sessionsBeadStore = store
	base.stores = map[string]beads.Store{}
	work := beads.Bead{
		ID: "test-claim-work", Title: "admitted work", Type: "task", Status: "open", Revision: revision,
		Metadata: map[string]string{
			beadmeta.LifecycleAdmissionReceiptV2MetadataKey: "v2-admission",
			beadmeta.LifecycleTransitionHeadMetadataKey:     "head-1",
		},
	}
	manager := session.NewManagerWithOptions(store, base.sp)
	info, err := manager.CreateSession(context.Background(), session.CreateOptions{
		Template: "worker", Command: "test-agent", WorkDir: t.TempDir(), Provider: "test-agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	return &lifecycleClaimTestState{fakeState: base}, store, info, work
}

func lifecycleClaimInput(info session.Info, work beads.Bead) *LifecycleClaimSubmitInput {
	return &LifecycleClaimSubmitInput{Body: LifecycleClaimSubmitRequest{
		WorkID: work.ID, SourceStoreRef: "city:test-city", ExpectedRevision: work.Revision,
		ExpectedTransitionHead: "head-1", SessionID: info.ID, InstanceToken: info.InstanceToken,
		RuntimeEpoch: info.Generation,
	}}
}

// The HTTP edge authenticates the managed session before asking the trusted
// controller to verify inherited workflow authority. It grants no claim from
// the lineage marker itself.
func TestLifecycleClaimSubmitDescendantWithoutOwnHeadReachesVerifier(t *testing.T) {
	state, store, info, _ := newLifecycleClaimTestState(t)
	work, err := store.Create(beads.Bead{Title: "generated step", Type: "bug", ParentID: "workflow-root", Metadata: map[string]string{
		beadmeta.LifecycleMaterializationMetadataKey: "unverified-parent-lineage",
	}})
	if err != nil {
		t.Fatal(err)
	}
	called := false
	state.claim = func(_ context.Context, request LifecycleClaimTransitionRequest) (LifecycleClaimTransitionResult, error) {
		called = true
		if request.Work.ID != work.ID || request.ExpectedTransitionHead != "" || request.ExpectedRevision != work.Revision || request.Session.ID != info.ID {
			t.Fatal("descendant verifier did not receive the exact authenticated observation")
		}
		return LifecycleClaimTransitionResult{}, worklifecycle.ErrTransitionChainEvidence
	}
	input := lifecycleClaimInput(info, work)
	input.Body.ExpectedTransitionHead = ""
	body, err := json.Marshal(input.Body)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	newTestCityHandler(t, state).ServeHTTP(response, newPostRequest(cityURL(state, "/lifecycle/claims"), bytes.NewReader(body)))
	if !called || response.Code != http.StatusConflict {
		t.Fatalf("descendant HTTP %d, verifier called=%t; want authenticated verifier refusal", response.Code, called)
	}
	claimID, _, err := session.NewStore(state.SessionsBeadStore()).CurrentClaim(info.ID)
	if err != nil || claimID != "" {
		t.Fatalf("rejected descendant stamped session claim %q: %v", claimID, err)
	}
}

func TestLifecycleSourceClaimPreservesExecutionClaim(t *testing.T) {
	state, store, info, work := newLifecycleClaimTestState(t)
	step := beads.Bead{ID: "test-step", Revision: 9, Title: "execution step", Status: "in_progress", Assignee: session.AssigneeIdentifier(info), Metadata: map[string]string{
		beadmeta.SessionIDMetadataKey:                info.ID,
		beadmeta.ClaimGenerationMetadataKey:          "3",
		beadmeta.LifecycleTransitionHeadMetadataKey:  "step-claim-head",
		beadmeta.LifecycleMaterializationMetadataKey: "provider-verifies-lineage",
	}}
	sessionRow, err := store.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	store = beads.NewMemStoreFrom(1, []beads.Bead{work, step, sessionRow}, nil)
	state.cityBeadStore = store
	state.sessionsBeadStore = store
	sessions := session.NewStore(state.SessionsBeadStore())
	if _, err := sessions.SetCurrentClaimForGeneration(info.ID, step.ID, "3"); err != nil {
		t.Fatal(err)
	}
	called := false
	state.claim = func(_ context.Context, request LifecycleClaimTransitionRequest) (LifecycleClaimTransitionResult, error) {
		called = true
		if request.Work.ID != work.ID {
			t.Fatalf("claimed %q instead of source", request.Work.ID)
		}
		if request.ExecutionWork.ID != step.ID || request.ExecutionClaimGeneration != "3" {
			t.Fatal("execution context did not reach controller")
		}
		return LifecycleClaimTransitionResult{ClaimGeneration: "1", ReceiptID: "source-claim-head"}, nil
	}
	body, err := json.Marshal(lifecycleClaimInput(info, work).Body)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	wire["execution_claim"] = map[string]string{"work_id": step.ID, "claim_generation": "3"}
	body, err = json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	newTestCityHandler(t, state).ServeHTTP(response, newPostRequest(cityURL(state, "/lifecycle/claims"), bytes.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("source claim HTTP %d: %s", response.Code, response.Body.String())
	}
	client := NewCityScopedClient("http://127.0.0.1", "test-city", WithHTTPTransport(loopbackTransport{h: newTestCityHandler(t, state)}))
	request := lifecycleClaimInput(info, work).Body
	request.ExecutionClaim = &LifecycleExecutionClaim{WorkID: step.ID, ClaimGeneration: "3"}
	if _, err := client.ClaimLifecycleWork(context.Background(), request); err != nil {
		t.Fatalf("typed source claim client: %v", err)
	}
	claimID, generation, err := sessions.CurrentClaim(info.ID)
	if !called || err != nil || claimID != step.ID || generation != "3" {
		t.Fatalf("source claim replaced execution claim: got %s/%s, error=%v called=%v; want %s/3", claimID, generation, err, called, step.ID)
	}
}
