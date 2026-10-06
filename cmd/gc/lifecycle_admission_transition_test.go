package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/sling"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

func TestLifecycleAdmissionTransitionReservationGraphOnlyAttachAndReplay(t *testing.T) {
	setup := newLifecycleAdmissionTransitionSetup(t)
	fixture, cityStore, rigStores, store := setup.fixture, setup.cityStore, setup.rigStores, setup.store
	source, receipt, projection, q43, permitResolver := setup.source, setup.receipt, setup.policy.projection, setup.q43, setup.permitResolver

	var stderr strings.Builder
	reconcileLifecycleAdmissionWithPermitResolver("pilot", fixture.cityPath, fixture.cfg, cityStore, rigStores, nil, &stderr, permitResolver)
	attached, err := store.Get(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	marker, ok := lifecycleMaterializationFor(attached)
	if !ok || marker.State != "attached" || marker.WorkflowID == "" || attached.Metadata[beadmeta.RoutedToMetadataKey] != receipt.Route ||
		attached.Metadata[beadmeta.MoleculeIDMetadataKey] != marker.WorkflowID || attached.Metadata[beadmeta.MergeStrategyMetadataKey] != receipt.MergeStrategy {
		t.Fatalf("admission source = %+v marker %+v; want one atomic attached Q54 result; stderr=%s", attached, marker, stderr.String())
	}
	if got := len(store.patchRequests); got != 2 {
		t.Fatalf("Q54 patch requests = %d, want reservation and one atomic attachment", got)
	}
	if store.patchRequests[0].Kind != "lifecycle_source_reservation_v1" || store.patchRequests[1].Kind != "lifecycle_source_materialization_v1" ||
		store.patchRequests[0].Scope != receipt.Scope || store.patchRequests[1].Scope != receipt.Scope ||
		store.patchRequests[1].PriorReceiptID != store.patchReceipts[store.patchRequests[1].ReceiptID].PriorReceiptID {
		t.Fatalf("Q54 patches did not keep one lifecycle scope and direct reservation parent: %+v", store.patchRequests)
	}
	if len(store.patchRequests[1].Patch.Metadata) != 5 {
		t.Fatalf("attached patch did not atomically include four source fields and transition head: %+v", store.patchRequests[1].Patch)
	}
	if store.sourceGenericWrites != 0 {
		t.Fatalf("source row had %d writes through generic store/Sling APIs between receipts", store.sourceGenericWrites)
	}
	root, err := store.Get(marker.WorkflowID)
	if err != nil {
		t.Fatalf("get exact materialized graph root %s: %v", marker.WorkflowID, err)
	}
	if root.Metadata[beadmeta.IdempotencyKeyMetadataKey] != lifecycleMaterializationID(source.ID, receipt.Scope, marker.Contract) {
		t.Fatalf("graph root idempotency identity = %q, want deterministic admission identity", root.Metadata[beadmeta.IdempotencyKeyMetadataKey])
	}
	if err := verifyLifecycleAttachedWorkflow(store, store, attached, receipt, projection, worklifecycle.AttachedWorkflowEvidence{
		SourceID: source.ID, SourceStoreRef: marker.SourceStoreRef, WorkflowStoreRef: marker.WorkflowStoreRef,
		WorkflowID: marker.WorkflowID, Scope: receipt.Scope, AdmissionDigest: marker.Contract,
		Route: marker.Route, Workflow: marker.Workflow, MergeStrategy: marker.MergeStrategy, Token: marker.Token,
	}); err != nil {
		t.Fatalf("exact graph/workflow verifier rejected attached graph: %v", err)
	}
	wrongWorkflow := worklifecycle.AttachedWorkflowEvidence{
		SourceID: source.ID, SourceStoreRef: marker.SourceStoreRef, WorkflowStoreRef: marker.WorkflowStoreRef,
		WorkflowID: "pilot-rig-forged-workflow", Scope: receipt.Scope, AdmissionDigest: marker.Contract,
		Route: marker.Route, Workflow: marker.Workflow, MergeStrategy: marker.MergeStrategy, Token: marker.Token,
	}
	if err := verifyLifecycleAttachedWorkflow(store, store, attached, receipt, projection, wrongWorkflow); err == nil {
		t.Fatal("attached workflow verifier accepted an arbitrary workflow ID")
	}
	if q43.ReceiptID == "" {
		t.Fatal("Q43 attachment proof had no durable receipt identity")
	}

	// A restart reconstructs the same deterministic reservation and attach IDs,
	// verifies the service-managed current head, and replays exact receipts.
	before := len(store.patchRequests)
	reconcileLifecycleAdmissionWithPermitResolver("pilot", fixture.cityPath, fixture.cfg, cityStore, rigStores, nil, &stderr, permitResolver)
	if got := len(store.patchRequests); got != before {
		t.Fatalf("restart replay submitted %d new Q54 writes", got-before)
	}
	if store.sourceGenericWrites != 0 {
		t.Fatalf("restart used %d generic source writes", store.sourceGenericWrites)
	}
}

func TestLifecycleAdmissionTransitionFailsClosedWithoutPermitOrPatchCapability(t *testing.T) {
	t.Run("missing host permit resolver", func(t *testing.T) {
		setup := newLifecycleAdmissionTransitionSetup(t)
		fixture, cityStore, rigStores, store := setup.fixture, setup.cityStore, setup.rigStores, setup.store
		var stderr strings.Builder
		reconcileLifecycleAdmissionWithPermitResolver("pilot", fixture.cityPath, fixture.cfg, cityStore, rigStores, nil, &stderr, nil)
		assertLifecycleAdmissionStillOnlyQ43(t, store)
		if !strings.Contains(stderr.String(), "no host-authorized transition permit issuer") {
			t.Fatalf("missing permit was not reported as a held admission: %s", stderr.String())
		}
	})

	t.Run("missing typed patch capability", func(t *testing.T) {
		setup := newLifecycleAdmissionTransitionSetup(t)
		fixture := setup.fixture
		wrapper := &lifecycleAdmissionNoPatchStore{Store: setup.store, inner: setup.store}
		rigStores := map[string]beads.Store{"pilot": wrapper}
		var stderr strings.Builder
		reconcileLifecycleAdmissionWithPermitResolver("pilot", fixture.cityPath, fixture.cfg, setup.cityStore, rigStores, nil, &stderr, setup.permitResolver)
		assertLifecycleAdmissionStillOnlyQ43(t, setup.store)
		if !strings.Contains(stderr.String(), "transition chain unavailable") {
			t.Fatalf("missing typed patch capability did not fail closed: %s", stderr.String())
		}
	})
}

func TestLifecycleAdmissionTransitionRechecksCurrentPolicyBeforeReservation(t *testing.T) {
	setup := newLifecycleAdmissionTransitionSetup(t)
	// The signed Q43 receipt names pilot/worker. Removing that route from the
	// live config must prevent reservation even though the source proof remains
	// valid and the host has an exact protected permit.
	setup.fixture.cfg.Agents[0].Name = "replacement-worker"
	var stderr strings.Builder
	reconcileLifecycleAdmissionWithPermitResolver("pilot", setup.fixture.cityPath, setup.fixture.cfg, setup.cityStore, setup.rigStores, nil, &stderr, setup.permitResolver)
	assertLifecycleAdmissionStillOnlyQ43(t, setup.store)
	if !strings.Contains(stderr.String(), "current route-policy proof") {
		t.Fatalf("stale current route policy was not reported as held: %s", stderr.String())
	}
}

func TestLifecycleAdmissionTransitionConcurrentPassSharesExactReceipts(t *testing.T) {
	setup := newLifecycleAdmissionTransitionSetup(t)
	fixture, cityStore, rigStores, resolver := setup.fixture, setup.cityStore, setup.rigStores, setup.permitResolver
	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var stderr strings.Builder
			reconcileLifecycleAdmissionWithPermitResolver("pilot", fixture.cityPath, fixture.cfg, cityStore, rigStores, nil, &stderr, resolver)
			if stderr.Len() != 0 {
				errCh <- fmt.Errorf("concurrent admission reported held work: %s", stderr.String())
			}
		}()
	}
	wg.Wait()
	close(errCh)
	var concurrentErrors []error
	for err := range errCh {
		concurrentErrors = append(concurrentErrors, err)
	}
	if len(concurrentErrors) != 0 {
		t.Fatalf("concurrent reconcilers reported errors: %v", concurrentErrors)
	}
	attached, err := setup.store.Get(setup.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	marker, ok := lifecycleMaterializationFor(attached)
	if !ok || marker.State != "attached" || marker.WorkflowID == "" {
		t.Fatalf("concurrent admission did not reach one attached materialization: marker=%+v", marker)
	}
	if got := len(setup.store.patchReceipts); got != 2 {
		t.Fatalf("concurrent admission persisted %d unique Q54 receipts, want reservation and attachment", got)
	}
	if setup.store.sourceGenericWrites != 0 {
		t.Fatalf("concurrent admission used %d generic source writes", setup.store.sourceGenericWrites)
	}
}

