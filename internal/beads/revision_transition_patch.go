package beads

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// RevisionTransitionPatchProtectedMutationOperation is the exact operation
// name Beads verifies for a typed atomic issue patch.
const RevisionTransitionPatchProtectedMutationOperation = "issue.revision_transition_patch"

var (
	ErrRevisionTransitionPatchUnavailable     = errors.New("revision transition patch HTTP transport unavailable")
	ErrRevisionTransitionPatchProtocol        = errors.New("revision transition patch HTTP protocol unsupported")
	ErrRevisionTransitionPatchReceiptConflict = errors.New("revision transition patch receipt ID conflict")
	ErrRevisionTransitionPatchConflict        = ErrRevisionTransitionPatchReceiptConflict
	ErrRevisionTransitionPatchPrecondition    = errors.New("revision transition patch precondition failed")
	ErrRevisionTransitionPatchProtected       = errors.New("revision transition patch refused by protected-record policy")
)

// RevisionTransitionMetadataPatch compares and replaces one metadata value.
// A nil Expected or Value means the metadata key is absent; a raw JSON null is
// a present value.
type RevisionTransitionMetadataPatch struct {
	Key      string           `json:"key"`
	Expected *json.RawMessage `json:"expected,omitempty"`
	Value    *json.RawMessage `json:"value,omitempty"`
}

// RevisionTransitionStringPatch compares and replaces one string field.
type RevisionTransitionStringPatch struct {
	Expected string `json:"expected"`
	Value    string `json:"value"`
}

// RevisionTransitionLabelsPatch compares and replaces the complete label set.
// Both slices are canonical sorted sets.
type RevisionTransitionLabelsPatch struct {
	Expected []string `json:"expected"`
	Value    []string `json:"value"`
}

// RevisionTransitionIssuePatch is one canonical atomic patch to the issue
// source row. It may update metadata, status, assignee, and labels together.
type RevisionTransitionIssuePatch struct {
	Metadata []RevisionTransitionMetadataPatch `json:"metadata,omitempty"`
	Status   *RevisionTransitionStringPatch    `json:"status,omitempty"`
	Assignee *RevisionTransitionStringPatch    `json:"assignee,omitempty"`
	Labels   *RevisionTransitionLabelsPatch    `json:"labels,omitempty"`
}

// RevisionTransitionPatchRequest binds an atomic source patch to one exact
// prior receipt and source revision. ReceiptID must remain stable on retry.
// ProtectedPermit is an opaque signed capability passed to the Beads server;
// Gas City does not create or reinterpret it.
type RevisionTransitionPatchRequest struct {
	ReceiptID          string
	Scope              string
	Kind               string
	Actor              string
	ExpectedVersion    int64
	PriorReceiptID     string
	PriorReceiptDigest string
	Patch              RevisionTransitionIssuePatch
	ProtectedPermit    string
}

// RevisionTransitionPatchReceipt is the immutable backend receipt for one
// applied source patch. ToVersion is the backend-produced source revision.
type RevisionTransitionPatchReceipt struct {
	ReceiptID          string
	IssueID            string
	Scope              string
	Kind               string
	Actor              string
	ExpectedVersion    int64
	ToVersion          int64
	PriorReceiptID     string
	PriorReceiptDigest string
	Patch              RevisionTransitionIssuePatch
}

// RevisionTransitionPatchResult reports a new atomic patch or exact replay.
// A replay's ToVersion is the historical committed revision, not a claim about
// the row's current revision.
type RevisionTransitionPatchResult struct {
	Applied  bool
	Replayed bool
	Receipt  *RevisionTransitionPatchReceipt
}

// RevisionTransitionPatchWriter applies one atomic source patch through the
// configured protected Beads endpoint.
type RevisionTransitionPatchWriter interface {
	TransitionPatch(issueID string, request RevisionTransitionPatchRequest) (RevisionTransitionPatchResult, error)
}

// RevisionTransitionPatchReceiptReader reads one immutable exact patch receipt.
type RevisionTransitionPatchReceiptReader interface {
	ReadRevisionTransitionPatchReceipt(receiptID string) (RevisionTransitionPatchReceipt, bool, error)
}

// RevisionTransitionPatchWriterHandleProvider exposes this capability only
// when the wrapper preserves its write boundary and invalidation behavior.
type RevisionTransitionPatchWriterHandleProvider interface {
	RevisionTransitionPatchWriterHandle() (RevisionTransitionPatchWriter, bool)
}

// RevisionTransitionPatchReceiptReaderHandleProvider forwards exact receipt
// reads without placing receipts in the ordinary issue cache.
type RevisionTransitionPatchReceiptReaderHandleProvider interface {
	RevisionTransitionPatchReceiptReaderHandle() (RevisionTransitionPatchReceiptReader, bool)
}

