package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/qualification"
	"github.com/gastownhall/gascity/internal/sling"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
	"github.com/gastownhall/gascity/internal/storeref"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

// reconcileLifecycleAdmission discovers explicitly admitted, not-yet-routed
// work on the controller's authoritative routed-work legs. A signed receipt
// supplies the route and workflow contract; exact current Q43 attachment and
// recomputed current-policy proof are required before it reaches the guarded
// reservation write.
func reconcileLifecycleAdmission(
	cityName string,
	cityPath string,
	cfg *config.City,
	store beads.Store,
	rigStores map[string]beads.Store,
	suspendedRigPaths map[string]bool,
	stderr io.Writer,
	authorities ...qualification.CompatibilityAuthority,
) {
	reconcileLifecycleAdmissionWithPermitResolver(cityName, cityPath, cfg, store, rigStores, suspendedRigPaths, stderr, nil, authorities...)
}

func reconcileLifecycleAdmissionWithPermitResolver(
	cityName string,
	cityPath string,
	cfg *config.City,
	store beads.Store,
	rigStores map[string]beads.Store,
	suspendedRigPaths map[string]bool,
	stderr io.Writer,
	permitResolver *hostBeadsPermitResolver,
	authorities ...qualification.CompatibilityAuthority,
) {
	var authority qualification.CompatibilityAuthority
	if len(authorities) > 0 {
		authority = authorities[0]
	}
	if cfg == nil || !cfg.Lifecycle.AdmissionEnabled {
		return
	}
	runner := sling.SlingRunner(shellSlingRunner)
	legs, err := routedWorkStoreCandidates(cityPath, cfg, store, rigStores, suspendedRigPaths)
	if err != nil {
		fmt.Fprintf(stderr, "lifecycle admission: resolving work stores: %v\n", err) //nolint:errcheck
		return
	}
	seen := make(map[storeScopedBeadKey]struct{}, 16)
	for _, leg := range legs {
		if leg.store == nil {
			continue
		}
		rows, err := leg.store.List(beads.ListQuery{
			Status: "open",
			Live:   true,
		})
		if err != nil {
			fmt.Fprintf(stderr, "lifecycle admission: listing %s intent: %v\n", leg.ref, err) //nolint:errcheck
			continue
		}
		readyRows, err := beads.HandlesFor(leg.store).Live.Ready(beads.ReadyQuery{TierMode: beads.FederatedReadTier})
		if err != nil {
			fmt.Fprintf(stderr, "lifecycle admission: reading ready work in %s: %v\n", leg.ref, err) //nolint:errcheck
			continue
		}
		ready := make(map[string]struct{}, len(readyRows))
		for _, row := range readyRows {
			ready[row.ID] = struct{}{}
		}
		for _, bead := range rows {
			key := storeScopedBeadKey{StoreRef: leg.ref, ID: bead.ID}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			if !rootStoreRefMatchesCandidate(bead.Metadata[beadmeta.RootStoreRefMetadataKey], leg.ref) {
				continue
			}
			scope := lifecycleScopeForRef(cityName, cfg, leg.ref)
			baseAdmission := worklifecycle.EvaluateAdmission(bead, cfg.Lifecycle, scope)
			if !baseAdmission.Requested {
				continue
			}
			receipt, err := worklifecycle.VerifyAdmissionReceiptV2(bead, cfg.Lifecycle, scope)
			if err != nil {
				fmt.Fprintf(stderr, "lifecycle admission: holding %s: %s\n", bead.ID, err) //nolint:errcheck
				continue
			}
			if _, ok := ready[bead.ID]; !ok {
				continue
			}
			// The route write is not an eligibility override. Do not mutate held,
			// assigned, excluded, or deferred work; normal ready/dependency checks
			// still decide when an admitted route becomes demand.
			if !demandRowServable(bead) || beads.IsDeferred(bead, time.Now()) {
				continue
			}
			adapter, err := worklifecycle.NewAdmissionAttachmentAdapter(leg.store)
			if err != nil {
				fmt.Fprintf(stderr, "lifecycle admission: holding %s: v2 admission requires verified attachment and current route-policy proof: %v\n", bead.ID, err) //nolint:errcheck
				continue
			}
			current, attachmentProof, err := adapter.VerifyForTransition(bead.ID, cfg.Lifecycle, scope)
			if err != nil {
				fmt.Fprintf(stderr, "lifecycle admission: holding %s: v2 admission requires verified attachment and current route-policy proof: %v\n", bead.ID, err) //nolint:errcheck
				continue
			}
			if current.ID != bead.ID || !rootStoreRefMatchesCandidate(current.Metadata[beadmeta.RootStoreRefMetadataKey], leg.ref) {
				fmt.Fprintf(stderr, "lifecycle admission: holding %s: verified Q43 source no longer belongs to selected store %s\n", bead.ID, leg.ref) //nolint:errcheck
				continue
			}
			currentReceipt, err := worklifecycle.VerifyAdmissionReceiptV2(current, cfg.Lifecycle, scope)
			if err != nil {
				fmt.Fprintf(stderr, "lifecycle admission: holding %s: %v\n", bead.ID, err) //nolint:errcheck
				continue
			}
			currentReady, err := beads.HandlesFor(leg.store).Live.Ready(beads.ReadyQuery{TierMode: beads.FederatedReadTier})
			if err != nil {
				fmt.Fprintf(stderr, "lifecycle admission: rereading ready work in %s for %s: %v\n", leg.ref, bead.ID, err) //nolint:errcheck
				continue
			}
			isReady := false
			for _, row := range currentReady {
				if row.ID == current.ID {
					isReady = true
					break
				}
			}
			if !isReady || !demandRowServable(current) || beads.IsDeferred(current, time.Now()) {
				continue
			}
			route := currentReceipt.Route
			routes := controllerDemandRouteCandidates(current)
			alreadyAttached := false
			var priorAttached lifecycleMaterialization
			if len(routes) != 0 {
				marker, markerOK := lifecycleMaterializationFor(current)
				if len(routes) != 1 || routes[0] != route || !markerOK || marker.State != "attached" || !lifecycleWorkflowAttached(current) {
					fmt.Fprintf(stderr, "lifecycle admission: %s route conflicts with its signed admission receipt; preserving current route\n", bead.ID) //nolint:errcheck
					continue
				}
				alreadyAttached = true
				priorAttached = marker
			}
			if lifecycleWorkflowAttached(current) && !alreadyAttached {
				fmt.Fprintf(stderr, "lifecycle admission: %s has workflow evidence without a route; preserving it for review\n", current.ID) //nolint:errcheck
				continue
			}
			if current.Revision == 0 {
				fmt.Fprintf(stderr, "lifecycle admission: %s has no usable revision; workflow materialization held\n", current.ID) //nolint:errcheck
				continue
			}
			if permitResolver == nil {
				fmt.Fprintf(stderr, "lifecycle admission: %s has no host-authorized transition permit issuer; workflow materialization held\n", current.ID) //nolint:errcheck
				continue
			}
			permitIssuer, permitPolicy, authorized := permitResolver.resolve(cityName, leg.ref)
			if !authorized || permitIssuer == nil || strings.TrimSpace(permitPolicy.Actor) == "" {
				fmt.Fprintf(stderr, "lifecycle admission: %s has no exact host authority for %s; workflow materialization held\n", current.ID, leg.ref) //nolint:errcheck
				continue
			}
			policy, err := buildLifecycleAdmissionPolicy(current, currentReceipt, scope, cityName, cityPath, cfg, store, rigStores, leg, legs, runner, authority)
			if err != nil {
				fmt.Fprintf(stderr, "lifecycle admission: holding %s: v2 admission requires verified attachment and current route-policy proof: %v\n", current.ID, err) //nolint:errcheck
				continue
			}
			deps, graphStoreRef := policy.deps, policy.graphStoreRef
			previous := strings.TrimSpace(current.Metadata[beadmeta.LifecycleMaterializationMetadataKey])
			policyResolver := worklifecycle.CurrentAdmissionPolicyResolverFunc(func(source beads.Bead, admission worklifecycle.AdmissionReceiptV2) (worklifecycle.AdmissionPolicyProjectionV2, error) {
				if source.ID != current.ID || source.Metadata[beadmeta.RootStoreRefMetadataKey] != current.Metadata[beadmeta.RootStoreRefMetadataKey] {
					return worklifecycle.AdmissionPolicyProjectionV2{}, fmt.Errorf("source identity changed while resolving admission policy")
				}
				resolved, resolveErr := buildLifecycleAdmissionPolicy(source, admission, scope, cityName, cityPath, cfg, store, rigStores, leg, legs, runner, authority)
				if resolveErr != nil {
					return worklifecycle.AdmissionPolicyProjectionV2{}, resolveErr
				}
				return resolved.projection, nil
			})
			workflowVerifier := worklifecycle.AttachedWorkflowEvidenceVerifierFunc(func(source beads.Bead, admission worklifecycle.AdmissionReceiptV2, currentPolicy worklifecycle.AdmissionPolicyProjectionV2, evidence worklifecycle.AttachedWorkflowEvidence) error {
				return verifyLifecycleAttachedWorkflow(leg.store, deps.GraphStore, source, admission, currentPolicy, evidence)
			})
			chain, err := newLifecycleAdmissionTransitionChain(leg.store, cfg.Lifecycle, scope, permitPolicy.Actor, permitIssuer, policyResolver, workflowVerifier)
			if err != nil {
				fmt.Fprintf(stderr, "lifecycle admission: %s transition chain unavailable: %v; workflow materialization held\n", current.ID, err) //nolint:errcheck
				continue
			}
			evidence := worklifecycle.TransitionEvidence{Attachment: attachmentProof}
			head, err := chain.CurrentHead(current.ID, evidence)
			if err != nil || previous == "" && (!head.FromAttachment || head.ReceiptID != attachmentProof.ReceiptID || head.ToVersion != current.Revision) ||
				previous != "" && head.FromAttachment {
				fmt.Fprintf(stderr, "lifecycle admission: %s current transition head is invalid: %v; workflow materialization held\n", current.ID, err) //nolint:errcheck
				continue
			}
			admission := worklifecycle.EvaluateAdmissionWithTransitionProof(current, cfg.Lifecycle, scope, worklifecycle.AdmissionProofInputsV2{
				Attachment: &attachmentProof, PolicyProjection: &policy.projection,
			}, head)
			if !admission.Admitted {
				fmt.Fprintf(stderr, "lifecycle admission: holding %s: %s\n", current.ID, admission.Reason) //nolint:errcheck
				continue
			}
			receipt = admission.Receipt
			route = receipt.Route
			agentCfg := &policy.target
			if agentCfg.Suspended || !agentCfg.SupportsGenericEphemeralSessions() || isCustomSlingQuery(*agentCfg) {
				fmt.Fprintf(stderr, "lifecycle admission: %s exact route %q is no longer eligible in current config\n", current.ID, route) //nolint:errcheck
				continue
			}
			digest, err := worklifecycle.AdmissionDigestV2(receipt)
			if err != nil {
				fmt.Fprintf(stderr, "lifecycle admission: hashing admission contract for %s: %v\n", current.ID, err) //nolint:errcheck
				continue
			}
			token := lifecycleReservationToken(current.ID, scope, digest)
			reservationValue, err := encodeLifecycleMaterialization(lifecycleMaterialization{
				Version:          1,
				State:            "reserved",
				Scope:            scope,
				Contract:         digest,
				Route:            route,
				Workflow:         receipt.Workflow,
				MergeStrategy:    receipt.MergeStrategy,
				Token:            token,
				SourceID:         current.ID,
				SourceStoreRef:   leg.ref,
				WorkflowStoreRef: graphStoreRef,
				AdmissionReceipt: current.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey],
			})
			if err != nil {
				fmt.Fprintf(stderr, "lifecycle admission: encoding materialization reservation for %s: %v\n", current.ID, err) //nolint:errcheck
				continue
			}
			attachedReplayValue := ""
			if alreadyAttached {
				attachedReplayValue, err = encodeLifecycleMaterialization(lifecycleMaterialization{
					Version: 1, State: "attached", Scope: scope, Contract: digest, Route: route,
					Workflow: receipt.Workflow, MergeStrategy: receipt.MergeStrategy, Token: token,
					WorkflowID: priorAttached.WorkflowID, SourceID: current.ID, SourceStoreRef: leg.ref,
					WorkflowStoreRef: graphStoreRef,
					AdmissionReceipt: current.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey],
				})
				if err != nil || priorAttached.WorkflowID == "" || previous != attachedReplayValue ||
					priorAttached.Contract != digest || priorAttached.Scope != scope || priorAttached.Route != route ||
					priorAttached.Workflow != receipt.Workflow || priorAttached.MergeStrategy != receipt.MergeStrategy ||
					priorAttached.Token != token || priorAttached.SourceID != current.ID || priorAttached.SourceStoreRef != leg.ref ||
					priorAttached.WorkflowStoreRef != graphStoreRef ||
					priorAttached.AdmissionReceipt != current.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey] {
					fmt.Fprintf(stderr, "lifecycle admission: %s attached marker does not match the current signed admission; holding for review\n", current.ID) //nolint:errcheck
					continue
				}
			} else if previous != "" && previous != reservationValue {
				fmt.Fprintf(stderr, "lifecycle admission: %s has a nonmatching prior materialization reservation; holding for review\n", current.ID) //nolint:errcheck
				continue
			}
			reservationResult, err := chain.Apply(worklifecycle.TransitionRequest{
				IssueID: current.ID, Step: worklifecycle.TransitionStepReservation,
				OperationID:    lifecycleTransitionOperationID("reservation", current.ID, scope, digest, ""),
				PriorReceiptID: attachmentProof.ReceiptID,
				Evidence:       evidence,
				Patch: worklifecycle.SourceWorkPatch{Metadata: map[string]worklifecycle.MetadataStringPatch{
					beadmeta.LifecycleMaterializationMetadataKey: {Value: reservationValue},
				}},
			})
			if err != nil {
				fmt.Fprintf(stderr, "lifecycle admission: durable reservation for %s: %v; workflow materialization held\n", current.ID, err) //nolint:errcheck
				continue
			}
			if alreadyAttached {
				workflowID := priorAttached.WorkflowID
				attachedEvidence := worklifecycle.AttachedWorkflowEvidence{
					SourceID: current.ID, SourceStoreRef: leg.ref, WorkflowStoreRef: graphStoreRef,
					WorkflowID: workflowID, Scope: scope, AdmissionDigest: digest, Route: route,
					Workflow: receipt.Workflow, MergeStrategy: receipt.MergeStrategy, Token: token,
				}
				if err := workflowVerifier.VerifyAttachedWorkflow(current, receipt, policy.projection, attachedEvidence); err != nil {
					fmt.Fprintf(stderr, "lifecycle admission: exact attached workflow for %s no longer verifies: %v; source is held\n", current.ID, err) //nolint:errcheck
					continue
				}
				attachedResult, replayErr := chain.Apply(worklifecycle.TransitionRequest{
					IssueID: current.ID, Step: worklifecycle.TransitionStepAttachedMaterialization,
					OperationID:    lifecycleTransitionOperationID("attached", current.ID, scope, digest, workflowID),
					PriorReceiptID: reservationResult.Receipt.ReceiptID,
					Evidence:       evidence,
					Patch: worklifecycle.SourceWorkPatch{Metadata: map[string]worklifecycle.MetadataStringPatch{
						beadmeta.LifecycleMaterializationMetadataKey: {Expected: &reservationValue, Value: attachedReplayValue},
						beadmeta.RoutedToMetadataKey:                 {Value: route},
						beadmeta.MoleculeIDMetadataKey:               {Value: workflowID},
						beadmeta.MergeStrategyMetadataKey:            {Value: receipt.MergeStrategy},
					}},
				})
				current, getErr := leg.store.Get(current.ID)
				attachedHead, headErr := chain.CurrentHead(current.ID, evidence)
				if replayErr != nil || attachedResult.Receipt.ReceiptID == "" || getErr != nil || headErr != nil ||
					attachedHead.FromAttachment || attachedHead.ReceiptID != attachedResult.Receipt.ReceiptID || attachedHead.ToVersion != current.Revision {
					fmt.Fprintf(stderr, "lifecycle admission: attached workflow receipt for %s could not be replayed against the current head: %v; work remains held\n", bead.ID, errors.Join(replayErr, getErr, headErr)) //nolint:errcheck
				}
				continue
			}
			current, err = leg.store.Get(current.ID)
			if err != nil || current.Metadata[beadmeta.LifecycleMaterializationMetadataKey] != reservationValue {
				fmt.Fprintf(stderr, "lifecycle admission: reservation for %s did not read back exactly; workflow materialization held\n", bead.ID) //nolint:errcheck
				continue
			}
			reservedHead, headErr := chain.CurrentHead(current.ID, evidence)
			if headErr != nil || reservedHead.FromAttachment || reservedHead.ReceiptID != reservationResult.Receipt.ReceiptID || reservedHead.ToVersion != current.Revision {
				fmt.Fprintf(stderr, "lifecycle admission: reservation head for %s does not match its exact Q54 receipt: %v; workflow materialization held\n", current.ID, headErr) //nolint:errcheck
				continue
			}
			lineageValue, err := encodeLifecycleMaterialization(lifecycleMaterialization{
				Version:          1,
				State:            "lineage_pending",
				Scope:            scope,
				Contract:         digest,
				Route:            route,
				Workflow:         admission.Receipt.Workflow,
				MergeStrategy:    admission.Receipt.MergeStrategy,
				Token:            token,
				SourceID:         bead.ID,
				SourceStoreRef:   leg.ref,
				WorkflowStoreRef: graphStoreRef,
				AdmissionReceipt: bead.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey],
			})
			if err != nil {
				fmt.Fprintf(stderr, "lifecycle admission: encoding graph lineage for %s: %v\n", bead.ID, err) //nolint:errcheck
				continue
			}
			deps.LifecycleRecipeMetadata = map[string]string{
				beadmeta.LifecycleMaterializationMetadataKey: lineageValue,
				beadmeta.MergeStrategyMetadataKey:            admission.Receipt.MergeStrategy,
				beadmeta.IdempotencyKeyMetadataKey:           lifecycleMaterializationID(current.ID, scope, digest),
			}
			scopeKind, scopeRef := lifecycleSlingScope(leg.ref, cityName)
			materializationID := lifecycleMaterializationID(current.ID, scope, digest)
			var workflowID string
			materialized := false
			err = sourceworkflow.WithLock(context.Background(), cityPath, leg.ref, current.ID, func() error {
				if recoveredID, found, recoverErr := findLifecycleMaterializedWorkflow(deps.GraphStore, materializationID, lineageValue, admission.Receipt.Workflow); recoverErr != nil {
					return recoverErr
				} else if found {
					workflowID = recoveredID
					materialized = true
					return nil
				}
				// Recheck both source readiness and the authoritative policy at the
				// effect boundary. This callback only reads stores/configuration.
				beforeAttach := func() error {
					return recheckLifecycleMaterialization(
						leg.store, current.ID, scope, reservationValue, digest, cfg.Lifecycle,
						policyResolver, chain, reservationResult.Receipt.ReceiptID,
					)
				}
				result, slingErr := sling.DoSling(sling.SlingOpts{
					Target:                   *agentCfg,
					BeadOrFormula:            current.ID,
					Merge:                    admission.Receipt.MergeStrategy,
					RequireFormulaAttach:     true,
					GraphOnlyMaterialization: true,
					MaterializationID:        materializationID,
					ScopeKind:                scopeKind,
					ScopeRef:                 scopeRef,
					BeforeFormulaAttach:      beforeAttach,
				}, deps, leg.store)
				if slingErr != nil {
					return slingErr
				}
				workflowID = strings.TrimSpace(result.WorkflowID)
				if workflowID == "" {
					workflowID = strings.TrimSpace(result.WispRootID)
				}
				if workflowID == "" {
					return fmt.Errorf("sling did not report a materialized workflow root")
				}
				materialized = true
				return nil
			})
			if err != nil || !materialized {
				fmt.Fprintf(stderr, "lifecycle admission: materializing workflow %q for %s: %v; work remains held\n", admission.Receipt.Workflow, bead.ID, err) //nolint:errcheck
				continue
			}
			attachedValue, err := encodeLifecycleMaterialization(lifecycleMaterialization{
				Version:          1,
				State:            "attached",
				Scope:            scope,
				Contract:         digest,
				Route:            route,
				Workflow:         admission.Receipt.Workflow,
				MergeStrategy:    admission.Receipt.MergeStrategy,
				Token:            token,
				WorkflowID:       workflowID,
				SourceID:         bead.ID,
				SourceStoreRef:   leg.ref,
				WorkflowStoreRef: graphStoreRef,
				AdmissionReceipt: bead.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey],
			})
			if err != nil {
				fmt.Fprintf(stderr, "lifecycle admission: encoding attached workflow evidence for %s: %v\n", bead.ID, err) //nolint:errcheck
				continue
			}
			current, err = leg.store.Get(bead.ID)
			if err != nil || current.Metadata[beadmeta.LifecycleMaterializationMetadataKey] != reservationValue {
				fmt.Fprintf(stderr, "lifecycle admission: source reservation for %s changed before attachment; work remains held\n", bead.ID) //nolint:errcheck
				continue
			}
			_, hasMerge := current.Metadata[beadmeta.MergeStrategyMetadataKey]
			if len(controllerDemandRouteCandidates(current)) != 0 || current.Metadata[beadmeta.MoleculeIDMetadataKey] != "" ||
				current.Metadata[beadmeta.WorkflowIDMetadataKey] != "" || current.Metadata[beadmeta.LegacyWorkflowIDMetadataKey] != "" || hasMerge {
				fmt.Fprintf(stderr, "lifecycle admission: source %s changed before atomic attachment; work remains held\n", bead.ID) //nolint:errcheck
				continue
			}
			attachHead, headErr := chain.CurrentHead(current.ID, evidence)
			if headErr != nil || attachHead.FromAttachment || attachHead.ReceiptID != reservationResult.Receipt.ReceiptID || attachHead.ToVersion != current.Revision {
				fmt.Fprintf(stderr, "lifecycle admission: source %s no longer has its reservation as transition head; work remains held\n", bead.ID) //nolint:errcheck
				continue
			}
			attachedResult, err := chain.Apply(worklifecycle.TransitionRequest{
				IssueID: current.ID, Step: worklifecycle.TransitionStepAttachedMaterialization,
				OperationID:    lifecycleTransitionOperationID("attached", current.ID, scope, digest, workflowID),
				PriorReceiptID: reservationResult.Receipt.ReceiptID,
				Evidence:       worklifecycle.TransitionEvidence{Attachment: attachmentProof},
				Patch: worklifecycle.SourceWorkPatch{Metadata: map[string]worklifecycle.MetadataStringPatch{
					beadmeta.LifecycleMaterializationMetadataKey: {Expected: &reservationValue, Value: attachedValue},
					beadmeta.RoutedToMetadataKey:                 {Value: route},
					beadmeta.MoleculeIDMetadataKey:               {Value: workflowID},
					beadmeta.MergeStrategyMetadataKey:            {Value: admission.Receipt.MergeStrategy},
				}},
			})
			if err != nil || attachedResult.Receipt.ReceiptID == "" {
				fmt.Fprintf(stderr, "lifecycle admission: atomic workflow attachment for %s: %v; work remains held\n", bead.ID, err) //nolint:errcheck
			}
		}
	}
}