// Admission must acquire its per-source admission lock before the Q43 source
// reread, not only while creating the graph. Otherwise another admission can
// publish a reservation or attachment between that snapshot and CurrentHead.
func TestLifecycleAdmissionTransitionLocksBeforeSourceSnapshot(t *testing.T) {
	for _, reserved := range []bool{false, true} {
		t.Run(fmt.Sprintf("reserved=%v", reserved), func(t *testing.T) {
			setup := newLifecycleAdmissionTransitionSetup(t)
			if reserved {
				leaveLifecycleAdmissionReservation(t, setup)
			}
			snapshots := make(chan struct{}, 1)
			setup.store.afterSourceSnapshot = func() {
				select {
				case snapshots <- struct{}{}:
				default:
				}
			}
			synctest.Test(t, func(t *testing.T) {
				locked := make(chan struct{})
				release := make(chan struct{})
				holderDone := make(chan error, 1)
				go func() {
					holderDone <- sourceworkflow.WithLock(context.Background(), setup.fixture.cityPath, "lifecycle-admission:rig:pilot", setup.source.ID, func() error {
						close(locked)
						<-release
						return nil
					})
				}()
				<-locked
				var stderr strings.Builder
				admissionDone := make(chan struct{})
				go func() {
					reconcileLifecycleAdmissionWithPermitResolver("pilot", setup.fixture.cityPath, setup.fixture.cfg, setup.cityStore, setup.rigStores, nil, &stderr, setup.permitResolver)
					close(admissionDone)
				}()
				synctest.Wait()
				select {
				case <-snapshots:
					t.Error("admission read authoritative source while a peer held its per-source lock")
				default:
				}
				close(release)
				if err := <-holderDone; err != nil {
					t.Fatalf("source lock holder: %v", err)
				}
				<-admissionDone
				if stderr.Len() != 0 {
					t.Fatalf("admission after peer release reported held work: %s", stderr.String())
				}
			})
			attached, err := setup.store.Get(setup.source.ID)
			marker, ok := lifecycleMaterializationFor(attached)
			if err != nil || !ok || marker.State != "attached" || marker.WorkflowID == "" || len(setup.store.patchReceipts) != 2 || setup.store.sourceGenericWrites != 0 {
				t.Fatalf("serialized admission = marker %+v receipts=%d generic writes=%d err=%v", marker, len(setup.store.patchReceipts), setup.store.sourceGenericWrites, err)
			}
		})
	}
}

