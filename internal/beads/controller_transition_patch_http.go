package beads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
	"unicode/utf8"
)

const transitionPatchReceiptPath = "/v0/beads/issue-transition-patch-receipts/"

type revisionTransitionPatchWireRequest struct {
	ReceiptID          string                       `json:"receipt_id"`
	Scope              string                       `json:"scope"`
	Kind               string                       `json:"kind"`
	Actor              string                       `json:"actor"`
	ExpectedVersion    string                       `json:"expected_version"`
	PriorReceiptID     string                       `json:"prior_receipt_id"`
	PriorReceiptDigest string                       `json:"prior_receipt_digest"`
	Patch              RevisionTransitionIssuePatch `json:"patch"`
	ProtectedPermit    string                       `json:"protected_permit"`
}

// RevisionTransitionPatchProblem retains only the stable HTTP status and
// problem code. Server-provided detail, permit data, and receipt content are
// never included in its error text.
type RevisionTransitionPatchProblem struct {
	Status int
	Code   string
}

func (p *RevisionTransitionPatchProblem) Error() string {
	if p == nil {
		return "revision transition patch problem: <nil>"
	}
	return fmt.Sprintf("revision transition patch refused with HTTP %d (%s)", p.Status, p.Code)
}

func (p *RevisionTransitionPatchProblem) Unwrap() error {
	if p == nil {
		return nil
	}
	switch p.Code {
	case "transition_receipt_conflict":
		return ErrRevisionTransitionPatchConflict
	case "precondition_failed":
		return ErrRevisionTransitionPatchPrecondition
	case "protected_record":
		return ErrRevisionTransitionPatchProtected
	case "not_found":
		return ErrNotFound
	case "not_implemented", "unsupported_resource":
		return ErrRevisionTransitionPatchUnavailable
	default:
		return nil
	}
}

func (s *BdStore) RevisionTransitionPatchWriterHandle() (RevisionTransitionPatchWriter, bool) {
	if s == nil || s.privateEvidenceHTTP == nil || s.privateEvidenceHTTPInitErr != nil || !s.privateEvidenceHTTP.revisionTransitionPatches {
		return nil, false
	}
	return s, true
}

func (s *BdStore) RevisionTransitionPatchReceiptReaderHandle() (RevisionTransitionPatchReceiptReader, bool) {
	if s == nil || s.privateEvidenceHTTP == nil || s.privateEvidenceHTTPInitErr != nil || !s.privateEvidenceHTTP.revisionTransitionPatches {
		return nil, false
	}
	return s, true
}

// ValidateRevisionTransitionPatchTransport verifies the authenticated Beads
// workspace and all patch, receipt-read, and protected-mutation capabilities.
func (s *BdStore) ValidateRevisionTransitionPatchTransport(ctx context.Context) error {
	if s == nil || s.privateEvidenceHTTP == nil || s.privateEvidenceHTTPInitErr != nil || ctx == nil || !s.privateEvidenceHTTP.revisionTransitionPatches {
		return ErrRevisionTransitionPatchUnavailable
	}
	return s.privateEvidenceHTTP.verifyRevisionTransitionPatchContext(ctx)
}

// RevisionTransitionPatchTransportReady reports the current authenticated
// readiness of the explicitly configured atomic patch transport.
func (s *BdStore) RevisionTransitionPatchTransportReady() bool {
	ctx, cancel := context.WithTimeout(context.Background(), privateEvidenceHTTPTimeout)
	defer cancel()
	return s.ValidateRevisionTransitionPatchTransport(ctx) == nil
}

// TransitionPatch applies a typed patch through Beads and recovers uncertain
// responses only from the exact immutable receipt or by retrying the identical
// request body with the same receipt ID.
func (s *BdStore) TransitionPatch(issueID string, request RevisionTransitionPatchRequest) (RevisionTransitionPatchResult, error) {
	if s == nil || s.privateEvidenceHTTP == nil || s.privateEvidenceHTTPInitErr != nil || !s.privateEvidenceHTTP.revisionTransitionPatches {
		return RevisionTransitionPatchResult{}, ErrRevisionTransitionPatchUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(controllerTransitionMaxRequests)*privateEvidenceHTTPTimeout)
	defer cancel()
	return s.privateEvidenceHTTP.transitionPatch(ctx, issueID, request)
}

