package beads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

const (
	controllerTransitionPath        = "/v0/beads/issues/"
	controllerTransitionReceiptPath = "/v0/beads/issue-transition-receipts/"
)

const controllerTransitionMaxProtectedPermitBytes = 65536

type controllerTransitionWireRequest struct {
	ReceiptID       string           `json:"receipt_id"`
	Scope           string           `json:"scope"`
	Kind            string           `json:"kind"`
	Actor           string           `json:"actor"`
	ExpectedVersion string           `json:"expected_version"`
	Key             string           `json:"key"`
	Expected        *json.RawMessage `json:"expected,omitempty"`
	Value           *json.RawMessage `json:"value,omitempty"`
	Payload         json.RawMessage  `json:"payload,omitempty"`
	ProtectedPermit string           `json:"protected_permit,omitempty"`
}

type controllerTransitionPlan struct {
	issueID string
	request ControllerMetadataTransitionRequest
	payload json.RawMessage
}

func (c *privateEvidenceHTTPClient) transitionMetadata(ctx context.Context, issueID string, request ControllerMetadataTransitionRequest) (ControllerMetadataTransitionResult, error) {
	plan, err := planControllerMetadataTransition(issueID, request)
	if err != nil {
		return ControllerMetadataTransitionResult{}, err
	}
	if plan.request.Scope != c.scopeRef {
		return ControllerMetadataTransitionResult{}, controllerTransitionProtocolError("transition scope does not match the configured store")
	}
	body, err := encodeControllerMetadataTransition(plan)
	if err != nil {
		return ControllerMetadataTransitionResult{}, controllerTransitionProtocolError("request could not be encoded")
	}
	protectedPermitContextVerified := false
	if plan.request.ProtectedPermit != "" {
		if err := c.verifyRevisionTransitionContext(ctx, true); err != nil {
			return ControllerMetadataTransitionResult{}, err
		}
		protectedPermitContextVerified = true
	}
	result, status, postErr := c.postControllerMetadataTransition(ctx, plan, body, protectedPermitContextVerified)
	if status >= 300 && status < 400 {
		return ControllerMetadataTransitionResult{}, controllerTransitionProtocolError("redirect refused")
	}
	if postErr == nil && status == http.StatusOK {
		decoded, decodeErr := decodeControllerMetadataTransitionResult(result, plan)
		if decodeErr == nil {
			return decoded, nil
		}
	}
	if controllerTransitionStatusIsDefinitive(status) {
		return ControllerMetadataTransitionResult{}, controllerTransitionStatusError(status, result)
	}
	return c.recoverAmbiguousControllerTransition(ctx, plan, body, status)
}

func (c *privateEvidenceHTTPClient) postControllerMetadataTransition(ctx context.Context, plan controllerTransitionPlan, body []byte, contextVerified bool) ([]byte, int, error) {
	if !contextVerified {
		if err := c.verifyRevisionTransitionContext(ctx, plan.request.ProtectedPermit != ""); err != nil {
			return nil, 0, err
		}
	}
	path := controllerTransitionPath + url.PathEscape(plan.issueID) + ":transitionMetadata"
	response, status, err := c.requestWithResponseCaps(ctx, http.MethodPost, path, body,
		controllerTransitionMaxSuccessBody, controllerTransitionMaxProblemBody)
	if err != nil {
		return nil, status, controllerTransitionProtocolError("transition response was unavailable or invalid")
	}
	return response, status, nil
}

func (c *privateEvidenceHTTPClient) recoverAmbiguousControllerTransition(ctx context.Context, plan controllerTransitionPlan, body []byte, status int) (ControllerMetadataTransitionResult, error) {
	receipt, found, err := c.getControllerTransitionReceipt(ctx, plan)
	if err != nil {
		return ControllerMetadataTransitionResult{}, controllerTransitionProtocolError("receipt recovery failed")
	}
	if found {
		return recoveredControllerTransitionResult(receipt), nil
	}
	if controllerTransitionStatusIsDefinitive(status) {
		return ControllerMetadataTransitionResult{}, controllerTransitionStatusError(status, nil)
	}

	// A not-found receipt is the one safe point to retry. The receipt ID and
	// canonical request body stay byte-for-byte stable so an in-flight first
	// request converges with this bounded retry.
	resultBody, retryStatus, retryErr := c.postControllerMetadataTransition(ctx, plan, body, false)
	if retryStatus >= 300 && retryStatus < 400 {
		return ControllerMetadataTransitionResult{}, controllerTransitionProtocolError("redirect refused")
	}
	if retryErr == nil && retryStatus == http.StatusOK {
		result, decodeErr := decodeControllerMetadataTransitionResult(resultBody, plan)
		if decodeErr == nil && result.Applied {
			return result, nil
		}
	}
	receipt, found, err = c.getControllerTransitionReceipt(ctx, plan)
	if err == nil && found {
		return recoveredControllerTransitionResult(receipt), nil
	}
	return ControllerMetadataTransitionResult{}, controllerTransitionProtocolError("transition outcome could not be established")
}

