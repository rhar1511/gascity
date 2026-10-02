package worklifecycle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

const claimOperationDomain = "gascity.lifecycle.claim_identity.operation.v1\n"

// TransitionReceiptID returns the deterministic durable ID for one lifecycle
// transition operation. It is exposed so narrow controller adapters can find
// the exact receipt needed to repair an interrupted reciprocal write.
func TransitionReceiptID(issueID, scope string, step TransitionStep, operationID string) (string, error) {
	return transitionReceiptID(issueID, scope, step, operationID)
}

// ClaimIdentityRequest is the controller-authorized input for an enrolled
// work claim. The caller must derive Actor from the authoritative session
// record before constructing the TransitionChain. InstanceToken and
// RuntimeEpoch bind retries to one authenticated runtime incarnation.
type ClaimIdentityRequest struct {
	IssueID                string
	SessionID              string
	InstanceToken          string
	RuntimeEpoch           string
	ExpectedRevision       int64
	ExpectedTransitionHead string
	SessionName            string
	WorkDir                string
	WorkBranch             string
	Evidence               TransitionEvidence
}

// ClaimIdentityResult reports the durable claim transition and the generation
// that must be stamped reciprocally on the authenticated session bead.
type ClaimIdentityResult struct {
	Receipt         beads.RevisionTransitionPatchReceipt
	ClaimGeneration string
	Replayed        bool
	Recovered       bool
}

// NextClaimGeneration advances an absent/empty generation to 1 and a
// canonical positive int64 generation by one. Malformed and exhausted values
// fail closed so distinct attempts cannot reuse or wrap an identity.
func NextClaimGeneration(previous string) (string, error) {
	if previous == "" {
		return "1", nil
	}
	if strings.TrimSpace(previous) != previous {
		return "", ErrTransitionChainInvalid
	}
	generation, err := strconv.ParseInt(previous, 10, 64)
	if err != nil || generation <= 0 || strconv.FormatInt(generation, 10) != previous || generation == math.MaxInt64 {
		return "", ErrTransitionChainInvalid
	}
	return strconv.FormatInt(generation+1, 10), nil
}

// ClaimIdentityOperationID returns a stable opaque operation ID bound to the
// authenticated session incarnation and resulting generation. The instance
// token is hashed into the binding and never appears in the returned ID.
func ClaimIdentityOperationID(sessionID, instanceToken, runtimeEpoch, generation string) (string, error) {
	if !validTransitionText(sessionID, 200) || strings.TrimSpace(sessionID) != sessionID ||
		!validTransitionText(instanceToken, 512) || strings.TrimSpace(instanceToken) != instanceToken ||
		!canonicalPositiveInt64(runtimeEpoch) || !canonicalPositiveInt64(generation) {
		return "", ErrTransitionChainInvalid
	}
	identity, err := json.Marshal(struct {
		SessionID     string `json:"session_id"`
		InstanceToken string `json:"instance_token"`
		RuntimeEpoch  string `json:"runtime_epoch"`
		Generation    string `json:"generation"`
	}{sessionID, instanceToken, runtimeEpoch, generation})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(append([]byte(claimOperationDomain), identity...))
	return "gc-claim-v1-" + hex.EncodeToString(digest[:]), nil
}