func assertLifecycleAdmissionStillOnlyQ43(t *testing.T, store *lifecycleAdmissionTransitionTestStore) {
	t.Helper()
	if len(store.patchRequests) != 0 || len(store.patchReceipts) != 0 {
		t.Fatalf("fail-closed admission submitted Q54 writes: requests=%d receipts=%d", len(store.patchRequests), len(store.patchReceipts))
	}
	current, err := store.Get(store.sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if marker, ok := lifecycleMaterializationFor(current); ok && marker.State != "" {
		t.Fatalf("fail-closed admission wrote a materialization marker: %+v", marker)
	}
	if store.sourceGenericWrites != 0 {
		t.Fatalf("fail-closed admission used %d generic source writes", store.sourceGenericWrites)
	}
}

func TestLifecycleAdmissionTransitionRecoversLostAttachResponse(t *testing.T) {
	setup := newLifecycleAdmissionTransitionSetup(t)
	fixture, cityStore, rigStores, store := setup.fixture, setup.cityStore, setup.rigStores, setup.store
	source, resolver := setup.source, setup.permitResolver
	store.loseNextPatchKind = "lifecycle_source_materialization_v1"
	var stderr strings.Builder
	reconcileLifecycleAdmissionWithPermitResolver("pilot", fixture.cityPath, fixture.cfg, cityStore, rigStores, nil, &stderr, resolver)
	attached, err := store.Get(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if marker, ok := lifecycleMaterializationFor(attached); !ok || marker.State != "attached" {
		t.Fatalf("lost attach response did not recover exact committed materialization: marker=%+v stderr=%s", marker, stderr.String())
	}
	if len(store.patchRequests) != 2 || store.sourceGenericWrites != 0 {
		t.Fatalf("patch calls=%d generic source writes=%d, want two typed receipts and zero generic writes", len(store.patchRequests), store.sourceGenericWrites)
	}
}

func TestLifecycleAdmissionTransitionHoldsReopenedBlockerBeforeAttachment(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		t.Run(fmt.Sprintf("recovered=%v", recovered), func(t *testing.T) {
			const blockerID = "pilot-rig-blocker"
			setup := newLifecycleAdmissionTransitionSetupWithCompletion(t, false, blockerID)
			var stderr strings.Builder
			if recovered {
				setup.store.failNextPatchKind = "lifecycle_source_materialization_v1"
				reconcileLifecycleAdmissionWithPermitResolver("pilot", setup.fixture.cityPath, setup.fixture.cfg, setup.cityStore, setup.rigStores, nil, &stderr, setup.permitResolver)
				current, err := setup.store.Get(setup.source.ID)
				marker, ok := lifecycleMaterializationFor(current)
				if err != nil || !ok || marker.State != "reserved" || len(setup.store.patchReceipts) != 1 {
					t.Fatalf("recovery fixture did not retain its exact reservation: marker=%+v err=%v stderr=%s", marker, err, stderr.String())
				}
				stderr.Reset()
			}
			digest, err := worklifecycle.AdmissionDigestV2(setup.receipt)
			if err != nil {
				t.Fatal(err)
			}
			identity := map[string]string{beadmeta.IdempotencyKeyMetadataKey: lifecycleMaterializationID(setup.source.ID, setup.receipt.Scope, digest)}
			reopened := false
			reopenAfterGraph := func() {
				if reopened {
					return
				}
				roots, err := setup.store.MemStore.ListByMetadata(identity, 0, beads.WithBothTiers)
				if err != nil {
					t.Fatal(err)
				}
				if len(roots) == 0 {
					return
				}
				before, err := setup.store.Get(setup.source.ID)
				if err != nil {
					t.Fatal(err)
				}
				open := "open"
				if err := setup.store.Update(blockerID, beads.UpdateOpts{Status: &open}); err != nil {
					t.Fatal(err)
				}
				after, err := setup.store.Get(setup.source.ID)
				if err != nil || before.Revision != after.Revision || before.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] != after.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] {
					t.Fatalf("fixture blocker reopen unexpectedly changed source token/head: before=%d after=%d err=%v", before.Revision, after.Revision, err)
				}
				t.Logf("blocker reopened: source revision before=%d after=%d; exact transition head unchanged", before.Revision, after.Revision)
				ready, err := setup.store.Ready()
				if err != nil {
					t.Fatal(err)
				}
				for _, row := range ready {
					if row.ID == setup.source.ID {
						t.Fatal("reopened blocking dependency left source ready")
					}
				}
				reopened = true
			}
			if recovered {
				setup.store.afterWorkflowRead = reopenAfterGraph
			} else {
				setup.store.afterSourceSnapshot = reopenAfterGraph
			}
			reconcileLifecycleAdmissionWithPermitResolver("pilot", setup.fixture.cityPath, setup.fixture.cfg, setup.cityStore, setup.rigStores, nil, &stderr, setup.permitResolver)
			current, err := setup.store.Get(setup.source.ID)
			marker, ok := lifecycleMaterializationFor(current)
			if err != nil || !reopened || !ok || marker.State != "reserved" || len(setup.store.patchReceipts) != 1 || len(controllerDemandRouteCandidates(current)) != 0 || stderr.Len() == 0 {
				t.Fatalf("reopened blocker progressed attachment: reopened=%v marker=%+v receipts=%d err=%v stderr=%s", reopened, marker, len(setup.store.patchReceipts), err, stderr.String())
			}
			setup.store.afterSourceSnapshot = nil
			setup.store.afterWorkflowRead = nil
			closed := "closed"
			if err := setup.store.Update(blockerID, beads.UpdateOpts{Status: &closed}); err != nil {
				t.Fatal(err)
			}
			stderr.Reset()
			reconcileLifecycleAdmissionWithPermitResolver("pilot", setup.fixture.cityPath, setup.fixture.cfg, setup.cityStore, setup.rigStores, nil, &stderr, setup.permitResolver)
			current, err = setup.store.Get(setup.source.ID)
			marker, ok = lifecycleMaterializationFor(current)
			roots, rootsErr := setup.store.MemStore.ListByMetadata(identity, 0, beads.WithBothTiers)
			var workflowRoots []beads.Bead
			for _, root := range roots {
				if root.ParentID == "" {
					workflowRoots = append(workflowRoots, root)
				}
			}
			if err != nil || rootsErr != nil || !ok || marker.State != "attached" || len(workflowRoots) != 1 || workflowRoots[0].ID != marker.WorkflowID || len(setup.store.patchReceipts) != 2 || setup.store.sourceGenericWrites != 0 || stderr.Len() != 0 {
				t.Fatalf("ready recovery did not reuse one graph/two receipts: marker=%+v roots=%d receipts=%d generic=%d err=%v stderr=%s", marker, len(roots), len(setup.store.patchReceipts), setup.store.sourceGenericWrites, errors.Join(err, rootsErr), stderr.String())
			}
		})
	}
}