func (c *privateEvidenceHTTPClient) getControllerTransitionReceipt(ctx context.Context, plan controllerTransitionPlan) (*ControllerMetadataTransitionReceipt, bool, error) {
	if err := c.verifyRevisionTransitionContext(ctx, plan.request.ProtectedPermit != ""); err != nil {
		return nil, false, err
	}
	path := controllerTransitionReceiptPath + url.PathEscape(plan.request.ReceiptID)
	body, status, err := c.requestWithResponseCaps(ctx, http.MethodGet, path, nil,
		controllerTransitionMaxSuccessBody, controllerTransitionMaxProblemBody)
	if err != nil {
		return nil, false, err
	}
	if status == http.StatusNotFound {
		var problem struct {
			Code string `json:"code"`
		}
		if decodePrivateEvidenceJSON(body, &problem) == nil && problem.Code == "not_found" {
			return nil, false, nil
		}
		return nil, false, controllerTransitionProtocolError("receipt lookup returned an unrecognized not-found response")
	}
	if status != http.StatusOK {
		return nil, false, controllerTransitionStatusError(status, body)
	}
	receipt, err := decodeControllerMetadataTransitionReceipt(body)
	if err != nil || !controllerTransitionReceiptMatchesPlan(receipt, plan) {
		return nil, false, controllerTransitionReceiptConflictError()
	}
	return &receipt, true, nil
}

func planControllerMetadataTransition(issueID string, request ControllerMetadataTransitionRequest) (controllerTransitionPlan, error) {
	request.Actor = strings.TrimSpace(request.Actor)
	if err := validateControllerTransitionText(issueID, controllerTransitionMaxIssueID); err != nil || hasControllerTransitionControl(issueID) {
		return controllerTransitionPlan{}, controllerTransitionProtocolError("issue ID is invalid")
	}
	if request.ReceiptID == "" || len(request.ReceiptID) > controllerTransitionMaxReceiptID || !utf8.ValidString(request.ReceiptID) || strings.ContainsRune(request.ReceiptID, '\x00') {
		return controllerTransitionPlan{}, controllerTransitionProtocolError("receipt ID is invalid")
	}
	if err := validateControllerTransitionText(request.Scope, controllerTransitionMaxScope); err != nil {
		return controllerTransitionPlan{}, controllerTransitionProtocolError("scope is invalid")
	}
	if err := validateControllerTransitionText(request.Kind, controllerTransitionMaxKind); err != nil {
		return controllerTransitionPlan{}, controllerTransitionProtocolError("kind is invalid")
	}
	if err := validateControllerTransitionText(request.Actor, controllerTransitionMaxActor); err != nil || len(request.Actor) > 256 || hasControllerTransitionControl(request.Actor) {
		return controllerTransitionPlan{}, controllerTransitionProtocolError("actor is invalid")
	}
	if request.ProtectedPermit != "" && (len(request.ProtectedPermit) > controllerTransitionMaxProtectedPermitBytes ||
		!utf8.ValidString(request.ProtectedPermit) || strings.TrimSpace(request.ProtectedPermit) != request.ProtectedPermit ||
		hasControllerTransitionControl(request.ProtectedPermit)) {
		return controllerTransitionPlan{}, controllerTransitionProtocolError("protected permit is invalid")
	}
	if request.ExpectedVersion == 0 {
		return controllerTransitionPlan{}, controllerTransitionProtocolError("expected revision is invalid")
	}
	if err := validateControllerTransitionText(request.Key, controllerTransitionMaxKey); err != nil || !beadmeta.ValidKey(request.Key) {
		return controllerTransitionPlan{}, controllerTransitionProtocolError("metadata key is invalid")
	}
	expected, err := canonicalControllerTransitionPointer(request.Expected)
	if err != nil {
		return controllerTransitionPlan{}, controllerTransitionProtocolError("expected marker is invalid")
	}
	value, err := canonicalControllerTransitionPointer(request.Value)
	if err != nil {
		return controllerTransitionPlan{}, controllerTransitionProtocolError("next marker is invalid")
	}
	if controllerTransitionValuesEqual(expected, value) {
		return controllerTransitionPlan{}, controllerTransitionProtocolError("transition does not change the marker")
	}
	payload := json.RawMessage(`{}`)
	if len(request.Payload) != 0 {
		payload, err = canonicalControllerTransitionJSON(request.Payload)
		if err != nil {
			return controllerTransitionPlan{}, controllerTransitionProtocolError("receipt payload is invalid")
		}
		request.Payload = append(json.RawMessage(nil), payload...)
	}
	request.Expected = expected
	request.Value = value
	return controllerTransitionPlan{issueID: issueID, request: request, payload: payload}, nil
}