// ReadRevisionTransitionPatchReceipt reads and validates one exact durable
// patch receipt. A canonical not-found response is reported as found=false.
func (s *BdStore) ReadRevisionTransitionPatchReceipt(receiptID string) (RevisionTransitionPatchReceipt, bool, error) {
	if s == nil || s.privateEvidenceHTTP == nil || s.privateEvidenceHTTPInitErr != nil || !s.privateEvidenceHTTP.revisionTransitionPatches {
		return RevisionTransitionPatchReceipt{}, false, ErrRevisionTransitionPatchUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*privateEvidenceHTTPTimeout)
	defer cancel()
	return s.privateEvidenceHTTP.readTransitionPatchReceipt(ctx, receiptID)
}

func (c *privateEvidenceHTTPClient) verifyRevisionTransitionPatchContext(ctx context.Context) error {
	if c == nil || !c.revisionTransitionPatches {
		return ErrRevisionTransitionPatchUnavailable
	}
	response, err := c.readRevisionTransitionContext(ctx)
	if err != nil {
		return fmt.Errorf("%w: context handshake failed", ErrRevisionTransitionPatchUnavailable)
	}
	if err := c.verifyContextIdentity(response); err != nil {
		return fmt.Errorf("%w: configured workspace identity mismatch", ErrRevisionTransitionPatchProtocol)
	}
	for _, required := range []string{"issues.transitionPatch", "issues.transitionPatchReceipt.get", "issues.protectedMutation"} {
		if !containsString(response.Capabilities, required) {
			return fmt.Errorf("%w: Beads server lacks required capability %q", ErrRevisionTransitionPatchProtocol, required)
		}
	}
	return nil
}

func (c *privateEvidenceHTTPClient) transitionPatch(ctx context.Context, issueID string, request RevisionTransitionPatchRequest) (RevisionTransitionPatchResult, error) {
	plan, err := planRevisionTransitionPatch(issueID, request)
	if err != nil {
		return RevisionTransitionPatchResult{}, fmt.Errorf("%w: invalid patch request", ErrRevisionTransitionPatchProtocol)
	}
	if !c.patchTransitionScopeAllowed(plan.Scope, plan.Kind) {
		return RevisionTransitionPatchResult{}, fmt.Errorf("%w: patch scope and kind do not match the configured store", ErrRevisionTransitionPatchProtocol)
	}
	wire := revisionTransitionPatchWireRequest{
		ReceiptID: plan.ReceiptID, Scope: plan.Scope, Kind: plan.Kind, Actor: plan.Actor,
		ExpectedVersion: strconv.FormatInt(plan.ExpectedVersion, 10), PriorReceiptID: plan.PriorReceiptID,
		PriorReceiptDigest: plan.PriorReceiptDigest, Patch: plan.Patch,
		ProtectedPermit: request.ProtectedPermit,
	}
	body, err := json.Marshal(wire)
	if err != nil || len(body) > privateEvidenceHTTPMaxRequestBody {
		return RevisionTransitionPatchResult{}, fmt.Errorf("%w: patch request encoding exceeded limits", ErrRevisionTransitionPatchProtocol)
	}
	if err := c.verifyRevisionTransitionPatchContext(ctx); err != nil {
		return RevisionTransitionPatchResult{}, err
	}
	response, status, postErr := c.postTransitionPatch(ctx, issueID, body, true)
	if status >= 300 && status < 400 {
		return RevisionTransitionPatchResult{}, fmt.Errorf("%w: redirect refused", ErrRevisionTransitionPatchProtocol)
	}
	if postErr == nil && status == http.StatusOK {
		result, decodeErr := decodeRevisionTransitionPatchResult(response, plan)
		if decodeErr == nil {
			return result, nil
		}
	}
	if revisionTransitionPatchStatusDefinitive(status) {
		return RevisionTransitionPatchResult{}, revisionTransitionPatchStatusError(status, response)
	}
	return c.recoverTransitionPatch(ctx, issueID, plan, body, status)
}