type lifecycleMaterialization struct {
	Version          int    `json:"version"`
	State            string `json:"state"`
	Scope            string `json:"scope"`
	Contract         string `json:"contract"`
	Route            string `json:"route"`
	Workflow         string `json:"workflow"`
	MergeStrategy    string `json:"merge_strategy"`
	Token            string `json:"token"`
	WorkflowID       string `json:"workflow_id,omitempty"`
	SourceID         string `json:"source_id,omitempty"`
	SourceStoreRef   string `json:"source_store_ref,omitempty"`
	WorkflowStoreRef string `json:"workflow_store_ref,omitempty"`
	AdmissionReceipt string `json:"admission_receipt,omitempty"`
}

func encodeLifecycleMaterialization(value lifecycleMaterialization) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func lifecycleReservationToken(sourceID, scope, digest string) string {
	return lifecycleStableID("lifecycle-reservation-v1", sourceID, scope, digest)
}

func lifecycleMaterializationID(sourceID, scope, digest string) string {
	return lifecycleStableID("lifecycle-materialization-v1", sourceID, scope, digest)
}

func lifecycleTransitionOperationID(step, sourceID, scope, digest, workflowID string) string {
	return lifecycleStableID("lifecycle-transition-"+step+"-v1", sourceID, scope, digest, workflowID)
}