func validateControllerTransitionText(value string, maxRunes int) error {
	if value == "" || !utf8.ValidString(value) || utf8.RuneCountInString(value) > maxRunes {
		return errors.New("invalid text")
	}
	return nil
}

func hasControllerTransitionControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r >= 0x7f && r <= 0x9f || r == '\u2028' || r == '\u2029' {
			return true
		}
	}
	return false
}

func canonicalControllerTransitionPointer(raw *json.RawMessage) (*json.RawMessage, error) {
	if raw == nil {
		return nil, nil
	}
	canonical, err := canonicalControllerTransitionJSON(*raw)
	if err != nil {
		return nil, err
	}
	return &canonical, nil
}

func canonicalControllerTransitionJSON(raw json.RawMessage) (json.RawMessage, error) {
	if !json.Valid(raw) {
		return nil, errors.New("invalid JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var canonical bytes.Buffer
	if err := writeControllerTransitionJSON(&canonical, value); err != nil {
		return nil, err
	}
	return json.RawMessage(canonical.Bytes()), nil
}

func writeControllerTransitionJSON(dst *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		dst.WriteString("null")
	case bool:
		if typed {
			dst.WriteString("true")
		} else {
			dst.WriteString("false")
		}
	case json.Number:
		dst.WriteString(typed.String())
	case string:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return err
		}
		dst.Write(encoded)
	case []any:
		dst.WriteByte('[')
		for i, item := range typed {
			if i > 0 {
				dst.WriteByte(',')
			}
			if err := writeControllerTransitionJSON(dst, item); err != nil {
				return err
			}
		}
		dst.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		dst.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				dst.WriteByte(',')
			}
			encoded, err := json.Marshal(key)
			if err != nil {
				return err
			}
			dst.Write(encoded)
			dst.WriteByte(':')
			if err := writeControllerTransitionJSON(dst, typed[key]); err != nil {
				return err
			}
		}
		dst.WriteByte('}')
	default:
		return fmt.Errorf("unsupported JSON value")
	}
	return nil
}

func controllerTransitionValuesEqual(left, right *json.RawMessage) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return bytes.Equal(*left, *right)
}

func encodeControllerMetadataTransition(plan controllerTransitionPlan) ([]byte, error) {
	request := controllerTransitionWireRequest{
		ReceiptID:       plan.request.ReceiptID,
		Scope:           plan.request.Scope,
		Kind:            plan.request.Kind,
		Actor:           plan.request.Actor,
		ExpectedVersion: strconv.FormatInt(plan.request.ExpectedVersion, 10),
		Key:             plan.request.Key,
		Expected:        plan.request.Expected,
		Value:           plan.request.Value,
		ProtectedPermit: plan.request.ProtectedPermit,
	}
	if len(plan.request.Payload) != 0 {
		request.Payload = plan.request.Payload
	}
	return json.Marshal(request)
}

func decodeControllerMetadataTransitionResult(data []byte, plan controllerTransitionPlan) (ControllerMetadataTransitionResult, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil || members == nil {
		return ControllerMetadataTransitionResult{}, controllerTransitionProtocolError("transition result is malformed")
	}
	if !controllerTransitionOnlyMembers(members, "applied", "replayed", "current", "receipt") {
		return ControllerMetadataTransitionResult{}, controllerTransitionProtocolError("transition result has unsupported members")
	}
	var result ControllerMetadataTransitionResult
	if err := decodeControllerTransitionBool(members, "applied", &result.Applied); err != nil {
		return result, controllerTransitionProtocolError("transition result is malformed")
	}
	if err := decodeControllerTransitionBool(members, "replayed", &result.Replayed); err != nil {
		return result, controllerTransitionProtocolError("transition result is malformed")
	}
	if current, ok := members["current"]; ok {
		if !json.Valid(current) {
			return result, controllerTransitionProtocolError("transition result is malformed")
		}
		result.Current = append(json.RawMessage(nil), current...)
	}
	if raw, ok := members["receipt"]; ok {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return result, controllerTransitionProtocolError("transition result is inconsistent")
		}
		receipt, err := decodeControllerMetadataTransitionReceipt(raw)
		if err != nil || !controllerTransitionReceiptMatchesPlan(receipt, plan) {
			return result, controllerTransitionReceiptConflictError()
		}
		result.Receipt = &receipt
	}
	if result.Replayed && !result.Applied || result.Applied && result.Receipt == nil || !result.Applied && result.Receipt != nil {
		return result, controllerTransitionProtocolError("transition result is inconsistent")
	}
	return result, nil
}