func (c *privateEvidenceHTTPClient) postTransitionPatch(ctx context.Context, issueID string, body []byte, contextVerified bool) ([]byte, int, error) {
	if !contextVerified {
		if err := c.verifyRevisionTransitionPatchContext(ctx); err != nil {
			return nil, 0, err
		}
	}
	path := controllerTransitionPath + url.PathEscape(issueID) + ":transitionPatch"
	return c.requestWithResponseCaps(ctx, http.MethodPost, path, body,
		controllerTransitionMaxSuccessBody, controllerTransitionMaxProblemBody)
}

func (c *privateEvidenceHTTPClient) recoverTransitionPatch(ctx context.Context, issueID string, plan RevisionTransitionPatchPlan, body []byte, firstStatus int) (RevisionTransitionPatchResult, error) {
	receipt, found, err := c.readTransitionPatchReceipt(ctx, plan.ReceiptID)
	if err != nil {
		return RevisionTransitionPatchResult{}, fmt.Errorf("%w: exact receipt recovery failed", ErrRevisionTransitionPatchUnavailable)
	}
	if found {
		return transitionPatchReplayResult(receipt, plan)
	}
	if revisionTransitionPatchStatusDefinitive(firstStatus) {
		return RevisionTransitionPatchResult{}, revisionTransitionPatchStatusError(firstStatus, nil)
	}

	// Reuse byte-identical JSON and the same receipt ID. The backend's exact
	// replay contract arbitrates a retry racing with the original request.
	response, status, retryErr := c.postTransitionPatch(ctx, issueID, body, false)
	if status >= 300 && status < 400 {
		return RevisionTransitionPatchResult{}, fmt.Errorf("%w: redirect refused", ErrRevisionTransitionPatchProtocol)
	}
	if retryErr == nil && status == http.StatusOK {
		result, decodeErr := decodeRevisionTransitionPatchResult(response, plan)
		if decodeErr == nil {
			return result, nil
		}
	}

	receipt, found, err = c.readTransitionPatchReceipt(ctx, plan.ReceiptID)
	if err == nil && found {
		return transitionPatchReplayResult(receipt, plan)
	}
	if err == nil && revisionTransitionPatchStatusDefinitive(status) {
		return RevisionTransitionPatchResult{}, revisionTransitionPatchStatusError(status, response)
	}
	return RevisionTransitionPatchResult{}, fmt.Errorf("%w: patch outcome could not be established", ErrRevisionTransitionPatchUnavailable)
}

func transitionPatchReplayResult(receipt RevisionTransitionPatchReceipt, plan RevisionTransitionPatchPlan) (RevisionTransitionPatchResult, error) {
	if !sameRevisionTransitionPatchRequest(plan, receipt) {
		return RevisionTransitionPatchResult{}, ErrRevisionTransitionPatchConflict
	}
	return RevisionTransitionPatchResult{Applied: true, Replayed: true, Receipt: &receipt}, nil
}

func (c *privateEvidenceHTTPClient) readTransitionPatchReceipt(ctx context.Context, receiptID string) (RevisionTransitionPatchReceipt, bool, error) {
	if err := validateRevisionTransitionPatchReceiptID(receiptID); err != nil {
		return RevisionTransitionPatchReceipt{}, false, fmt.Errorf("%w: receipt ID is invalid", ErrRevisionTransitionPatchProtocol)
	}
	if err := c.verifyRevisionTransitionPatchContext(ctx); err != nil {
		return RevisionTransitionPatchReceipt{}, false, err
	}
	path := transitionPatchReceiptPath + url.PathEscape(receiptID)
	body, status, err := c.requestWithResponseCaps(ctx, http.MethodGet, path, nil,
		controllerTransitionMaxSuccessBody, controllerTransitionMaxProblemBody)
	if err != nil {
		return RevisionTransitionPatchReceipt{}, false, err
	}
	if status == http.StatusNotFound {
		problemErr := revisionTransitionPatchStatusError(status, body)
		if errors.Is(problemErr, ErrNotFound) {
			return RevisionTransitionPatchReceipt{}, false, nil
		}
		return RevisionTransitionPatchReceipt{}, false, problemErr
	}
	if status != http.StatusOK {
		return RevisionTransitionPatchReceipt{}, false, revisionTransitionPatchStatusError(status, body)
	}
	receipt, err := decodeRevisionTransitionPatchReceipt(body)
	if err != nil || receipt.ReceiptID != receiptID || !c.patchTransitionScopeAllowed(receipt.Scope, receipt.Kind) {
		return RevisionTransitionPatchReceipt{}, false, fmt.Errorf("%w: receipt response is malformed or outside the configured scope", ErrRevisionTransitionPatchProtocol)
	}
	return receipt, true, nil
}