func TestLifecycleAdmissionTransitionRestartAfterReservationUsesVerifiedHistoricalQ43(t *testing.T) {
	setup := newLifecycleAdmissionTransitionSetup(t)
	leaveLifecycleAdmissionReservation(t, setup)

	var stderr strings.Builder
	reconcileLifecycleAdmissionWithPermitResolver("pilot", setup.fixture.cityPath, setup.fixture.cfg, setup.cityStore, setup.rigStores, nil, &stderr, setup.permitResolver)
	attached, err := setup.store.Get(setup.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	marker, ok := lifecycleMaterializationFor(attached)
	if !ok || marker.State != "attached" || marker.WorkflowID == "" || len(setup.store.patchReceipts) != 2 {
		t.Fatalf("restart after reservation = marker %+v receipts %d stderr=%s; want exact graph materialized and attached", marker, len(setup.store.patchReceipts), stderr.String())
	}
	if setup.store.sourceGenericWrites != 0 {
		t.Fatalf("reservation restart used %d generic source writes", setup.store.sourceGenericWrites)
	}
}

func TestLifecycleAdmissionTransitionRejectsInvalidOrStaleReservationHead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*lifecycleAdmissionTransitionTestStore)
	}{
		{
			name: "forged head",
			mutate: func(store *lifecycleAdmissionTransitionTestStore) {
				current := store.rows[store.sourceID]
				current.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] = "forged-transition-head"
				store.rows[store.sourceID] = current
			},
		},
		{
			name: "missing head receipt",
			mutate: func(store *lifecycleAdmissionTransitionTestStore) {
				current := store.rows[store.sourceID]
				head := current.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey]
				delete(store.patchReceipts, head)
				store.rows[store.sourceID] = current
			},
		},
		{
			name: "head source revision is stale",
			mutate: func(store *lifecycleAdmissionTransitionTestStore) {
				current := store.rows[store.sourceID]
				current.Revision++
				store.rows[store.sourceID] = current
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup := newLifecycleAdmissionTransitionSetup(t)
			leaveLifecycleAdmissionReservation(t, setup)
			setup.store.mu.Lock()
			tc.mutate(setup.store)
			beforeRequests := len(setup.store.patchRequests)
			setup.store.mu.Unlock()

			var stderr strings.Builder
			reconcileLifecycleAdmissionWithPermitResolver("pilot", setup.fixture.cityPath, setup.fixture.cfg, setup.cityStore, setup.rigStores, nil, &stderr, setup.permitResolver)
			current, err := setup.store.Get(setup.source.ID)
			if err != nil {
				t.Fatal(err)
			}
			marker, ok := lifecycleMaterializationFor(current)
			if !ok || marker.State != "reserved" || len(setup.store.patchRequests) != beforeRequests {
				t.Fatalf("bad transition head progressed admission: marker=%+v patch requests=%d (before %d) stderr=%s", marker, len(setup.store.patchRequests), beforeRequests, stderr.String())
			}
			if stderr.Len() == 0 || setup.store.sourceGenericWrites != 0 {
				t.Fatalf("invalid/stale head was not held cleanly: stderr=%s generic writes=%d", stderr.String(), setup.store.sourceGenericWrites)
			}
		})
	}
}