// ClaimIdentity validates the current admitted transition head, then changes
// status, assignee, and claim metadata in one Q54 transition patch. Exact
// retries reuse the deterministic claim receipt and are accepted only when
// the current verified chain still contains that receipt for this exact
// authenticated runtime incarnation.
func (c *TransitionChain) ClaimIdentity(request ClaimIdentityRequest) (ClaimIdentityResult, error) {
	if c == nil || c.sourceReader == nil || c.patchReceiptReader == nil {
		return ClaimIdentityResult{}, ErrTransitionChainUnavailable
	}
	if !validTransitionText(request.IssueID, 200) || !validTransitionText(request.SessionID, 200) ||
		strings.TrimSpace(request.SessionID) != request.SessionID || request.ExpectedRevision <= 0 ||
		!validTransitionText(request.ExpectedTransitionHead, 200) ||
		strings.TrimSpace(request.ExpectedTransitionHead) != request.ExpectedTransitionHead {
		return ClaimIdentityResult{}, ErrTransitionChainInvalid
	}
	operationFor := func(generation string) (string, error) {
		return ClaimIdentityOperationID(request.SessionID, request.InstanceToken, request.RuntimeEpoch, generation)
	}

	// CurrentHead validates the signed v2 admission, exact current policy,
	// workflow evidence, Q43 attachment, every transition receipt, and that the
	// source revision matches the current head before either a fresh claim or a
	// reciprocal-repair replay is considered.
	head, err := c.CurrentHead(request.IssueID, request.Evidence)
	if err != nil {
		return ClaimIdentityResult{}, err
	}
	source, err := c.sourceReader.DecisionFrontierSourceSnapshot(request.IssueID)
	if err != nil {
		return ClaimIdentityResult{}, fmt.Errorf("read source before claim transition: %w", err)
	}
	if source.ID != request.IssueID || source.Revision != head.ToVersion || source.Revision == 0 {
		return ClaimIdentityResult{}, ErrTransitionChainStale
	}
	if source.Status == "in_progress" && source.Assignee == c.actor &&
		source.Metadata[beadmeta.SessionIDMetadataKey] == request.SessionID {
		return c.replayClaimIdentity(request, source, head, operationFor)
	}
	if source.Status != "open" || source.Assignee != "" {
		return ClaimIdentityResult{}, fmt.Errorf("claim predecessor is already assigned or not open: %w", ErrTransitionChainStale)
	}
	if request.ExpectedRevision != source.Revision || request.ExpectedTransitionHead != head.ReceiptID {
		return ClaimIdentityResult{}, ErrTransitionChainStale
	}
	previousGeneration, hasGeneration := source.Metadata[beadmeta.ClaimGenerationMetadataKey]
	generation, err := NextClaimGeneration(previousGeneration)
	if err != nil {
		return ClaimIdentityResult{}, fmt.Errorf("derive next claim generation: %w", err)
	}
	operationID, err := operationFor(generation)
	if err != nil {
		return ClaimIdentityResult{}, err
	}
	metadata := map[string]MetadataStringPatch{}
	var expectedGeneration *string
	if hasGeneration {
		value := previousGeneration
		expectedGeneration = &value
	}
	metadata[beadmeta.ClaimGenerationMetadataKey] = MetadataStringPatch{Expected: expectedGeneration, Value: generation}
	metadata[beadmeta.ClaimedAtMetadataKey] = MetadataStringPatch{Value: c.now().UTC().Format(time.RFC3339)}
	metadata[beadmeta.SessionIDMetadataKey] = MetadataStringPatch{Value: request.SessionID}
	for _, optional := range []struct{ key, value string }{
		{beadmeta.SessionNameMetadataKey, request.SessionName},
		{beadmeta.WorkDirMetadataKey, request.WorkDir},
		{beadmeta.WorkBranchMetadataKey, request.WorkBranch},
	} {
		if optional.value == "" {
			continue
		}
		if previous, present := source.Metadata[optional.key]; present {
			value := previous
			metadata[optional.key] = MetadataStringPatch{Expected: &value, Value: optional.value}
		} else {
			metadata[optional.key] = MetadataStringPatch{Value: optional.value}
		}
	}
	patch := SourceWorkPatch{
		Metadata: metadata,
		Status:   &StringTransition{Expected: "open", Value: "in_progress"},
		Assignee: &StringTransition{Expected: "", Value: c.actor},
	}
	transition, err := c.Apply(TransitionRequest{
		IssueID: request.IssueID, Step: TransitionStepClaimIdentity, OperationID: operationID,
		PriorReceiptID: head.ReceiptID, Evidence: request.Evidence, Patch: patch,
	})
	if err != nil {
		return ClaimIdentityResult{}, err
	}
	committed, err := c.sourceReader.DecisionFrontierSourceSnapshot(request.IssueID)
	if err != nil {
		return ClaimIdentityResult{}, fmt.Errorf("read source after claim transition: %w", err)
	}
	if committed.ID != request.IssueID || committed.Status != "in_progress" || committed.Assignee != c.actor ||
		committed.Metadata[beadmeta.SessionIDMetadataKey] != request.SessionID ||
		committed.Metadata[beadmeta.ClaimGenerationMetadataKey] != generation ||
		committed.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] != transition.Receipt.ReceiptID ||
		committed.Revision != transition.Receipt.ToVersion {
		return ClaimIdentityResult{}, fmt.Errorf("source claim readback differs from committed receipt: %w", ErrTransitionChainReceipt)
	}
	return ClaimIdentityResult{Receipt: transition.Receipt, ClaimGeneration: generation, Replayed: transition.Replayed, Recovered: transition.Recovered}, nil
}