func decodeRevisionTransitionPatchResult(data []byte, plan RevisionTransitionPatchPlan) (RevisionTransitionPatchResult, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil || members == nil || !controllerTransitionOnlyMembers(members, "applied", "replayed", "receipt") {
		return RevisionTransitionPatchResult{}, fmt.Errorf("%w: transition result is malformed", ErrRevisionTransitionPatchProtocol)
	}
	var result RevisionTransitionPatchResult
	if err := decodeControllerTransitionBool(members, "applied", &result.Applied); err != nil ||
		decodeControllerTransitionBool(members, "replayed", &result.Replayed) != nil {
		return RevisionTransitionPatchResult{}, fmt.Errorf("%w: transition result booleans are malformed", ErrRevisionTransitionPatchProtocol)
	}
	rawReceipt, ok := members["receipt"]
	if !ok || bytes.Equal(bytes.TrimSpace(rawReceipt), []byte("null")) {
		return RevisionTransitionPatchResult{}, fmt.Errorf("%w: applied transition omitted its receipt", ErrRevisionTransitionPatchProtocol)
	}
	receipt, err := decodeRevisionTransitionPatchReceipt(rawReceipt)
	if err != nil || !sameRevisionTransitionPatchRequest(plan, receipt) {
		return RevisionTransitionPatchResult{}, fmt.Errorf("%w: receipt does not match the submitted patch", ErrRevisionTransitionPatchProtocol)
	}
	if !result.Applied || result.Replayed && !result.Applied {
		return RevisionTransitionPatchResult{}, fmt.Errorf("%w: transition result is inconsistent", ErrRevisionTransitionPatchProtocol)
	}
	result.Receipt = &receipt
	return result, nil
}

func decodeRevisionTransitionPatchReceipt(data []byte) (RevisionTransitionPatchReceipt, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil || members == nil ||
		!controllerTransitionOnlyMembers(members, "receipt_id", "issue_id", "scope", "kind", "actor", "expected_version", "to_version", "prior_receipt_id", "prior_receipt_digest", "patch") {
		return RevisionTransitionPatchReceipt{}, errors.New("receipt object is malformed or contains unsupported fields")
	}
	var receipt RevisionTransitionPatchReceipt
	for name, destination := range map[string]*string{
		"receipt_id": &receipt.ReceiptID, "issue_id": &receipt.IssueID, "scope": &receipt.Scope,
		"kind": &receipt.Kind, "actor": &receipt.Actor, "prior_receipt_id": &receipt.PriorReceiptID,
		"prior_receipt_digest": &receipt.PriorReceiptDigest,
	} {
		if err := decodeControllerTransitionString(members, name, destination); err != nil {
			return RevisionTransitionPatchReceipt{}, err
		}
	}
	if err := decodeControllerTransitionRevision(members, "expected_version", &receipt.ExpectedVersion); err != nil {
		return RevisionTransitionPatchReceipt{}, err
	}
	if err := decodeControllerTransitionRevision(members, "to_version", &receipt.ToVersion); err != nil {
		return RevisionTransitionPatchReceipt{}, err
	}
	rawPatch, ok := members["patch"]
	if !ok {
		return RevisionTransitionPatchReceipt{}, errors.New("receipt omitted patch")
	}
	patch, err := decodeRevisionTransitionIssuePatch(rawPatch)
	if err != nil {
		return RevisionTransitionPatchReceipt{}, err
	}
	receipt.Patch = patch
	if err := validateRevisionTransitionPatchReceipt(receipt); err != nil {
		return RevisionTransitionPatchReceipt{}, err
	}
	return receipt, nil
}