func lifecycleStableID(domain string, values ...string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(domain))
	for _, value := range values {
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(value))
	}
	return domain + ":" + hex.EncodeToString(hash.Sum(nil))
}

func newLifecycleAdmissionTransitionChain(
	store beads.Store,
	admissionConfig config.LifecycleConfig,
	scope, actor string,
	permitIssuer beads.ControllerProtectedMutationPermitIssuer,
	policyResolver worklifecycle.CurrentAdmissionPolicyResolver,
	workflowVerifier worklifecycle.AttachedWorkflowEvidenceVerifier,
) (*worklifecycle.TransitionChain, error) {
	patchWriter, patchWriterOK := beads.RevisionTransitionPatchWriterFor(store)
	patchReceiptReader, patchReceiptReaderOK := beads.RevisionTransitionPatchReceiptReaderFor(store)
	sourceReader, sourceReaderOK := beads.DecisionFrontierSourceReaderFor(store)
	attachmentReceiptReader, attachmentReceiptReaderOK := beads.ControllerMetadataTransitionReceiptReaderFor(store)
	if !patchWriterOK || !patchReceiptReaderOK || !sourceReaderOK || !attachmentReceiptReaderOK ||
		patchWriter == nil || patchReceiptReader == nil || sourceReader == nil || attachmentReceiptReader == nil {
		return nil, worklifecycle.ErrTransitionChainUnavailable
	}
	return worklifecycle.NewTransitionChain(worklifecycle.TransitionChainConfig{
		Scope: scope, Actor: actor, AdmissionConfig: admissionConfig,
		PatchWriter: patchWriter, PatchReceiptReader: patchReceiptReader,
		SourceReader: sourceReader, AttachmentReceiptReader: attachmentReceiptReader,
		PermitIssuer: permitIssuer, PolicyResolver: policyResolver,
		WorkflowEvidenceVerifier: workflowVerifier, Now: time.Now,
	})
}

type lifecycleClaimTransitionHeadVerifier func(
	source beads.Bead,
	sourceStore beads.Store,
	sourceStoreRef string,
	attachment worklifecycle.AdmissionAttachmentProof,
) (worklifecycle.TransitionHead, worklifecycle.AdmissionPolicyProjectionV2, error)

func newLifecycleClaimTransitionHeadVerifier(
	cityPath string,
	cfg *config.City,
	resolveStore func(string) (beads.Store, error),
) lifecycleClaimTransitionHeadVerifier {
	var storesOnce sync.Once
	var cityStore beads.Store
	var rigStores map[string]beads.Store
	var legs []classStoreCandidate
	var storesErr error
	return func(
		source beads.Bead,
		sourceStore beads.Store,
		sourceStoreRef string,
		attachment worklifecycle.AdmissionAttachmentProof,
	) (worklifecycle.TransitionHead, worklifecycle.AdmissionPolicyProjectionV2, error) {
		if cfg == nil || resolveStore == nil || sourceStore == nil || source.ID == "" || attachment.WorkItemID != source.ID {
			return worklifecycle.TransitionHead{}, worklifecycle.AdmissionPolicyProjectionV2{}, worklifecycle.ErrTransitionChainUnavailable
		}
		storesOnce.Do(func() {
			cityName := censusCityName(cfg)
			cityStore, storesErr = resolveStore("city:" + cityName)
			if storesErr != nil {
				storesErr = fmt.Errorf("resolve exact lifecycle city store: %w", storesErr)
				return
			}
			if cityStore == nil {
				storesErr = fmt.Errorf("exact lifecycle city store is unavailable")
				return
			}
			rigStores = make(map[string]beads.Store, len(cfg.Rigs))
			for _, rig := range cfg.Rigs {
				ref := "rig:" + strings.TrimSpace(rig.Name)
				store, err := resolveStore(ref)
				if err != nil {
					storesErr = fmt.Errorf("resolve exact lifecycle rig store %s: %w", ref, err)
					return
				}
				if store == nil {
					storesErr = fmt.Errorf("exact lifecycle rig store %s is unavailable", ref)
					return
				}
				rigStores[strings.TrimSpace(rig.Name)] = store
			}
			legs, storesErr = routedWorkStoreCandidates(cityPath, cfg, cityStore, rigStores, nil)
		})
		if storesErr != nil {
			return worklifecycle.TransitionHead{}, worklifecycle.AdmissionPolicyProjectionV2{}, storesErr
		}
		var selected classStoreCandidate
		for _, leg := range legs {
			if leg.ref == sourceStoreRef {
				selected = leg
				break
			}
		}
		if selected.store == nil || sourceStoreRef == "" || attachment.Scope == "" {
			return worklifecycle.TransitionHead{}, worklifecycle.AdmissionPolicyProjectionV2{}, fmt.Errorf("exact lifecycle source store %q is not in the authoritative routed-work plan", sourceStoreRef)
		}
		cityName := censusCityName(cfg)
		buildCurrentPolicy := func(candidate beads.Bead, admission worklifecycle.AdmissionReceiptV2) (lifecycleAdmissionPolicy, error) {
			if candidate.ID != source.ID || candidate.Metadata[beadmeta.RootStoreRefMetadataKey] != source.Metadata[beadmeta.RootStoreRefMetadataKey] {
				return lifecycleAdmissionPolicy{}, fmt.Errorf("source identity changed while resolving current claim policy")
			}
			return buildLifecycleAdmissionPolicy(
				candidate, admission, attachment.Scope, cityName, cityPath, cfg, cityStore, rigStores,
				selected, legs, sling.SlingRunner(shellSlingRunner), nil,
			)
		}
		receipt, err := worklifecycle.VerifyAdmissionReceiptV2(source, cfg.Lifecycle, attachment.Scope)
		if err != nil {
			return worklifecycle.TransitionHead{}, worklifecycle.AdmissionPolicyProjectionV2{}, err
		}
		currentPolicy, err := buildCurrentPolicy(source, receipt)
		if err != nil {
			return worklifecycle.TransitionHead{}, worklifecycle.AdmissionPolicyProjectionV2{}, err
		}
		policyResolver := worklifecycle.CurrentAdmissionPolicyResolverFunc(func(candidate beads.Bead, admission worklifecycle.AdmissionReceiptV2) (worklifecycle.AdmissionPolicyProjectionV2, error) {
			resolved, resolveErr := buildCurrentPolicy(candidate, admission)
			if resolveErr != nil {
				return worklifecycle.AdmissionPolicyProjectionV2{}, resolveErr
			}
			return resolved.projection, nil
		})
		workflowVerifier := worklifecycle.AttachedWorkflowEvidenceVerifierFunc(func(candidate beads.Bead, admission worklifecycle.AdmissionReceiptV2, projection worklifecycle.AdmissionPolicyProjectionV2, evidence worklifecycle.AttachedWorkflowEvidence) error {
			resolved, resolveErr := buildCurrentPolicy(candidate, admission)
			if resolveErr != nil {
				return resolveErr
			}
			return verifyLifecycleAttachedWorkflow(selected.store, resolved.deps.GraphStore, candidate, admission, projection, evidence)
		})
		patchReceiptReader, patchOK := beads.RevisionTransitionPatchReceiptReaderFor(selected.store)
		sourceReader, sourceOK := beads.DecisionFrontierSourceReaderFor(selected.store)
		attachmentReceiptReader, attachmentOK := beads.ControllerMetadataTransitionReceiptReaderFor(selected.store)
		if !patchOK || !sourceOK || !attachmentOK || patchReceiptReader == nil || sourceReader == nil || attachmentReceiptReader == nil {
			return worklifecycle.TransitionHead{}, worklifecycle.AdmissionPolicyProjectionV2{}, worklifecycle.ErrTransitionChainUnavailable
		}
		chain, err := worklifecycle.NewTransitionChainVerifier(worklifecycle.TransitionChainConfig{
			Scope: attachment.Scope, AdmissionConfig: cfg.Lifecycle,
			PatchReceiptReader: patchReceiptReader, SourceReader: sourceReader,
			AttachmentReceiptReader: attachmentReceiptReader, PolicyResolver: policyResolver,
			WorkflowEvidenceVerifier: workflowVerifier, Now: time.Now,
		})
		if err != nil {
			return worklifecycle.TransitionHead{}, worklifecycle.AdmissionPolicyProjectionV2{}, err
		}
		head, err := chain.CurrentHead(source.ID, worklifecycle.TransitionEvidence{Attachment: attachment})
		if err != nil {
			return worklifecycle.TransitionHead{}, worklifecycle.AdmissionPolicyProjectionV2{}, err
		}
		return head, currentPolicy.projection, nil
	}
}