func decodeControllerMetadataTransitionReceipt(data []byte) (ControllerMetadataTransitionReceipt, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil || members == nil {
		return ControllerMetadataTransitionReceipt{}, errors.New("malformed receipt")
	}
	if !controllerTransitionOnlyMembers(members, "receipt_id", "issue_id", "scope", "kind", "actor", "expected_version", "to_version", "key", "expected", "value", "payload") {
		return ControllerMetadataTransitionReceipt{}, errors.New("receipt has unsupported members")
	}
	var receipt ControllerMetadataTransitionReceipt
	for name, destination := range map[string]*string{
		"receipt_id": &receipt.ReceiptID, "issue_id": &receipt.IssueID, "scope": &receipt.Scope,
		"kind": &receipt.Kind, "actor": &receipt.Actor, "key": &receipt.Key,
	} {
		if err := decodeControllerTransitionString(members, name, destination); err != nil {
			return ControllerMetadataTransitionReceipt{}, err
		}
	}
	if err := decodeControllerTransitionRevision(members, "expected_version", &receipt.ExpectedVersion); err != nil {
		return ControllerMetadataTransitionReceipt{}, err
	}
	if err := decodeControllerTransitionRevision(members, "to_version", &receipt.ToVersion); err != nil {
		return ControllerMetadataTransitionReceipt{}, err
	}
	if raw, ok := members["expected"]; ok {
		if !json.Valid(raw) {
			return ControllerMetadataTransitionReceipt{}, errors.New("invalid expected marker")
		}
		receipt.Expected = append(json.RawMessage(nil), raw...)
	}
	if raw, ok := members["value"]; ok {
		if !json.Valid(raw) {
			return ControllerMetadataTransitionReceipt{}, errors.New("invalid value marker")
		}
		receipt.Value = append(json.RawMessage(nil), raw...)
	}
	payload, ok := members["payload"]
	if !ok || !json.Valid(payload) {
		return ControllerMetadataTransitionReceipt{}, errors.New("missing payload")
	}
	receipt.Payload = append(json.RawMessage(nil), payload...)
	if err := validateControllerMetadataTransitionReceipt(receipt); err != nil {
		return ControllerMetadataTransitionReceipt{}, err
	}
	return receipt, nil
}

func validateControllerMetadataTransitionReceipt(receipt ControllerMetadataTransitionReceipt) error {
	if receipt.ReceiptID == "" || len(receipt.ReceiptID) > controllerTransitionMaxReceiptID || !utf8.ValidString(receipt.ReceiptID) || strings.ContainsRune(receipt.ReceiptID, '\x00') {
		return errors.New("invalid receipt ID")
	}
	for _, field := range []struct {
		value string
		limit int
	}{{receipt.IssueID, controllerTransitionMaxIssueID}, {receipt.Scope, controllerTransitionMaxScope}, {receipt.Kind, controllerTransitionMaxKind}, {receipt.Actor, controllerTransitionMaxActor}, {receipt.Key, controllerTransitionMaxKey}} {
		if validateControllerTransitionText(field.value, field.limit) != nil {
			return errors.New("invalid receipt text")
		}
	}
	if strings.TrimSpace(receipt.Actor) != receipt.Actor || len(receipt.Actor) > 256 || hasControllerTransitionControl(receipt.Actor) || !beadmeta.ValidKey(receipt.Key) {
		return errors.New("invalid receipt actor or key")
	}
	if receipt.ExpectedVersion == 0 || receipt.ToVersion == 0 || receipt.ToVersion == receipt.ExpectedVersion {
		return errors.New("invalid receipt revisions")
	}
	expected, err := canonicalControllerTransitionPointer(rawControllerTransitionPointer(receipt.Expected))
	if err != nil || !bytes.Equal(nilSafeRaw(expected), nilSafeRaw(rawControllerTransitionPointer(receipt.Expected))) {
		return errors.New("invalid expected marker")
	}
	value, err := canonicalControllerTransitionPointer(rawControllerTransitionPointer(receipt.Value))
	if err != nil || !bytes.Equal(nilSafeRaw(value), nilSafeRaw(rawControllerTransitionPointer(receipt.Value))) {
		return errors.New("invalid value marker")
	}
	if controllerTransitionValuesEqual(expected, value) {
		return errors.New("unchanged marker")
	}
	payload, err := canonicalControllerTransitionJSON(receipt.Payload)
	if err != nil || !bytes.Equal(payload, receipt.Payload) {
		return errors.New("invalid payload")
	}
	return nil
}