// RevisionTransitionPatchWriterFor resolves only an explicit provider or
// direct implementation. It never unwraps a store behind a capability wrapper.
func RevisionTransitionPatchWriterFor(store Store) (RevisionTransitionPatchWriter, bool) {
	if store == nil {
		return nil, false
	}
	if provider, ok := store.(RevisionTransitionPatchWriterHandleProvider); ok {
		writer, available := provider.RevisionTransitionPatchWriterHandle()
		return writer, available && writer != nil
	}
	writer, ok := store.(RevisionTransitionPatchWriter)
	return writer, ok && writer != nil
}

// RevisionTransitionPatchReceiptReaderFor resolves only an explicit provider
// or direct implementation. It does not use a generic read or unwrap target.
func RevisionTransitionPatchReceiptReaderFor(store Store) (RevisionTransitionPatchReceiptReader, bool) {
	if store == nil {
		return nil, false
	}
	if provider, ok := store.(RevisionTransitionPatchReceiptReaderHandleProvider); ok {
		reader, available := provider.RevisionTransitionPatchReceiptReaderHandle()
		return reader, available && reader != nil
	}
	reader, ok := store.(RevisionTransitionPatchReceiptReader)
	return reader, ok && reader != nil
}

// RevisionTransitionPatchTransportReady reports whether the store has an
// explicitly configured patch endpoint whose workspace and route capabilities
// currently pass their authenticated readiness handshake.
type RevisionTransitionPatchTransportReady interface {
	RevisionTransitionPatchTransportReady() bool
}

// RevisionTransitionPatchPlan is the validated canonical request snapshot
// used by the HTTP adapter and exact receipt verifier.
type RevisionTransitionPatchPlan struct {
	ReceiptID          string
	IssueID            string
	Scope              string
	Kind               string
	Actor              string
	ExpectedVersion    int64
	PriorReceiptID     string
	PriorReceiptDigest string
	Patch              RevisionTransitionIssuePatch
}

