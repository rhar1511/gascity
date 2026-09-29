package worklifecycle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

const admissionAttachmentKind = "lifecycle_admission_v2_attach"

var (
	ErrAdmissionAttachmentUnavailable = errors.New("v2 admission attachment proof is unavailable")
	ErrAdmissionAttachmentInvalid     = errors.New("v2 admission attachment proof is invalid")
)

// AdmissionAttachmentProof identifies the immutable Q43 transition that
// attached one signed v2 receipt at its reviewed source revision.
type AdmissionAttachmentProof struct {
	SchemaVersion int    `json:"schema_version"`
	ReceiptID     string `json:"receipt_id"`
	WorkItemID    string `json:"work_item_id"`
	Scope         string `json:"scope"`
	ReceiptDigest string `json:"receipt_digest"`
	FromRevision  int64  `json:"from_revision"`
	ToRevision    int64  `json:"to_revision"`
}

type admissionAttachmentPayload struct {
	SchemaVersion int    `json:"schema_version"`
	WorkItemID    string `json:"work_item_id"`
	Scope         string `json:"scope"`
	ReceiptDigest string `json:"receipt_digest"`
	FromRevision  int64  `json:"from_revision"`
}

// AdmissionAttachmentAdapter composes the Q43 metadata transition with an
// authoritative source snapshot and exact durable receipt reader. It does not
// issue signatures or infer post-write revisions.
type AdmissionAttachmentAdapter struct {
	writer        beads.ControllerMetadataTransitionWriter
	sourceReader  beads.DecisionFrontierSourceReader
	receiptReader beads.ControllerMetadataTransitionReceiptReader
}

// NewAdmissionAttachmentAdapter returns a usable adapter only when the store
// exposes all three Q43 capabilities through safe outer handles.
func NewAdmissionAttachmentAdapter(store beads.Store) (*AdmissionAttachmentAdapter, error) {
	if store == nil {
		return nil, ErrAdmissionAttachmentUnavailable
	}
	writer, writerOK := beads.ControllerMetadataTransitionWriterFor(store)
	sourceReader, sourceOK := beads.DecisionFrontierSourceReaderFor(store)
	receiptReader, receiptOK := beads.ControllerMetadataTransitionReceiptReaderFor(store)
	if !writerOK || !sourceOK || !receiptOK || writer == nil || sourceReader == nil || receiptReader == nil {
		return nil, ErrAdmissionAttachmentUnavailable
	}
	return &AdmissionAttachmentAdapter{writer: writer, sourceReader: sourceReader, receiptReader: receiptReader}, nil
}