func (c *TransitionChain) replayClaimIdentity(
	request ClaimIdentityRequest,
	source beads.Bead,
	head TransitionHead,
	operationFor func(string) (string, error),
) (ClaimIdentityResult, error) {
	generation := source.Metadata[beadmeta.ClaimGenerationMetadataKey]
	if !canonicalPositiveInt64(generation) {
		return ClaimIdentityResult{}, fmt.Errorf("current claim generation is malformed: %w", ErrTransitionChainInvalid)
	}
	operationID, err := operationFor(generation)
	if err != nil {
		return ClaimIdentityResult{}, err
	}
	receiptID, err := TransitionReceiptID(request.IssueID, c.scope, TransitionStepClaimIdentity, operationID)
	if err != nil {
		return ClaimIdentityResult{}, err
	}
	receipt, found, err := c.patchReceiptReader.ReadRevisionTransitionPatchReceipt(receiptID)
	if err != nil {
		return ClaimIdentityResult{}, fmt.Errorf("read prior claim transition: %w", err)
	}
	if !found {
		return ClaimIdentityResult{}, fmt.Errorf("current assignment has no receipt for this session incarnation: %w", ErrTransitionChainStale)
	}
	if receipt.ReceiptID != receiptID || receipt.IssueID != request.IssueID || receipt.Scope != c.scope ||
		receipt.Kind != transitionKindClaimIdentity || receipt.Actor != c.actor ||
		!transitionReceiptIsAncestor(c.patchReceiptReader, request.IssueID, head.ReceiptID, receiptID) {
		return ClaimIdentityResult{}, fmt.Errorf("prior claim receipt is not part of the current verified chain: %w", ErrTransitionChainReceipt)
	}
	patch, err := sourcePatchFromCanonical(receipt.Patch)
	if err != nil {
		return ClaimIdentityResult{}, fmt.Errorf("decode prior claim patch: %w", errors.Join(ErrTransitionChainReceipt, err))
	}
	callerPatch, err := stripTransitionHeadPatch(patch)
	if err != nil || validateTransitionStepPatch(TransitionStepClaimIdentity, callerPatch) != nil ||
		callerPatch.Metadata[beadmeta.ClaimGenerationMetadataKey].Value != generation ||
		callerPatch.Metadata[beadmeta.SessionIDMetadataKey].Value != request.SessionID ||
		callerPatch.Assignee == nil || callerPatch.Assignee.Value != c.actor {
		return ClaimIdentityResult{}, fmt.Errorf("prior claim patch does not match current ownership: %w", errors.Join(ErrTransitionChainReceipt, err))
	}
	requestBindsPrior := request.ExpectedRevision == receipt.ExpectedVersion && request.ExpectedTransitionHead == receipt.PriorReceiptID
	requestBindsCurrent := request.ExpectedRevision == source.Revision && request.ExpectedTransitionHead == head.ReceiptID
	if !requestBindsPrior && !requestBindsCurrent {
		return ClaimIdentityResult{}, ErrTransitionChainStale
	}
	transition, err := c.Apply(TransitionRequest{
		IssueID: request.IssueID, Step: TransitionStepClaimIdentity, OperationID: operationID,
		PriorReceiptID: receipt.PriorReceiptID, Evidence: request.Evidence, Patch: callerPatch,
	})
	if err != nil {
		return ClaimIdentityResult{}, err
	}
	if transition.Receipt.ReceiptID != receiptID {
		return ClaimIdentityResult{}, fmt.Errorf("claim replay returned a different receipt: %w", ErrTransitionChainReceipt)
	}
	return ClaimIdentityResult{Receipt: transition.Receipt, ClaimGeneration: generation, Replayed: true, Recovered: transition.Recovered}, nil
}

func transitionReceiptIsAncestor(reader beads.RevisionTransitionPatchReceiptReader, issueID, headID, targetID string) bool {
	if reader == nil || headID == "" || targetID == "" {
		return false
	}
	seen := make(map[string]struct{}, maxTransitionParentHops)
	for hops := 0; hops <= maxTransitionParentHops; hops++ {
		if headID == targetID {
			return true
		}
		if _, duplicate := seen[headID]; duplicate {
			return false
		}
		seen[headID] = struct{}{}
		receipt, found, err := reader.ReadRevisionTransitionPatchReceipt(headID)
		if err != nil || !found || receipt.ReceiptID != headID || receipt.IssueID != issueID || receipt.PriorReceiptID == "" {
			return false
		}
		headID = receipt.PriorReceiptID
	}
	return false
}

func canonicalPositiveInt64(raw string) bool {
	value, err := strconv.ParseInt(raw, 10, 64)
	return err == nil && value > 0 && strconv.FormatInt(value, 10) == raw
}
