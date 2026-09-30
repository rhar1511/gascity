package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/attemptevidence"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/qualification"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
	"github.com/gastownhall/gascity/internal/worker"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

// reconcileLifecycleRecoveryRequests scans only held, controller-owned intent
// beads. A signed request is inert until it matches the exact admitted source,
// workflow, owner, session generation, and reciprocal claim at the effect
// boundary. Only a successful conditional budget reservation may submit; every
// replay reads the existing receipt without re-sending.
//
//nolint:unparam // Direct callers use this fail-closed compatibility seam; the controller supplies routed stores and permit authority.
func reconcileLifecycleRecoveryRequests(
	ctx context.Context,
	cityName string,
	cityPath string,
	cfg *config.City,
	workStore beads.Store,
	rigStores map[string]beads.Store,
	sessionStore beads.SessionStore,
	provider runtime.Provider,
	stderr io.Writer,
) {
	reconcileLifecycleRecoveryRequestsWithPermitResolver(
		ctx, cityName, cityPath, cfg, workStore, rigStores, sessionStore, provider, stderr, nil, nil,
	)
}

func reconcileLifecycleRecoveryRequestsWithPermitResolver(
	ctx context.Context,
	cityName string,
	cityPath string,
	cfg *config.City,
	workStore beads.Store,
	rigStores map[string]beads.Store,
	sessionStore beads.SessionStore,
	provider runtime.Provider,
	stderr io.Writer,
	permitResolver *hostBeadsPermitResolver,
	authority qualification.CompatibilityAuthority,
	outboxes ...lifecycleRecoveryOutbox,
) {
	if cfg == nil || !cfg.Lifecycle.AdmissionEnabled || !cfg.Lifecycle.RecoveryEnabled || workStore == nil || sessionStore.Store == nil || provider == nil {
		return
	}
	if stderr == nil {
		stderr = io.Discard
	}
	var outbox *lifecycleRecoveryOutbox
	if len(outboxes) > 0 {
		outbox = &outboxes[0]
	}
	legs, err := censusStoreCandidates(cityPath, cfg, workStore, rigStores, nil, censusRefScoped)
	if err != nil {
		fmt.Fprintf(stderr, "lifecycle recovery: resolving intent stores: %v\n", err) //nolint:errcheck
		return
	}
	storesByRef := make(map[string]beads.Store, len(legs))
	for _, leg := range legs {
		if leg.store != nil {
			storesByRef[leg.ref] = leg.store
		}
	}
	front := session.NewStore(sessionStore)
	workerFactory, err := worker.NewFactory(worker.FactoryConfig{
		Store: sessionStore.Store, Provider: provider, CityPath: cityPath,
	})
	if err != nil {
		fmt.Fprintf(stderr, "lifecycle recovery: constructing worker request service: %v\n", err) //nolint:errcheck
		return
	}
	now := time.Now().UTC()
	seen := make(map[string]struct{})
	for _, leg := range legs {
		if leg.store == nil {
			continue
		}
		rows, err := leg.store.List(beads.ListQuery{
			Type:     "lifecycle-intent",
			Status:   "open",
			Live:     true,
			TierMode: beads.FederatedReadTier,
		})
		if err != nil {
			fmt.Fprintf(stderr, "lifecycle recovery: listing intents in %s: %v\n", leg.ref, err) //nolint:errcheck
			continue
		}
		scope := lifecycleScopeForRef(cityName, cfg, leg.ref)
		for _, intentBead := range rows {
			if intentBead.Type != "lifecycle-intent" || !beads.HasReadyExcludedLabel(intentBead) {
				continue
			}
			intent, err := worklifecycle.DecodeRecoveryIntent(intentBead)
			if err != nil || intent.Request.Scope != scope {
				continue
			}
			request := intent.Request
			key := scope + "\x00" + request.WorkItemID + "\x00" + request.RequestID
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			if _, err := worklifecycle.VerifyRecoveryRequestProof(request, cfg.Lifecycle); err != nil {
				fmt.Fprintf(stderr, "lifecycle recovery: intent %s has an invalid signer proof; holding\n", intentBead.ID) //nolint:errcheck
				continue
			}
			if err := verifyRecoveryIntentStore(leg.store, intentBead, intent, scope); err != nil {
				fmt.Fprintf(stderr, "lifecycle recovery: intent %s failed exact readback: %v\n", intentBead.ID, err) //nolint:errcheck
				continue
			}
			binding, err := lifecycleRecoveryAttemptBinding(request, leg.ref)
			if err != nil {
				fmt.Fprintf(stderr, "lifecycle recovery: intent %s has no verified execution-store binding: %v\n", intentBead.ID, err) //nolint:errcheck
				continue
			}
			state, attempt, reserved, err := worklifecycle.RecoveryRequestAttempt(leg.store, request, intent.Digest)
			if err != nil {
				fmt.Fprintf(stderr, "lifecycle recovery: reading reservation for %s: %v\n", request.WorkItemID, err) //nolint:errcheck
				continue
			}
			if reserved {
				recoveryObserveReceipt(front, request, binding, attempt, stderr)
				if len(state.Attempts) >= worklifecycle.MaxRecoveryAttempts {
					handleExhaustedLifecycleRecovery(cityName, cityPath, cfg, workStore, rigStores, legs, leg, state, scope,
						permitResolver, authority, outbox, stderr)
				}
				continue
			}
			if len(state.Attempts) >= worklifecycle.MaxRecoveryAttempts {
				handleExhaustedLifecycleRecovery(cityName, cityPath, cfg, workStore, rigStores, legs, leg, state, scope,
					permitResolver, authority, outbox, stderr)
				continue
			}
			if _, err := worklifecycle.VerifyRecoveryRequest(request, cfg.Lifecycle, now); err != nil {
				fmt.Fprintf(stderr, "lifecycle recovery: intent %s is expired or not yet valid; no attempt reserved\n", intentBead.ID) //nolint:errcheck
				continue
			}
			work, err := leg.store.Get(request.WorkItemID)
			if err != nil || work.Revision != request.ExpectedRevision || worklifecycle.ValidateRecoveryTargetTuple(work, request) != nil ||
				!recoveryAttemptBindingMatchesWork(binding, work) ||
				worklifecycle.ValidateRecoveryWorkEnvelopeEvidence(work, cfg.Lifecycle, scope) != nil ||
				!lifecycleRecoveryAttachedWorkflowMatches(work, leg.ref, leg.store, storesByRef, cfg, scope) {
				fmt.Fprintf(stderr, "lifecycle recovery: target %s changed or lost attached-workflow evidence; no attempt reserved\n", request.WorkItemID) //nolint:errcheck
				continue
			}
			info, err := front.Get(request.SessionID)
			if err != nil || !recoverySessionTupleMatches(front, request, work, info) || !provider.IsRunning(info.SessionName) {
				fmt.Fprintf(stderr, "lifecycle recovery: current owner/session tuple for %s is not live; no attempt reserved\n", request.WorkItemID) //nolint:errcheck
				continue
			}
			generation, err := strconv.Atoi(request.SessionGeneration)
			if err != nil || generation <= 0 {
				fmt.Fprintf(stderr, "lifecycle recovery: invalid session generation for %s\n", request.WorkItemID) //nolint:errcheck
				continue
			}
			if receipt, err := front.GetRequest(request.SessionID, request.RequestID); err == nil {
				if recoveryReceiptMatches(receipt, request, binding) {
					fmt.Fprintf(stderr, "lifecycle recovery: request %s already has a session receipt; observing without sending\n", request.RequestID) //nolint:errcheck
				} else {
					fmt.Fprintf(stderr, "lifecycle recovery: request %s conflicts with an existing session receipt; holding\n", request.RequestID) //nolint:errcheck
				}
				continue
			} else if !errors.Is(err, session.ErrRequestNotFound) {
				fmt.Fprintf(stderr, "lifecycle recovery: reading prior request receipt %s: %v\n", request.RequestID, err) //nolint:errcheck
				continue
			}
			transition, err := buildLifecycleTransitionContext(
				cityName, cityPath, cfg, workStore, rigStores, legs, leg,
				request.WorkItemID, scope, permitResolver, authority,
			)
			if err != nil || transition.source.Revision != request.ExpectedRevision || transition.head.ToVersion != request.ExpectedRevision {
				fmt.Fprintf(stderr, "lifecycle recovery: target %s has no exact current Q54 transition head; no attempt reserved: %v\n", request.WorkItemID, errors.Join(err, worklifecycle.ErrTransitionChainStale)) //nolint:errcheck
				continue
			}
			if transition.source.Revision != work.Revision || transition.source.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey] != work.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey] {
				fmt.Fprintf(stderr, "lifecycle recovery: target %s changed while rebuilding transition evidence; no attempt reserved\n", request.WorkItemID) //nolint:errcheck
				continue
			}
			reservedAt := time.Now().UTC().Format(time.RFC3339Nano)
			newAttempt := worklifecycle.RecoveryAttempt{
				ID: lifecycleRecoveryAttemptID(request, intent.Digest), ReservedAt: reservedAt,
				RequestID: request.RequestID, RequestDigest: intent.Digest, ExpectedRevision: request.ExpectedRevision,
			}
			nextState := state
			nextState.Attempts = append(append([]worklifecycle.RecoveryAttempt(nil), state.Attempts...), newAttempt)
			encodedState, err := json.Marshal(nextState)
			if err != nil {
				fmt.Fprintf(stderr, "lifecycle recovery: encoding budget transition for %s: %v\n", request.WorkItemID, err) //nolint:errcheck
				continue
			}
			recoveryStatePatch := worklifecycle.MetadataStringPatch{Value: string(encodedState)}
			previousState := transition.source.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey]
			if previousState != "" {
				recoveryStatePatch.Expected = &previousState
			}
			operationID, err := worklifecycle.RecoveryBudgetOperationID(request.RequestID, intent.Digest)
			if err != nil {
				fmt.Fprintf(stderr, "lifecycle recovery: deriving request-bound transition identity for %s: %v\n", request.WorkItemID, err) //nolint:errcheck
				continue
			}
			budgetResult, err := transition.chain.Apply(worklifecycle.TransitionRequest{
				IssueID: request.WorkItemID, Step: worklifecycle.TransitionStepRecoveryBudget,
				OperationID: operationID, PriorReceiptID: transition.head.ReceiptID,
				Evidence: transition.evidence, RecoveryRequest: &request,
				Patch: worklifecycle.SourceWorkPatch{Metadata: map[string]worklifecycle.MetadataStringPatch{
					beadmeta.LifecycleRecoveryStateMetadataKey: recoveryStatePatch,
				}},
			})
			if err != nil {
				fmt.Fprintf(stderr, "lifecycle recovery: cannot reserve request %s through Q54: %v\n", request.RequestID, err) //nolint:errcheck
				continue
			}
			if budgetResult.Replayed || budgetResult.Recovered {
				observedState, observedAttempt, found, observeErr := worklifecycle.RecoveryRequestAttempt(leg.store, request, intent.Digest)
				switch {
				case observeErr != nil:
					fmt.Fprintf(stderr, "lifecycle recovery: reading replayed request %s: %v\n", request.RequestID, observeErr) //nolint:errcheck
				case found:
					recoveryObserveReceipt(front, request, binding, observedAttempt, stderr)
					if len(observedState.Attempts) >= worklifecycle.MaxRecoveryAttempts {
						handleExhaustedLifecycleRecovery(cityName, cityPath, cfg, workStore, rigStores, legs, leg, observedState, scope,
							permitResolver, authority, outbox, stderr)
					}
				default:
					fmt.Fprintf(stderr, "lifecycle recovery: request %s transition outcome remains unknown; it will not be replayed\n", request.RequestID) //nolint:errcheck
				}
				continue
			}
			if err := ctx.Err(); err != nil {
				fmt.Fprintf(stderr, "lifecycle recovery: reserved request %s has unknown outcome after tick cancellation; it will not be replayed\n", request.RequestID) //nolint:errcheck
				continue
			}
			currentHead, headErr := transition.chain.CurrentHead(request.WorkItemID, transition.evidence)
			reservedWork, readErr := leg.store.Get(request.WorkItemID)
			if headErr != nil || readErr != nil || budgetResult.Receipt.ReceiptID == "" ||
				currentHead.ReceiptID != budgetResult.Receipt.ReceiptID || currentHead.ToVersion != reservedWork.Revision {
				fmt.Fprintf(stderr, "lifecycle recovery: reserved request %s lacks its exact current Q54 receipt; slot remains consumed (%v)\n", request.RequestID, errors.Join(headErr, readErr, worklifecycle.ErrTransitionChainReceipt)) //nolint:errcheck
				continue
			}
			if !recoveryEffectRecheck(leg.ref, storesByRef, leg.store, front, provider, work, request, binding, cfg, scope,
				currentHead.ToVersion, currentHead.ReceiptID, transition.chain, transition.evidence, time.Now().UTC()) {
				fmt.Fprintf(stderr, "lifecycle recovery: reserved request %s lost a prerequisite before delivery; slot remains consumed\n", request.RequestID) //nolint:errcheck
				continue
			}
			_, sendErr := workerFactory.SubmitRequestForAttempt(ctx, request.SessionID, request.RequestID, generation, request.Message, binding)
			readback, readErr := front.GetRequest(request.SessionID, request.RequestID)
			if readErr == nil && recoveryReceiptMatches(readback, request, binding) {
				fmt.Fprintf(stderr, "lifecycle recovery: request %s provider delivery=%s acknowledged=%t effect=%s (not useful-progress evidence)\n", request.RequestID, readback.Delivery, readback.AcknowledgedAt != nil, readback.Effect) //nolint:errcheck
			} else {
				fmt.Fprintf(stderr, "lifecycle recovery: request %s outcome unknown (submit=%v, receipt=%v); reservation prevents replay\n", request.RequestID, sendErr, readErr) //nolint:errcheck
			}
			if sendErr != nil {
				fmt.Fprintf(stderr, "lifecycle recovery: request %s submit returned: %v\n", request.RequestID, sendErr) //nolint:errcheck
			}
			if len(nextState.Attempts) >= worklifecycle.MaxRecoveryAttempts {
				handleExhaustedLifecycleRecovery(cityName, cityPath, cfg, workStore, rigStores, legs, leg, nextState, scope,
					permitResolver, authority, outbox, stderr)
			}
		}
	}
}

