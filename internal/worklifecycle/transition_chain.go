package worklifecycle

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

const (
	transitionReceiptDomain = "gascity.lifecycle.source_transition.v1\n"
	transitionReceiptPrefix = "gc-lifecycle-patch-v1-"
	completionReceiptDomain = "gascity.lifecycle.completion-receipt.v1\n"
	maxTransitionParentHops = 32
)

// transitionMaterialization is the existing lifecycle materialization marker
// written by cmd/gc. Reservation and attachment both CAS this same key.
type transitionMaterialization struct {
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

var (
	ErrTransitionChainUnavailable = errors.New("durable source-work transition chain is unavailable")
	ErrTransitionChainInvalid     = errors.New("durable source-work transition request is invalid")
	ErrTransitionChainEvidence    = errors.New("durable source-work transition evidence is invalid")
	ErrTransitionChainReceipt     = errors.New("durable source-work transition receipt is invalid")
	ErrTransitionChainStale       = errors.New("durable source-work transition source revision is stale")
	ErrTransitionChainPermit      = errors.New("durable source-work transition permit was refused")
)

// TransitionStep names one supported source-work lifecycle mutation. The
// corresponding Beads receipt kind is fixed by this package.
type TransitionStep string

const (
	TransitionStepReservation             TransitionStep = "reservation"
	TransitionStepAttachedMaterialization TransitionStep = "attached_materialization"
	TransitionStepClaimIdentity           TransitionStep = "claim_identity"
	TransitionStepRecoveryBudget          TransitionStep = "recovery_budget"
	TransitionStepRecoveryEscalation      TransitionStep = "recovery_escalation"
	TransitionStepCompletionBudget        TransitionStep = "completion_budget"
	TransitionStepClose                   TransitionStep = "close"
)

const (
	transitionKindReservation             = "lifecycle_source_reservation_v1"
	transitionKindAttachedMaterialization = "lifecycle_source_materialization_v1"
	transitionKindClaimIdentity           = "lifecycle_source_claim_identity_v1"
	transitionKindRecoveryBudget          = "lifecycle_source_recovery_budget_v1"
	transitionKindRecoveryEscalation      = "lifecycle_source_recovery_escalation_v1"
	transitionKindCompletionBudget        = "lifecycle_source_completion_budget_v1"
	transitionKindClose                   = "lifecycle_source_close_v1"
)

// TransitionChainConfig contains the explicit proof and Beads capabilities
// used by the service. The permit issuer must already be authorized for
// protected source patches; the chain does not load or sign keys.
type TransitionChainConfig struct {
	Scope                    string
	Actor                    string
	AdmissionConfig          config.LifecycleConfig
	PatchWriter              beads.RevisionTransitionPatchWriter
	PatchReceiptReader       beads.RevisionTransitionPatchReceiptReader
	SourceReader             beads.DecisionFrontierSourceReader
	AttachmentReceiptReader  beads.ControllerMetadataTransitionReceiptReader
	PermitIssuer             beads.ControllerProtectedMutationPermitIssuer
	PolicyResolver           CurrentAdmissionPolicyResolver
	WorkflowEvidenceVerifier AttachedWorkflowEvidenceVerifier
	Now                      func() time.Time
}

// CurrentAdmissionPolicyResolver recomputes the exact route/formula projection
// for the current source and its already verified admission contract. The
// transition chain compares that projection to the signed digest but leaves
// resolution policy to the trusted composition edge.
type CurrentAdmissionPolicyResolver interface {
	CurrentAdmissionPolicy(source beads.Bead, admission AdmissionReceiptV2) (AdmissionPolicyProjectionV2, error)
}

// CurrentAdmissionPolicyResolverFunc adapts a function to the resolver seam.
type CurrentAdmissionPolicyResolverFunc func(source beads.Bead, admission AdmissionReceiptV2) (AdmissionPolicyProjectionV2, error)

func (f CurrentAdmissionPolicyResolverFunc) CurrentAdmissionPolicy(source beads.Bead, admission AdmissionReceiptV2) (AdmissionPolicyProjectionV2, error) {
	return f(source, admission)
}

// AttachedWorkflowEvidence contains the exact materialized workflow identity
// and lineage fields a verifier must resolve against authoritative stores.
type AttachedWorkflowEvidence struct {
	SourceID         string
	SourceStoreRef   string
	WorkflowStoreRef string
	WorkflowID       string
	Scope            string
	AdmissionDigest  string
	Route            string
	Workflow         string
	MergeStrategy    string
	Token            string
}

// AttachedWorkflowEvidenceVerifier confirms that the named workflow exists
// and its route, formula, merge strategy, and source lineage match the
// admission proof before the source attachment transition is authorized.
type AttachedWorkflowEvidenceVerifier interface {
	VerifyAttachedWorkflow(source beads.Bead, admission AdmissionReceiptV2, policy AdmissionPolicyProjectionV2, evidence AttachedWorkflowEvidence) error
}

// AttachedWorkflowEvidenceVerifierFunc adapts a function to the workflow
// evidence verification seam.
type AttachedWorkflowEvidenceVerifierFunc func(source beads.Bead, admission AdmissionReceiptV2, policy AdmissionPolicyProjectionV2, evidence AttachedWorkflowEvidence) error

func (f AttachedWorkflowEvidenceVerifierFunc) VerifyAttachedWorkflow(source beads.Bead, admission AdmissionReceiptV2, policy AdmissionPolicyProjectionV2, evidence AttachedWorkflowEvidence) error {
	return f(source, admission, policy, evidence)
}

// TransitionChain advances an admitted source bead through immutable Beads
// patch receipts. Current route/formula resolution and workflow verification
// are trusted injected capabilities; callers cannot supply stale projections.
type TransitionChain struct {
	scope                    string
	actor                    string
	admissionConfig          config.LifecycleConfig
	patchWriter              beads.RevisionTransitionPatchWriter
	patchReceiptReader       beads.RevisionTransitionPatchReceiptReader
	sourceReader             beads.DecisionFrontierSourceReader
	attachmentReceiptReader  beads.ControllerMetadataTransitionReceiptReader
	permitIssuer             beads.ControllerProtectedMutationPermitIssuer
	policyResolver           CurrentAdmissionPolicyResolver
	workflowEvidenceVerifier AttachedWorkflowEvidenceVerifier
	now                      func() time.Time
}

// NewTransitionChain requires every durable proof and authorization
// capability. It never unwraps a store to discover a missing capability.
func NewTransitionChain(config TransitionChainConfig) (*TransitionChain, error) {
	if strings.TrimSpace(config.Scope) == "" || strings.TrimSpace(config.Scope) != config.Scope ||
		strings.TrimSpace(config.Actor) == "" || strings.TrimSpace(config.Actor) != config.Actor ||
		config.PatchWriter == nil || config.PatchReceiptReader == nil || config.SourceReader == nil ||
		config.AttachmentReceiptReader == nil || config.PermitIssuer == nil || config.PolicyResolver == nil ||
		config.WorkflowEvidenceVerifier == nil || config.Now == nil {
		return nil, ErrTransitionChainUnavailable
	}
	return &TransitionChain{
		scope: config.Scope, actor: config.Actor, admissionConfig: config.AdmissionConfig,
		patchWriter: config.PatchWriter, patchReceiptReader: config.PatchReceiptReader,
		sourceReader: config.SourceReader, attachmentReceiptReader: config.AttachmentReceiptReader,
		permitIssuer: config.PermitIssuer, policyResolver: config.PolicyResolver,
		workflowEvidenceVerifier: config.WorkflowEvidenceVerifier, now: config.Now,
	}, nil
}

// TransitionEvidence identifies the exact Q43 admission attachment. Current
// route/formula policy is always recomputed through the injected resolver.
type TransitionEvidence struct {
	Attachment AdmissionAttachmentProof
}

type verifiedTransitionEvidence struct {
	Attachment       AdmissionAttachmentProof
	Admission        AdmissionReceiptV2
	Policy           AdmissionPolicyProjectionV2
	AdmissionReceipt string
}

// MetadataStringPatch changes one metadata string. A nil Expected means the
// key was absent; Value is always written as a canonical JSON string by the
// chain before it reaches Beads.
type MetadataStringPatch struct {
	Expected *string
	Value    string
}

// StringTransition compares and replaces one source string field.
type StringTransition struct {
	Expected string
	Value    string
}

// SourceWorkPatch is the lifecycle-facing patch shape. Metadata is expressed
// as strings so callers cannot accidentally write JSON numbers, objects, or
// noncanonical raw JSON into Beads metadata.
type SourceWorkPatch struct {
	Metadata map[string]MetadataStringPatch
	Status   *StringTransition
	Assignee *StringTransition
}

// TransitionRequest names one idempotent lifecycle step and its direct parent
// receipt. OperationID must remain stable across retries of the same step.
type TransitionRequest struct {
	IssueID        string
	Step           TransitionStep
	OperationID    string
	PriorReceiptID string
	Evidence       TransitionEvidence
	// RecoveryRequest carries the separately signed proof for a recovery
	// budget transition. Its request ID, digest, and expected revision must
	// match the appended recovery attempt and this transition's parent.
	RecoveryRequest           *RecoveryRequest
	RecoveryEscalationRequest *RecoveryEscalationRequest
	Patch                     SourceWorkPatch
}

type completionBudgetMetadata struct {
	Version                 int    `json:"version"`
	WorkItemID              string `json:"work_item_id"`
	Scope                   string `json:"scope"`
	AdmissionDigest         string `json:"admission_digest"`
	CompletionReceiptDigest string `json:"completion_receipt_digest"`
	Consumed                bool   `json:"consumed"`
}

// BuildCompletionBudgetMetadata returns the canonical single-use budget value
// for the exact completion receipt that a later close transition will attach.
// The close transition still verifies the completion receipt's signature and
// freshness before it can write.
func BuildCompletionBudgetMetadata(workItemID, scope, admissionDigest, completionReceipt string) (string, error) {
	if !validTransitionText(workItemID, 200) || !validTransitionText(scope, 500) ||
		!validAdmissionDigest(admissionDigest) || completionReceipt == "" {
		return "", ErrTransitionChainInvalid
	}
	metadata := completionBudgetMetadata{
		Version: 1, WorkItemID: workItemID, Scope: scope, AdmissionDigest: admissionDigest,
		CompletionReceiptDigest: completionReceiptMetadataDigest(completionReceipt), Consumed: true,
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// TransitionResult returns the immutable committed receipt and its canonical
// digest. Replayed identifies a receipt found before a write; Recovered marks
// a lost writer response re-established from that exact durable receipt.
type TransitionResult struct {
	Receipt       beads.RevisionTransitionPatchReceipt
	ReceiptDigest string
	Replayed      bool
	Recovered     bool
}

// TransitionHead is the current source row's validated lifecycle parent. The
// Q43 attachment is the head until the first typed patch writes a transition
// head key onto the source row.
type TransitionHead struct {
	// ReceiptID is the exact Q43 or typed patch receipt ID callers should use.
	ReceiptID string
	// ToVersion is the opaque source revision written by that receipt.
	ToVersion int64
	// FromAttachment marks the Q43 receipt as the initial head before reservation.
	FromAttachment bool
}

// Apply validates the current admission and policy proof, resolves the exact
// parent receipt, then submits one atomic typed Beads patch. Ambiguous writer
// results are accepted only when the deterministic receipt ID reads back with
// every request field matching exactly.
func (c *TransitionChain) Apply(request TransitionRequest) (TransitionResult, error) {
	if c == nil || c.patchWriter == nil || c.patchReceiptReader == nil || c.sourceReader == nil ||
		c.attachmentReceiptReader == nil || c.permitIssuer == nil || c.policyResolver == nil ||
		c.workflowEvidenceVerifier == nil || c.now == nil {
		return TransitionResult{}, ErrTransitionChainUnavailable
	}
	if !validTransitionStep(request.Step) || !validTransitionText(request.IssueID, 200) ||
		!validTransitionText(request.OperationID, 256) || strings.TrimSpace(request.OperationID) != request.OperationID ||
		!validTransitionText(request.PriorReceiptID, 200) {
		return TransitionResult{}, ErrTransitionChainInvalid
	}
	kind := transitionStepKind(request.Step)
	receiptID, err := transitionReceiptID(request.IssueID, c.scope, request.Step, request.OperationID)
	if err != nil {
		return TransitionResult{}, fmt.Errorf("derive deterministic lifecycle receipt ID: %w", err)
	}

	source, err := c.sourceReader.DecisionFrontierSourceSnapshot(request.IssueID)
	if err != nil {
		return TransitionResult{}, fmt.Errorf("read lifecycle source snapshot: %w", err)
	}
	if source.ID != request.IssueID || source.Revision == 0 {
		return TransitionResult{}, fmt.Errorf("source snapshot has no exact identity or revision: %w", ErrTransitionChainEvidence)
	}
	evidence, attachmentReceipt, err := c.verifyTransitionEvidence(source, request.Evidence)
	if err != nil {
		return TransitionResult{}, err
	}
	parent, err := c.verifyParentChain(request.IssueID, request.PriorReceiptID, attachmentReceipt, evidence)
	if err != nil {
		return TransitionResult{}, err
	}
	if err := validateTransitionPredecessor(request.Step, parent); err != nil {
		return TransitionResult{}, err
	}
	if err := validateTransitionStepPatch(request.Step, request.Patch); err != nil {
		return TransitionResult{}, err
	}
	if err := c.validateTransitionStepPatchEvidence(request, source, evidence, parent); err != nil {
		return TransitionResult{}, err
	}
	if request.Step == TransitionStepClose {
		if err := verifyCompletionBudgetBinding(parent, request.Patch, request.IssueID, c.scope, evidence.Attachment.ReceiptDigest); err != nil {
			return TransitionResult{}, err
		}
	}
	effectivePatch, err := transitionPatchWithHead(request.Patch, request.Step, parent.ReceiptID, receiptID)
	if err != nil {
		return TransitionResult{}, err
	}
	patch, err := transitionPatch(effectivePatch)
	if err != nil {
		return TransitionResult{}, fmt.Errorf("build canonical lifecycle patch: %w", err)
	}
	patchRequest := beads.RevisionTransitionPatchRequest{
		ReceiptID: receiptID, Scope: c.scope, Kind: kind, Actor: c.actor,
		ExpectedVersion: parent.ToVersion, PriorReceiptID: parent.ReceiptID,
		PriorReceiptDigest: parent.Digest, Patch: patch,
	}
	permitDigest, err := beads.RevisionTransitionPatchProtectedMutationDigest(request.IssueID, patchRequest)
	if err != nil {
		return TransitionResult{}, fmt.Errorf("validate canonical lifecycle patch: %w", errors.Join(ErrTransitionChainInvalid, err))
	}

	if actual, found, readErr := c.patchReceiptReader.ReadRevisionTransitionPatchReceipt(receiptID); readErr != nil {
		return TransitionResult{}, fmt.Errorf("read prior lifecycle patch receipt: %w", readErr)
	} else if found {
		verified, digest, verifyErr := verifyTransitionPatchReceipt(actual, request.IssueID, patchRequest)
		if verifyErr != nil {
			return TransitionResult{}, verifyErr
		}
		return TransitionResult{Receipt: verified, ReceiptDigest: digest, Replayed: true}, nil
	}
	if request.Step == TransitionStepAttachedMaterialization {
		if err := c.verifyAttachedWorkflow(source, evidence, request.Patch); err != nil {
			return TransitionResult{}, err
		}
	}
	if request.Step == TransitionStepRecoveryBudget {
		if _, err := VerifyRecoveryRequest(*request.RecoveryRequest, c.admissionConfig, c.now()); err != nil {
			return TransitionResult{}, fmt.Errorf("verify current recovery request authorization: %w", errors.Join(ErrTransitionChainEvidence, err))
		}
		if err := validateRecoveryTargetRowFromBead(source, *request.RecoveryRequest); err != nil {
			return TransitionResult{}, fmt.Errorf("recovery request target differs from the current source: %w", errors.Join(ErrTransitionChainStale, err))
		}
	}
	if request.Step == TransitionStepRecoveryEscalation {
		if _, err := VerifyRecoveryEscalationRequest(*request.RecoveryEscalationRequest, c.admissionConfig, c.now()); err != nil {
			return TransitionResult{}, fmt.Errorf("verify current recovery escalation authorization: %w", errors.Join(ErrTransitionChainEvidence, err))
		}
		if source.Status != "in_progress" {
			return TransitionResult{}, fmt.Errorf("recovery escalation source is not currently in progress: %w", ErrTransitionChainStale)
		}
	}
	if request.Step == TransitionStepClose {
		if err := c.verifyCloseReceipt(source, evidence, parent, request.Patch); err != nil {
			return TransitionResult{}, err
		}
	}

	if source.Revision != parent.ToVersion {
		return TransitionResult{}, fmt.Errorf("source revision %d does not match parent ToVersion %d: %w", source.Revision, parent.ToVersion, ErrTransitionChainStale)
	}
	if err := verifyCurrentTransitionHead(source, request.Step, parent.ReceiptID); err != nil {
		return TransitionResult{}, err
	}
	if err := verifyTransitionPatchExpected(source, patch); err != nil {
		return TransitionResult{}, fmt.Errorf("source does not match lifecycle patch preconditions: %w", errors.Join(ErrTransitionChainStale, err))
	}

	permitRequest := beads.ControllerProtectedMutationRequest{
		Operation:   beads.RevisionTransitionPatchProtectedMutationOperation,
		ResourceIDs: []string{request.IssueID}, RequestDigest: permitDigest,
	}
	permit, err := c.permitIssuer.IssueProtectedMutation(permitRequest, receiptID)
	if err != nil || !validTransitionPermit(permit) {
		return TransitionResult{}, fmt.Errorf("issue exact lifecycle patch permit: %w", errors.Join(ErrTransitionChainPermit, err))
	}
	patchRequest.ProtectedPermit = permit

	result, writeErr := c.patchWriter.TransitionPatch(request.IssueID, patchRequest)
	actual, found, readErr := c.patchReceiptReader.ReadRevisionTransitionPatchReceipt(receiptID)
	if readErr != nil {
		return TransitionResult{}, errors.Join(writeErr, fmt.Errorf("read lifecycle patch receipt after write: %w", readErr))
	}
	if !found {
		if errors.Is(writeErr, beads.ErrRevisionTransitionPatchPrecondition) {
			return TransitionResult{}, errors.Join(ErrTransitionChainStale, writeErr)
		}
		if writeErr != nil {
			return TransitionResult{}, fmt.Errorf("apply lifecycle source patch: %w", writeErr)
		}
		return TransitionResult{}, fmt.Errorf("Beads returned no exact durable patch receipt: %w", ErrTransitionChainReceipt)
	}
	verified, digest, verifyErr := verifyTransitionPatchReceipt(actual, request.IssueID, patchRequest)
	if verifyErr != nil {
		return TransitionResult{}, errors.Join(writeErr, verifyErr)
	}
	if writeErr == nil {
		if !result.Applied || result.Receipt == nil {
			return TransitionResult{}, fmt.Errorf("Beads returned a non-applied result with a durable patch receipt: %w", ErrTransitionChainReceipt)
		}
		if _, _, resultErr := verifyTransitionPatchReceipt(*result.Receipt, request.IssueID, patchRequest); resultErr != nil {
			return TransitionResult{}, errors.Join(ErrTransitionChainReceipt, resultErr)
		}
	}
	if evidence.Admission.WorkItemID != request.IssueID {
		return TransitionResult{}, ErrTransitionChainEvidence
	}
	return TransitionResult{
		Receipt: verified, ReceiptDigest: digest,
		Replayed:  writeErr == nil && result.Replayed,
		Recovered: writeErr != nil,
	}, nil
}

// CurrentHead reads and validates the exact current lifecycle parent from the
// source row. Before reservation it returns the Q43 attachment receipt; after
// reservation it requires the protected source head to identify a committed
// patch receipt whose ToVersion equals the live source revision.
func (c *TransitionChain) CurrentHead(issueID string, evidence TransitionEvidence) (TransitionHead, error) {
	if c == nil || c.sourceReader == nil || c.patchReceiptReader == nil || c.attachmentReceiptReader == nil ||
		c.policyResolver == nil || c.workflowEvidenceVerifier == nil || c.now == nil {
		return TransitionHead{}, ErrTransitionChainUnavailable
	}
	if !validTransitionText(issueID, 200) {
		return TransitionHead{}, ErrTransitionChainInvalid
	}
	source, err := c.sourceReader.DecisionFrontierSourceSnapshot(issueID)
	if err != nil {
		return TransitionHead{}, fmt.Errorf("read lifecycle source snapshot: %w", err)
	}
	if source.ID != issueID || source.Revision == 0 {
		return TransitionHead{}, fmt.Errorf("source snapshot has no exact identity or revision: %w", ErrTransitionChainEvidence)
	}
	verified, attachmentReceipt, err := c.verifyTransitionEvidence(source, evidence)
	if err != nil {
		return TransitionHead{}, err
	}
	encoded, present := source.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey]
	if !present {
		if source.Revision != verified.Attachment.ToRevision {
			return TransitionHead{}, fmt.Errorf("source advanced after attachment without a transition head: %w", ErrTransitionChainStale)
		}
		return TransitionHead{ReceiptID: verified.Attachment.ReceiptID, ToVersion: verified.Attachment.ToRevision, FromAttachment: true}, nil
	}
	if !validTransitionText(encoded, 200) || strings.TrimSpace(encoded) != encoded {
		return TransitionHead{}, fmt.Errorf("source transition head is malformed: %w", ErrTransitionChainReceipt)
	}
	parent, err := c.verifyParentChain(issueID, encoded, attachmentReceipt, verified)
	if err != nil {
		return TransitionHead{}, err
	}
	if parent.Attachment || parent.ToVersion != source.Revision {
		return TransitionHead{}, fmt.Errorf("source transition head does not identify its current patch revision: %w", ErrTransitionChainStale)
	}
	return TransitionHead{ReceiptID: parent.ReceiptID, ToVersion: parent.ToVersion}, nil
}

type transitionParent struct {
	ReceiptID  string
	Digest     string
	ToVersion  int64
	Step       TransitionStep
	Attachment bool
	Patch      SourceWorkPatch
}

func (c *TransitionChain) verifyTransitionEvidence(source beads.Bead, evidence TransitionEvidence) (verifiedTransitionEvidence, beads.ControllerMetadataTransitionReceipt, error) {
	encoded := source.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey]
	admission, err := VerifyAdmissionReceiptV2(source, c.admissionConfig, c.scope)
	if err != nil {
		return verifiedTransitionEvidence{}, beads.ControllerMetadataTransitionReceipt{}, fmt.Errorf("verify exact v2 source admission: %w", errors.Join(ErrTransitionChainEvidence, err))
	}
	if source.Metadata[beadmeta.LifecycleAdmissionReceiptMetadataKey] != "" {
		return verifiedTransitionEvidence{}, beads.ControllerMetadataTransitionReceipt{}, fmt.Errorf("historical v1 evidence conflicts with v2 admission: %w", ErrTransitionChainEvidence)
	}
	digest, err := AdmissionDigestV2(admission)
	if err != nil || evidence.Attachment.SchemaVersion != 1 || evidence.Attachment.WorkItemID != source.ID ||
		evidence.Attachment.Scope != c.scope || evidence.Attachment.ReceiptDigest != digest ||
		evidence.Attachment.FromRevision != admission.ExpectedWorkRevision || evidence.Attachment.ToRevision == 0 ||
		evidence.Attachment.ToRevision == evidence.Attachment.FromRevision {
		return verifiedTransitionEvidence{}, beads.ControllerMetadataTransitionReceipt{}, fmt.Errorf("Q43 attachment does not bind the exact signed v2 admission: %w", ErrTransitionChainEvidence)
	}
	policy, err := c.policyResolver.CurrentAdmissionPolicy(source, admission)
	if err != nil {
		return verifiedTransitionEvidence{}, beads.ControllerMetadataTransitionReceipt{}, fmt.Errorf("resolve current route/formula policy proof: %w", errors.Join(ErrTransitionChainEvidence, err))
	}
	policyDigest, err := DigestAdmissionPolicyV2(policy)
	if err != nil || policyDigest != admission.RoutingPolicyDigest || policy.SourceScope != c.scope ||
		policy.Target.Identity != admission.Route || policy.Workflow != admission.Workflow ||
		policy.MergeStrategy != admission.MergeStrategy {
		return verifiedTransitionEvidence{}, beads.ControllerMetadataTransitionReceipt{}, fmt.Errorf("current route/formula policy proof differs from signed admission: %w", errors.Join(ErrTransitionChainEvidence, err))
	}
	request, err := admissionAttachmentRequest(admission, encoded, digest)
	if err != nil {
		return verifiedTransitionEvidence{}, beads.ControllerMetadataTransitionReceipt{}, fmt.Errorf("rebuild exact Q43 attachment request: %w", errors.Join(ErrTransitionChainEvidence, err))
	}
	actual, found, err := c.attachmentReceiptReader.ControllerMetadataTransitionReceipt(source.ID, evidence.Attachment.ReceiptID)
	if err != nil || !found {
		if err != nil {
			return verifiedTransitionEvidence{}, beads.ControllerMetadataTransitionReceipt{}, fmt.Errorf("read exact Q43 attachment receipt: %w", err)
		}
		return verifiedTransitionEvidence{}, beads.ControllerMetadataTransitionReceipt{}, fmt.Errorf("exact Q43 attachment receipt is absent: %w", ErrTransitionChainEvidence)
	}
	proof, err := verifyAdmissionAttachmentReceipt(actual, source.ID, request, digest)
	if err != nil || proof != evidence.Attachment {
		return verifiedTransitionEvidence{}, beads.ControllerMetadataTransitionReceipt{}, fmt.Errorf("Q43 attachment receipt does not match supplied proof: %w", errors.Join(ErrTransitionChainReceipt, err))
	}
	return verifiedTransitionEvidence{
		Attachment: evidence.Attachment, Admission: admission, Policy: policy, AdmissionReceipt: encoded,
	}, actual, nil
}

func (c *TransitionChain) verifyParentChain(issueID, receiptID string, attachmentReceipt beads.ControllerMetadataTransitionReceipt, evidence verifiedTransitionEvidence) (transitionParent, error) {
	attachment := evidence.Attachment
	seen := map[string]struct{}{}
	var verify func(string, int) (transitionParent, error)
	verify = func(id string, hops int) (transitionParent, error) {
		if hops > maxTransitionParentHops {
			return transitionParent{}, fmt.Errorf("lifecycle parent chain exceeds %d receipts: %w", maxTransitionParentHops, ErrTransitionChainReceipt)
		}
		if _, duplicate := seen[id]; duplicate {
			return transitionParent{}, fmt.Errorf("lifecycle parent chain contains a cycle: %w", ErrTransitionChainReceipt)
		}
		seen[id] = struct{}{}
		if id == attachment.ReceiptID {
			if attachmentReceipt.ReceiptID != attachment.ReceiptID || attachmentReceipt.IssueID != issueID ||
				attachmentReceipt.Scope != c.scope || attachmentReceipt.ExpectedVersion != attachment.FromRevision ||
				attachmentReceipt.ToVersion != attachment.ToRevision {
				return transitionParent{}, fmt.Errorf("Q43 attachment parent identity or ToVersion changed: %w", ErrTransitionChainReceipt)
			}
			digest, err := controllerMetadataTransitionReceiptDigest(attachmentReceipt)
			if err != nil {
				return transitionParent{}, fmt.Errorf("digest Q43 attachment parent receipt: %w", errors.Join(ErrTransitionChainReceipt, err))
			}
			return transitionParent{ReceiptID: attachmentReceipt.ReceiptID, Digest: digest, ToVersion: attachmentReceipt.ToVersion, Attachment: true}, nil
		}
		parentReceipt, found, err := c.patchReceiptReader.ReadRevisionTransitionPatchReceipt(id)
		if err != nil || !found {
			if err != nil {
				return transitionParent{}, fmt.Errorf("read parent patch receipt %q: %w", id, err)
			}
			return transitionParent{}, fmt.Errorf("parent patch receipt %q is absent: %w", id, ErrTransitionChainReceipt)
		}
		if parentReceipt.ReceiptID != id {
			return transitionParent{}, fmt.Errorf("parent patch reader returned receipt %q for requested ID %q: %w", parentReceipt.ReceiptID, id, ErrTransitionChainReceipt)
		}
		parentInfo, err := validateStoredTransitionPatchReceipt(parentReceipt, issueID, c.scope, evidence, c.admissionConfig)
		if err != nil {
			return transitionParent{}, err
		}
		upstream, err := verify(parentReceipt.PriorReceiptID, hops+1)
		if err != nil {
			return transitionParent{}, err
		}
		if upstream.ToVersion != parentReceipt.ExpectedVersion || upstream.Digest != parentReceipt.PriorReceiptDigest {
			return transitionParent{}, fmt.Errorf("parent patch does not bind its parent's digest and ToVersion: %w", ErrTransitionChainReceipt)
		}
		if err := validateTransitionPredecessor(parentInfo.Step, upstream); err != nil {
			return transitionParent{}, fmt.Errorf("stored lifecycle receipt has an invalid predecessor: %w", errors.Join(ErrTransitionChainReceipt, err))
		}
		if parentInfo.Step == TransitionStepRecoveryBudget {
			if _, err := validateRecoveryBudgetAppend(parentInfo.Patch, upstream.Patch, upstream.Step, issueID, c.scope,
				parentReceipt.ExpectedVersion); err != nil {
				return transitionParent{}, fmt.Errorf("stored recovery budget is not an append-only request reservation: %w", errors.Join(ErrTransitionChainReceipt, err))
			}
		}
		if parentInfo.Step == TransitionStepRecoveryEscalation {
			if err := validateRecoveryEscalationAppend(parentInfo.Patch, upstream.Patch, upstream.Step, issueID, c.scope,
				parentReceipt.ExpectedVersion, c.admissionConfig, nil); err != nil {
				return transitionParent{}, fmt.Errorf("stored recovery escalation is not authorized and append-only: %w", errors.Join(ErrTransitionChainReceipt, err))
			}
		}
		return parentInfo, nil
	}

	return verify(receiptID, 0)
}

func validateTransitionPredecessor(step TransitionStep, parent transitionParent) error {
	allowed := false
	switch step {
	case TransitionStepReservation:
		allowed = parent.Attachment
	case TransitionStepAttachedMaterialization:
		allowed = parent.Step == TransitionStepReservation
	case TransitionStepClaimIdentity:
		allowed = parent.Step == TransitionStepAttachedMaterialization
	case TransitionStepRecoveryBudget:
		allowed = parent.Step == TransitionStepClaimIdentity || parent.Step == TransitionStepRecoveryBudget
	case TransitionStepRecoveryEscalation:
		allowed = parent.Step == TransitionStepRecoveryBudget
	case TransitionStepCompletionBudget:
		allowed = parent.Step == TransitionStepClaimIdentity || parent.Step == TransitionStepRecoveryBudget || parent.Step == TransitionStepRecoveryEscalation
	case TransitionStepClose:
		allowed = parent.Step == TransitionStepCompletionBudget
	}
	if !allowed {
		return fmt.Errorf("step %q cannot follow parent step %q (attachment=%t): %w", step, parent.Step, parent.Attachment, ErrTransitionChainInvalid)
	}
	return nil
}

func (c *TransitionChain) verifyAttachedWorkflow(source beads.Bead, evidence verifiedTransitionEvidence, patch SourceWorkPatch) error {
	change := patch.Metadata[beadmeta.LifecycleMaterializationMetadataKey]
	marker, err := parseTransitionMaterialization(change.Value)
	if err != nil {
		return fmt.Errorf("decode attached workflow evidence: %w", errors.Join(ErrTransitionChainEvidence, err))
	}
	workflow := AttachedWorkflowEvidence{
		SourceID: marker.SourceID, SourceStoreRef: marker.SourceStoreRef, WorkflowStoreRef: marker.WorkflowStoreRef,
		WorkflowID: marker.WorkflowID, Scope: marker.Scope, AdmissionDigest: marker.Contract,
		Route: marker.Route, Workflow: marker.Workflow, MergeStrategy: marker.MergeStrategy, Token: marker.Token,
	}
	if err := c.workflowEvidenceVerifier.VerifyAttachedWorkflow(source, evidence.Admission, evidence.Policy, workflow); err != nil {
		return fmt.Errorf("verify attached workflow and source lineage: %w", errors.Join(ErrTransitionChainEvidence, err))
	}
	return nil
}

func (c *TransitionChain) verifyRecoveryBudgetTransition(source beads.Bead, request TransitionRequest, parent transitionParent) error {
	recoveryRequest := request.RecoveryRequest
	if recoveryRequest == nil {
		return fmt.Errorf("recovery budget requires its signed request proof: %w", ErrTransitionChainEvidence)
	}
	digest, err := VerifyRecoveryRequestProof(*recoveryRequest, c.admissionConfig)
	if err != nil {
		return fmt.Errorf("verify recovery request proof: %w", errors.Join(ErrTransitionChainEvidence, err))
	}
	if request.OperationID != recoveryRequest.RequestID || recoveryRequest.WorkItemID != source.ID ||
		recoveryRequest.Scope != c.scope || recoveryRequest.ExpectedRevision != parent.ToVersion {
		return fmt.Errorf("recovery request identity or parent revision differs from transition: %w", ErrTransitionChainEvidence)
	}
	next, err := validateRecoveryBudgetAppend(request.Patch, parent.Patch, parent.Step, source.ID, c.scope, parent.ToVersion)
	if err != nil {
		return err
	}
	last := next.Attempts[len(next.Attempts)-1]
	if last.RequestID != recoveryRequest.RequestID || last.RequestDigest != digest ||
		last.ExpectedRevision != recoveryRequest.ExpectedRevision {
		return fmt.Errorf("appended recovery attempt does not bind the exact signed request: %w", ErrTransitionChainEvidence)
	}
	return nil
}

func (c *TransitionChain) verifyRecoveryEscalationTransition(request TransitionRequest, parent transitionParent) error {
	proof := request.RecoveryEscalationRequest
	if proof == nil || request.OperationID != proof.RequestID {
		return fmt.Errorf("recovery escalation requires its exact signed request ID: %w", ErrTransitionChainEvidence)
	}
	return validateRecoveryEscalationAppend(request.Patch, parent.Patch, parent.Step, request.IssueID, c.scope,
		parent.ToVersion, c.admissionConfig, proof)
}

func (c *TransitionChain) verifyCloseReceipt(source beads.Bead, evidence verifiedTransitionEvidence, parent transitionParent, patch SourceWorkPatch) error {
	change, ok := patch.Metadata[beadmeta.LifecycleCompletionReceiptMetadataKey]
	if !ok {
		return fmt.Errorf("close patch has no completion receipt: %w", ErrTransitionChainEvidence)
	}
	candidate := source
	candidate.Metadata = make(map[string]string, len(source.Metadata)+1)
	for key, value := range source.Metadata {
		candidate.Metadata[key] = value
	}
	candidate.Metadata[beadmeta.LifecycleCompletionReceiptMetadataKey] = change.Value
	decision := evaluateCompletionReceiptAt(candidate, c.admissionConfig, c.scope, evidence.Admission, c.now())
	if !decision.Accepted {
		return fmt.Errorf("close completion receipt is not currently valid: %s: %w", decision.Reason, ErrTransitionChainEvidence)
	}
	budget, err := parseCompletionBudgetMetadata(
		parent.Patch.Metadata[beadmeta.LifecycleCompletionBudgetMetadataKey].Value,
		source.ID, c.scope, evidence.Attachment.ReceiptDigest,
	)
	if err != nil || budget.CompletionReceiptDigest != completionReceiptMetadataDigest(change.Value) {
		return fmt.Errorf("close completion receipt differs from consumed completion budget: %w", errors.Join(ErrTransitionChainEvidence, err))
	}
	return nil
}

func validateRecoveryBudgetAppend(patch, parentPatch SourceWorkPatch, parentStep TransitionStep, workID, scope string, expectedRevision int64) (RecoveryState, error) {
	change, ok := patch.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey]
	if !ok {
		return RecoveryState{}, fmt.Errorf("recovery budget omits its state: %w", ErrTransitionChainInvalid)
	}
	var prior RecoveryState
	switch parentStep {
	case TransitionStepClaimIdentity:
		if change.Expected != nil {
			return RecoveryState{}, fmt.Errorf("first recovery budget must compare the absent state: %w", ErrTransitionChainInvalid)
		}
		prior = RecoveryState{Version: recoveryStateVersion, WorkItemID: workID, Scope: scope, Attempts: []RecoveryAttempt{}}
	case TransitionStepRecoveryBudget:
		parentChange, parentOK := parentPatch.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey]
		if !parentOK || change.Expected == nil || *change.Expected != parentChange.Value {
			return RecoveryState{}, fmt.Errorf("recovery budget does not compare the exact parent state: %w", ErrTransitionChainInvalid)
		}
		decoded, err := decodeCanonicalRecoveryState(parentChange.Value, workID, scope)
		if err != nil {
			return RecoveryState{}, fmt.Errorf("decode parent recovery state: %w", errors.Join(ErrTransitionChainInvalid, err))
		}
		prior = decoded
	default:
		return RecoveryState{}, fmt.Errorf("recovery budget parent is not a claim or recovery receipt: %w", ErrTransitionChainInvalid)
	}
	if change.Expected != nil {
		decoded, err := decodeCanonicalRecoveryState(*change.Expected, workID, scope)
		if err != nil {
			return RecoveryState{}, fmt.Errorf("decode expected recovery state: %w", errors.Join(ErrTransitionChainInvalid, err))
		}
		if !sameRecoveryState(decoded, prior) {
			return RecoveryState{}, fmt.Errorf("expected recovery state differs from the lifecycle parent: %w", ErrTransitionChainInvalid)
		}
	}
	next, err := decodeCanonicalRecoveryState(change.Value, workID, scope)
	if err != nil {
		return RecoveryState{}, fmt.Errorf("decode next recovery state: %w", errors.Join(ErrTransitionChainInvalid, err))
	}
	if len(prior.Attempts) >= MaxRecoveryAttempts || len(next.Attempts) != len(prior.Attempts)+1 ||
		!sameRecoveryEscalation(prior.Escalation, next.Escalation) {
		return RecoveryState{}, fmt.Errorf("recovery state must append exactly one attempt without changing escalation or exceeding the budget: %w", ErrTransitionChainInvalid)
	}
	for index, attempt := range prior.Attempts {
		if next.Attempts[index] != attempt {
			return RecoveryState{}, fmt.Errorf("recovery state changed prior attempt %d: %w", index, ErrTransitionChainInvalid)
		}
	}
	last := next.Attempts[len(next.Attempts)-1]
	reservedAt, err := time.Parse(time.RFC3339Nano, last.ReservedAt)
	if !validSessionRequestID(last.RequestID) || !validLowercaseDigest(last.RequestDigest) ||
		last.ExpectedRevision != expectedRevision || expectedRevision == 0 || err != nil ||
		reservedAt.UTC().Format(time.RFC3339Nano) != last.ReservedAt {
		return RecoveryState{}, fmt.Errorf("new recovery attempt is not bound to a canonical request and parent revision: %w", ErrTransitionChainInvalid)
	}
	return next, nil
}