// Attach installs the exact canonical signed receipt using one Q43
// compare-and-transition at ExpectedWorkRevision. A v1 receipt, stale source,
// pre-existing v2 value, unsupported transition, or unverifiable response
// fails closed. Exact retries recover only the matching immutable receipt.
func (a *AdmissionAttachmentAdapter) Attach(encoded string, cfg config.LifecycleConfig, scope string) (beads.Bead, AdmissionAttachmentProof, error) {
	if a == nil || a.writer == nil || a.sourceReader == nil || a.receiptReader == nil {
		return beads.Bead{}, AdmissionAttachmentProof{}, ErrAdmissionAttachmentUnavailable
	}
	canonical, err := CanonicalAdmissionReceiptV2(encoded)
	if err != nil || canonical != encoded {
		return beads.Bead{}, AdmissionAttachmentProof{}, fmt.Errorf("%w: receipt encoding is not canonical", ErrAdmissionAttachmentInvalid)
	}
	var receipt AdmissionReceiptV2
	if err := decodeStrict(encoded, &receipt); err != nil {
		return beads.Bead{}, AdmissionAttachmentProof{}, fmt.Errorf("%w: receipt cannot be decoded", ErrAdmissionAttachmentInvalid)
	}
	if receipt.ExpectedWorkRevision == 0 || receipt.Scope != scope || receipt.WorkItemID == "" {
		return beads.Bead{}, AdmissionAttachmentProof{}, fmt.Errorf("%w: receipt has no exact reviewed identity or revision", ErrAdmissionAttachmentInvalid)
	}
	digest, err := AdmissionDigestV2(receipt)
	if err != nil {
		return beads.Bead{}, AdmissionAttachmentProof{}, fmt.Errorf("%w: receipt digest failed", ErrAdmissionAttachmentInvalid)
	}
	request, err := admissionAttachmentRequest(receipt, encoded, digest)
	if err != nil {
		return beads.Bead{}, AdmissionAttachmentProof{}, err
	}
	if prior, found, readErr := a.receiptReader.ControllerMetadataTransitionReceipt(receipt.WorkItemID, request.ReceiptID); readErr != nil {
		return beads.Bead{}, AdmissionAttachmentProof{}, fmt.Errorf("read prior admission attachment receipt: %w", readErr)
	} else if found {
		proof, verifyErr := verifyAdmissionAttachmentReceipt(prior, receipt.WorkItemID, request, digest)
		if verifyErr != nil {
			return beads.Bead{}, AdmissionAttachmentProof{}, verifyErr
		}
		current, snapshotErr := a.sourceReader.DecisionFrontierSourceSnapshot(receipt.WorkItemID)
		if snapshotErr != nil {
			return beads.Bead{}, AdmissionAttachmentProof{}, fmt.Errorf("read source after prior admission attachment: %w", snapshotErr)
		}
		if err := verifyAdmissionAttachmentCurrent(current, encoded, proof); err != nil {
			return beads.Bead{}, AdmissionAttachmentProof{}, err
		}
		if _, err := verifyAdmissionReceiptForSource(current, encoded, cfg, scope); err != nil {
			return beads.Bead{}, AdmissionAttachmentProof{}, err
		}
		return current, proof, nil
	}

	current, err := a.sourceReader.DecisionFrontierSourceSnapshot(receipt.WorkItemID)
	if err != nil {
		return beads.Bead{}, AdmissionAttachmentProof{}, fmt.Errorf("read reviewed source snapshot: %w", err)
	}
	if current.ID != receipt.WorkItemID || current.Revision != receipt.ExpectedWorkRevision || current.Revision == 0 ||
		current.Metadata[beadmeta.LifecycleAdmissionReceiptMetadataKey] != "" ||
		current.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey] != "" {
		return beads.Bead{}, AdmissionAttachmentProof{}, fmt.Errorf("%w: current source does not match the exact reviewed revision or has prior admission evidence", ErrAdmissionAttachmentInvalid)
	}
	if _, err := verifyAdmissionReceiptForSource(current, encoded, cfg, scope); err != nil {
		return beads.Bead{}, AdmissionAttachmentProof{}, err
	}

	result, transitionErr := a.writer.TransitionMetadata(receipt.WorkItemID, request)
	if transitionErr != nil && result.Receipt != nil && !controllerAdmissionTransitionResultMatches(result, receipt.WorkItemID, request) {
		return beads.Bead{}, AdmissionAttachmentProof{}, errors.Join(transitionErr, ErrAdmissionAttachmentInvalid)
	}
	actual, found, readErr := a.receiptReader.ControllerMetadataTransitionReceipt(receipt.WorkItemID, request.ReceiptID)
	if readErr != nil {
		return beads.Bead{}, AdmissionAttachmentProof{}, errors.Join(transitionErr, fmt.Errorf("read committed admission attachment receipt: %w", readErr))
	}
	if !found {
		if transitionErr != nil {
			return beads.Bead{}, AdmissionAttachmentProof{}, fmt.Errorf("apply Q43 admission attachment: %w", transitionErr)
		}
		return beads.Bead{}, AdmissionAttachmentProof{}, fmt.Errorf("%w: Q43 did not persist the exact transition receipt", ErrAdmissionAttachmentInvalid)
	}
	proof, err := verifyAdmissionAttachmentReceipt(actual, receipt.WorkItemID, request, digest)
	if err != nil {
		return beads.Bead{}, AdmissionAttachmentProof{}, errors.Join(transitionErr, err)
	}
	committed, err := a.sourceReader.DecisionFrontierSourceSnapshot(receipt.WorkItemID)
	if err != nil {
		return beads.Bead{}, AdmissionAttachmentProof{}, errors.Join(transitionErr, fmt.Errorf("read source after admission attachment: %w", err))
	}
	if err := verifyAdmissionAttachmentCurrent(committed, encoded, proof); err != nil {
		return beads.Bead{}, AdmissionAttachmentProof{}, errors.Join(transitionErr, err)
	}
	if transitionErr != nil {
		// A lost HTTP response is recoverable only because the exact durable
		// receipt and current source state re-establish the committed transition.
		return committed, proof, nil
	}
	if !result.Applied || !controllerAdmissionTransitionResultMatches(result, receipt.WorkItemID, request) {
		return beads.Bead{}, AdmissionAttachmentProof{}, fmt.Errorf("%w: Q43 returned a nonmatching transition result", ErrAdmissionAttachmentInvalid)
	}
	return committed, proof, nil
}