// RevisionTransitionPatchProtectedMutationDigest returns the request digest
// a trusted host permit issuer must bind for this operation. It hashes the
// canonical planned request JSON Beads hashes, including the URL issue ID and
// excluding ProtectedPermit. The resource ID is issueID and the operation is
// RevisionTransitionPatchProtectedMutationOperation.
//
// ProtectedPermit may be empty while computing the digest. All other request
// fields must satisfy the same strict validation and canonicalization as the
// HTTP client.
func RevisionTransitionPatchProtectedMutationDigest(issueID string, request RevisionTransitionPatchRequest) (string, error) {
	plan, err := planRevisionTransitionPatchInternal(issueID, request, false)
	if err != nil {
		return "", fmt.Errorf("invalid revision transition patch for protected permit digest: %w", err)
	}
	type canonicalRequest struct {
		ReceiptID          string                       `json:"receipt_id"`
		IssueID            string                       `json:"issue_id"`
		Scope              string                       `json:"scope"`
		Kind               string                       `json:"kind"`
		Actor              string                       `json:"actor"`
		ExpectedVersion    int64                        `json:"expected_version"`
		PriorReceiptID     string                       `json:"prior_receipt_id"`
		PriorReceiptDigest string                       `json:"prior_receipt_digest"`
		Patch              RevisionTransitionIssuePatch `json:"patch"`
	}
	encoded, err := json.Marshal(canonicalRequest{
		ReceiptID: plan.ReceiptID, IssueID: plan.IssueID, Scope: plan.Scope,
		Kind: plan.Kind, Actor: plan.Actor, ExpectedVersion: plan.ExpectedVersion,
		PriorReceiptID: plan.PriorReceiptID, PriorReceiptDigest: plan.PriorReceiptDigest,
		Patch: plan.Patch,
	})
	if err != nil {
		return "", fmt.Errorf("encode canonical revision transition patch: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

const revisionTransitionPatchMaxPermitBytes = 65536

func planRevisionTransitionPatch(issueID string, request RevisionTransitionPatchRequest) (RevisionTransitionPatchPlan, error) {
	return planRevisionTransitionPatchInternal(issueID, request, true)
}

func planRevisionTransitionPatchInternal(issueID string, request RevisionTransitionPatchRequest, requirePermit bool) (RevisionTransitionPatchPlan, error) {
	if err := validateRevisionTransitionPatchReceiptID(request.ReceiptID); err != nil {
		return RevisionTransitionPatchPlan{}, err
	}
	if err := validateRevisionTransitionPatchReceiptID(request.PriorReceiptID); err != nil {
		return RevisionTransitionPatchPlan{}, fmt.Errorf("prior receipt ID: %w", err)
	}
	for _, field := range []struct {
		name  string
		value string
		limit int
	}{
		{"issue ID", issueID, controllerTransitionMaxIssueID},
		{"scope", request.Scope, controllerTransitionMaxScope},
		{"kind", request.Kind, controllerTransitionMaxKind},
		{"actor", request.Actor, controllerTransitionMaxActor},
	} {
		if err := validateRevisionTransitionPatchText(field.value, field.limit); err != nil || hasControllerTransitionControl(field.value) {
			return RevisionTransitionPatchPlan{}, fmt.Errorf("%s is invalid", field.name)
		}
	}
	if strings.TrimSpace(request.Actor) != request.Actor || len(request.Actor) > 256 {
		return RevisionTransitionPatchPlan{}, errors.New("actor is not canonical")
	}
	if request.ExpectedVersion == 0 {
		return RevisionTransitionPatchPlan{}, errors.New("expected version must be nonzero")
	}
	if len(request.PriorReceiptDigest) != sha256.Size*2 || strings.ToLower(request.PriorReceiptDigest) != request.PriorReceiptDigest {
		return RevisionTransitionPatchPlan{}, errors.New("prior receipt digest must be 64 lowercase hexadecimal characters")
	}
	if _, err := hex.DecodeString(request.PriorReceiptDigest); err != nil {
		return RevisionTransitionPatchPlan{}, errors.New("prior receipt digest must be 64 lowercase hexadecimal characters")
	}
	if requirePermit && (request.ProtectedPermit == "" || strings.TrimSpace(request.ProtectedPermit) != request.ProtectedPermit ||
		!utf8.ValidString(request.ProtectedPermit) || len(request.ProtectedPermit) > revisionTransitionPatchMaxPermitBytes ||
		hasControllerTransitionControl(request.ProtectedPermit)) {
		return RevisionTransitionPatchPlan{}, errors.New("protected permit is empty or invalid")
	}

	patch, err := canonicalRevisionTransitionIssuePatch(request.Patch)
	if err != nil {
		return RevisionTransitionPatchPlan{}, err
	}
	return RevisionTransitionPatchPlan{
		ReceiptID: request.ReceiptID, IssueID: issueID, Scope: request.Scope,
		Kind: request.Kind, Actor: request.Actor, ExpectedVersion: request.ExpectedVersion,
		PriorReceiptID: request.PriorReceiptID, PriorReceiptDigest: request.PriorReceiptDigest,
		Patch: patch,
	}, nil
}

func validateRevisionTransitionPatchReceipt(receipt RevisionTransitionPatchReceipt) error {
	if receipt.ToVersion == 0 || receipt.ToVersion == receipt.ExpectedVersion {
		return errors.New("receipt has invalid source versions")
	}
	request := RevisionTransitionPatchRequest{
		ReceiptID: receipt.ReceiptID, Scope: receipt.Scope, Kind: receipt.Kind, Actor: receipt.Actor,
		ExpectedVersion: receipt.ExpectedVersion, PriorReceiptID: receipt.PriorReceiptID,
		PriorReceiptDigest: receipt.PriorReceiptDigest, Patch: receipt.Patch,
	}
	plan, err := planRevisionTransitionPatchInternal(receipt.IssueID, request, false)
	if err != nil {
		return err
	}
	canonical, err := json.Marshal(plan.Patch)
	if err != nil {
		return err
	}
	actual, err := json.Marshal(receipt.Patch)
	if err != nil || !bytes.Equal(canonical, actual) {
		return errors.New("receipt patch is not canonical")
	}
	return nil
}

func canonicalRevisionTransitionIssuePatch(in RevisionTransitionIssuePatch) (RevisionTransitionIssuePatch, error) {
	plan := RevisionTransitionIssuePatch{}
	seenKeys := make(map[string]struct{}, len(in.Metadata))
	for _, item := range in.Metadata {
		if !beadmeta.ValidKey(item.Key) || utf8.RuneCountInString(item.Key) > controllerTransitionMaxKey || hasControllerTransitionControl(item.Key) {
			return RevisionTransitionIssuePatch{}, fmt.Errorf("metadata key %q is invalid", item.Key)
		}
		if _, exists := seenKeys[item.Key]; exists {
			return RevisionTransitionIssuePatch{}, fmt.Errorf("duplicate metadata patch key %q", item.Key)
		}
		seenKeys[item.Key] = struct{}{}
		expected, err := canonicalRevisionTransitionJSONPointer(item.Expected)
		if err != nil {
			return RevisionTransitionIssuePatch{}, fmt.Errorf("metadata key %q expected value is invalid", item.Key)
		}
		value, err := canonicalRevisionTransitionJSONPointer(item.Value)
		if err != nil {
			return RevisionTransitionIssuePatch{}, fmt.Errorf("metadata key %q next value is invalid", item.Key)
		}
		if controllerTransitionValuesEqual(expected, value) {
			return RevisionTransitionIssuePatch{}, fmt.Errorf("metadata key %q does not change", item.Key)
		}
		plan.Metadata = append(plan.Metadata, RevisionTransitionMetadataPatch{Key: item.Key, Expected: expected, Value: value})
	}
	sort.Slice(plan.Metadata, func(i, j int) bool { return plan.Metadata[i].Key < plan.Metadata[j].Key })

	if in.Status != nil {
		status := *in.Status
		if err := validateRevisionTransitionPatchText(status.Expected, 32); err != nil || hasControllerTransitionControl(status.Expected) {
			return RevisionTransitionIssuePatch{}, errors.New("expected status is invalid")
		}
		if err := validateRevisionTransitionPatchText(status.Value, 32); err != nil || hasControllerTransitionControl(status.Value) {
			return RevisionTransitionIssuePatch{}, errors.New("next status is invalid")
		}
		if status.Expected == status.Value {
			return RevisionTransitionIssuePatch{}, errors.New("status patch does not change")
		}
		plan.Status = &status
	}
	if in.Assignee != nil {
		assignee := *in.Assignee
		for _, value := range []string{assignee.Expected, assignee.Value} {
			if !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') || utf8.RuneCountInString(value) > 255 {
				return RevisionTransitionIssuePatch{}, errors.New("assignee patch value is invalid")
			}
		}
		if assignee.Expected == assignee.Value {
			return RevisionTransitionIssuePatch{}, errors.New("assignee patch does not change")
		}
		plan.Assignee = &assignee
	}
	if in.Labels != nil {
		labels := RevisionTransitionLabelsPatch{
			Expected: append([]string{}, in.Labels.Expected...),
			Value:    append([]string{}, in.Labels.Value...),
		}
		if err := canonicalRevisionTransitionPatchLabels(labels.Expected); err != nil {
			return RevisionTransitionIssuePatch{}, fmt.Errorf("expected labels are invalid: %w", err)
		}
		if err := canonicalRevisionTransitionPatchLabels(labels.Value); err != nil {
			return RevisionTransitionIssuePatch{}, fmt.Errorf("next labels are invalid: %w", err)
		}
		if equalRevisionTransitionPatchLabels(labels.Expected, labels.Value) {
			return RevisionTransitionIssuePatch{}, errors.New("labels patch does not change")
		}
		plan.Labels = &labels
	}
	if len(plan.Metadata) == 0 && plan.Status == nil && plan.Assignee == nil && plan.Labels == nil {
		return RevisionTransitionIssuePatch{}, errors.New("patch has no changes")
	}
	return plan, nil
}

func canonicalRevisionTransitionJSONPointer(raw *json.RawMessage) (*json.RawMessage, error) {
	if raw == nil {
		return nil, nil
	}
	canonical, err := canonicalControllerTransitionJSON(*raw)
	if err != nil {
		return nil, err
	}
	return &canonical, nil
}

func validateRevisionTransitionPatchText(value string, maxRunes int) error {
	if value == "" || !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') || utf8.RuneCountInString(value) > maxRunes {
		return errors.New("invalid text")
	}
	return nil
}

func validateRevisionTransitionPatchReceiptID(value string) error {
	if value == "" || len(value) > controllerTransitionMaxReceiptID || !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
		return errors.New("receipt ID is invalid")
	}
	return nil
}