func decodeCanonicalRecoveryState(raw, workID, scope string) (RecoveryState, error) {
	state, err := decodeRecoveryState(raw, workID, scope)
	if err != nil {
		return RecoveryState{}, err
	}
	canonical, err := json.Marshal(state)
	if err != nil || !bytes.Equal(canonical, []byte(raw)) {
		return RecoveryState{}, fmt.Errorf("recovery state is not canonical JSON: %w", ErrRecoveryStateInvalid)
	}
	return state, nil
}

func sameRecoveryState(left, right RecoveryState) bool {
	if left.Version != right.Version || left.WorkItemID != right.WorkItemID || left.Scope != right.Scope ||
		len(left.Attempts) != len(right.Attempts) || !sameRecoveryEscalation(left.Escalation, right.Escalation) {
		return false
	}
	for index := range left.Attempts {
		if left.Attempts[index] != right.Attempts[index] {
			return false
		}
	}
	return true
}

func sameRecoveryEscalation(left, right *RecoveryEscalation) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	if left.ID != right.ID || left.Target != right.Target || left.RequestedAt != right.RequestedAt || left.DedupKey != right.DedupKey {
		return false
	}
	if left.Request == nil || right.Request == nil {
		return left.Request == nil && right.Request == nil
	}
	return *left.Request == *right.Request
}