// VerifyCurrent re-reads the authoritative source row and requires its exact
// current revision to equal the backend-generated Q43 ToVersion. It verifies
// attachment only; it does not recompute route policy or authorize effects.
// Callers must combine it with the current policy projection and a proof for
// any later controller transition before reservation or materialization.
func (a *AdmissionAttachmentAdapter) VerifyCurrent(id string, cfg config.LifecycleConfig, scope string) (beads.Bead, AdmissionAttachmentProof, error) {
	if a == nil || a.sourceReader == nil || a.receiptReader == nil {
		return beads.Bead{}, AdmissionAttachmentProof{}, ErrAdmissionAttachmentUnavailable
	}
	current, err := a.sourceReader.DecisionFrontierSourceSnapshot(id)
	if err != nil {
		return beads.Bead{}, AdmissionAttachmentProof{}, fmt.Errorf("read current admission source: %w", err)
	}
	encoded := current.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey]
	canonical, err := CanonicalAdmissionReceiptV2(encoded)
	if err != nil || canonical != encoded {
		return beads.Bead{}, AdmissionAttachmentProof{}, fmt.Errorf("%w: current v2 receipt is missing or noncanonical", ErrAdmissionAttachmentInvalid)
	}
	var receipt AdmissionReceiptV2
	if err := decodeStrict(encoded, &receipt); err != nil || receipt.WorkItemID != id || receipt.Scope != scope {
		return beads.Bead{}, AdmissionAttachmentProof{}, fmt.Errorf("%w: current v2 receipt identity does not match", ErrAdmissionAttachmentInvalid)
	}
	if _, err := verifyAdmissionReceiptForSource(current, encoded, cfg, scope); err != nil {
		return beads.Bead{}, AdmissionAttachmentProof{}, err
	}
	digest, err := AdmissionDigestV2(receipt)
	if err != nil {
		return beads.Bead{}, AdmissionAttachmentProof{}, fmt.Errorf("%w: receipt digest failed", ErrAdmissionAttachmentInvalid)
	}
	request, err := admissionAttachmentRequest(receipt, encoded, digest)
	if err != nil {
		return beads.Bead{}, AdmissionAttachmentProof{}, err
	}
	actual, found, err := a.receiptReader.ControllerMetadataTransitionReceipt(id, request.ReceiptID)
	if err != nil || !found {
		if err != nil {
			return beads.Bead{}, AdmissionAttachmentProof{}, fmt.Errorf("read exact admission attachment receipt: %w", err)
		}
		return beads.Bead{}, AdmissionAttachmentProof{}, fmt.Errorf("%w: exact Q43 attachment receipt is absent", ErrAdmissionAttachmentInvalid)
	}
	proof, err := verifyAdmissionAttachmentReceipt(actual, id, request, digest)
	if err != nil {
		return beads.Bead{}, AdmissionAttachmentProof{}, err
	}
	if err := verifyAdmissionAttachmentCurrent(current, encoded, proof); err != nil {
		return beads.Bead{}, AdmissionAttachmentProof{}, err
	}
	return current, proof, nil
}

func admissionAttachmentRequest(receipt AdmissionReceiptV2, encoded, digest string) (beads.ControllerMetadataTransitionRequest, error) {
	payload, err := json.Marshal(admissionAttachmentPayload{
		SchemaVersion: 1, WorkItemID: receipt.WorkItemID, Scope: receipt.Scope,
		ReceiptDigest: digest, FromRevision: receipt.ExpectedWorkRevision,
	})
	if err != nil {
		return beads.ControllerMetadataTransitionRequest{}, err
	}
	valueBytes, err := json.Marshal(encoded)
	if err != nil {
		return beads.ControllerMetadataTransitionRequest{}, err
	}
	value := json.RawMessage(valueBytes)
	receiptID, err := admissionAttachmentReceiptID(payload, encoded)
	if err != nil {
		return beads.ControllerMetadataTransitionRequest{}, fmt.Errorf("encode deterministic admission attachment identity: %w", err)
	}
	return beads.ControllerMetadataTransitionRequest{
		ReceiptID: receiptID,
		Scope:     receipt.Scope, Kind: admissionAttachmentKind, Actor: receipt.AdmittedBy,
		ExpectedVersion: receipt.ExpectedWorkRevision, Key: beadmeta.LifecycleAdmissionReceiptV2MetadataKey,
		Value: &value, Payload: payload,
	}, nil
}