func findLifecycleMaterializedWorkflow(store beads.Store, materializationID, pendingLineage, formulaName string) (string, bool, error) {
	if store == nil || materializationID == "" || pendingLineage == "" || formulaName == "" {
		return "", false, fmt.Errorf("workflow recovery identity is incomplete")
	}
	rows, err := store.ListByMetadata(map[string]string{beadmeta.IdempotencyKeyMetadataKey: materializationID}, 0, beads.WithBothTiers)
	if err != nil {
		return "", false, fmt.Errorf("list exact lifecycle materialization identity: %w", err)
	}
	var pending lifecycleMaterialization
	if err := json.Unmarshal([]byte(pendingLineage), &pending); err != nil || pending.State != "lineage_pending" ||
		materializationID != lifecycleMaterializationID(pending.SourceID, pending.Scope, pending.Contract) {
		return "", false, fmt.Errorf("pending workflow lineage does not match deterministic materialization identity")
	}
	var rootID string
	for _, row := range rows {
		if row.ID == "" || row.Metadata[beadmeta.IdempotencyKeyMetadataKey] != materializationID || strings.TrimSpace(row.ParentID) != "" {
			continue
		}
		root, getErr := store.Get(row.ID)
		if getErr != nil {
			return "", false, fmt.Errorf("read recovered workflow root %s: %w", row.ID, getErr)
		}
		if root.ID != row.ID || root.Metadata[beadmeta.IdempotencyKeyMetadataKey] != materializationID {
			return "", false, fmt.Errorf("recovered workflow root identity changed")
		}
		marker, markerOK := lifecycleMaterializationFor(root)
		if strings.TrimSpace(root.Metadata[beadmeta.FormulaNameMetadataKey]) != formulaName || !markerOK ||
			!sameLifecycleMaterializationContract(pending, marker) ||
			marker.State != "lineage_pending" && !(marker.State == "attached" && marker.WorkflowID == root.ID) {
			return "", false, fmt.Errorf("deterministic materialization identity points to a workflow with mismatched formula or lineage")
		}
		if strings.EqualFold(strings.TrimSpace(root.Status), "closed") || root.Metadata[beadmeta.FailureReasonMetadataKey] != "" ||
			root.Metadata["molecule_failed"] != "" {
			return "", false, fmt.Errorf("deterministic lifecycle workflow is failed or closed")
		}
		if strings.EqualFold(strings.TrimSpace(root.Metadata[beadmeta.FormulaContractMetadataKey]), beadmeta.FormulaContractGraphV2) {
			return "", false, fmt.Errorf("graph.v2 lifecycle workflow recovery is unsupported without proven invocation inputs")
		}
		if rootID != "" && rootID != root.ID {
			return "", false, fmt.Errorf("deterministic lifecycle identity resolves to multiple workflow roots")
		}
		rootID = root.ID
	}
	return rootID, rootID != "", nil
}

func sameLifecycleMaterializationContract(left, right lifecycleMaterialization) bool {
	return left.Version == right.Version && left.Scope == right.Scope && left.Contract == right.Contract &&
		left.Route == right.Route && left.Workflow == right.Workflow && left.MergeStrategy == right.MergeStrategy &&
		left.Token == right.Token && left.SourceID == right.SourceID && left.SourceStoreRef == right.SourceStoreRef &&
		left.WorkflowStoreRef == right.WorkflowStoreRef && left.AdmissionReceipt == right.AdmissionReceipt
}

func verifyLifecycleAttachedWorkflow(
	workStore, workflowStore beads.Store,
	source beads.Bead,
	admission worklifecycle.AdmissionReceiptV2,
	policy worklifecycle.AdmissionPolicyProjectionV2,
	evidence worklifecycle.AttachedWorkflowEvidence,
) error {
	if workStore == nil || workflowStore == nil || source.ID == "" || evidence.WorkflowID == "" {
		return fmt.Errorf("exact source and workflow stores are required")
	}
	if evidence.SourceID != source.ID || evidence.SourceStoreRef != policy.StorePlacement.SourceStoreRef ||
		evidence.WorkflowStoreRef != policy.StorePlacement.WorkflowStoreRef || evidence.WorkflowID == "" ||
		evidence.Scope != policy.SourceScope || evidence.Route != policy.Target.Identity ||
		evidence.Workflow != policy.Workflow || evidence.Workflow != admission.Workflow ||
		evidence.MergeStrategy != policy.MergeStrategy || evidence.MergeStrategy != admission.MergeStrategy {
		return fmt.Errorf("attached workflow identity differs from the exact admission policy")
	}
	digest, err := worklifecycle.AdmissionDigestV2(admission)
	if err != nil || digest != evidence.AdmissionDigest {
		return fmt.Errorf("attached workflow admission digest does not match")
	}
	reservation, ok := lifecycleMaterializationFor(source)
	if !ok || reservation.Version != 1 || reservation.Scope != evidence.Scope || reservation.Contract != digest ||
		reservation.Route != evidence.Route || reservation.Workflow != evidence.Workflow || reservation.MergeStrategy != evidence.MergeStrategy ||
		reservation.Token != evidence.Token || reservation.SourceID != source.ID || reservation.SourceStoreRef != evidence.SourceStoreRef ||
		reservation.WorkflowStoreRef != evidence.WorkflowStoreRef || reservation.AdmissionReceipt != source.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey] {
		return fmt.Errorf("source does not retain the exact durable reservation or attached materialization")
	}
	switch reservation.State {
	case "reserved":
		if reservation.WorkflowID != "" || len(controllerDemandRouteCandidates(source)) != 0 ||
			source.Metadata[beadmeta.MoleculeIDMetadataKey] != "" || source.Metadata[beadmeta.WorkflowIDMetadataKey] != "" ||
			source.Metadata[beadmeta.LegacyWorkflowIDMetadataKey] != "" || source.Metadata[beadmeta.MergeStrategyMetadataKey] != "" {
			return fmt.Errorf("reserved source already carries attachment fields")
		}
	case "attached":
		if reservation.WorkflowID != evidence.WorkflowID || source.Metadata[beadmeta.RoutedToMetadataKey] != evidence.Route ||
			source.Metadata[beadmeta.MoleculeIDMetadataKey] != evidence.WorkflowID ||
			source.Metadata[beadmeta.MergeStrategyMetadataKey] != evidence.MergeStrategy {
			return fmt.Errorf("source attached fields do not match the exact materialization")
		}
	default:
		return fmt.Errorf("source does not retain the exact durable reservation")
	}
	root, err := workflowStore.Get(evidence.WorkflowID)
	if err != nil || root.ID != evidence.WorkflowID {
		return fmt.Errorf("exact workflow root is unavailable in the selected store")
	}
	if strings.TrimSpace(root.Metadata[beadmeta.FormulaNameMetadataKey]) != evidence.Workflow ||
		strings.EqualFold(strings.TrimSpace(root.Metadata[beadmeta.FormulaContractMetadataKey]), beadmeta.FormulaContractGraphV2) ||
		strings.EqualFold(strings.TrimSpace(root.Status), "closed") || root.Metadata[beadmeta.FailureReasonMetadataKey] != "" ||
		root.Metadata[beadmeta.IdempotencyKeyMetadataKey] != lifecycleMaterializationID(source.ID, evidence.Scope, digest) ||
		strings.TrimSpace(root.Metadata[beadmeta.MergeStrategyMetadataKey]) != evidence.MergeStrategy {
		return fmt.Errorf("workflow root formula, status, merge, or deterministic identity does not match")
	}
	var formulaHash string
	for _, formulaSource := range policy.FormulaSources {
		if formulaSource.LogicalID == evidence.Workflow {
			formulaHash = formulaSource.SHA256
			break
		}
	}
	if formulaHash == "" || root.Metadata[beadmeta.FormulaHashMetadataKey] != formulaHash {
		return fmt.Errorf("workflow root does not match the signed formula source hash")
	}
	lineageValue, err := encodeLifecycleMaterialization(lifecycleMaterialization{
		Version: 1, State: "lineage_pending", Scope: evidence.Scope, Contract: digest,
		Route: evidence.Route, Workflow: evidence.Workflow, MergeStrategy: evidence.MergeStrategy,
		Token: evidence.Token, SourceID: source.ID,
		SourceStoreRef: evidence.SourceStoreRef, WorkflowStoreRef: evidence.WorkflowStoreRef,
		AdmissionReceipt: source.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey],
	})
	if err != nil {
		return err
	}
	if strings.TrimSpace(root.Metadata[beadmeta.LifecycleMaterializationMetadataKey]) != lineageValue {
		return fmt.Errorf("workflow root does not retain the exact graph-only lineage")
	}
	rows, err := workflowStore.ListByMetadata(map[string]string{beadmeta.LifecycleMaterializationMetadataKey: lineageValue}, 0, beads.WithBothTiers)
	if err != nil {
		return fmt.Errorf("list exact pending workflow lineage: %w", err)
	}
	rows = append(rows, root)
	if err := verifyLifecycleWorkflowDescendants(workflowStore, root, rows, lineageValue); err != nil {
		return err
	}
	return nil
}

func verifyLifecycleWorkflowDescendants(store beads.Store, root beads.Bead, rows []beads.Bead, lineage string) error {
	byID := make(map[string]beads.Bead, len(rows))
	for _, row := range rows {
		if row.ID == "" || row.Metadata[beadmeta.LifecycleMaterializationMetadataKey] != lineage {
			return fmt.Errorf("workflow row is missing its exact attached lifecycle lineage")
		}
		byID[row.ID] = row
	}
	if byID[root.ID].ID == "" {
		return fmt.Errorf("workflow lineage omitted the exact root")
	}
	for _, row := range rows {
		if row.ID == root.ID {
			continue
		}
		if strings.TrimSpace(row.Metadata[beadmeta.RootBeadIDMetadataKey]) == root.ID {
			continue
		}
		parentID := strings.TrimSpace(row.ParentID)
		seen := map[string]struct{}{}
		found := false
		for parentID != "" {
			if parentID == root.ID {
				found = true
				break
			}
			if _, duplicate := seen[parentID]; duplicate {
				break
			}
			seen[parentID] = struct{}{}
			parent, ok := byID[parentID]
			if !ok {
				var err error
				parent, err = store.Get(parentID)
				if err != nil {
					break
				}
			}
			parentID = strings.TrimSpace(parent.ParentID)
		}
		if !found {
			return fmt.Errorf("workflow row %s is not a descendant of exact root %s", row.ID, root.ID)
		}
	}
	return nil
}