func validateRecoveryEscalationAppend(patch, parentPatch SourceWorkPatch, parentStep TransitionStep, workID, scope string,
	expectedRevision int64, cfg config.LifecycleConfig, supplied *RecoveryEscalationRequest) error {
	change, ok := patch.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey]
	parentChange, parentOK := parentPatch.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey]
	if !ok || !parentOK || change.Expected == nil || *change.Expected != parentChange.Value || parentStep != TransitionStepRecoveryBudget {
		return fmt.Errorf("recovery escalation must compare its direct recovery-budget parent state: %w", ErrTransitionChainInvalid)
	}
	prior, err := decodeCanonicalRecoveryState(parentChange.Value, workID, scope)
	if err != nil || len(prior.Attempts) != MaxRecoveryAttempts || prior.Escalation != nil {
		return fmt.Errorf("recovery escalation requires two attempts and no prior escalation: %w", errors.Join(ErrTransitionChainInvalid, err))
	}
	expectedPrior, err := decodeCanonicalRecoveryState(*change.Expected, workID, scope)
	if err != nil || !sameRecoveryState(expectedPrior, prior) {
		return fmt.Errorf("recovery escalation expected state differs from its parent: %w", errors.Join(ErrTransitionChainInvalid, err))
	}
	next, err := decodeCanonicalRecoveryState(change.Value, workID, scope)
	if err != nil || len(next.Attempts) != len(prior.Attempts) || next.Escalation == nil || next.Escalation.Request == nil {
		return fmt.Errorf("recovery escalation must preserve attempts and append one escalation: %w", errors.Join(ErrTransitionChainInvalid, err))
	}
	for index := range prior.Attempts {
		if next.Attempts[index] != prior.Attempts[index] {
			return fmt.Errorf("recovery escalation changed prior attempt %d: %w", index, ErrTransitionChainInvalid)
		}
	}
	request := *next.Escalation.Request
	if supplied != nil && *supplied != request {
		return fmt.Errorf("recovery escalation state differs from the supplied signed request: %w", ErrTransitionChainEvidence)
	}
	if _, err := VerifyRecoveryEscalationRequestProof(request, cfg); err != nil {
		return fmt.Errorf("verify stored recovery escalation request: %w", errors.Join(ErrTransitionChainEvidence, err))
	}
	if request.WorkItemID != workID || request.Scope != scope || request.ExpectedRevision != expectedRevision || expectedRevision == 0 {
		return fmt.Errorf("recovery escalation request differs from its source parent revision: %w", ErrTransitionChainStale)
	}
	wantID, err := recoveryEscalationID(request)
	if err != nil {
		return fmt.Errorf("derive deterministic recovery escalation ID: %w", errors.Join(ErrTransitionChainInvalid, err))
	}
	want := &RecoveryEscalation{
		ID: wantID, Target: request.Target, RequestedAt: request.IssuedAt,
		DedupKey: recoveryEscalationDedupKey(scope, workID, wantID), Request: &request,
	}
	if !sameRecoveryEscalation(next.Escalation, want) {
		return fmt.Errorf("recovery escalation fields are not derived from its signed request: %w", ErrTransitionChainInvalid)
	}
	return nil
}