func admissionAttachmentReceiptID(payload []byte, encoded string) (string, error) {
	requestBinding, err := json.Marshal(struct {
		Payload json.RawMessage `json:"payload"`
		Value   string          `json:"value"`
	}{Payload: payload, Value: encoded})
	if err != nil {
		return "", err
	}
	input := append([]byte("gascity.lifecycle.admission_attachment.v2\n"), requestBinding...)
	sum := sha256.Sum256(input)
	return "admission-v2-" + hex.EncodeToString(sum[:]), nil
}

func verifyAdmissionReceiptForSource(current beads.Bead, encoded string, cfg config.LifecycleConfig, scope string) (AdmissionReceiptV2, error) {
	check := current
	check.Metadata = cloneStringMap(current.Metadata)
	check.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey] = encoded
	receipt, err := VerifyAdmissionReceiptV2(check, cfg, scope)
	if err != nil {
		return AdmissionReceiptV2{}, errors.Join(ErrAdmissionAttachmentInvalid, err)
	}
	return receipt, nil
}

func verifyAdmissionAttachmentReceipt(actual beads.ControllerMetadataTransitionReceipt, issueID string, request beads.ControllerMetadataTransitionRequest, digest string) (AdmissionAttachmentProof, error) {
	if actual.ReceiptID != request.ReceiptID || actual.IssueID != issueID || actual.Scope != request.Scope ||
		actual.Kind != request.Kind || actual.Actor != request.Actor || actual.ExpectedVersion != request.ExpectedVersion ||
		actual.ToVersion == 0 || actual.ToVersion == actual.ExpectedVersion || actual.Key != request.Key ||
		len(actual.Expected) != 0 || request.Expected != nil || request.Value == nil ||
		!bytes.Equal(actual.Value, *request.Value) || !bytes.Equal(actual.Payload, request.Payload) {
		return AdmissionAttachmentProof{}, fmt.Errorf("%w: durable Q43 receipt does not match the exact v2 attachment request", ErrAdmissionAttachmentInvalid)
	}
	return AdmissionAttachmentProof{
		SchemaVersion: 1, ReceiptID: actual.ReceiptID, WorkItemID: actual.IssueID, Scope: actual.Scope,
		ReceiptDigest: digest, FromRevision: actual.ExpectedVersion, ToRevision: actual.ToVersion,
	}, nil
}

func verifyAdmissionAttachmentCurrent(current beads.Bead, encoded string, proof AdmissionAttachmentProof) error {
	if current.ID != proof.WorkItemID || current.Revision != proof.ToRevision || current.Revision == 0 ||
		current.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey] != encoded ||
		current.Metadata[beadmeta.LifecycleAdmissionReceiptMetadataKey] != "" {
		return fmt.Errorf("%w: current source row does not match exact Q43 ToVersion and receipt bytes", ErrAdmissionAttachmentInvalid)
	}
	return nil
}

func controllerAdmissionTransitionResultMatches(result beads.ControllerMetadataTransitionResult, issueID string, request beads.ControllerMetadataTransitionRequest) bool {
	if result.Receipt == nil {
		return false
	}
	actual := result.Receipt
	return actual.ReceiptID == request.ReceiptID && actual.IssueID == issueID && actual.Scope == request.Scope &&
		actual.Kind == request.Kind && actual.Actor == request.Actor && actual.ExpectedVersion == request.ExpectedVersion &&
		actual.ToVersion != 0 && actual.ToVersion != actual.ExpectedVersion && actual.Key == request.Key &&
		len(actual.Expected) == 0 && request.Expected == nil && request.Value != nil &&
		bytes.Equal(actual.Value, *request.Value) && bytes.Equal(actual.Payload, request.Payload)
}

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return map[string]string{}
	}
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}