func verifyRecoveryIntentStore(store beads.Store, bead beads.Bead, intent worklifecycle.RecoveryIntent, scope string) error {
	current, err := store.Get(bead.ID)
	if err != nil {
		return err
	}
	decoded, err := worklifecycle.DecodeRecoveryIntent(current)
	if err != nil || current.ID != bead.ID || decoded.Digest != intent.Digest || decoded.Request != intent.Request || intent.Request.Scope != scope {
		return worklifecycle.ErrRecoveryRequestConflict
	}
	return nil
}

func lifecycleRecoveryAttemptBinding(request worklifecycle.RecoveryRequest, physicalStoreRef string) (session.RequestAttemptBinding, error) {
	physicalStoreRef = strings.TrimSpace(physicalStoreRef)
	if !strings.HasPrefix(physicalStoreRef, "city:") && !strings.HasPrefix(physicalStoreRef, "rig:") {
		return session.RequestAttemptBinding{}, fmt.Errorf("unsupported physical work-store reference %q", physicalStoreRef)
	}
	if request.ExpectedRevision == 0 {
		return session.RequestAttemptBinding{}, worklifecycle.ErrRecoveryRequestInvalid
	}
	identity := attemptevidence.Identity{
		Kind: attemptevidence.KindWorkbench, OwnerBeadID: request.WorkItemID,
		ExecutionBeadID: request.WorkItemID, SessionID: request.SessionID,
		SessionGeneration: request.SessionGeneration, ClaimGeneration: request.ClaimGeneration,
	}
	attemptID, err := attemptevidence.AttemptID(identity)
	if err != nil {
		return session.RequestAttemptBinding{}, fmt.Errorf("derive Workbench attempt identity: %w", err)
	}
	return session.RequestAttemptBinding{
		StoreRef: physicalStoreRef, AttemptID: attemptID,
		WorkRevision: strconv.FormatInt(request.ExpectedRevision, 10), Identity: identity,
	}, nil
}