func TestLifecycleAdmissionV1ClaimFilterHoldsReservationAndAllowsExactAttachedSteps(t *testing.T) {
	setup := newLifecycleAdmissionTransitionSetup(t)
	setup.store.rejectNextPatchKind = "lifecycle_source_materialization_v1"
	var attachErr strings.Builder
	reconcileLifecycleAdmissionWithPermitResolver("pilot", setup.fixture.cityPath, setup.fixture.cfg, setup.cityStore, setup.rigStores, nil, &attachErr, setup.permitResolver)
	reserved, err := setup.store.Get(setup.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	reservation, ok := lifecycleMaterializationFor(reserved)
	if !ok || reservation.State != "reserved" {
		t.Fatalf("rejected attachment did not preserve the reservation: marker=%+v stderr=%s", reservation, attachErr.String())
	}
	step := lifecycleAdmissionV1StepCandidate(t, setup, reservation)
	opts := lifecycleAdmissionV1ClaimOptions(setup)
	var filterErr strings.Builder
	if got := filterHookLifecycleCandidates([]beads.Bead{step}, opts, &filterErr); len(got) != 0 {
		t.Fatalf("reserved formula-v1 step remained claimable: %+v", got)
	}
	if filterErr.Len() == 0 {
		t.Fatal("reserved formula-v1 step was filtered without a hold reason")
	}

	setup.store.rejectNextPatchKind = ""
	var attachSuccess strings.Builder
	reconcileLifecycleAdmissionWithPermitResolver("pilot", setup.fixture.cityPath, setup.fixture.cfg, setup.cityStore, setup.rigStores, nil, &attachSuccess, setup.permitResolver)
	attached, err := setup.store.Get(setup.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	attachedMarker, ok := lifecycleMaterializationFor(attached)
	if !ok || attachedMarker.State != "attached" || attachedMarker.WorkflowID == "" {
		t.Fatalf("formula-v1 source did not attach after retry: marker=%+v stderr=%s", attachedMarker, attachSuccess.String())
	}
	filterErr.Reset()
	got := filterHookLifecycleCandidates([]beads.Bead{step}, opts, &filterErr)
	if len(got) != 1 || got[0].ID != step.ID {
		t.Fatalf("exact attached formula-v1 step was not claimable: got=%+v stderr=%s", got, filterErr.String())
	}

	attachedSource, err := setup.store.Get(setup.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	validHead := attachedSource.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey]
	setup.store.mu.Lock()
	attachedReceipt, found := setup.store.patchReceipts[validHead]
	setup.store.mu.Unlock()
	if !found || validHead == "" {
		t.Fatalf("attached source has no exact Q54 head receipt %q", validHead)
	}
	setup.store.mu.Lock()
	forgedSource := attachedSource
	forgedSource.Metadata = cloneLifecycleAdmissionMetadata(attachedSource.Metadata)
	forgedSource.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] = "forged-q54-head"
	setup.store.rows[setup.source.ID] = forgedSource
	setup.store.mu.Unlock()
	filterErr.Reset()
	if got := filterHookLifecycleCandidates([]beads.Bead{step}, opts, &filterErr); len(got) != 0 {
		t.Fatalf("attached v1 descendant remained claimable with a forged Q54 head: %+v", got)
	}

	setup.store.mu.Lock()
	setup.store.rows[setup.source.ID] = attachedSource
	delete(setup.store.patchReceipts, validHead)
	setup.store.mu.Unlock()
	filterErr.Reset()
	if got := filterHookLifecycleCandidates([]beads.Bead{step}, opts, &filterErr); len(got) != 0 {
		t.Fatalf("attached v1 descendant remained claimable with a missing attached Q54 receipt: %+v", got)
	}
	setup.store.mu.Lock()
	setup.store.patchReceipts[validHead] = attachedReceipt
	setup.store.mu.Unlock()

	tamperedStep := step
	tamperedStep.Metadata = cloneLifecycleAdmissionMetadata(step.Metadata)
	tamperedStep.Metadata[beadmeta.MergeStrategyMetadataKey] = "forged"
	setup.store.mu.Lock()
	setup.store.workflowReadOverrides[step.ID] = tamperedStep
	setup.store.mu.Unlock()
	filterErr.Reset()
	if got := filterHookLifecycleCandidates([]beads.Bead{step}, opts, &filterErr); len(got) != 0 {
		t.Fatalf("tampered exact formula-v1 descendant remained claimable: %+v", got)
	}

	root, err := setup.store.Get(attachedMarker.WorkflowID)
	if err != nil {
		t.Fatal(err)
	}
	root.SourceStoreRef = attachedMarker.WorkflowStoreRef
	root.LifecycleScope = attachedMarker.Scope
	tamperedRoot := root
	tamperedRoot.Metadata = cloneLifecycleAdmissionMetadata(root.Metadata)
	tamperedRoot.Metadata[beadmeta.FormulaContractMetadataKey] = beadmeta.FormulaContractGraphV2
	setup.store.mu.Lock()
	setup.store.workflowReadOverrides[root.ID] = tamperedRoot
	delete(setup.store.workflowReadOverrides, step.ID)
	setup.store.mu.Unlock()
	filterErr.Reset()
	if got := filterHookLifecycleCandidates([]beads.Bead{root}, opts, &filterErr); len(got) != 0 {
		t.Fatalf("exact graph.v2 root without proven invocation inputs remained claimable: %+v", got)
	}
}

func leaveLifecycleAdmissionReservation(t *testing.T, setup lifecycleAdmissionTransitionSetup) {
	t.Helper()
	setup.store.mu.Lock()
	setup.store.afterNextPatch = func(kind string) {
		if kind == "lifecycle_source_reservation_v1" {
			setup.fixture.cfg.Agents[0].Name = "replacement-worker"
		}
	}
	setup.store.mu.Unlock()
	var stderr strings.Builder
	reconcileLifecycleAdmissionWithPermitResolver("pilot", setup.fixture.cityPath, setup.fixture.cfg, setup.cityStore, setup.rigStores, nil, &stderr, setup.permitResolver)
	current, err := setup.store.Get(setup.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	marker, ok := lifecycleMaterializationFor(current)
	if !ok || marker.State != "reserved" || len(setup.store.patchReceipts) != 1 {
		t.Fatalf("expected restart fixture to stop after reservation: marker=%+v receipts=%d stderr=%s", marker, len(setup.store.patchReceipts), stderr.String())
	}
	setup.fixture.cfg.Agents[0].Name = "worker"
}

func lifecycleAdmissionV1StepCandidate(t *testing.T, setup lifecycleAdmissionTransitionSetup, marker lifecycleMaterialization) beads.Bead {
	t.Helper()
	rows, err := setup.store.MemStore.List(beads.ListQuery{Status: "open", Live: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		lineage, ok := lifecycleMaterializationFor(row)
		if ok && row.ID != setup.source.ID && lineage.State == "lineage_pending" && lineage.SourceID == setup.source.ID {
			row.SourceStoreRef = marker.WorkflowStoreRef
			row.LifecycleScope = marker.Scope
			return row
		}
	}
	t.Fatalf("materialized formula-v1 workflow has no pending step row: %+v", rows)
	return beads.Bead{}
}

func lifecycleAdmissionV1ClaimOptions(setup lifecycleAdmissionTransitionSetup) hookClaimOptions {
	resolveStore := func(ref string) (beads.Store, error) {
		switch ref {
		case "city:pilot":
			return setup.cityStore, nil
		case "rig:pilot":
			return setup.store, nil
		default:
			return nil, fmt.Errorf("unexpected lifecycle store %q", ref)
		}
	}
	return hookClaimOptions{
		Lifecycle: setup.fixture.cfg.Lifecycle, LifecycleCity: setup.fixture.cfg,
		TrustedLifecycleScope: true, RouteTargets: []string{setup.receipt.Route},
		ResolveLifecycleStore:         resolveStore,
		VerifyLifecycleTransitionHead: newLifecycleClaimTransitionHeadVerifier(setup.fixture.cityPath, setup.fixture.cfg, resolveStore),
	}
}

func encodeAdmissionTestKey(key ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(key)
}

type lifecycleAdmissionTransitionSetup struct {
	fixture        *lifecycleAdmissionPolicyFixture
	cityStore      beads.Store
	rigStores      map[string]beads.Store
	store          *lifecycleAdmissionTransitionTestStore
	source         beads.Bead
	receipt        worklifecycle.AdmissionReceiptV2
	policy         lifecycleAdmissionPolicy
	q43            worklifecycle.AdmissionAttachmentProof
	permitResolver *hostBeadsPermitResolver
}

// lifecycleAdmissionNoPatchStore preserves Q43 verification while hiding the
// Q54 typed patch capability. Embedding beads.Store intentionally does not
// promote optional capability-provider methods.
type lifecycleAdmissionNoPatchStore struct {
	beads.Store
	inner *lifecycleAdmissionTransitionTestStore
}

func (s *lifecycleAdmissionNoPatchStore) ControllerMetadataTransitionWriterHandle() (beads.ControllerMetadataTransitionWriter, bool) {
	return s.inner.ControllerMetadataTransitionWriterHandle()
}

func (s *lifecycleAdmissionNoPatchStore) ControllerMetadataTransitionReceiptReaderHandle() (beads.ControllerMetadataTransitionReceiptReader, bool) {
	return s.inner.ControllerMetadataTransitionReceiptReaderHandle()
}

func (s *lifecycleAdmissionNoPatchStore) DecisionFrontierSourceReaderHandle() (beads.DecisionFrontierSourceReader, bool) {
	return s.inner.DecisionFrontierSourceReaderHandle()
}

func newLifecycleAdmissionTransitionSetup(t *testing.T) lifecycleAdmissionTransitionSetup {
	return newLifecycleAdmissionTransitionSetupWithCompletion(t, false)
}

func newLifecycleAdmissionTransitionSetupWithCompletion(t *testing.T, withCompletion bool, closedBlockerIDs ...string) lifecycleAdmissionTransitionSetup {
	t.Helper()
	fixture := newLifecycleAdmissionPolicyFixture(t, "formula = \"review\"\nversion = 1\n\n[[steps]]\nid = \"work\"\ntitle = \"Review work\"\n")
	base := &beads.MemStore{IDPrefix: "pilot-rig", HonorExplicitIDs: true}
	source, err := base.Create(beads.Bead{
		ID: "pilot-rig-work-1", Title: "review", Type: "task", Status: "open",
		Labels:   []string{worklifecycle.AdmissionIntentLabel},
		Metadata: map[string]string{beadmeta.RootStoreRefMetadataKey: "rig:pilot"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, blockerID := range closedBlockerIDs {
		if _, err := base.Create(beads.Bead{ID: blockerID, Title: "prerequisite", Type: "task"}); err != nil {
			t.Fatal(err)
		}
		closed := "closed"
		if err := base.Update(blockerID, beads.UpdateOpts{Status: &closed}); err != nil {
			t.Fatal(err)
		}
		if err := base.DepAdd(source.ID, blockerID, "blocks"); err != nil {
			t.Fatal(err)
		}
	}
	source, err = base.Get(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	store := &lifecycleAdmissionTransitionTestStore{
		MemStore: base, sourceID: source.ID,
		q43Receipts:           map[string]beads.ControllerMetadataTransitionReceipt{},
		patchReceipts:         map[string]beads.RevisionTransitionPatchReceipt{},
		rows:                  map[string]beads.Bead{},
		workflowReadOverrides: map[string]beads.Bead{},
	}
	cityStore := beads.NewMemStore()
	rigStores := map[string]beads.Store{"pilot": store}
	legs, err := routedWorkStoreCandidates(fixture.cityPath, fixture.cfg, cityStore, rigStores, nil)
	if err != nil {
		t.Fatalf("resolve routed-work legs: %v", err)
	}
	var leg classStoreCandidate
	for _, candidate := range legs {
		if candidate.ref == "rig:pilot" {
			leg = candidate
			break
		}
	}
	if leg.store == nil {
		t.Fatalf("routed-work legs omitted exact rig store: %+v", legs)
	}
	admissionPublic, admissionPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	acceptancePublic, acceptancePrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fixture.cfg.Lifecycle.AdmissionEnabled = true
	fixture.cfg.Lifecycle.AdmissionV2PrimaryAuthority = "triage"
	fixture.cfg.Lifecycle.AdmissionV2Authorities = map[string]string{"triage": encodeAdmissionTestKey(admissionPublic)}
	fixture.cfg.Lifecycle.AcceptanceAuthorities = map[string]string{"reviewer": encodeAdmissionTestKey(acceptancePublic)}
	if withCompletion {
		fixture.cfg.Lifecycle.CompletionReceiptMaxAge = "24h"
		fixture.cfg.Lifecycle.CompletionClockSkew = "5m"
	}
	receipt := fixture.receipt
	receipt.WorkItemID = source.ID
	receipt.Scope = worklifecycle.ScopeForStore("pilot", leg.ref)
	receipt.ExpectedWorkRevision = source.Revision
	policy, err := buildLifecycleAdmissionPolicy(source, receipt, receipt.Scope, "pilot", fixture.cityPath,
		fixture.cfg, cityStore, rigStores, leg, legs, sling.SlingRunner(shellSlingRunner), nil)
	if err != nil {
		t.Fatalf("build current exact route policy: %v", err)
	}
	receipt.RoutingPolicyDigest, err = worklifecycle.DigestAdmissionPolicyV2(policy.projection)
	if err != nil {
		t.Fatal(err)
	}
	if withCompletion {
		admissionDigest, digestErr := worklifecycle.AdmissionDigestV2(receipt)
		if digestErr != nil {
			t.Fatal(digestErr)
		}
		completionReceipt, signErr := worklifecycle.SignCompletionReceipt(worklifecycle.CompletionReceipt{
			Version: 1, WorkItemID: source.ID, Scope: receipt.Scope, AdmissionDigest: admissionDigest,
			DeliverableRef: "commit:reviewed", VerificationRef: "report:acceptance", AcceptedBy: "reviewer",
			AcceptedAt: time.Now().UTC().Format(time.RFC3339Nano),
		}, acceptancePrivate)
		if signErr != nil {
			t.Fatal(signErr)
		}
		finalBase := &beads.MemStore{IDPrefix: "pilot-rig", HonorExplicitIDs: true}
		metadata := cloneLifecycleAdmissionMetadata(source.Metadata)
		metadata[beadmeta.LifecycleCompletionReceiptMetadataKey] = completionReceipt
		finalSource, createErr := finalBase.Create(beads.Bead{
			ID: source.ID, Title: source.Title, Type: source.Type, Status: source.Status,
			Labels: append([]string(nil), source.Labels...), Metadata: metadata,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		store = &lifecycleAdmissionTransitionTestStore{
			MemStore: finalBase, sourceID: source.ID,
			q43Receipts:   map[string]beads.ControllerMetadataTransitionReceipt{},
			patchReceipts: map[string]beads.RevisionTransitionPatchReceipt{}, rows: map[string]beads.Bead{},
		}
		source = finalSource
		rigStores["pilot"] = store
		leg.store = store
		for index := range legs {
			if legs[index].ref == leg.ref {
				legs[index].store = store
			}
		}
		policy, err = buildLifecycleAdmissionPolicy(source, receipt, receipt.Scope, "pilot", fixture.cityPath,
			fixture.cfg, cityStore, rigStores, leg, legs, sling.SlingRunner(shellSlingRunner), nil)
		if err != nil {
			t.Fatalf("rebuild current exact route policy with signed completion evidence: %v", err)
		}
		finalPolicyDigest, digestErr := worklifecycle.DigestAdmissionPolicyV2(policy.projection)
		if digestErr != nil || finalPolicyDigest != receipt.RoutingPolicyDigest {
			t.Fatalf("completion receipt metadata changed current route policy: digest=%q err=%v", finalPolicyDigest, digestErr)
		}
	}
	encoded, err := worklifecycle.SignAdmissionReceiptV2(receipt, admissionPrivate)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := worklifecycle.NewAdmissionAttachmentAdapter(store)
	if err != nil {
		t.Fatal(err)
	}
	_, q43, err := adapter.Attach(encoded, fixture.cfg.Lifecycle, receipt.Scope)
	if err != nil {
		t.Fatalf("attach signed Q43 receipt: %v", err)
	}
	entry, key := hostBeadsPermitTestEntry(t, "pilot", leg.ref, "key-admission", "")
	permitResolver := controllerPermitTestResolver(t, entry, key)
	return lifecycleAdmissionTransitionSetup{
		fixture: fixture, cityStore: cityStore, rigStores: rigStores, store: store,
		source: source, receipt: receipt, policy: policy, q43: q43, permitResolver: permitResolver,
	}
}

type lifecycleAdmissionTransitionTestStore struct {
	*beads.MemStore
	mu                    sync.Mutex
	sourceID              string
	rows                  map[string]beads.Bead
	workflowReadOverrides map[string]beads.Bead
	q43Receipts           map[string]beads.ControllerMetadataTransitionReceipt
	patchReceipts         map[string]beads.RevisionTransitionPatchReceipt
	patchRequests         []beads.RevisionTransitionPatchRequest
	sourceGenericWrites   int
	loseNextPatchKind     string
	rejectNextPatchKind   string
	failNextPatchKind     string
	afterNextPatch        func(kind string)
	afterSourceSnapshot   func()
	afterWorkflowRead     func()
}

var (
	_ beads.ControllerMetadataTransitionWriterHandleProvider        = (*lifecycleAdmissionTransitionTestStore)(nil)
	_ beads.ControllerMetadataTransitionReceiptReaderHandleProvider = (*lifecycleAdmissionTransitionTestStore)(nil)
	_ beads.RevisionTransitionPatchWriterHandleProvider             = (*lifecycleAdmissionTransitionTestStore)(nil)
	_ beads.RevisionTransitionPatchReceiptReaderHandleProvider      = (*lifecycleAdmissionTransitionTestStore)(nil)
	_ beads.DecisionFrontierSourceReaderHandleProvider              = (*lifecycleAdmissionTransitionTestStore)(nil)
)

func (s *lifecycleAdmissionTransitionTestStore) Get(id string) (beads.Bead, error) {
	s.mu.Lock()
	row, ok := s.rows[id]
	if ok {
		row.Metadata = cloneLifecycleAdmissionMetadata(row.Metadata)
		s.mu.Unlock()
		return row, nil
	}
	if row, ok := s.workflowReadOverrides[id]; ok {
		row.Metadata = cloneLifecycleAdmissionMetadata(row.Metadata)
		s.mu.Unlock()
		return row, nil
	}
	s.mu.Unlock()
	return s.MemStore.Get(id)
}

func (s *lifecycleAdmissionTransitionTestStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.MemStore.List(query)
	if err != nil {
		return nil, err
	}
	return s.overlayLifecycleAdmissionRowsForQuery(rows, query), nil
}

func (s *lifecycleAdmissionTransitionTestStore) ListByMetadata(filters map[string]string, limit int, tiers ...beads.QueryOpt) ([]beads.Bead, error) {
	rows, err := s.MemStore.ListByMetadata(filters, limit, tiers...)
	if err == nil && len(rows) != 0 && s.afterWorkflowRead != nil {
		s.afterWorkflowRead()
	}
	return rows, err
}

func (s *lifecycleAdmissionTransitionTestStore) Ready(queries ...beads.ReadyQuery) ([]beads.Bead, error) {
	rows, err := s.MemStore.Ready(queries...)
	if err != nil {
		return nil, err
	}
	return s.overlayLifecycleAdmissionRows(rows), nil
}

func (s *lifecycleAdmissionTransitionTestStore) overlayLifecycleAdmissionRows(rows []beads.Bead) []beads.Bead {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range rows {
		if current, ok := s.rows[rows[index].ID]; ok {
			rows[index] = current
			rows[index].Metadata = cloneLifecycleAdmissionMetadata(current.Metadata)
		}
	}
	return rows
}

func (s *lifecycleAdmissionTransitionTestStore) overlayLifecycleAdmissionRowsForQuery(rows []beads.Bead, query beads.ListQuery) []beads.Bead {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := make(map[string]bool, len(rows))
	filtered := make([]beads.Bead, 0, len(rows)+len(s.rows))
	for _, row := range rows {
		seen[row.ID] = true
		if current, ok := s.rows[row.ID]; ok {
			row = current
		}
		if query.Status != "" && row.Status != query.Status || query.Type != "" && row.Type != query.Type {
			continue
		}
		row.Metadata = cloneLifecycleAdmissionMetadata(row.Metadata)
		filtered = append(filtered, row)
	}
	for id, current := range s.rows {
		if seen[id] || query.Status != "" && current.Status != query.Status || query.Type != "" && current.Type != query.Type {
			continue
		}
		current.Metadata = cloneLifecycleAdmissionMetadata(current.Metadata)
		filtered = append(filtered, current)
	}
	return filtered
}

func (s *lifecycleAdmissionTransitionTestStore) DecisionFrontierSourceReaderHandle() (beads.DecisionFrontierSourceReader, bool) {
	return s, true
}

func (s *lifecycleAdmissionTransitionTestStore) DecisionFrontierSourceSnapshot(id string) (beads.Bead, error) {
	row, err := s.Get(id)
	if err == nil && s.afterSourceSnapshot != nil {
		s.afterSourceSnapshot()
	}
	return row, err
}

func (s *lifecycleAdmissionTransitionTestStore) ControllerMetadataTransitionWriterHandle() (beads.ControllerMetadataTransitionWriter, bool) {
	return s, true
}

func (s *lifecycleAdmissionTransitionTestStore) StableCreateIDResolveTarget() beads.Store {
	return s.MemStore
}

func (s *lifecycleAdmissionTransitionTestStore) ControllerMetadataTransitionReceiptReaderHandle() (beads.ControllerMetadataTransitionReceiptReader, bool) {
	return s, true
}

func (s *lifecycleAdmissionTransitionTestStore) TransitionMetadata(issueID string, request beads.ControllerMetadataTransitionRequest) (beads.ControllerMetadataTransitionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if prior, ok := s.q43Receipts[request.ReceiptID]; ok {
		return beads.ControllerMetadataTransitionResult{Applied: true, Replayed: true, Receipt: &prior}, nil
	}
	current, err := s.MemStore.Get(issueID)
	if err != nil || issueID != s.sourceID || current.Revision != request.ExpectedVersion || request.Expected != nil ||
		request.Value == nil || current.Metadata[request.Key] != "" {
		return beads.ControllerMetadataTransitionResult{}, errors.New("Q43 test transition precondition failed")
	}
	var next string
	if err := json.Unmarshal(*request.Value, &next); err != nil {
		return beads.ControllerMetadataTransitionResult{}, err
	}
	if err := s.MemStore.UpdateIfMatch(issueID, request.ExpectedVersion, beads.UpdateOpts{Metadata: map[string]string{request.Key: next}}); err != nil {
		return beads.ControllerMetadataTransitionResult{}, err
	}
	committed, err := s.MemStore.Get(issueID)
	if err != nil {
		return beads.ControllerMetadataTransitionResult{}, err
	}
	receipt := beads.ControllerMetadataTransitionReceipt{
		ReceiptID: request.ReceiptID, IssueID: issueID, Scope: request.Scope, Kind: request.Kind, Actor: request.Actor,
		ExpectedVersion: request.ExpectedVersion, ToVersion: committed.Revision, Key: request.Key,
		Value: append(json.RawMessage(nil), (*request.Value)...), Payload: append(json.RawMessage(nil), request.Payload...),
	}
	s.q43Receipts[receipt.ReceiptID] = receipt
	return beads.ControllerMetadataTransitionResult{Applied: true, Receipt: &receipt}, nil
}

func (s *lifecycleAdmissionTransitionTestStore) ControllerMetadataTransitionReceipt(issueID, receiptID string) (beads.ControllerMetadataTransitionReceipt, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	receipt, ok := s.q43Receipts[receiptID]
	if ok && receipt.IssueID != issueID {
		return beads.ControllerMetadataTransitionReceipt{}, true, errors.New("Q43 test receipt issue mismatch")
	}
	return receipt, ok, nil
}

func (s *lifecycleAdmissionTransitionTestStore) RevisionTransitionPatchWriterHandle() (beads.RevisionTransitionPatchWriter, bool) {
	return s, true
}

func (s *lifecycleAdmissionTransitionTestStore) RevisionTransitionPatchReceiptReaderHandle() (beads.RevisionTransitionPatchReceiptReader, bool) {
	return s, true
}

func (s *lifecycleAdmissionTransitionTestStore) ReadRevisionTransitionPatchReceipt(receiptID string) (beads.RevisionTransitionPatchReceipt, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	receipt, ok := s.patchReceipts[receiptID]
	return receipt, ok, nil
}

func (s *lifecycleAdmissionTransitionTestStore) TransitionPatch(issueID string, request beads.RevisionTransitionPatchRequest) (beads.RevisionTransitionPatchResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.patchRequests = append(s.patchRequests, request)
	if prior, ok := s.patchReceipts[request.ReceiptID]; ok {
		return beads.RevisionTransitionPatchResult{Applied: true, Replayed: true, Receipt: &prior}, nil
	}
	current, err := s.currentLocked(issueID)
	if err != nil || issueID != s.sourceID || current.Revision != request.ExpectedVersion || request.ProtectedPermit == "" {
		return beads.RevisionTransitionPatchResult{}, beads.ErrRevisionTransitionPatchPrecondition
	}
	if request.Kind == s.rejectNextPatchKind {
		s.rejectNextPatchKind = ""
		return beads.RevisionTransitionPatchResult{}, errors.New("simulated protected patch rejection")
	}
	if request.Kind == s.failNextPatchKind {
		s.failNextPatchKind = ""
		return beads.RevisionTransitionPatchResult{}, errors.New("simulated pre-commit protected patch failure")
	}
	for _, change := range request.Patch.Metadata {
		got, present := current.Metadata[change.Key]
		if change.Expected == nil {
			if present {
				return beads.RevisionTransitionPatchResult{}, beads.ErrRevisionTransitionPatchPrecondition
			}
		} else {
			var expected string
			if json.Unmarshal(*change.Expected, &expected) != nil || !present || got != expected {
				return beads.RevisionTransitionPatchResult{}, beads.ErrRevisionTransitionPatchPrecondition
			}
		}
		if change.Value == nil {
			delete(current.Metadata, change.Key)
			continue
		}
		var value string
		if err := json.Unmarshal(*change.Value, &value); err != nil {
			return beads.RevisionTransitionPatchResult{}, err
		}
		current.Metadata[change.Key] = value
	}
	if request.Patch.Status != nil {
		if current.Status != request.Patch.Status.Expected {
			return beads.RevisionTransitionPatchResult{}, beads.ErrRevisionTransitionPatchPrecondition
		}
		current.Status = request.Patch.Status.Value
	}
	if request.Patch.Assignee != nil {
		if current.Assignee != request.Patch.Assignee.Expected {
			return beads.RevisionTransitionPatchResult{}, beads.ErrRevisionTransitionPatchPrecondition
		}
		current.Assignee = request.Patch.Assignee.Value
	}
	current.Revision++
	s.rows[issueID] = current
	receipt := beads.RevisionTransitionPatchReceipt{
		ReceiptID: request.ReceiptID, IssueID: issueID, Scope: request.Scope, Kind: request.Kind, Actor: request.Actor,
		ExpectedVersion: request.ExpectedVersion, ToVersion: current.Revision,
		PriorReceiptID: request.PriorReceiptID, PriorReceiptDigest: request.PriorReceiptDigest, Patch: request.Patch,
	}
	s.patchReceipts[receipt.ReceiptID] = receipt
	afterPatch := s.afterNextPatch
	s.afterNextPatch = nil
	if afterPatch != nil {
		afterPatch(request.Kind)
	}
	if request.Kind == s.loseNextPatchKind {
		s.loseNextPatchKind = ""
		return beads.RevisionTransitionPatchResult{}, errors.New("simulated lost protected patch response")
	}
	return beads.RevisionTransitionPatchResult{Applied: true, Receipt: &receipt}, nil
}

func (s *lifecycleAdmissionTransitionTestStore) currentLocked(id string) (beads.Bead, error) {
	if current, ok := s.rows[id]; ok {
		current.Metadata = cloneLifecycleAdmissionMetadata(current.Metadata)
		return current, nil
	}
	return s.MemStore.Get(id)
}

func (s *lifecycleAdmissionTransitionTestStore) Update(id string, opts beads.UpdateOpts) error {
	if id == s.sourceID {
		s.mu.Lock()
		s.sourceGenericWrites++
		s.mu.Unlock()
	}
	return s.MemStore.Update(id, opts)
}

func (s *lifecycleAdmissionTransitionTestStore) SetMetadata(id, key, value string) error {
	if id == s.sourceID {
		s.mu.Lock()
		s.sourceGenericWrites++
		s.mu.Unlock()
	}
	return s.MemStore.SetMetadata(id, key, value)
}

func (s *lifecycleAdmissionTransitionTestStore) SetMetadataBatch(id string, values map[string]string) error {
	if id == s.sourceID {
		s.mu.Lock()
		s.sourceGenericWrites++
		s.mu.Unlock()
	}
	return s.MemStore.SetMetadataBatch(id, values)
}

func (s *lifecycleAdmissionTransitionTestStore) UpdateIfMatch(id string, version int64, opts beads.UpdateOpts) error {
	if id == s.sourceID {
		s.mu.Lock()
		s.sourceGenericWrites++
		s.mu.Unlock()
	}
	return s.MemStore.UpdateIfMatch(id, version, opts)
}

func (s *lifecycleAdmissionTransitionTestStore) DepAdd(issueID, dependsOnID, depType string) error {
	if issueID == s.sourceID || dependsOnID == s.sourceID {
		s.mu.Lock()
		s.sourceGenericWrites++
		s.mu.Unlock()
	}
	return s.MemStore.DepAdd(issueID, dependsOnID, depType)
}

func cloneLifecycleAdmissionMetadata(metadata map[string]string) map[string]string {
	cloned := make(map[string]string, len(metadata))
	for key, value := range metadata {
		cloned[key] = value
	}
	return cloned
}