func validateStoredRecoveryBudget(patch SourceWorkPatch, receiptID, workID, scope string, expectedVersion int64) error {
	change, ok := patch.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey]
	if !ok {
		return fmt.Errorf("stored recovery receipt omits state: %w", ErrTransitionChainReceipt)
	}
	state, err := decodeCanonicalRecoveryState(change.Value, workID, scope)
	if err != nil || len(state.Attempts) == 0 {
		return fmt.Errorf("decode stored recovery state: %w", errors.Join(ErrTransitionChainReceipt, err))
	}
	last := state.Attempts[len(state.Attempts)-1]
	wantReceiptID, err := transitionReceiptID(workID, scope, TransitionStepRecoveryBudget, last.RequestID)
	if err != nil || wantReceiptID != receiptID || last.ExpectedRevision != expectedVersion ||
		!validSessionRequestID(last.RequestID) || !validLowercaseDigest(last.RequestDigest) {
		return fmt.Errorf("stored recovery receipt is not bound to its request and expected revision: %w", errors.Join(ErrTransitionChainReceipt, err))
	}
	return nil
}

func validateCompletionBudgetPatch(patch SourceWorkPatch, workID, scope, admissionDigest string) error {
	change, ok := patch.Metadata[beadmeta.LifecycleCompletionBudgetMetadataKey]
	if !ok || change.Expected != nil {
		return fmt.Errorf("completion budget must consume one absent budget record: %w", ErrTransitionChainInvalid)
	}
	if _, err := parseCompletionBudgetMetadata(change.Value, workID, scope, admissionDigest); err != nil {
		return fmt.Errorf("completion budget metadata is invalid: %w", errors.Join(ErrTransitionChainInvalid, err))
	}
	return nil
}