func recoveryAttemptBindingMatchesWork(binding session.RequestAttemptBinding, work beads.Bead) bool {
	identity := binding.Identity
	return attemptevidence.IsExecutionRecord(work) && work.ID == identity.OwnerBeadID && work.ID == identity.ExecutionBeadID &&
		strings.TrimSpace(work.Metadata[beadmeta.SessionIDMetadataKey]) == identity.SessionID &&
		strings.TrimSpace(work.Metadata[beadmeta.ClaimGenerationMetadataKey]) == identity.ClaimGeneration
}

func lifecycleRecoveryAttachedWorkflowMatches(work beads.Bead, sourceRef string, sourceStore beads.Store, storesByRef map[string]beads.Store, cfg *config.City, scope string) bool {
	if cfg == nil || sourceStore == nil {
		return false
	}
	marker, ok := lifecycleMaterializationFor(work)
	if !ok || marker.State != "attached" || marker.SourceID != work.ID || marker.SourceStoreRef != sourceRef || marker.Scope != scope ||
		!lifecycleRoutesMatch(cfg, work, marker.Route) || strings.TrimSpace(work.Metadata[beadmeta.MergeStrategyMetadataKey]) != marker.MergeStrategy {
		return false
	}
	admission, err := worklifecycle.VerifyAdmissionReceiptV2(work, cfg.Lifecycle, scope)
	digest, digestErr := worklifecycle.AdmissionDigestV2(admission)
	if err != nil || digestErr != nil || digest != marker.Contract || marker.Route != admission.Route || marker.Workflow != admission.Workflow ||
		marker.MergeStrategy != admission.MergeStrategy || marker.AdmissionReceipt != work.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey] {
		return false
	}
	workflowStore := storesByRef[marker.WorkflowStoreRef]
	if workflowStore == nil {
		return false
	}
	root, err := workflowStore.Get(marker.WorkflowID)
	if err != nil || root.Status == "closed" || strings.TrimSpace(root.Metadata[beadmeta.FormulaNameMetadataKey]) != marker.Workflow {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(root.Metadata[beadmeta.FormulaContractMetadataKey]), beadmeta.FormulaContractGraphV2) {
		rootMarker, rootMarkerOK := lifecycleMaterializationFor(root)
		if !sourceworkflow.IsWorkflowRoot(root) || !rootMarkerOK || rootMarker.State != "attached" || rootMarker.SourceID != work.ID ||
			rootMarker.SourceStoreRef != sourceRef || rootMarker.WorkflowStoreRef != marker.WorkflowStoreRef || rootMarker.Contract != digest ||
			!lifecycleRoutesMatch(cfg, root, marker.Route) || strings.TrimSpace(root.Metadata[beadmeta.MergeStrategyMetadataKey]) != marker.MergeStrategy {
			return false
		}
		inputConvoy := strings.TrimSpace(root.Metadata[beadmeta.InputConvoyIDMetadataKey])
		if inputConvoy == "" {
			return false
		}
		members, err := convoycore.Members(sourceStore, inputConvoy, false)
		if err != nil || !containsBeadID(members, work.ID) {
			return false
		}
		return true
	}
	return strings.TrimSpace(work.Metadata[beadmeta.MoleculeIDMetadataKey]) == marker.WorkflowID ||
		strings.TrimSpace(work.Metadata[beadmeta.WorkflowIDMetadataKey]) == marker.WorkflowID ||
		strings.TrimSpace(work.Metadata[beadmeta.LegacyWorkflowIDMetadataKey]) == marker.WorkflowID
}