func recheckLifecycleMaterialization(
	store beads.Store,
	beadID, scope, reservation, digest string,
	cfg config.LifecycleConfig,
	policyResolver worklifecycle.CurrentAdmissionPolicyResolver,
	chain *worklifecycle.TransitionChain,
	expectedHeadID string,
) error {
	adapter, err := worklifecycle.NewAdmissionAttachmentAdapter(store)
	if err != nil {
		return fmt.Errorf("reread exact Q43 attachment: %w", err)
	}
	current, attachmentProof, err := adapter.VerifyForTransition(beadID, cfg, scope)
	if err != nil {
		return fmt.Errorf("reread exact Q43 attachment: %w", err)
	}
	receipt, err := worklifecycle.VerifyAdmissionReceiptV2(current, cfg, scope)
	if err != nil {
		return fmt.Errorf("verify exact signed admission: %w", err)
	}
	projection, err := policyResolver.CurrentAdmissionPolicy(current, receipt)
	if err != nil {
		return fmt.Errorf("resolve current admission policy: %w", err)
	}
	policyDigest, err := worklifecycle.DigestAdmissionPolicyV2(projection)
	if err != nil || policyDigest != receipt.RoutingPolicyDigest || projection.SourceScope != scope ||
		projection.Target.Identity != receipt.Route || projection.Workflow != receipt.Workflow ||
		projection.MergeStrategy != receipt.MergeStrategy {
		return fmt.Errorf("current route/formula policy no longer matches the signed admission")
	}
	currentDigest, err := worklifecycle.AdmissionDigestV2(receipt)
	if err != nil || currentDigest != digest {
		return fmt.Errorf("signed workflow contract changed before materialization")
	}
	if chain == nil || expectedHeadID == "" {
		return fmt.Errorf("durable transition head verifier is unavailable")
	}
	head, err := chain.CurrentHead(current.ID, worklifecycle.TransitionEvidence{Attachment: attachmentProof})
	if err != nil || head.FromAttachment || head.ReceiptID != expectedHeadID || head.ToVersion != current.Revision {
		return fmt.Errorf("source no longer has its exact reservation as transition head: %w", errors.Join(worklifecycle.ErrTransitionChainStale, err))
	}
	if strings.TrimSpace(current.Metadata[beadmeta.LifecycleMaterializationMetadataKey]) != reservation {
		return fmt.Errorf("controller materialization reservation changed before effect")
	}
	if len(controllerDemandRouteCandidates(current)) != 0 || lifecycleWorkflowAttached(current) {
		return fmt.Errorf("work was routed or attached before materialization")
	}
	if !demandRowServable(current) || beads.IsDeferred(current, time.Now()) {
		return fmt.Errorf("work is held, assigned, or deferred")
	}
	ready, err := beads.HandlesFor(store).Live.Ready(beads.ReadyQuery{TierMode: beads.FederatedReadTier})
	if err != nil {
		return fmt.Errorf("reread ready work: %w", err)
	}
	for _, row := range ready {
		if row.ID == beadID {
			return nil
		}
	}
	return fmt.Errorf("work is no longer in the live ready set")
}

func lifecycleSlingResultMatches(bead beads.Bead, workStore, graphStore beads.Store, cfg *config.City, scope, digest, route, workflow, merge, workflowID string, graphWorkflow bool, reservation, plannedGraphStoreRef string) bool {
	decision := worklifecycle.EvaluateAdmission(bead, cfg.Lifecycle, scope)
	currentDigest, err := worklifecycle.AdmissionDigestV2(decision.Receipt)
	marker, markerOK := lifecycleMaterializationFor(bead)
	if err != nil || !decision.Admitted || currentDigest != digest ||
		!markerOK || marker.State != "reserved" || strings.TrimSpace(bead.Metadata[beadmeta.LifecycleMaterializationMetadataKey]) != reservation ||
		marker.Scope != scope || marker.Contract != digest || marker.Token == "" || marker.SourceID != bead.ID || marker.SourceStoreRef == "" ||
		marker.WorkflowStoreRef != plannedGraphStoreRef || bead.Status != "open" || !demandRowServable(bead) || beads.IsDeferred(bead, time.Now()) {
		return false
	}
	if !graphWorkflow {
		if !lifecycleRoutesMatch(cfg, bead, route) || strings.TrimSpace(bead.Metadata[beadmeta.MergeStrategyMetadataKey]) != merge {
			return false
		}
	}
	if strings.TrimSpace(workflowID) == "" {
		return false
	}
	workflowStore := workStore
	if graphWorkflow {
		workflowStore = graphStore
	}
	if workflowStore == nil {
		return false
	}
	root, err := workflowStore.Get(workflowID)
	if err != nil || strings.TrimSpace(root.Metadata[beadmeta.FormulaNameMetadataKey]) != workflow || strings.EqualFold(strings.TrimSpace(root.Status), "closed") {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(root.Metadata[beadmeta.FormulaContractMetadataKey]), beadmeta.FormulaContractGraphV2) {
		if !lifecycleRoutesMatch(cfg, root, route) || strings.TrimSpace(root.Metadata[beadmeta.MergeStrategyMetadataKey]) != merge {
			return false
		}
		rootMarker, rootMarkerOK := lifecycleMaterializationFor(root)
		if !rootMarkerOK || rootMarker.State != "lineage_pending" || rootMarker.SourceID != bead.ID ||
			rootMarker.SourceStoreRef != marker.SourceStoreRef || rootMarker.WorkflowStoreRef != plannedGraphStoreRef || rootMarker.Contract != digest {
			return false
		}
		if !sourceworkflow.IsWorkflowRoot(root) {
			return false
		}
		inputConvoy := strings.TrimSpace(root.Metadata[beadmeta.InputConvoyIDMetadataKey])
		if inputConvoy == "" {
			return false
		}
		members, err := convoycore.Members(workStore, inputConvoy, false)
		if err != nil || !slices.ContainsFunc(members, func(member beads.Bead) bool { return member.ID == bead.ID }) {
			return false
		}
	} else if strings.TrimSpace(bead.Metadata[beadmeta.MoleculeIDMetadataKey]) != workflowID &&
		strings.TrimSpace(bead.Metadata[beadmeta.WorkflowIDMetadataKey]) != workflowID &&
		strings.TrimSpace(bead.Metadata[beadmeta.LegacyWorkflowIDMetadataKey]) != workflowID {
		return false
	}
	return true
}

func lifecycleScopeForRef(cityName string, cfg *config.City, ref string) string {
	if strings.TrimSpace(cityName) == "" {
		cityName = censusCityName(cfg)
	}
	return worklifecycle.ScopeForStore(cityName, ref)
}

func lifecycleScopeForDemand(cityName string, cfg *config.City, storeKey string) string {
	storeKey = strings.TrimSpace(storeKey)
	if storeKey == "city" {
		storeKey = "city:" + censusCityName(cfg)
	}
	return lifecycleScopeForRef(cityName, cfg, storeKey)
}

func lifecycleWorkflowAttached(bead beads.Bead) bool {
	return strings.TrimSpace(bead.Metadata["workflow_id"]) != "" || strings.TrimSpace(bead.Metadata["molecule_id"]) != "" || lifecycleMaterializationEvidence(bead)
}

func lifecycleMaterializationEvidence(bead beads.Bead) bool {
	marker, ok := lifecycleMaterializationFor(bead)
	if !ok {
		return false
	}
	return marker.Version == 1 && marker.State == "attached" && strings.TrimSpace(marker.Scope) != "" &&
		strings.TrimSpace(marker.Contract) != "" && strings.TrimSpace(marker.Route) != "" &&
		strings.TrimSpace(marker.Workflow) != "" && strings.TrimSpace(marker.MergeStrategy) != "" &&
		strings.TrimSpace(marker.Token) != "" && strings.TrimSpace(marker.WorkflowID) != "" &&
		strings.TrimSpace(marker.SourceID) != "" && strings.TrimSpace(marker.SourceStoreRef) != "" &&
		strings.TrimSpace(marker.WorkflowStoreRef) != "" && strings.TrimSpace(marker.AdmissionReceipt) != ""
}

func lifecycleMaterializationFor(bead beads.Bead) (lifecycleMaterialization, bool) {
	raw := strings.TrimSpace(bead.Metadata[beadmeta.LifecycleMaterializationMetadataKey])
	if raw == "" {
		return lifecycleMaterialization{}, false
	}
	var marker lifecycleMaterialization
	if err := json.Unmarshal([]byte(raw), &marker); err != nil {
		return lifecycleMaterialization{}, false
	}
	return marker, true
}

func lifecycleSlingDeps(
	cityName, cityPath string,
	cfg *config.City,
	store beads.Store,
	rigStores map[string]beads.Store,
	suspendedRigPaths map[string]bool,
	selected classStoreCandidate,
	sourceStores []classStoreCandidate,
	runner sling.SlingRunner,
	authority qualification.CompatibilityAuthority,
) (sling.SlingDeps, string, error) {
	work := censusWorkLeg(cityPath, store)
	topology := residencyTopologyForCity(cityPath, cfg, work, servingRigStores(cfg, rigStores, suspendedRigPaths))
	var graphStore beads.Store
	graphStoreRef := ""
	if rig, scoped := storeref.ScopeRigContext(selected.ref); scoped && rig != "" {
		// Rig-scoped formulas materialize their graph in the rig scope. A city
		// graph-class binding is authoritative only for city-scoped formulas.
		graphStore = selected.store
		graphStoreRef = selected.ref
	} else {
		graphPlan, err := storeref.Plan(storeref.Class{C: coordclass.ClassGraph}, topology)
		if err != nil {
			return sling.SlingDeps{}, "", fmt.Errorf("resolving graph workflow store: %w", err)
		}
		graphLeg, err := storeref.ResolvePlacement(graphPlan)
		if err != nil {
			return sling.SlingDeps{}, "", fmt.Errorf("resolving graph workflow placement: %w", err)
		}
		graphStore = graphLeg.Store
		graphStoreRef = censusRef(cfg, graphLeg.Ref, censusRefScoped)
	}
	if graphStore == nil {
		return sling.SlingDeps{}, "", fmt.Errorf("graph workflow store is unavailable")
	}
	if strings.TrimSpace(cityName) == "" {
		cityName = censusCityName(cfg)
	}
	deps := sling.SlingDeps{
		CityName:                 cityName,
		CityPath:                 cityPath,
		Cfg:                      cfg,
		Runner:                   runner,
		Store:                    selected.store,
		GraphStore:               graphStore,
		GraphStoreRef:            graphStoreRef,
		FormulaActionGate:        controllerFormulaActionGateWithCurrent(cityPath, cfg, graphStoreRef, authority, nil),
		RequireFormulaActionGate: true,
		ExecutionWorkStore:       selected.store,
		StoreRef:                 selected.ref,
		SourceWorkflowStores: func() ([]sling.SourceWorkflowStore, error) {
			out := make([]sling.SourceWorkflowStore, 0, len(sourceStores))
			for _, source := range sourceStores {
				if source.store == nil {
					continue
				}
				out = append(out, sling.SourceWorkflowStore{Store: source.store, StoreRef: source.ref, Strict: true})
			}
			return out, nil
		},
	}
	populateSlingDepsCallbacks(&deps)
	return deps, graphStoreRef, nil
}