func verifyCompletionBudgetBinding(parent transitionParent, closePatch SourceWorkPatch, workID, scope, admissionDigest string) error {
	if parent.Step != TransitionStepCompletionBudget {
		return fmt.Errorf("close parent is not a completion budget: %w", ErrTransitionChainInvalid)
	}
	change, ok := closePatch.Metadata[beadmeta.LifecycleCompletionReceiptMetadataKey]
	if !ok {
		return fmt.Errorf("close patch omits its completion receipt: %w", ErrTransitionChainInvalid)
	}
	budgetChange, ok := parent.Patch.Metadata[beadmeta.LifecycleCompletionBudgetMetadataKey]
	if !ok || budgetChange.Expected != nil {
		return fmt.Errorf("completion budget parent has no canonical consumed state: %w", ErrTransitionChainReceipt)
	}
	budget, err := parseCompletionBudgetMetadata(budgetChange.Value, workID, scope, admissionDigest)
	if err != nil || budget.CompletionReceiptDigest != completionReceiptMetadataDigest(change.Value) {
		return fmt.Errorf("close completion receipt differs from its consumed budget: %w", errors.Join(ErrTransitionChainEvidence, err))
	}
	return nil
}

func parseCompletionBudgetMetadata(raw, workID, scope, admissionDigest string) (completionBudgetMetadata, error) {
	var metadata completionBudgetMetadata
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		return completionBudgetMetadata{}, err
	}
	canonical, err := json.Marshal(metadata)
	if err != nil || !bytes.Equal(canonical, []byte(raw)) || metadata.Version != 1 ||
		metadata.WorkItemID != workID || metadata.Scope != scope || metadata.AdmissionDigest != admissionDigest ||
		!validAdmissionDigest(metadata.AdmissionDigest) || !validLowercaseDigest(metadata.CompletionReceiptDigest) || !metadata.Consumed {
		return completionBudgetMetadata{}, errors.New("completion budget identity, digests, version, consumed flag, or encoding is invalid")
	}
	return metadata, nil
}

func completionReceiptMetadataDigest(encoded string) string {
	digest := sha256.Sum256([]byte(completionReceiptDomain + encoded))
	return hex.EncodeToString(digest[:])
}

func validLowercaseDigest(digest string) bool {
	if !validDigest(digest) {
		return false
	}
	decoded, _ := hex.DecodeString(digest)
	return hex.EncodeToString(decoded) == digest
}

func validAdmissionDigest(digest string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(digest)
	return err == nil && len(decoded) == sha256.Size && base64.RawURLEncoding.EncodeToString(decoded) == digest
}

func transitionPatchWithHead(patch SourceWorkPatch, step TransitionStep, parentReceiptID, receiptID string) (SourceWorkPatch, error) {
	if _, supplied := patch.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey]; supplied {
		return SourceWorkPatch{}, fmt.Errorf("transition head is service-owned: %w", ErrTransitionChainInvalid)
	}
	metadata := make(map[string]MetadataStringPatch, len(patch.Metadata)+1)
	for key, change := range patch.Metadata {
		if change.Expected != nil {
			expected := *change.Expected
			change.Expected = &expected
		}
		metadata[key] = change
	}
	head := MetadataStringPatch{Value: receiptID}
	if step != TransitionStepReservation {
		parent := parentReceiptID
		head.Expected = &parent
	}
	metadata[beadmeta.LifecycleTransitionHeadMetadataKey] = head
	patch.Metadata = metadata
	return patch, nil
}

func verifyCurrentTransitionHead(source beads.Bead, step TransitionStep, parentReceiptID string) error {
	current, present := source.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey]
	if step == TransitionStepReservation {
		if present {
			return fmt.Errorf("source already has a lifecycle transition head %q before reservation: %w", current, ErrTransitionChainStale)
		}
		return nil
	}
	if !present || current != parentReceiptID {
		return fmt.Errorf("source transition head %q does not match requested parent %q: %w", current, parentReceiptID, ErrTransitionChainStale)
	}
	return nil
}

func stripTransitionHeadPatch(patch SourceWorkPatch) (SourceWorkPatch, error) {
	if _, ok := patch.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey]; !ok {
		return SourceWorkPatch{}, fmt.Errorf("stored lifecycle patch omits its transition head: %w", ErrTransitionChainReceipt)
	}
	metadata := make(map[string]MetadataStringPatch, len(patch.Metadata)-1)
	for key, change := range patch.Metadata {
		if key == beadmeta.LifecycleTransitionHeadMetadataKey {
			continue
		}
		metadata[key] = change
	}
	patch.Metadata = metadata
	return patch, nil
}

func validateStoredTransitionHead(receipt beads.RevisionTransitionPatchReceipt, step TransitionStep, patch SourceWorkPatch) error {
	change, ok := patch.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey]
	if !ok || change.Value != receipt.ReceiptID {
		return fmt.Errorf("stored lifecycle patch does not point its head at its own receipt: %w", ErrTransitionChainReceipt)
	}
	if step == TransitionStepReservation {
		if change.Expected != nil {
			return fmt.Errorf("reservation head must compare the absent source head: %w", ErrTransitionChainReceipt)
		}
	} else if change.Expected == nil || *change.Expected != receipt.PriorReceiptID {
		return fmt.Errorf("stored lifecycle patch head does not compare its exact parent: %w", ErrTransitionChainReceipt)
	}
	return nil
}

func transitionPatch(patch SourceWorkPatch) (beads.RevisionTransitionIssuePatch, error) {
	if len(patch.Metadata) == 0 && patch.Status == nil && patch.Assignee == nil {
		return beads.RevisionTransitionIssuePatch{}, ErrTransitionChainInvalid
	}
	keys := make([]string, 0, len(patch.Metadata))
	for key := range patch.Metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	converted := beads.RevisionTransitionIssuePatch{Metadata: make([]beads.RevisionTransitionMetadataPatch, 0, len(keys))}
	for _, key := range keys {
		if !beadmeta.ValidKey(key) || !validTransitionText(key, 255) {
			return beads.RevisionTransitionIssuePatch{}, fmt.Errorf("metadata key %q is invalid: %w", key, ErrTransitionChainInvalid)
		}
		change := patch.Metadata[key]
		valueBytes, err := json.Marshal(change.Value)
		if err != nil {
			return beads.RevisionTransitionIssuePatch{}, fmt.Errorf("encode metadata string %q: %w", key, err)
		}
		value := json.RawMessage(valueBytes)
		var expected *json.RawMessage
		if change.Expected != nil {
			expectedBytes, err := json.Marshal(*change.Expected)
			if err != nil {
				return beads.RevisionTransitionIssuePatch{}, fmt.Errorf("encode expected metadata string %q: %w", key, err)
			}
			raw := json.RawMessage(expectedBytes)
			expected = &raw
		}
		converted.Metadata = append(converted.Metadata, beads.RevisionTransitionMetadataPatch{Key: key, Expected: expected, Value: &value})
	}
	if patch.Status != nil {
		value := beads.RevisionTransitionStringPatch{Expected: patch.Status.Expected, Value: patch.Status.Value}
		converted.Status = &value
	}
	if patch.Assignee != nil {
		value := beads.RevisionTransitionStringPatch{Expected: patch.Assignee.Expected, Value: patch.Assignee.Value}
		converted.Assignee = &value
	}
	return converted, nil
}