func containsBeadID(beadsList []beads.Bead, id string) bool {
	for _, bead := range beadsList {
		if bead.ID == id {
			return true
		}
	}
	return false
}

func recoverySessionTupleMatches(front *session.Store, request worklifecycle.RecoveryRequest, work beads.Bead, info session.Info) bool {
	if front == nil || info.ID != request.SessionID || info.Closed || info.Generation != request.SessionGeneration ||
		work.Assignee != request.Owner || work.Metadata[beadmeta.SessionIDMetadataKey] != request.SessionID ||
		work.Metadata[beadmeta.ClaimGenerationMetadataKey] != request.ClaimGeneration {
		return false
	}
	claim, err := front.CurrentClaimBeadID(request.SessionID)
	return err == nil && claim == work.ID
}

func recoveryEffectRecheck(sourceRef string, storesByRef map[string]beads.Store, store beads.Store, front *session.Store, provider runtime.Provider, previous beads.Bead,
	request worklifecycle.RecoveryRequest, binding session.RequestAttemptBinding, cfg *config.City, scope string,
	reservedRevision int64, reservedHeadID string, chain *worklifecycle.TransitionChain,
	evidence worklifecycle.TransitionEvidence, now time.Time,
) bool {
	if store == nil || front == nil || provider == nil || chain == nil || reservedHeadID == "" || now.IsZero() {
		return false
	}
	if _, err := worklifecycle.VerifyRecoveryRequest(request, cfg.Lifecycle, now); err != nil {
		return false
	}
	current, err := store.Get(request.WorkItemID)
	if err != nil || current.Revision != reservedRevision || worklifecycle.ValidateRecoveryTargetTuple(current, request) != nil ||
		!recoveryAttemptBindingMatchesWork(binding, current) ||
		worklifecycle.ValidateRecoveryWorkEnvelopeEvidence(current, cfg.Lifecycle, scope) != nil ||
		!lifecycleRecoveryAttachedWorkflowMatches(current, sourceRef, store, storesByRef, cfg, scope) {
		return false
	}
	head, err := chain.CurrentHead(request.WorkItemID, evidence)
	if err != nil || head.ReceiptID != reservedHeadID || head.ToVersion != reservedRevision {
		return false
	}
	info, err := front.Get(request.SessionID)
	return err == nil && recoverySessionTupleMatches(front, request, current, info) &&
		(info.State == session.StateActive || info.State == session.StateAwake) && provider.IsRunning(info.SessionName) &&
		previous.ID == current.ID
}