func lifecycleSlingScope(storeRef, cityName string) (kind, ref string) {
	if rig, scoped := storeref.ScopeRigContext(storeRef); scoped && rig != "" {
		return "rig", rig
	}
	cityName = strings.TrimSpace(cityName)
	if cityName == "" {
		cityName = "city"
	}
	return "city", cityName
}

// lifecycleStoreForRef resolves a persisted census/store ref to exactly the
// store that ref names. It is intentionally strict: a missing class binding or
// rig path is an unavailable authority, never a reason to try the city store.
func lifecycleStoreForRef(cityPath string, cfg *config.City, ref string) (beads.Store, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("empty lifecycle store ref")
	}
	if strings.HasPrefix(ref, "city:") {
		city := strings.TrimSpace(strings.TrimPrefix(ref, "city:"))
		if cfg == nil || city == "" || city != censusCityName(cfg) {
			return nil, fmt.Errorf("lifecycle store ref %q does not name this city", ref)
		}
		return openStoreAtForCity(cityPath, cityPath)
	}
	if strings.HasPrefix(ref, "rig:") {
		rigName := strings.TrimSpace(strings.TrimPrefix(ref, "rig:"))
		if cfg == nil || rigName == "" {
			return nil, fmt.Errorf("invalid lifecycle rig store ref %q", ref)
		}
		for _, rig := range cfg.Rigs {
			if strings.TrimSpace(rig.Name) == rigName {
				return openStoreAtForCity(rig.Path, cityPath)
			}
		}
		return nil, fmt.Errorf("lifecycle rig store %q is not configured", ref)
	}
	if !storeref.IsClassRef(ref) {
		return nil, fmt.Errorf("unsupported lifecycle store ref %q", ref)
	}
	if cfg == nil {
		return nil, fmt.Errorf("cannot resolve class store %q without city config", ref)
	}
	work, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		return nil, fmt.Errorf("open city work store for class resolution: %w", err)
	}
	rigs := make(map[string]beads.Store, len(cfg.Rigs))
	for _, rig := range cfg.Rigs {
		rigStore, err := openStoreAtForCity(rig.Path, cityPath)
		if err != nil {
			return nil, fmt.Errorf("open rig %q while resolving class store %s: %w", rig.Name, ref, err)
		}
		rigs[rig.Name] = rigStore
	}
	topology := residencyTopologyForCity(cityPath, cfg, work, servingRigStores(cfg, rigs, nil))
	for _, class := range coordclass.Classes() {
		plan, err := storeref.Plan(storeref.Class{C: class}, topology)
		if err != nil {
			return nil, fmt.Errorf("resolve class %s while looking for %s: %w", class, ref, err)
		}
		leg, err := storeref.ResolvePlacement(plan)
		if err != nil {
			return nil, fmt.Errorf("resolve class %s placement while looking for %s: %w", class, ref, err)
		}
		if censusRef(cfg, leg.Ref, censusRefScoped) == ref {
			return leg.Store, nil
		}
	}
	return nil, fmt.Errorf("configured topology has no store named %q", ref)
}

func lifecycleWorkflowRowDescendsFrom(root beads.Bead, row beads.Bead, rows []beads.Bead) bool {
	byID := make(map[string]beads.Bead, len(rows))
	for _, candidate := range rows {
		byID[candidate.ID] = candidate
	}
	if strings.TrimSpace(row.Metadata[beadmeta.RootBeadIDMetadataKey]) == root.ID {
		return true
	}
	parentID := strings.TrimSpace(row.ParentID)
	seen := map[string]struct{}{}
	for parentID != "" {
		if parentID == root.ID {
			return true
		}
		if _, duplicate := seen[parentID]; duplicate {
			return false
		}
		seen[parentID] = struct{}{}
		parent, ok := byID[parentID]
		if !ok {
			return false
		}
		parentID = strings.TrimSpace(parent.ParentID)
	}
	return false
}

// lifecycleAdmissionRouteMatches reports whether a work item whose lifecycle
// enrollment has been verified still carries the route the trusted receipt
// authorized. It deliberately does not repair a conflicting pre-existing
// route; an operator must resolve that ambiguity.
func lifecycleAdmissionRouteMatches(cfg *config.City, bead beads.Bead, scope string) bool {
	if cfg == nil || !cfg.Lifecycle.AdmissionEnabled {
		return true
	}
	if marker, ok := lifecycleMaterializationFor(bead); ok && marker.SourceID != "" && marker.SourceID != bead.ID {
		return lifecycleLineageAdmissionMatches(cfg, bead, marker)
	}
	decision := worklifecycle.EvaluateAdmission(bead, cfg.Lifecycle, scope)
	if !decision.Requested {
		return !lifecycleEnrollmentEvidence(bead)
	}
	if !decision.Admitted || !lifecycleMaterializationEvidence(bead) {
		return false
	}
	marker, ok := lifecycleMaterializationFor(bead)
	if !ok {
		return false
	}
	digest, err := worklifecycle.AdmissionDigestV2(decision.Receipt)
	if err != nil || marker.Scope != scope || marker.Contract != digest || marker.Workflow != decision.Receipt.Workflow ||
		marker.MergeStrategy != decision.Receipt.MergeStrategy || marker.WorkflowID == "" ||
		agentutil.NormalizePoolRouteTarget(cfg, marker.Route) != agentutil.NormalizePoolRouteTarget(cfg, decision.Receipt.Route) {
		return false
	}
	// Formula-v1 attachments write one of these identifiers onto the source.
	// If present, it must agree with the root that the controller verified. A
	// graph.v2 attachment is convoy-first and can legitimately have neither.
	for _, sourceWorkflowID := range []string{
		bead.Metadata[beadmeta.MoleculeIDMetadataKey],
		bead.Metadata[beadmeta.WorkflowIDMetadataKey],
		bead.Metadata[beadmeta.LegacyWorkflowIDMetadataKey],
	} {
		if sourceWorkflowID = strings.TrimSpace(sourceWorkflowID); sourceWorkflowID != "" && sourceWorkflowID != marker.WorkflowID {
			return false
		}
	}
	permitted := agentutil.NormalizePoolRouteTarget(cfg, decision.Receipt.Route)
	agentCfg := findAgentByTemplate(cfg, permitted)
	if agentCfg == nil || agentCfg.Suspended || !agentCfg.SupportsGenericEphemeralSessions() ||
		isCustomSlingQuery(*agentCfg) || strings.TrimSpace(agentCfg.EffectiveDefaultSlingFormula()) != decision.Receipt.Workflow {
		return false
	}
	if strings.TrimSpace(bead.Metadata[beadmeta.MergeStrategyMetadataKey]) != decision.Receipt.MergeStrategy {
		return false
	}
	return lifecycleRoutesMatch(cfg, bead, permitted)
}

// lifecycleRouteCandidates includes the execution route attached to graph.v2
// recipe steps. Those steps are selected through the workflow's input convoy,
// so they intentionally do not carry gc.routed_to (the source work's direct
// demand route); their gc.execution_routed_to is covered by the controller's
// signed lineage metadata instead.
func lifecycleRouteCandidates(bead beads.Bead) []string {
	candidates := controllerDemandRouteCandidates(bead)
	if executionRoute := strings.TrimSpace(bead.Metadata[beadmeta.ExecutionRoutedToMetadataKey]); executionRoute != "" {
		candidates = append(candidates, executionRoute)
	}
	return candidates
}

func lifecycleRoutesMatch(cfg *config.City, bead beads.Bead, authorizedRoute string) bool {
	routes := lifecycleRouteCandidates(bead)
	if len(routes) == 0 {
		return false
	}
	authorizedRoute = agentutil.NormalizePoolRouteTarget(cfg, authorizedRoute)
	for _, route := range routes {
		if agentutil.NormalizePoolRouteTarget(cfg, route) != authorizedRoute {
			return false
		}
	}
	return true
}

// lifecycleLineageAdmissionMatches verifies the controller-carried signed
// contract on one graph.v2 node without trusting the node's own role or route
// strings. The hook adds live source/root reads before it can execute the node;
// demand uses this local signature check so an attached descendant remains
// visible to its configured worker.
func lifecycleLineageAdmissionMatches(cfg *config.City, bead beads.Bead, marker lifecycleMaterialization) bool {
	if cfg == nil || !cfg.Lifecycle.AdmissionEnabled || marker.Version != 1 ||
		(marker.State != "attached" && marker.State != "lineage_pending") ||
		strings.TrimSpace(marker.SourceID) == "" || strings.TrimSpace(marker.SourceStoreRef) == "" ||
		strings.TrimSpace(marker.WorkflowStoreRef) == "" ||
		(marker.State == "attached" && strings.TrimSpace(marker.WorkflowID) == "") ||
		(marker.State == "lineage_pending" && strings.TrimSpace(marker.WorkflowID) != "") ||
		strings.TrimSpace(marker.AdmissionReceipt) == "" {
		return false
	}
	if rootStoreRef := strings.TrimSpace(bead.Metadata[beadmeta.RootStoreRefMetadataKey]); rootStoreRef != "" && rootStoreRef != marker.SourceStoreRef {
		return false
	}
	workflowID := strings.TrimSpace(marker.WorkflowID)
	rootCandidate := sourceworkflow.IsWorkflowRoot(bead) || lifecycleFormulaV1Root(bead, marker.Workflow)
	if sourceworkflow.IsWorkflowRoot(bead) && strings.EqualFold(strings.TrimSpace(bead.Metadata[beadmeta.FormulaContractMetadataKey]), beadmeta.FormulaContractGraphV2) {
		return false
	}
	if workflowID == "" && rootCandidate {
		workflowID = bead.ID
	} else if workflowID == "" {
		workflowID = strings.TrimSpace(bead.Metadata[beadmeta.RootBeadIDMetadataKey])
	}
	if workflowID == "" && (strings.TrimSpace(bead.ParentID) == "" || marker.State != "lineage_pending") {
		return false
	}
	if rootCandidate || bead.ID == workflowID {
		if !rootCandidate || strings.TrimSpace(bead.Metadata[beadmeta.FormulaNameMetadataKey]) != marker.Workflow {
			return false
		}
		if routes := lifecycleRouteCandidates(bead); len(routes) != 0 && !lifecycleRoutesMatch(cfg, bead, marker.Route) {
			return false
		}
	} else {
		rootID := strings.TrimSpace(bead.Metadata[beadmeta.RootBeadIDMetadataKey])
		if rootID != "" && workflowID != "" && rootID != workflowID {
			return false
		}
		if rootID == "" && strings.TrimSpace(bead.ParentID) == "" {
			return false
		}
	}
	if strings.TrimSpace(bead.Metadata[beadmeta.MergeStrategyMetadataKey]) != marker.MergeStrategy {
		return false
	}
	if bead.ID != workflowID && !lifecycleRoutesMatch(cfg, bead, marker.Route) {
		return false
	}
	receiptBead := beads.Bead{
		ID:     marker.SourceID,
		Labels: []string{worklifecycle.AdmissionIntentLabel},
		Metadata: map[string]string{
			beadmeta.LifecycleAdmissionReceiptV2MetadataKey: marker.AdmissionReceipt,
		},
	}
	receipt, err := worklifecycle.VerifyAdmissionReceiptV2(receiptBead, cfg.Lifecycle, marker.Scope)
	digest, digestErr := worklifecycle.AdmissionDigestV2(receipt)
	return err == nil && digestErr == nil && digest == marker.Contract &&
		receipt.Route == marker.Route && receipt.Workflow == marker.Workflow &&
		receipt.MergeStrategy == marker.MergeStrategy
}