func rawControllerTransitionPointer(raw json.RawMessage) *json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	clone := append(json.RawMessage(nil), raw...)
	return &clone
}

func nilSafeRaw(raw *json.RawMessage) []byte {
	if raw == nil {
		return nil
	}
	return *raw
}

func controllerTransitionReceiptMatchesPlan(receipt ControllerMetadataTransitionReceipt, plan controllerTransitionPlan) bool {
	if receipt.ReceiptID != plan.request.ReceiptID || receipt.IssueID != plan.issueID || receipt.Scope != plan.request.Scope ||
		receipt.Kind != plan.request.Kind || receipt.Actor != plan.request.Actor || receipt.ExpectedVersion != plan.request.ExpectedVersion ||
		receipt.Key != plan.request.Key || !bytes.Equal(nilSafeRaw(rawControllerTransitionPointer(receipt.Expected)), nilSafeRaw(plan.request.Expected)) ||
		!bytes.Equal(nilSafeRaw(rawControllerTransitionPointer(receipt.Value)), nilSafeRaw(plan.request.Value)) {
		return false
	}
	return bytes.Equal(receipt.Payload, plan.payload)
}

func decodeControllerTransitionBool(members map[string]json.RawMessage, name string, destination *bool) error {
	raw, ok := members[name]
	if !ok {
		return errors.New("missing boolean")
	}
	var value *bool
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return errors.New("invalid boolean")
	}
	*destination = *value
	return nil
}

func decodeControllerTransitionString(members map[string]json.RawMessage, name string, destination *string) error {
	raw, ok := members[name]
	if !ok {
		return errors.New("missing string")
	}
	var value *string
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return errors.New("invalid string")
	}
	*destination = *value
	return nil
}

func decodeControllerTransitionRevision(members map[string]json.RawMessage, name string, destination *int64) error {
	var token string
	if err := decodeControllerTransitionString(members, name, &token); err != nil {
		return err
	}
	value, err := strconv.ParseInt(token, 10, 64)
	if err != nil || value == 0 || strconv.FormatInt(value, 10) != token {
		return errors.New("invalid revision")
	}
	*destination = value
	return nil
}

func controllerTransitionOnlyMembers(members map[string]json.RawMessage, allowed ...string) bool {
	allow := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		allow[name] = struct{}{}
	}
	for name := range members {
		if _, ok := allow[name]; !ok {
			return false
		}
	}
	return true
}

func controllerTransitionStatusIsDefinitive(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusConflict,
		http.StatusPreconditionFailed, http.StatusNotImplemented:
		return true
	default:
		return false
	}
}

func controllerTransitionStatusError(status int, body []byte) error {
	var problem struct {
		Code string `json:"code"`
	}
	if len(body) == 0 || decodePrivateEvidenceJSON(body, &problem) != nil || !validControllerTransitionProblemCode(problem.Code) {
		return fmt.Errorf("%w: malformed HTTP %d problem response", ErrControllerMetadataTransitionProtocol, status)
	}
	return &ControllerMetadataTransitionProblem{Status: status, Code: problem.Code}
}

func validControllerTransitionProblemCode(code string) bool {
	if code == "" || len(code) > 64 {
		return false
	}
	for _, r := range code {
		if r != '_' && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func controllerTransitionProtocolError(reason string) error {
	return fmt.Errorf("%w: %s", ErrControllerMetadataTransitionProtocol, reason)
}

func controllerTransitionReceiptConflictError() error {
	return fmt.Errorf("%w: receipt is missing, malformed, or bound to another request", ErrControllerMetadataTransitionProtocol)
}

func recoveredControllerTransitionResult(receipt *ControllerMetadataTransitionReceipt) ControllerMetadataTransitionResult {
	clone := *receipt
	clone.Expected = append(json.RawMessage(nil), receipt.Expected...)
	clone.Value = append(json.RawMessage(nil), receipt.Value...)
	clone.Payload = append(json.RawMessage(nil), receipt.Payload...)
	return ControllerMetadataTransitionResult{Applied: true, Replayed: true, Receipt: &clone}
}