func recoveryReceiptMatches(receipt session.RequestReceipt, request worklifecycle.RecoveryRequest, binding session.RequestAttemptBinding) bool {
	digest := sha256.Sum256([]byte(request.Message))
	return receipt.RequestID == request.RequestID && receipt.SessionID == request.SessionID &&
		strconv.Itoa(receipt.Generation) == request.SessionGeneration && receipt.MessageDigest == hex.EncodeToString(digest[:]) &&
		receipt.Attempt != nil && *receipt.Attempt == binding
}

func recoveryObserveReceipt(front *session.Store, request worklifecycle.RecoveryRequest, binding session.RequestAttemptBinding, attempt worklifecycle.RecoveryAttempt, stderr io.Writer) {
	if front == nil {
		return
	}
	receipt, err := front.GetRequest(request.SessionID, request.RequestID)
	if err != nil {
		fmt.Fprintf(stderr, "lifecycle recovery: reserved request %s has no readable receipt; outcome remains unknown and is not replayed (reservation %s)\n", request.RequestID, attempt.ID) //nolint:errcheck
		return
	}
	if !recoveryReceiptMatches(receipt, request, binding) {
		fmt.Fprintf(stderr, "lifecycle recovery: reserved request %s receipt does not match its signed tuple; holding\n", request.RequestID) //nolint:errcheck
		return
	}
	fmt.Fprintf(stderr, "lifecycle recovery: request %s provider delivery=%s acknowledged=%t effect=%s; replay observed without sending\n", request.RequestID, receipt.Delivery, receipt.AcknowledgedAt != nil, receipt.Effect) //nolint:errcheck
}