func lifecycleFormulaV1Root(bead beads.Bead, formulaName string) bool {
	return strings.EqualFold(strings.TrimSpace(bead.Type), "molecule") &&
		!strings.EqualFold(strings.TrimSpace(bead.Metadata[beadmeta.FormulaContractMetadataKey]), beadmeta.FormulaContractGraphV2) &&
		strings.TrimSpace(bead.Metadata[beadmeta.FormulaNameMetadataKey]) == formulaName &&
		strings.TrimSpace(bead.ParentID) == ""
}

func lifecycleAdmissionRequested(bead beads.Bead, cfg *config.City) bool {
	if cfg == nil || !cfg.Lifecycle.AdmissionEnabled {
		return false
	}
	return worklifecycle.EvaluateAdmission(bead, cfg.Lifecycle, "").Requested || lifecycleEnrollmentEvidence(bead)
}

// lifecycleEnrollmentEvidence persists enforcement after the operator removes
// the original intent label. Missing or damaged signed admission evidence
// then becomes a hold instead of silently opting the item back out.
func lifecycleEnrollmentEvidence(bead beads.Bead) bool {
	return worklifecycle.HasDurableEnrollment(bead)
}

func lifecycleProtectedWork(bead beads.Bead, cfg *config.City) bool {
	// Enrollment is durable. Turning off admission stops new work from joining
	// this controller-owned lane; it must not return already enrolled work to
	// legacy release/restart writers.
	if lifecycleEnrollmentEvidence(bead) {
		return true
	}
	if cfg == nil || !cfg.Lifecycle.AdmissionEnabled {
		return false
	}
	for _, label := range bead.Labels {
		if strings.EqualFold(strings.TrimSpace(label), worklifecycle.AdmissionIntentLabel) {
			return true
		}
	}
	return false
}

func filterHookLifecycleCandidates(candidates []beads.Bead, opts hookClaimOptions, stderr io.Writer) []beads.Bead {
	filtered := make([]beads.Bead, 0, len(candidates))
	for _, bead := range candidates {
		if !opts.Lifecycle.AdmissionEnabled {
			if !lifecycleEnrollmentEvidence(bead) {
				filtered = append(filtered, bead)
				continue
			}
			fmt.Fprintf(stderr, "gc hook --claim: holding lifecycle item %s: lifecycle admission is disabled; existing enrollment remains protected\n", bead.ID) //nolint:errcheck
			continue
		}
		if marker, ok := lifecycleMaterializationFor(bead); ok && marker.SourceID != "" && marker.SourceID != bead.ID {
			reason := "graph lineage does not match a trusted admission contract"
			switch {
			case !opts.TrustedLifecycleScope:
				reason = "lifecycle scope came from a custom work query"
			case !lifecycleLineageAdmissionMatches(opts.LifecycleCity, bead, marker):
				// Keep the contract verifier's fail-closed result.
			case !lifecycleLineageCurrent(bead, marker, opts):
				reason = "graph lineage, source admission, or workflow store could not be verified"
			default:
				filtered = append(filtered, bead)
				continue
			}
			fmt.Fprintf(stderr, "gc hook --claim: holding lifecycle descendant %s: %s\n", bead.ID, reason) //nolint:errcheck
			continue
		}
		decision := worklifecycle.EvaluateAdmission(bead, opts.Lifecycle, bead.LifecycleScope)
		if !decision.Requested && !lifecycleEnrollmentEvidence(bead) {
			filtered = append(filtered, bead)
			continue
		}
		reason := decision.Reason
		switch {
		case !decision.Requested:
			reason = "durable lifecycle enrollment has no verifiable admission receipt"
		case !opts.TrustedLifecycleScope:
			reason = "lifecycle scope came from a custom work query"
		case strings.TrimSpace(bead.LifecycleScope) == "":
			reason = "trusted lifecycle scope is missing"
		case !decision.Admitted:
			// Keep the receipt verifier's item-specific explanation.
		case !lifecycleCurrentSource(bead, opts):
			reason = "canonical source admission or current eligibility could not be verified"
		case !lifecycleAdmissionRouteMatches(opts.LifecycleCity, bead, bead.LifecycleScope):
			reason = "attached workflow or route does not match the signed admission contract"
		case !hookLifecycleRouteMatches(opts.LifecycleCity, decision.Receipt.Route, opts.RouteTargets):
			reason = "candidate route does not match the signed admission contract"
		default:
			filtered = append(filtered, bead)
			continue
		}
		fmt.Fprintf(stderr, "gc hook --claim: holding lifecycle item %s: %s\n", bead.ID, reason) //nolint:errcheck
	}
	return filtered
}

func lifecycleCurrentSource(candidate beads.Bead, opts hookClaimOptions) bool {
	if opts.ResolveLifecycleStore == nil || strings.TrimSpace(candidate.SourceStoreRef) == "" || strings.TrimSpace(candidate.LifecycleScope) == "" {
		return false
	}
	store, err := opts.ResolveLifecycleStore(candidate.SourceStoreRef)
	if err != nil || store == nil {
		return false
	}
	current, err := store.Get(candidate.ID)
	if err != nil {
		return false
	}
	current.SourceStoreRef = candidate.SourceStoreRef
	current.LifecycleScope = candidate.LifecycleScope
	decision := worklifecycle.EvaluateAdmission(current, opts.Lifecycle, current.LifecycleScope)
	status := strings.ToLower(strings.TrimSpace(current.Status))
	owner := strings.TrimSpace(current.Assignee)
	return decision.Admitted && lifecycleAdmissionRouteMatches(opts.LifecycleCity, current, current.LifecycleScope) &&
		(status == "open" || status == "in_progress") && !beads.IsDeferred(current, time.Now()) && !lifecycleRowHeld(current) &&
		(owner == "" || hookClaimHasIdentity(owner, opts.IdentityCandidates))
}

func lifecycleLineageCurrent(candidate beads.Bead, lineage lifecycleMaterialization, opts hookClaimOptions) bool {
	if opts.ResolveLifecycleStore == nil || !opts.TrustedLifecycleScope ||
		strings.TrimSpace(candidate.SourceStoreRef) == "" ||
		(lineage.State != "attached" && lineage.State != "lineage_pending") {
		return false
	}
	workflowStore, err := opts.ResolveLifecycleStore(lineage.WorkflowStoreRef)
	if err != nil || workflowStore == nil {
		return false
	}
	sourceStore, err := opts.ResolveLifecycleStore(lineage.SourceStoreRef)
	if err != nil || sourceStore == nil {
		return false
	}
	source, err := sourceStore.Get(lineage.SourceID)
	if err != nil {
		return false
	}
	attachmentAdapter, err := worklifecycle.NewAdmissionAttachmentAdapter(sourceStore)
	if err != nil {
		return false
	}
	verifiedSource, attachmentProof, err := attachmentAdapter.VerifyForTransition(lineage.SourceID, opts.Lifecycle, lineage.Scope)
	if err != nil || verifiedSource.ID != source.ID || verifiedSource.Revision != source.Revision ||
		verifiedSource.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey] != source.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey] ||
		attachmentProof.Scope != lineage.Scope || attachmentProof.WorkItemID != lineage.SourceID {
		return false
	}
	source.LifecycleScope = lineage.Scope
	source.SourceStoreRef = lineage.SourceStoreRef
	receipt, err := worklifecycle.VerifyAdmissionReceiptV2(source, opts.Lifecycle, lineage.Scope)
	digest, digestErr := worklifecycle.AdmissionDigestV2(receipt)
	if opts.VerifyLifecycleTransitionHead == nil {
		return false
	}
	head, projection, headErr := opts.VerifyLifecycleTransitionHead(source, sourceStore, lineage.SourceStoreRef, attachmentProof)
	if headErr != nil || head.FromAttachment || head.ToVersion != source.Revision ||
		head.ReceiptID == "" || head.ReceiptID != source.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] {
		return false
	}
	transitionDecision := worklifecycle.EvaluateAdmissionWithTransitionProof(source, opts.Lifecycle, lineage.Scope,
		worklifecycle.AdmissionProofInputsV2{Attachment: &attachmentProof, PolicyProjection: &projection}, head)
	sourceStatus := strings.ToLower(strings.TrimSpace(source.Status))
	sourceOwner := strings.TrimSpace(source.Assignee)
	if err != nil || digestErr != nil || !transitionDecision.Admitted || digest != lineage.Contract || receipt.Route != lineage.Route ||
		receipt.Workflow != lineage.Workflow || receipt.MergeStrategy != lineage.MergeStrategy ||
		!lifecycleAttachedSourceRouteCurrent(opts.LifecycleCity, source, receipt, lineage.Scope) ||
		(sourceStatus != "open" && sourceStatus != "in_progress") ||
		(sourceOwner != "" && !hookClaimHasIdentity(sourceOwner, opts.IdentityCandidates)) ||
		beads.IsDeferred(source, time.Now()) || lifecycleRowHeld(source) {
		return false
	}
	attached, ok := lifecycleMaterializationFor(source)
	if !ok || attached.State != "attached" || !sameLifecycleLineage(attached, lineage) || attached.WorkflowID == "" ||
		source.Metadata[beadmeta.RoutedToMetadataKey] != lineage.Route ||
		source.Metadata[beadmeta.MoleculeIDMetadataKey] != attached.WorkflowID ||
		source.Metadata[beadmeta.MergeStrategyMetadataKey] != lineage.MergeStrategy ||
		source.Metadata[beadmeta.WorkflowIDMetadataKey] != "" || source.Metadata[beadmeta.LegacyWorkflowIDMetadataKey] != "" {
		return false
	}
	workflowID := attached.WorkflowID
	if lineage.State == "attached" && lineage.WorkflowID != workflowID || lineage.State == "lineage_pending" && lineage.WorkflowID != "" {
		return false
	}
	expectedLineageValue := lifecycleLineageValueForWorkflow(lineage, attached, workflowID)
	if expectedLineageValue == "" {
		return false
	}
	root, err := workflowStore.Get(workflowID)
	if err != nil || root.ID != workflowID || !lifecycleFormulaV1Root(root, lineage.Workflow) ||
		strings.EqualFold(strings.TrimSpace(root.Metadata[beadmeta.FormulaContractMetadataKey]), beadmeta.FormulaContractGraphV2) ||
		strings.TrimSpace(root.Metadata[beadmeta.FormulaNameMetadataKey]) != lineage.Workflow ||
		strings.TrimSpace(root.Metadata[beadmeta.MergeStrategyMetadataKey]) != lineage.MergeStrategy ||
		root.Metadata[beadmeta.IdempotencyKeyMetadataKey] != lifecycleMaterializationID(lineage.SourceID, lineage.Scope, lineage.Contract) ||
		root.Metadata[beadmeta.LifecycleMaterializationMetadataKey] != expectedLineageValue ||
		strings.EqualFold(strings.TrimSpace(root.Status), "closed") || root.Metadata[beadmeta.FailureReasonMetadataKey] != "" ||
		root.Metadata["molecule_failed"] != "" {
		return false
	}
	if rootStoreRef := strings.TrimSpace(root.Metadata[beadmeta.RootStoreRefMetadataKey]); rootStoreRef != "" && rootStoreRef != lineage.SourceStoreRef {
		return false
	}
	formulaHash := strings.TrimSpace(root.Metadata[beadmeta.FormulaHashMetadataKey])
	decodedHash, decodeErr := hex.DecodeString(formulaHash)
	if decodeErr != nil || len(decodedHash) != sha256.Size {
		return false
	}
	currentCandidate, err := workflowStore.Get(candidate.ID)
	if err != nil || currentCandidate.ID != candidate.ID ||
		currentCandidate.Metadata[beadmeta.LifecycleMaterializationMetadataKey] != expectedLineageValue ||
		strings.TrimSpace(currentCandidate.Metadata[beadmeta.MergeStrategyMetadataKey]) != lineage.MergeStrategy ||
		lifecycleRowHeld(currentCandidate) || beads.IsDeferred(currentCandidate, time.Now()) {
		return false
	}
	if rootStoreRef := strings.TrimSpace(currentCandidate.Metadata[beadmeta.RootStoreRefMetadataKey]); rootStoreRef != "" && rootStoreRef != lineage.SourceStoreRef {
		return false
	}
	if candidate.ID != workflowID {
		if rootID := strings.TrimSpace(currentCandidate.Metadata[beadmeta.RootBeadIDMetadataKey]); rootID != "" {
			return rootID == workflowID
		}
		parentID := strings.TrimSpace(currentCandidate.ParentID)
		seen := map[string]struct{}{currentCandidate.ID: {}}
		for hops := 0; parentID != "" && hops < 256; hops++ {
			if parentID == workflowID {
				return true
			}
			if _, duplicate := seen[parentID]; duplicate {
				return false
			}
			seen[parentID] = struct{}{}
			parent, getErr := workflowStore.Get(parentID)
			if getErr != nil || parent.Metadata[beadmeta.LifecycleMaterializationMetadataKey] != expectedLineageValue {
				return false
			}
			parentID = strings.TrimSpace(parent.ParentID)
		}
		return false
	}
	return true
}