func validateTransitionStepPatch(step TransitionStep, patch SourceWorkPatch) error {
	allowed := map[TransitionStep]map[string]struct{}{
		TransitionStepReservation:             {beadmeta.LifecycleMaterializationMetadataKey: {}},
		TransitionStepAttachedMaterialization: {beadmeta.LifecycleMaterializationMetadataKey: {}},
		TransitionStepRecoveryBudget:          {beadmeta.LifecycleRecoveryStateMetadataKey: {}},
		TransitionStepRecoveryEscalation:      {beadmeta.LifecycleRecoveryStateMetadataKey: {}},
		TransitionStepCompletionBudget:        {beadmeta.LifecycleCompletionBudgetMetadataKey: {}},
		TransitionStepClose:                   {beadmeta.LifecycleCompletionReceiptMetadataKey: {}},
		TransitionStepClaimIdentity: {
			beadmeta.ClaimGenerationMetadataKey: {}, beadmeta.ClaimedAtMetadataKey: {}, beadmeta.SessionIDMetadataKey: {},
			beadmeta.SessionNameMetadataKey: {}, beadmeta.WorkDirMetadataKey: {}, beadmeta.WorkBranchMetadataKey: {},
		},
	}
	allowedKeys, ok := allowed[step]
	if !ok {
		return ErrTransitionChainInvalid
	}
	if step == TransitionStepAttachedMaterialization {
		allowedKeys = map[string]struct{}{
			beadmeta.LifecycleMaterializationMetadataKey: {},
			beadmeta.ExecutionRoutedToMetadataKey:        {},
			beadmeta.RoutedToMetadataKey:                 {},
			beadmeta.MoleculeIDMetadataKey:               {},
			beadmeta.WorkflowIDMetadataKey:               {},
			beadmeta.LegacyWorkflowIDMetadataKey:         {},
			beadmeta.MergeStrategyMetadataKey:            {},
		}
	}
	if step == TransitionStepRecoveryBudget || step == TransitionStepRecoveryEscalation || step == TransitionStepCompletionBudget || step == TransitionStepClose {
		if len(patch.Metadata) != 1 {
			return fmt.Errorf("step %q must set exactly one metadata key: %w", step, ErrTransitionChainInvalid)
		}
	}
	for key := range patch.Metadata {
		if _, ok := allowedKeys[key]; !ok {
			return fmt.Errorf("step %q cannot mutate metadata key %q: %w", step, key, ErrTransitionChainInvalid)
		}
	}
	if len(patch.Metadata) > len(allowedKeys) {
		return fmt.Errorf("step %q has an incomplete metadata patch: %w", step, ErrTransitionChainInvalid)
	}
	switch step {
	case TransitionStepReservation:
		change, ok := patch.Metadata[beadmeta.LifecycleMaterializationMetadataKey]
		marker, err := parseTransitionMaterialization(change.Value)
		if !ok || err != nil || change.Expected != nil || marker.State != "reserved" || marker.WorkflowID != "" {
			return fmt.Errorf("reservation step must create the canonical reserved materialization marker: %w", ErrTransitionChainInvalid)
		}
	case TransitionStepAttachedMaterialization:
		if err := validateAttachedMaterializationShape(patch); err != nil {
			return err
		}
	case TransitionStepClaimIdentity:
		for _, key := range []string{beadmeta.ClaimGenerationMetadataKey, beadmeta.ClaimedAtMetadataKey, beadmeta.SessionIDMetadataKey} {
			if _, present := patch.Metadata[key]; !present {
				return fmt.Errorf("claim identity step omits required metadata key %q: %w", key, ErrTransitionChainInvalid)
			}
		}
		if patch.Status == nil || patch.Status.Expected != "open" || patch.Status.Value != "in_progress" ||
			patch.Assignee == nil || patch.Assignee.Expected != "" || strings.TrimSpace(patch.Assignee.Value) == "" {
			return fmt.Errorf("claim identity step must atomically set in_progress, assignee, claim generation, claimed_at, and session ID: %w", ErrTransitionChainInvalid)
		}
	case TransitionStepClose:
		if patch.Status == nil || patch.Status.Expected != "in_progress" || patch.Status.Value != "closed" {
			return fmt.Errorf("close step must atomically close in_progress source work: %w", ErrTransitionChainInvalid)
		}
	default:
		if patch.Status != nil || patch.Assignee != nil {
			return fmt.Errorf("step %q cannot change status or assignee: %w", step, ErrTransitionChainInvalid)
		}
	}
	for key, change := range patch.Metadata {
		if change.Value == "" {
			return fmt.Errorf("step %q metadata value %q is empty: %w", step, key, ErrTransitionChainInvalid)
		}
	}
	if step == TransitionStepClaimIdentity {
		generation, err := strconv.ParseInt(patch.Metadata[beadmeta.ClaimGenerationMetadataKey].Value, 10, 64)
		if err != nil || generation <= 0 || strconv.FormatInt(generation, 10) != patch.Metadata[beadmeta.ClaimGenerationMetadataKey].Value {
			return fmt.Errorf("claim identity generation is not a canonical positive integer: %w", ErrTransitionChainInvalid)
		}
		claimedAt := patch.Metadata[beadmeta.ClaimedAtMetadataKey].Value
		instant, err := time.Parse(time.RFC3339, claimedAt)
		if err != nil || instant.UTC().Format(time.RFC3339) != claimedAt {
			return fmt.Errorf("claim identity claimed_at is not canonical UTC RFC3339: %w", ErrTransitionChainInvalid)
		}
	}
	return nil
}

func validateAttachedMaterializationShape(patch SourceWorkPatch) error {
	if len(patch.Metadata) != 4 {
		return fmt.Errorf("attached materialization must atomically set the marker, one route, one workflow ID, and merge strategy: %w", ErrTransitionChainInvalid)
	}
	routeKey, routeCount := exactlyOneTransitionKey(patch.Metadata, beadmeta.ExecutionRoutedToMetadataKey, beadmeta.RoutedToMetadataKey)
	workflowKey, workflowCount := exactlyOneTransitionKey(patch.Metadata, beadmeta.MoleculeIDMetadataKey, beadmeta.WorkflowIDMetadataKey, beadmeta.LegacyWorkflowIDMetadataKey)
	if routeCount != 1 || workflowCount != 1 {
		return fmt.Errorf("attached materialization must choose exactly one canonical route key and one workflow ID key (route=%d workflow=%d): %w", routeCount, workflowCount, ErrTransitionChainInvalid)
	}
	markerChange, markerOK := patch.Metadata[beadmeta.LifecycleMaterializationMetadataKey]
	mergeChange, mergeOK := patch.Metadata[beadmeta.MergeStrategyMetadataKey]
	marker, err := parseTransitionMaterialization(markerChange.Value)
	if !markerOK || !mergeOK || err != nil || marker.State != "attached" || marker.WorkflowID == "" ||
		patch.Metadata[routeKey].Value != marker.Route || patch.Metadata[workflowKey].Value != marker.WorkflowID ||
		mergeChange.Value != marker.MergeStrategy {
		return fmt.Errorf("attached materialization fields differ from its attached marker: %w", ErrTransitionChainInvalid)
	}
	if markerChange.Expected == nil {
		return fmt.Errorf("attached materialization must compare-and-set the existing reserved marker: %w", ErrTransitionChainInvalid)
	}
	prior, err := parseTransitionMaterialization(*markerChange.Expected)
	if err != nil || prior.State != "reserved" || !sameTransitionMaterializationContract(prior, marker) {
		return fmt.Errorf("attached materialization does not continue the exact reserved marker: %w", ErrTransitionChainInvalid)
	}
	return nil
}

func exactlyOneTransitionKey(values map[string]MetadataStringPatch, keys ...string) (string, int) {
	selected := ""
	count := 0
	for _, key := range keys {
		if _, ok := values[key]; ok {
			selected = key
			count++
		}
	}
	return selected, count
}

func parseTransitionMaterialization(encoded string) (transitionMaterialization, error) {
	var marker transitionMaterialization
	if encoded == "" || json.Unmarshal([]byte(encoded), &marker) != nil {
		return transitionMaterialization{}, errors.New("materialization marker is not valid JSON")
	}
	canonical, err := json.Marshal(marker)
	if err != nil || string(canonical) != encoded || marker.Version != 1 || marker.Scope == "" || marker.Contract == "" ||
		marker.Route == "" || marker.Workflow == "" || marker.MergeStrategy == "" || marker.Token == "" ||
		marker.SourceID == "" || marker.SourceStoreRef == "" || marker.WorkflowStoreRef == "" || marker.AdmissionReceipt == "" {
		return transitionMaterialization{}, errors.New("materialization marker is incomplete or noncanonical")
	}
	if marker.State != "reserved" && marker.State != "attached" {
		return transitionMaterialization{}, errors.New("materialization marker state is unsupported")
	}
	if marker.State == "reserved" && marker.WorkflowID != "" || marker.State == "attached" && marker.WorkflowID == "" {
		return transitionMaterialization{}, errors.New("materialization workflow ID does not match marker state")
	}
	return marker, nil
}

func sameTransitionMaterializationContract(left, right transitionMaterialization) bool {
	return left.Version == right.Version && left.Scope == right.Scope && left.Contract == right.Contract &&
		left.Route == right.Route && left.Workflow == right.Workflow && left.MergeStrategy == right.MergeStrategy &&
		left.Token == right.Token && left.SourceID == right.SourceID && left.SourceStoreRef == right.SourceStoreRef &&
		left.WorkflowStoreRef == right.WorkflowStoreRef && left.AdmissionReceipt == right.AdmissionReceipt
}