func lifecycleRecoveryAttemptID(request worklifecycle.RecoveryRequest, digest string) string {
	material := "gascity.lifecycle.recovery-attempt.v1\n" + request.Scope + "\x00" + request.WorkItemID + "\x00" + request.RequestID + "\x00" + digest
	derived := sha256.Sum256([]byte(material))
	return hex.EncodeToString(derived[:16])
}

func handleExhaustedLifecycleRecovery(
	cityName, cityPath string,
	cfg *config.City,
	cityStore beads.Store,
	rigStores map[string]beads.Store,
	legs []classStoreCandidate,
	leg classStoreCandidate,
	state worklifecycle.RecoveryState,
	scope string,
	permitResolver *hostBeadsPermitResolver,
	authority qualification.CompatibilityAuthority,
	outbox *lifecycleRecoveryOutbox,
	stderr io.Writer,
) {
	if state.Escalation == nil || state.Escalation.Request == nil {
		fmt.Fprintf(stderr, "lifecycle recovery: automatic budget exhausted for %s; signed escalation authorization is unavailable, so the admitted work remains held\n", state.WorkItemID) //nolint:errcheck
		return
	}
	transition, err := buildLifecycleTransitionContext(cityName, cityPath, cfg, cityStore, rigStores, legs, leg, state.WorkItemID, scope, permitResolver, authority)
	if err != nil {
		fmt.Fprintf(stderr, "lifecycle recovery: signed escalation for %s has no exact current Q54 head; holding: %v\n", state.WorkItemID, err) //nolint:errcheck
		return
	}
	if _, err := worklifecycle.VerifyRecoveryEscalationRequestProof(*state.Escalation.Request, cfg.Lifecycle); err != nil {
		fmt.Fprintf(stderr, "lifecycle recovery: escalation for %s has no valid signed authority proof; holding: %v\n", state.WorkItemID, err) //nolint:errcheck
		return
	}
	if transition.source.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey] == "" ||
		transition.head.FromAttachment || transition.head.ReceiptID == "" {
		fmt.Fprintf(stderr, "lifecycle recovery: escalation for %s is not rooted in an exact Q54 transition chain; holding\n", state.WorkItemID) //nolint:errcheck
		return
	}
	deliverLifecycleRecoveryEscalation(state, scope, outbox, stderr)
}