func canonicalRevisionTransitionPatchLabels(labels []string) error {
	for _, label := range labels {
		if label == "" || !utf8.ValidString(label) || strings.ContainsRune(label, '\x00') || len(label) > 255 {
			return errors.New("labels must be nonempty valid text of at most 255 bytes")
		}
	}
	sort.Strings(labels)
	for i := 1; i < len(labels); i++ {
		if labels[i] == labels[i-1] {
			return fmt.Errorf("duplicate label %q", labels[i])
		}
	}
	return nil
}

func equalRevisionTransitionPatchLabels(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func sameRevisionTransitionPatchRequest(plan RevisionTransitionPatchPlan, receipt RevisionTransitionPatchReceipt) bool {
	if plan.ReceiptID != receipt.ReceiptID || plan.IssueID != receipt.IssueID || plan.Scope != receipt.Scope ||
		plan.Kind != receipt.Kind || plan.Actor != receipt.Actor || plan.ExpectedVersion != receipt.ExpectedVersion ||
		plan.PriorReceiptID != receipt.PriorReceiptID || plan.PriorReceiptDigest != receipt.PriorReceiptDigest {
		return false
	}
	planned, err := json.Marshal(plan.Patch)
	if err != nil {
		return false
	}
	actual, err := json.Marshal(receipt.Patch)
	return err == nil && bytes.Equal(planned, actual)
}