func (c *TransitionChain) validateTransitionStepPatchEvidence(request TransitionRequest, source beads.Bead, evidence verifiedTransitionEvidence, parent transitionParent) error {
	switch request.Step {
	case TransitionStepReservation:
		marker, err := parseTransitionMaterialization(request.Patch.Metadata[beadmeta.LifecycleMaterializationMetadataKey].Value)
		if err != nil || !transitionMaterializationMatchesEvidence(marker, source, evidence) {
			return fmt.Errorf("reserved materialization marker does not bind source admission and policy: %w", errors.Join(ErrTransitionChainEvidence, err))
		}
	case TransitionStepAttachedMaterialization:
		marker, err := parseTransitionMaterialization(request.Patch.Metadata[beadmeta.LifecycleMaterializationMetadataKey].Value)
		priorEncoded := *request.Patch.Metadata[beadmeta.LifecycleMaterializationMetadataKey].Expected
		prior, priorErr := parseTransitionMaterialization(priorEncoded)
		if err != nil || priorErr != nil || !transitionMaterializationMatchesEvidence(marker, source, evidence) ||
			!transitionMaterializationMatchesEvidence(prior, source, evidence) || !sameTransitionMaterializationContract(prior, marker) {
			return fmt.Errorf("attached materialization marker does not continue the exact admitted reservation: %w", errors.Join(ErrTransitionChainEvidence, err, priorErr))
		}
		routeKey, _ := exactlyOneTransitionKey(request.Patch.Metadata, beadmeta.ExecutionRoutedToMetadataKey, beadmeta.RoutedToMetadataKey)
		workflowKey, _ := exactlyOneTransitionKey(request.Patch.Metadata, beadmeta.MoleculeIDMetadataKey, beadmeta.WorkflowIDMetadataKey, beadmeta.LegacyWorkflowIDMetadataKey)
		if request.Patch.Metadata[routeKey].Value != evidence.Policy.Target.Identity || request.Patch.Metadata[workflowKey].Value != marker.WorkflowID ||
			request.Patch.Metadata[beadmeta.MergeStrategyMetadataKey].Value != evidence.Policy.MergeStrategy {
			return fmt.Errorf("attached source route, workflow, or merge value differs from exact admission: %w", ErrTransitionChainEvidence)
		}
		for _, key := range []string{beadmeta.ExecutionRoutedToMetadataKey, beadmeta.RoutedToMetadataKey} {
			current := source.Metadata[key]
			if key == routeKey {
				current = request.Patch.Metadata[key].Value
			}
			if strings.TrimSpace(current) != "" && key != routeKey {
				return fmt.Errorf("attached source has a second route key %q: %w", key, ErrTransitionChainEvidence)
			}
		}
		workflowKeys := []string{beadmeta.MoleculeIDMetadataKey, beadmeta.WorkflowIDMetadataKey, beadmeta.LegacyWorkflowIDMetadataKey}
		for _, key := range workflowKeys {
			current := source.Metadata[key]
			if key == workflowKey {
				current = request.Patch.Metadata[key].Value
			}
			if strings.TrimSpace(current) != "" && key != workflowKey {
				return fmt.Errorf("attached source has a second workflow ID key %q: %w", key, ErrTransitionChainEvidence)
			}
		}
	case TransitionStepRecoveryBudget:
		if err := c.verifyRecoveryBudgetTransition(source, request, parent); err != nil {
			return err
		}
	case TransitionStepRecoveryEscalation:
		if err := c.verifyRecoveryEscalationTransition(request, parent); err != nil {
			return err
		}
	case TransitionStepCompletionBudget:
		if err := validateCompletionBudgetPatch(request.Patch, source.ID, c.scope, evidence.Attachment.ReceiptDigest); err != nil {
			return err
		}
	}
	return nil
}

func transitionMaterializationMatchesEvidence(marker transitionMaterialization, source beads.Bead, evidence verifiedTransitionEvidence) bool {
	return marker.Scope == evidence.Policy.SourceScope && marker.Scope == evidence.Attachment.Scope &&
		marker.Contract == evidence.Attachment.ReceiptDigest && marker.Route == evidence.Policy.Target.Identity && marker.Workflow == evidence.Policy.Workflow &&
		marker.MergeStrategy == evidence.Policy.MergeStrategy && marker.SourceID == source.ID &&
		marker.AdmissionReceipt == evidence.AdmissionReceipt && marker.AdmissionReceipt == source.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey]
}

func verifyTransitionPatchExpected(source beads.Bead, patch beads.RevisionTransitionIssuePatch) error {
	for _, change := range patch.Metadata {
		current, present := source.Metadata[change.Key]
		if change.Expected == nil {
			if present {
				return fmt.Errorf("metadata key %q is present", change.Key)
			}
			continue
		}
		var expected string
		if err := json.Unmarshal(*change.Expected, &expected); err != nil || !present || current != expected {
			return fmt.Errorf("metadata key %q differs from its expected string", change.Key)
		}
	}
	if patch.Status != nil && source.Status != patch.Status.Expected {
		return errors.New("status differs from its expected value")
	}
	if patch.Assignee != nil && source.Assignee != patch.Assignee.Expected {
		return errors.New("assignee differs from its expected value")
	}
	return nil
}

func verifyTransitionPatchReceipt(receipt beads.RevisionTransitionPatchReceipt, issueID string, request beads.RevisionTransitionPatchRequest) (beads.RevisionTransitionPatchReceipt, string, error) {
	if receipt.ReceiptID != request.ReceiptID || receipt.IssueID != issueID || receipt.Scope != request.Scope ||
		receipt.Kind != request.Kind || receipt.Actor != request.Actor || receipt.ExpectedVersion != request.ExpectedVersion ||
		receipt.PriorReceiptID != request.PriorReceiptID || receipt.PriorReceiptDigest != request.PriorReceiptDigest ||
		receipt.ToVersion == 0 || receipt.ToVersion == receipt.ExpectedVersion {
		return beads.RevisionTransitionPatchReceipt{}, "", fmt.Errorf("durable receipt identity, parent, or ToVersion differs from request: %w", ErrTransitionChainReceipt)
	}
	wantPatch, err := canonicalPatch(request.Patch)
	if err != nil {
		return beads.RevisionTransitionPatchReceipt{}, "", fmt.Errorf("request patch is not canonical: %w", errors.Join(ErrTransitionChainReceipt, err))
	}
	gotPatch, err := canonicalPatch(receipt.Patch)
	if err != nil {
		return beads.RevisionTransitionPatchReceipt{}, "", fmt.Errorf("receipt patch is not canonical: %w", errors.Join(ErrTransitionChainReceipt, err))
	}
	actualPatchBytes, _ := json.Marshal(receipt.Patch)
	canonicalPatchBytes, _ := json.Marshal(gotPatch)
	if !bytes.Equal(actualPatchBytes, canonicalPatchBytes) {
		return beads.RevisionTransitionPatchReceipt{}, "", fmt.Errorf("receipt patch is not canonically ordered: %w", ErrTransitionChainReceipt)
	}
	wantBytes, _ := json.Marshal(wantPatch)
	gotBytes, _ := json.Marshal(gotPatch)
	if !bytes.Equal(wantBytes, gotBytes) {
		return beads.RevisionTransitionPatchReceipt{}, "", fmt.Errorf("durable receipt patch differs from request: %w", ErrTransitionChainReceipt)
	}
	canonical := receipt
	canonical.Patch = gotPatch
	digest, err := revisionTransitionPatchReceiptDigest(canonical)
	if err != nil {
		return beads.RevisionTransitionPatchReceipt{}, "", fmt.Errorf("digest durable patch receipt: %w", errors.Join(ErrTransitionChainReceipt, err))
	}
	return canonical, digest, nil
}

func validateStoredTransitionPatchReceipt(receipt beads.RevisionTransitionPatchReceipt, issueID, scope string, evidence verifiedTransitionEvidence, admissionConfig config.LifecycleConfig) (transitionParent, error) {
	if receipt.IssueID != issueID || receipt.Scope != scope || receipt.ToVersion == 0 || receipt.ToVersion == receipt.ExpectedVersion {
		return transitionParent{}, fmt.Errorf("stored parent patch receipt has wrong identity or version: %w", ErrTransitionChainReceipt)
	}
	request := beads.RevisionTransitionPatchRequest{
		ReceiptID: receipt.ReceiptID, Scope: receipt.Scope, Kind: receipt.Kind, Actor: receipt.Actor,
		ExpectedVersion: receipt.ExpectedVersion, PriorReceiptID: receipt.PriorReceiptID,
		PriorReceiptDigest: receipt.PriorReceiptDigest, Patch: receipt.Patch,
	}
	if _, err := beads.RevisionTransitionPatchProtectedMutationDigest(issueID, request); err != nil {
		return transitionParent{}, fmt.Errorf("stored parent patch receipt request is invalid: %w", errors.Join(ErrTransitionChainReceipt, err))
	}
	canonicalPatch, err := canonicalPatch(receipt.Patch)
	if err != nil {
		return transitionParent{}, fmt.Errorf("stored parent patch is not canonical: %w", errors.Join(ErrTransitionChainReceipt, err))
	}
	actualBytes, _ := json.Marshal(receipt.Patch)
	canonicalBytes, _ := json.Marshal(canonicalPatch)
	if !bytes.Equal(actualBytes, canonicalBytes) {
		return transitionParent{}, fmt.Errorf("stored parent patch is not canonical: %w", ErrTransitionChainReceipt)
	}
	step, ok := transitionStepForKind(receipt.Kind)
	if !ok {
		return transitionParent{}, fmt.Errorf("stored parent patch has an unsupported lifecycle kind: %w", ErrTransitionChainReceipt)
	}
	sourcePatch, err := sourcePatchFromCanonical(canonicalPatch)
	if err != nil {
		return transitionParent{}, fmt.Errorf("stored parent patch cannot be read: %w", errors.Join(ErrTransitionChainReceipt, err))
	}
	callerPatch, err := stripTransitionHeadPatch(sourcePatch)
	if err != nil || validateStoredTransitionHead(receipt, step, sourcePatch) != nil || validateTransitionStepPatch(step, callerPatch) != nil {
		return transitionParent{}, fmt.Errorf("stored parent patch does not match its lifecycle kind: %w", errors.Join(ErrTransitionChainReceipt, err))
	}
	// Historical receipts are checked against the same v2 contract and current
	// policy proof. Their source may since have advanced, so only fields carried
	// by each immutable patch receipt are checked here.
	if step == TransitionStepReservation || step == TransitionStepAttachedMaterialization {
		if !transitionPatchEvidenceMatchesMarker(step, sourcePatch, issueID, evidence) {
			return transitionParent{}, fmt.Errorf("stored parent patch differs from current admission proof: %w", ErrTransitionChainReceipt)
		}
	} else if step == TransitionStepRecoveryBudget {
		if err := validateStoredRecoveryBudget(sourcePatch, receipt.ReceiptID, issueID, scope, receipt.ExpectedVersion); err != nil {
			return transitionParent{}, err
		}
	} else if step == TransitionStepRecoveryEscalation {
		change := sourcePatch.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey]
		state, err := decodeCanonicalRecoveryState(change.Value, issueID, scope)
		if err != nil || state.Escalation == nil || state.Escalation.Request == nil {
			return transitionParent{}, fmt.Errorf("stored recovery escalation has no signed request: %w", errors.Join(ErrTransitionChainReceipt, err))
		}
		request := *state.Escalation.Request
		wantReceiptID, idErr := transitionReceiptID(issueID, scope, TransitionStepRecoveryEscalation, request.RequestID)
		if idErr != nil || wantReceiptID != receipt.ReceiptID || request.ExpectedRevision != receipt.ExpectedVersion {
			return transitionParent{}, fmt.Errorf("stored recovery escalation request is not bound to its deterministic receipt: %w", errors.Join(ErrTransitionChainReceipt, idErr))
		}
		if _, err := VerifyRecoveryEscalationRequestProof(request, admissionConfig); err != nil {
			return transitionParent{}, fmt.Errorf("stored recovery escalation authorization is invalid: %w", errors.Join(ErrTransitionChainReceipt, err))
		}
	} else if step == TransitionStepCompletionBudget {
		change := sourcePatch.Metadata[beadmeta.LifecycleCompletionBudgetMetadataKey]
		if _, err := parseCompletionBudgetMetadata(change.Value, issueID, scope, evidence.Attachment.ReceiptDigest); err != nil || change.Expected != nil {
			return transitionParent{}, fmt.Errorf("stored completion budget differs from current admission proof: %w", errors.Join(ErrTransitionChainReceipt, err))
		}
	}
	receipt.Patch = canonicalPatch
	digest, err := revisionTransitionPatchReceiptDigest(receipt)
	if err != nil {
		return transitionParent{}, fmt.Errorf("digest stored parent patch receipt: %w", errors.Join(ErrTransitionChainReceipt, err))
	}
	return transitionParent{ReceiptID: receipt.ReceiptID, Digest: digest, ToVersion: receipt.ToVersion, Step: step, Patch: sourcePatch}, nil
}