type lifecycleRecoveryOutbox struct {
	store        beads.Store
	sessionStore beads.Store
	providerName string
}

func deliverLifecycleRecoveryEscalation(state worklifecycle.RecoveryState, scope string, outbox *lifecycleRecoveryOutbox, stderr io.Writer) {
	if state.Escalation == nil || state.Escalation.Request == nil {
		return
	}
	if outbox == nil {
		return
	}
	if outbox.store == nil {
		fmt.Fprintf(stderr, "lifecycle recovery: escalation request %s remains pending; message store is unavailable\n", state.Escalation.ID) //nolint:errcheck
		return
	}
	messageID, ok := recoveryEscalationMessageID(outbox.store, state.Escalation.DedupKey)
	if !ok {
		fmt.Fprintf(stderr, "lifecycle recovery: escalation request %s remains pending; stable message IDs are unsupported by the configured store\n", state.Escalation.ID) //nolint:errcheck
		return
	}
	provider := newMailProviderNamedWithSessionStore(outbox.providerName, outbox.store, outbox.sessionStore, false)
	sender, ok := provider.(mail.StableIDSender)
	if !ok {
		fmt.Fprintf(stderr, "lifecycle recovery: escalation request %s remains pending; configured mail provider has no stable outbox capability\n", state.Escalation.ID) //nolint:errcheck
		return
	}
	workID := state.WorkItemID
	target := state.Escalation.Target
	subject := "Recovery attention requested for " + workID
	body := fmt.Sprintf("Authorized recovery attempts are exhausted.\nScope: %s\nWork item: %s\nEscalation request: %s\nAttempts reserved: %d\nRequested at: %s\n", scope, workID, state.Escalation.ID, len(state.Attempts), state.Escalation.RequestedAt)
	message, createdMessage, err := sender.SendStableID(messageID, "gc-controller", target, subject, body, state.Escalation.DedupKey)
	if err != nil {
		fmt.Fprintf(stderr, "lifecycle recovery: escalation request %s remains pending; stable outbox write failed: %v\n", state.Escalation.ID, err) //nolint:errcheck
		return
	}
	fmt.Fprintf(stderr, "lifecycle recovery: escalation request %s has durable mailbox record %s (created=%t; recipient acknowledgement unverified)\n", state.Escalation.ID, message.ID, createdMessage) //nolint:errcheck
}

func recoveryEscalationMessageID(store beads.Store, dedupKey string) (string, bool) {
	prefix, ok := beads.StableCreateIDPrefixFor(store)
	if !ok || strings.TrimSpace(dedupKey) == "" {
		return "", false
	}
	digest := sha256.Sum256([]byte(dedupKey))
	return prefix + "-lifecycle-" + hex.EncodeToString(digest[:16]), true
}
