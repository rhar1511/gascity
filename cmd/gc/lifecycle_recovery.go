package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

// reconcileLifecycleRecoveryRequests scans only held, controller-owned intent
// beads. A signed request is inert until it matches the exact admitted source,
// workflow, owner, session generation, and reciprocal claim at the effect
// boundary. Only a successful conditional budget reservation may submit; every
// replay reads the existing receipt without re-sending.
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
	manager := session.NewManagerWithOptions(sessionStore.Store, provider, session.WithCityPath(cityPath))
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
			state, attempt, reserved, err := worklifecycle.RecoveryRequestAttempt(leg.store, request, intent.Digest)
			if err != nil {
				fmt.Fprintf(stderr, "lifecycle recovery: reading reservation for %s: %v\n", request.WorkItemID, err) //nolint:errcheck
				continue
			}
			if reserved {
				recoveryObserveReceipt(front, request, attempt, stderr)
				if len(state.Attempts) >= worklifecycle.MaxRecoveryAttempts {
					requestRecoveryEscalation(leg.store, request.WorkItemID, scope, cfg.Lifecycle.EscalationTarget, outbox, stderr)
				}
				continue
			}
			if len(state.Attempts) >= worklifecycle.MaxRecoveryAttempts {
				requestRecoveryEscalation(leg.store, request.WorkItemID, scope, cfg.Lifecycle.EscalationTarget, outbox, stderr)
				continue
			}
			if _, err := worklifecycle.VerifyRecoveryRequest(request, cfg.Lifecycle, now); err != nil {
				fmt.Fprintf(stderr, "lifecycle recovery: intent %s is expired or not yet valid; no attempt reserved\n", intentBead.ID) //nolint:errcheck
				continue
			}
			work, err := leg.store.Get(request.WorkItemID)
			if err != nil || work.Revision != request.ExpectedRevision || worklifecycle.ValidateRecoveryTargetTuple(work, request) != nil ||
				worklifecycle.ValidateRecoveryWorkEvidence(work, cfg.Lifecycle, scope) != nil ||
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
				if recoveryReceiptMatches(receipt, request) {
					fmt.Fprintf(stderr, "lifecycle recovery: request %s already has a session receipt; observing without sending\n", request.RequestID) //nolint:errcheck
				} else {
					fmt.Fprintf(stderr, "lifecycle recovery: request %s conflicts with an existing session receipt; holding\n", request.RequestID) //nolint:errcheck
				}
				continue
			} else if !errors.Is(err, session.ErrRequestNotFound) {
				fmt.Fprintf(stderr, "lifecycle recovery: reading prior request receipt %s: %v\n", request.RequestID, err) //nolint:errcheck
				continue
			}
			reservedState, won, reservedRevision, err := worklifecycle.ReserveRecoveryRequestAttemptWithFence(leg.store, request, intent.Digest)
			if err != nil {
				if beads.IsPreconditionFailed(err) || strings.Contains(strings.ToLower(err.Error()), "stale") {
					fmt.Fprintf(stderr, "lifecycle recovery: target %s changed before reservation; no action\n", request.WorkItemID) //nolint:errcheck
				} else {
					fmt.Fprintf(stderr, "lifecycle recovery: cannot reserve attempt for %s: %v\n", request.WorkItemID, err) //nolint:errcheck
				}
				continue
			}
			if !won {
				if attempt, ok, findErr := recoveryAttemptFromState(reservedState, request.RequestID, intent.Digest); findErr == nil && ok {
					recoveryObserveReceipt(front, request, attempt, stderr)
				} else if findErr != nil {
					fmt.Fprintf(stderr, "lifecycle recovery: reservation state conflicts for %s: %v\n", request.WorkItemID, findErr) //nolint:errcheck
				}
				continue
			}
			if err := ctx.Err(); err != nil {
				fmt.Fprintf(stderr, "lifecycle recovery: reserved request %s has unknown outcome after tick cancellation; it will not be replayed\n", request.RequestID) //nolint:errcheck
				continue
			}
			if !recoveryEffectRecheck(leg.store, leg.ref, storesByRef, front, provider, work, request, cfg, scope, reservedRevision, time.Now().UTC()) {
				fmt.Fprintf(stderr, "lifecycle recovery: reserved request %s lost a prerequisite before delivery; slot remains consumed\n", request.RequestID) //nolint:errcheck
				continue
			}
			_, sendErr := manager.SubmitRequest(ctx, request.SessionID, request.RequestID, generation, request.Message)
			readback, readErr := front.GetRequest(request.SessionID, request.RequestID)
			if readErr == nil && recoveryReceiptMatches(readback, request) {
				fmt.Fprintf(stderr, "lifecycle recovery: request %s provider delivery=%s acknowledged=%t effect=%s (not useful-progress evidence)\n", request.RequestID, readback.Delivery, readback.AcknowledgedAt != nil, readback.Effect) //nolint:errcheck
			} else {
				fmt.Fprintf(stderr, "lifecycle recovery: request %s outcome unknown (submit=%v, receipt=%v); reservation prevents replay\n", request.RequestID, sendErr, readErr) //nolint:errcheck
			}
			if sendErr != nil {
				fmt.Fprintf(stderr, "lifecycle recovery: request %s submit returned: %v\n", request.RequestID, sendErr) //nolint:errcheck
			}
			if len(reservedState.Attempts) >= worklifecycle.MaxRecoveryAttempts {
				requestRecoveryEscalation(leg.store, request.WorkItemID, scope, cfg.Lifecycle.EscalationTarget, outbox, stderr)
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

func lifecycleRecoveryAttachedWorkflowMatches(work beads.Bead, sourceRef string, sourceStore beads.Store, storesByRef map[string]beads.Store, cfg *config.City, scope string) bool {
	if cfg == nil || sourceStore == nil {
		return false
	}
	marker, ok := lifecycleMaterializationFor(work)
	if !ok || marker.State != "attached" || marker.SourceID != work.ID || marker.SourceStoreRef != sourceRef || marker.Scope != scope ||
		!lifecycleRoutesMatch(cfg, work, marker.Route) || strings.TrimSpace(work.Metadata[beadmeta.MergeStrategyMetadataKey]) != marker.MergeStrategy {
		return false
	}
	decision := worklifecycle.EvaluateAdmission(work, cfg.Lifecycle, scope)
	digest, err := worklifecycle.AdmissionDigest(decision.Receipt)
	if err != nil || !decision.Admitted || digest != marker.Contract || marker.Workflow != decision.Receipt.Workflow ||
		marker.MergeStrategy != decision.Receipt.MergeStrategy || marker.AdmissionReceipt != work.Metadata[beadmeta.LifecycleAdmissionReceiptMetadataKey] {
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
	return strings.TrimSpace(work.Metadata[beadmeta.MoleculeIDMetadataKey]) == marker.WorkflowID || strings.TrimSpace(work.Metadata[beadmeta.WorkflowIDMetadataKey]) == marker.WorkflowID
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

func recoveryEffectRecheck(store beads.Store, sourceRef string, storesByRef map[string]beads.Store, front *session.Store, provider runtime.Provider, previous beads.Bead, request worklifecycle.RecoveryRequest, cfg *config.City, scope string, reservedRevision int64, now time.Time) bool {
	if store == nil || front == nil || provider == nil || now.IsZero() {
		return false
	}
	if _, err := worklifecycle.VerifyRecoveryRequest(request, cfg.Lifecycle, now); err != nil {
		return false
	}
	current, err := store.Get(request.WorkItemID)
	if err != nil || current.Revision != reservedRevision || worklifecycle.ValidateRecoveryTargetTuple(current, request) != nil ||
		worklifecycle.ValidateRecoveryWorkEvidence(current, cfg.Lifecycle, scope) != nil ||
		!lifecycleRecoveryAttachedWorkflowMatches(current, sourceRef, store, storesByRef, cfg, scope) {
		return false
	}
	info, err := front.Get(request.SessionID)
	return err == nil && recoverySessionTupleMatches(front, request, current, info) &&
		(info.State == session.StateActive || info.State == session.StateAwake) && provider.IsRunning(info.SessionName) &&
		previous.ID == current.ID
}

func recoveryReceiptMatches(receipt session.RequestReceipt, request worklifecycle.RecoveryRequest) bool {
	digest := sha256.Sum256([]byte(request.Message))
	return receipt.RequestID == request.RequestID && receipt.SessionID == request.SessionID &&
		strconv.Itoa(receipt.Generation) == request.SessionGeneration && receipt.MessageDigest == hex.EncodeToString(digest[:])
}

func recoveryObserveReceipt(front *session.Store, request worklifecycle.RecoveryRequest, attempt worklifecycle.RecoveryAttempt, stderr io.Writer) {
	if front == nil {
		return
	}
	receipt, err := front.GetRequest(request.SessionID, request.RequestID)
	if err != nil {
		fmt.Fprintf(stderr, "lifecycle recovery: reserved request %s has no readable receipt; outcome remains unknown and is not replayed (reservation %s)\n", request.RequestID, attempt.ID) //nolint:errcheck
		return
	}
	if !recoveryReceiptMatches(receipt, request) {
		fmt.Fprintf(stderr, "lifecycle recovery: reserved request %s receipt does not match its signed tuple; holding\n", request.RequestID) //nolint:errcheck
		return
	}
	fmt.Fprintf(stderr, "lifecycle recovery: request %s provider delivery=%s acknowledged=%t effect=%s; replay observed without sending\n", request.RequestID, receipt.Delivery, receipt.AcknowledgedAt != nil, receipt.Effect) //nolint:errcheck
}

func recoveryAttemptFromState(state worklifecycle.RecoveryState, requestID, digest string) (worklifecycle.RecoveryAttempt, bool, error) {
	for _, attempt := range state.Attempts {
		if attempt.RequestID != requestID {
			continue
		}
		if attempt.RequestDigest != digest {
			return worklifecycle.RecoveryAttempt{}, false, worklifecycle.ErrRecoveryRequestConflict
		}
		return attempt, true, nil
	}
	return worklifecycle.RecoveryAttempt{}, false, nil
}

type lifecycleRecoveryOutbox struct {
	store        beads.Store
	sessionStore beads.Store
	providerName string
}

func requestRecoveryEscalation(store beads.Store, workID, scope, target string, outbox *lifecycleRecoveryOutbox, stderr io.Writer) {
	state, created, err := worklifecycle.RequestRecoveryEscalation(store, workID, scope, target)
	if err != nil {
		fmt.Fprintf(stderr, "lifecycle recovery: recording escalation request for %s: %v\n", workID, err) //nolint:errcheck
		return
	}
	if state.Escalation == nil {
		return
	}
	if created {
		fmt.Fprintf(stderr, "lifecycle recovery: durable escalation request %s is pending delivery for %s\n", state.Escalation.ID, workID) //nolint:errcheck
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