func transitionPatchEvidenceMatchesMarker(step TransitionStep, patch SourceWorkPatch, issueID string, evidence verifiedTransitionEvidence) bool {
	change, ok := patch.Metadata[beadmeta.LifecycleMaterializationMetadataKey]
	if !ok {
		return false
	}
	marker, err := parseTransitionMaterialization(change.Value)
	if err != nil || marker.Scope != evidence.Policy.SourceScope || marker.Scope != evidence.Attachment.Scope ||
		marker.Contract != evidence.Attachment.ReceiptDigest || marker.Route != evidence.Policy.Target.Identity ||
		marker.Workflow != evidence.Policy.Workflow || marker.MergeStrategy != evidence.Policy.MergeStrategy ||
		marker.SourceID != issueID || marker.AdmissionReceipt != evidence.AdmissionReceipt {
		return false
	}
	if step == TransitionStepReservation {
		return marker.State == "reserved" && marker.WorkflowID == ""
	}
	if step != TransitionStepAttachedMaterialization || marker.State != "attached" || marker.WorkflowID == "" {
		return false
	}
	routeKey, _ := exactlyOneTransitionKey(patch.Metadata, beadmeta.ExecutionRoutedToMetadataKey, beadmeta.RoutedToMetadataKey)
	workflowKey, _ := exactlyOneTransitionKey(patch.Metadata, beadmeta.MoleculeIDMetadataKey, beadmeta.WorkflowIDMetadataKey, beadmeta.LegacyWorkflowIDMetadataKey)
	return patch.Metadata[routeKey].Value == evidence.Policy.Target.Identity &&
		patch.Metadata[workflowKey].Value == marker.WorkflowID &&
		patch.Metadata[beadmeta.MergeStrategyMetadataKey].Value == evidence.Policy.MergeStrategy
}

func canonicalPatch(patch beads.RevisionTransitionIssuePatch) (beads.RevisionTransitionIssuePatch, error) {
	if patch.Labels != nil {
		return beads.RevisionTransitionIssuePatch{}, errors.New("source lifecycle chain does not patch labels")
	}
	canonical := beads.RevisionTransitionIssuePatch{Metadata: append([]beads.RevisionTransitionMetadataPatch(nil), patch.Metadata...)}
	sort.Slice(canonical.Metadata, func(i, j int) bool { return canonical.Metadata[i].Key < canonical.Metadata[j].Key })
	for index := range canonical.Metadata {
		entry := &canonical.Metadata[index]
		if !beadmeta.ValidKey(entry.Key) || (index > 0 && canonical.Metadata[index-1].Key == entry.Key) {
			return beads.RevisionTransitionIssuePatch{}, errors.New("metadata patch keys are invalid or duplicated")
		}
		var err error
		entry.Expected, err = canonicalMetadataString(entry.Expected)
		if err != nil {
			return beads.RevisionTransitionIssuePatch{}, fmt.Errorf("metadata key %q expected value is not a canonical string", entry.Key)
		}
		entry.Value, err = canonicalMetadataString(entry.Value)
		if err != nil || entry.Value == nil {
			return beads.RevisionTransitionIssuePatch{}, fmt.Errorf("metadata key %q next value is not a canonical string", entry.Key)
		}
	}
	if patch.Status != nil {
		value := *patch.Status
		canonical.Status = &value
	}
	if patch.Assignee != nil {
		value := *patch.Assignee
		canonical.Assignee = &value
	}
	return canonical, nil
}

func canonicalMetadataString(raw *json.RawMessage) (*json.RawMessage, error) {
	if raw == nil {
		return nil, nil
	}
	var value string
	if err := json.Unmarshal(*raw, &value); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value)
	if err != nil || !bytes.Equal(encoded, *raw) {
		return nil, errors.New("metadata string JSON is not canonical")
	}
	canonical := json.RawMessage(encoded)
	return &canonical, nil
}

func revisionTransitionPatchReceiptDigest(receipt beads.RevisionTransitionPatchReceipt) (string, error) {
	type canonicalReceipt struct {
		ReceiptID          string                             `json:"receipt_id"`
		IssueID            string                             `json:"issue_id"`
		Scope              string                             `json:"scope"`
		Kind               string                             `json:"kind"`
		Actor              string                             `json:"actor"`
		ExpectedVersion    string                             `json:"expected_version"`
		ToVersion          string                             `json:"to_version"`
		PriorReceiptID     string                             `json:"prior_receipt_id"`
		PriorReceiptDigest string                             `json:"prior_receipt_digest"`
		Patch              beads.RevisionTransitionIssuePatch `json:"patch"`
	}
	encoded, err := json.Marshal(canonicalReceipt{
		ReceiptID: receipt.ReceiptID, IssueID: receipt.IssueID, Scope: receipt.Scope,
		Kind: receipt.Kind, Actor: receipt.Actor,
		ExpectedVersion: strconv.FormatInt(receipt.ExpectedVersion, 10), ToVersion: strconv.FormatInt(receipt.ToVersion, 10),
		PriorReceiptID: receipt.PriorReceiptID, PriorReceiptDigest: receipt.PriorReceiptDigest,
		Patch: receipt.Patch,
	})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func controllerMetadataTransitionReceiptDigest(receipt beads.ControllerMetadataTransitionReceipt) (string, error) {
	type canonicalReceipt struct {
		ReceiptID       string          `json:"receipt_id"`
		IssueID         string          `json:"issue_id"`
		Scope           string          `json:"scope"`
		Kind            string          `json:"kind"`
		Actor           string          `json:"actor"`
		ExpectedVersion string          `json:"expected_version"`
		ToVersion       string          `json:"to_version"`
		Key             string          `json:"key"`
		Expected        json.RawMessage `json:"expected,omitempty"`
		Value           json.RawMessage `json:"value,omitempty"`
		Payload         json.RawMessage `json:"payload"`
	}
	canonical := canonicalReceipt{
		ReceiptID: receipt.ReceiptID, IssueID: receipt.IssueID, Scope: receipt.Scope,
		Kind: receipt.Kind, Actor: receipt.Actor,
		ExpectedVersion: strconv.FormatInt(receipt.ExpectedVersion, 10), ToVersion: strconv.FormatInt(receipt.ToVersion, 10),
		Key: receipt.Key, Payload: append(json.RawMessage(nil), receipt.Payload...),
	}
	if len(receipt.Expected) > 0 {
		canonical.Expected = append(json.RawMessage(nil), receipt.Expected...)
	}
	if len(receipt.Value) > 0 {
		canonical.Value = append(json.RawMessage(nil), receipt.Value...)
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func transitionReceiptID(issueID, scope string, step TransitionStep, operationID string) (string, error) {
	identity, err := json.Marshal(struct {
		IssueID     string         `json:"issue_id"`
		Scope       string         `json:"scope"`
		Step        TransitionStep `json:"step"`
		OperationID string         `json:"operation_id"`
	}{IssueID: issueID, Scope: scope, Step: step, OperationID: operationID})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(append([]byte(transitionReceiptDomain), identity...))
	return transitionReceiptPrefix + hex.EncodeToString(digest[:]), nil
}

func validTransitionStep(step TransitionStep) bool {
	return transitionStepKind(step) != ""
}

func transitionStepKind(step TransitionStep) string {
	switch step {
	case TransitionStepReservation:
		return transitionKindReservation
	case TransitionStepAttachedMaterialization:
		return transitionKindAttachedMaterialization
	case TransitionStepClaimIdentity:
		return transitionKindClaimIdentity
	case TransitionStepRecoveryBudget:
		return transitionKindRecoveryBudget
	case TransitionStepRecoveryEscalation:
		return transitionKindRecoveryEscalation
	case TransitionStepCompletionBudget:
		return transitionKindCompletionBudget
	case TransitionStepClose:
		return transitionKindClose
	default:
		return ""
	}
}

func transitionStepForKind(kind string) (TransitionStep, bool) {
	for _, step := range []TransitionStep{
		TransitionStepReservation, TransitionStepAttachedMaterialization, TransitionStepClaimIdentity,
		TransitionStepRecoveryBudget, TransitionStepRecoveryEscalation, TransitionStepCompletionBudget, TransitionStepClose,
	} {
		if transitionStepKind(step) == kind {
			return step, true
		}
	}
	return "", false
}

func sourcePatchFromCanonical(patch beads.RevisionTransitionIssuePatch) (SourceWorkPatch, error) {
	converted := SourceWorkPatch{Metadata: make(map[string]MetadataStringPatch, len(patch.Metadata))}
	for _, entry := range patch.Metadata {
		var value string
		if entry.Value == nil || json.Unmarshal(*entry.Value, &value) != nil {
			return SourceWorkPatch{}, ErrTransitionChainReceipt
		}
		change := MetadataStringPatch{Value: value}
		if entry.Expected != nil {
			var expected string
			if json.Unmarshal(*entry.Expected, &expected) != nil {
				return SourceWorkPatch{}, ErrTransitionChainReceipt
			}
			change.Expected = &expected
		}
		converted.Metadata[entry.Key] = change
	}
	if patch.Status != nil {
		value := *patch.Status
		converted.Status = &StringTransition{Expected: value.Expected, Value: value.Value}
	}
	if patch.Assignee != nil {
		value := *patch.Assignee
		converted.Assignee = &StringTransition{Expected: value.Expected, Value: value.Value}
	}
	return converted, nil
}

func validTransitionText(value string, max int) bool {
	if value == "" || !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') || utf8.RuneCountInString(value) > max {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validTransitionPermit(value string) bool {
	if value == "" || strings.TrimSpace(value) != value || len(value) > 65536 || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