func lifecycleAttachedSourceRouteCurrent(cfg *config.City, source beads.Bead, receipt worklifecycle.AdmissionReceiptV2, scope string) bool {
	if cfg == nil || !cfg.Lifecycle.AdmissionEnabled || receipt.Scope != scope || receipt.WorkItemID != source.ID {
		return false
	}
	marker, ok := lifecycleMaterializationFor(source)
	if !ok || marker.State != "attached" || marker.Scope != scope || marker.Route != receipt.Route ||
		marker.Workflow != receipt.Workflow || marker.MergeStrategy != receipt.MergeStrategy || marker.WorkflowID == "" {
		return false
	}
	digest, err := worklifecycle.AdmissionDigestV2(receipt)
	if err != nil || digest != marker.Contract || source.Metadata[beadmeta.RoutedToMetadataKey] != receipt.Route ||
		source.Metadata[beadmeta.MoleculeIDMetadataKey] != marker.WorkflowID ||
		source.Metadata[beadmeta.WorkflowIDMetadataKey] != "" || source.Metadata[beadmeta.LegacyWorkflowIDMetadataKey] != "" ||
		source.Metadata[beadmeta.MergeStrategyMetadataKey] != receipt.MergeStrategy {
		return false
	}
	permitted := agentutil.NormalizePoolRouteTarget(cfg, receipt.Route)
	agentCfg := findAgentByTemplate(cfg, permitted)
	return agentCfg != nil && !agentCfg.Suspended && agentCfg.SupportsGenericEphemeralSessions() &&
		!isCustomSlingQuery(*agentCfg) && strings.TrimSpace(agentCfg.EffectiveDefaultSlingFormula()) == receipt.Workflow &&
		lifecycleRoutesMatch(cfg, source, permitted)
}

func lifecycleLineageValueForWorkflow(lineage, attached lifecycleMaterialization, workflowID string) string {
	marker := lifecycleMaterialization{
		Version: 1, State: lineage.State, Scope: attached.Scope, Contract: attached.Contract,
		Route: attached.Route, Workflow: attached.Workflow, MergeStrategy: attached.MergeStrategy,
		Token: attached.Token, SourceID: attached.SourceID, SourceStoreRef: attached.SourceStoreRef,
		WorkflowStoreRef: attached.WorkflowStoreRef, AdmissionReceipt: attached.AdmissionReceipt,
	}
	if lineage.State == "attached" {
		marker.WorkflowID = workflowID
	}
	encoded, err := encodeLifecycleMaterialization(marker)
	if err != nil {
		return ""
	}
	return encoded
}

func sameLifecycleLineage(a, b lifecycleMaterialization) bool {
	return a.Version == b.Version && a.Scope == b.Scope && a.Contract == b.Contract && a.Route == b.Route &&
		a.Workflow == b.Workflow && a.MergeStrategy == b.MergeStrategy && a.Token == b.Token &&
		a.SourceID == b.SourceID && a.SourceStoreRef == b.SourceStoreRef &&
		a.WorkflowStoreRef == b.WorkflowStoreRef && a.AdmissionReceipt == b.AdmissionReceipt
}

func lifecycleRowHeld(row beads.Bead) bool {
	for _, label := range row.Labels {
		for _, hold := range beadmeta.DispatchHoldLabels {
			if strings.EqualFold(strings.TrimSpace(label), hold) {
				return true
			}
		}
	}
	return false
}

// lifecycleAuthoritativeCandidate reloads an enrolled work row from the exact
// store provenance emitted by gc ready and revalidates its admission or graph
// lineage against current source/root rows. The work-query object is only a
// locator; it never authorizes a claim.
func lifecycleAuthoritativeCandidate(candidate beads.Bead, opts hookClaimOptions) (beads.Store, beads.Bead, bool) {
	if !lifecycleEnrollmentEvidence(candidate) || !opts.Lifecycle.AdmissionEnabled ||
		!opts.TrustedLifecycleScope || opts.ResolveLifecycleStore == nil ||
		strings.TrimSpace(candidate.SourceStoreRef) == "" {
		return nil, beads.Bead{}, false
	}
	store, err := opts.ResolveLifecycleStore(candidate.SourceStoreRef)
	if err != nil || store == nil {
		return nil, beads.Bead{}, false
	}
	current, err := store.Get(candidate.ID)
	if err != nil || current.ID != candidate.ID {
		return nil, beads.Bead{}, false
	}
	current.SourceStoreRef = candidate.SourceStoreRef
	current.LifecycleScope = candidate.LifecycleScope
	if strings.TrimSpace(current.LifecycleScope) == "" {
		return nil, beads.Bead{}, false
	}
	if filtered := filterHookLifecycleCandidates([]beads.Bead{current}, opts, io.Discard); len(filtered) != 1 {
		return nil, beads.Bead{}, false
	}
	if lifecycleRowHeld(current) || beads.IsDeferred(current, time.Now()) {
		return nil, beads.Bead{}, false
	}
	status := strings.ToLower(strings.TrimSpace(current.Status))
	if status != "open" && status != "in_progress" {
		return nil, beads.Bead{}, false
	}
	return store, current, true
}

// lifecycleConditionalClaim is the only claim writer for enrolled work. It
// fences the exact row revision and re-reads both the work row and its signed
// source/root lineage after the write. Stores without real revision CAS hold
// lifecycle work; the legacy `bd update --claim` path is never a fallback.
func lifecycleConditionalClaim(candidate beads.Bead, actor string, readyAssignment bool, opts hookClaimOptions, compatibilityChecks ...formulaActionCandidateCheck) (beads.Bead, bool, error) {
	store, current, ok := lifecycleAuthoritativeCandidate(candidate, opts)
	if !ok {
		return beads.Bead{}, false, nil
	}
	actor = strings.TrimSpace(actor)
	if actor == "" || !hookClaimHasIdentity(actor, opts.IdentityCandidates) || current.Revision == 0 {
		return beads.Bead{}, false, nil
	}
	status := strings.ToLower(strings.TrimSpace(current.Status))
	if status != "open" {
		return beads.Bead{}, false, nil
	}
	if len(compatibilityChecks) > 0 && compatibilityChecks[0] != nil {
		state, err := compatibilityChecks[0](context.Background(), current)
		if err != nil {
			return beads.Bead{}, false, fmt.Errorf("validating formula compatibility before lifecycle claim: %w", err)
		}
		if state.Bead.ID != current.ID || state.Bead.Revision != current.Revision ||
			state.Bead.SourceStoreRef != current.SourceStoreRef {
			return beads.Bead{}, false, fmt.Errorf("formula compatibility candidate changed before lifecycle claim")
		}
	}
	owner := strings.TrimSpace(current.Assignee)
	if readyAssignment {
		if owner == "" || !hookClaimHasIdentity(owner, opts.IdentityCandidates) || owner != actor {
			return beads.Bead{}, false, nil
		}
	} else if owner != "" {
		return beads.Bead{}, false, nil
	}
	writer, supported := beads.ConditionalWriterFor(store)
	if !supported || !beads.InspectConditionalWrites(store).Capable {
		return beads.Bead{}, false, beads.ErrConditionalWriteUnsupported
	}
	if err := writer.UpdateIfMatch(current.ID, current.Revision, beads.UpdateOpts{
		Status:   stringPtr("in_progress"),
		Assignee: stringPtr(actor),
	}); err != nil {
		return beads.Bead{}, false, err
	}
	claimed, err := store.Get(current.ID)
	if err != nil {
		return beads.Bead{}, true, fmt.Errorf("lifecycle claim %s committed but readback failed: %w", current.ID, err)
	}
	claimed.SourceStoreRef = candidate.SourceStoreRef
	claimed.LifecycleScope = candidate.LifecycleScope
	if strings.TrimSpace(claimed.Assignee) != actor || !strings.EqualFold(strings.TrimSpace(claimed.Status), "in_progress") {
		return claimed, true, fmt.Errorf("lifecycle claim %s readback does not show in_progress owned by %s", current.ID, actor)
	}
	if _, _, stillEligible := lifecycleAuthoritativeCandidate(claimed, opts); !stillEligible {
		return claimed, true, fmt.Errorf("lifecycle claim %s lost current admission or lineage after its fenced write", current.ID)
	}
	return claimed, true, nil
}

func hookLifecycleRouteMatches(cfg *config.City, route string, candidates []string) bool {
	if cfg == nil || strings.TrimSpace(route) == "" {
		return false
	}
	permitted := agentutil.NormalizePoolRouteTarget(cfg, route)
	for _, target := range candidates {
		if agentutil.NormalizePoolRouteTarget(cfg, target) == permitted {
			return true
		}
	}
	return false
}