func decodeRevisionTransitionIssuePatch(data []byte) (RevisionTransitionIssuePatch, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil || members == nil ||
		!controllerTransitionOnlyMembers(members, "metadata", "status", "assignee", "labels") {
		return RevisionTransitionIssuePatch{}, errors.New("patch is malformed or contains unsupported fields")
	}
	var patch RevisionTransitionIssuePatch
	if raw, ok := members["metadata"]; ok {
		var entries *[]json.RawMessage
		if err := json.Unmarshal(raw, &entries); err != nil || entries == nil || len(*entries) == 0 {
			return RevisionTransitionIssuePatch{}, errors.New("metadata patch must be a nonempty array")
		}
		for _, entryRaw := range *entries {
			var entry map[string]json.RawMessage
			if err := json.Unmarshal(entryRaw, &entry); err != nil || entry == nil ||
				!controllerTransitionOnlyMembers(entry, "key", "expected", "value") {
				return RevisionTransitionIssuePatch{}, errors.New("metadata patch entry is malformed")
			}
			key, ok := patchRequiredString(entry, "key")
			if !ok {
				return RevisionTransitionIssuePatch{}, errors.New("metadata patch key is missing")
			}
			patch.Metadata = append(patch.Metadata, RevisionTransitionMetadataPatch{
				Key: key, Expected: patchRawPointer(entry, "expected"), Value: patchRawPointer(entry, "value"),
			})
		}
	}
	for name, target := range map[string]**RevisionTransitionStringPatch{"status": &patch.Status, "assignee": &patch.Assignee} {
		if raw, ok := members[name]; ok {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil || fields == nil ||
				!controllerTransitionOnlyMembers(fields, "expected", "value") {
				return RevisionTransitionIssuePatch{}, fmt.Errorf("%s patch is malformed", name)
			}
			expected, expectedOK := patchRequiredString(fields, "expected")
			value, valueOK := patchRequiredString(fields, "value")
			if !expectedOK || !valueOK {
				return RevisionTransitionIssuePatch{}, fmt.Errorf("%s patch requires string expected and value members", name)
			}
			*target = &RevisionTransitionStringPatch{Expected: expected, Value: value}
		}
	}
	if raw, ok := members["labels"]; ok {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil || fields == nil ||
			!controllerTransitionOnlyMembers(fields, "expected", "value") {
			return RevisionTransitionIssuePatch{}, errors.New("labels patch is malformed")
		}
		expected, expectedOK := patchRequiredStringSlice(fields, "expected")
		value, valueOK := patchRequiredStringSlice(fields, "value")
		if !expectedOK || !valueOK {
			return RevisionTransitionIssuePatch{}, errors.New("labels patch requires expected and value arrays")
		}
		patch.Labels = &RevisionTransitionLabelsPatch{Expected: expected, Value: value}
	}
	return patch, nil
}

func patchRequiredString(members map[string]json.RawMessage, name string) (string, bool) {
	raw, ok := members[name]
	if !ok {
		return "", false
	}
	var value *string
	if err := json.Unmarshal(raw, &value); err != nil || value == nil || !validUTF8String(*value) {
		return "", false
	}
	return *value, true
}

func patchRequiredStringSlice(members map[string]json.RawMessage, name string) ([]string, bool) {
	raw, ok := members[name]
	if !ok {
		return nil, false
	}
	var values *[]string
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return nil, false
	}
	for _, value := range *values {
		if !validUTF8String(value) {
			return nil, false
		}
	}
	return *values, true
}

func patchRawPointer(members map[string]json.RawMessage, name string) *json.RawMessage {
	raw, ok := members[name]
	if !ok {
		return nil
	}
	copy := append(json.RawMessage(nil), raw...)
	return &copy
}

func validUTF8String(value string) bool {
	return utf8.ValidString(value)
}

func revisionTransitionPatchStatusDefinitive(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusConflict, http.StatusPreconditionFailed, http.StatusNotImplemented:
		return true
	default:
		return false
	}
}

func revisionTransitionPatchStatusError(status int, body []byte) error {
	var problem struct {
		Status int    `json:"status"`
		Code   string `json:"code"`
	}
	if len(body) == 0 || decodePrivateEvidenceJSON(body, &problem) != nil || problem.Status != status || !validControllerTransitionProblemCode(problem.Code) {
		return fmt.Errorf("%w: malformed HTTP %d problem response", ErrRevisionTransitionPatchProtocol, status)
	}
	return &RevisionTransitionPatchProblem{Status: status, Code: problem.Code}
}
