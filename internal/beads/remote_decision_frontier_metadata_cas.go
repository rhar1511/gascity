package beads

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	controllerDecisionFrontierMetadataCASReceiptPrefix = "gc-df-metadata-v1-"
	controllerDecisionFrontierMetadataCASReplayPrefix  = "gc-df-metadata-replay-v1-"
	controllerDecisionFrontierMetadataCASOperation     = "issue.revision_transition"
)

// ErrRemoteDecisionFrontierMetadataCASProtocol reports an unprovable remote
// decision-frontier record-CAS result.
var ErrRemoteDecisionFrontierMetadataCASProtocol = errors.New("remote decision-frontier metadata CAS result could not be verified")

func validateRemoteDecisionFrontierMetadataCASConfig(config RemoteDecisionFrontierRecordWriterConfig) error {
	configured := config.MetadataTransitionScope != "" || config.MetadataTransitionKind != "" ||
		config.MetadataRecordReader != nil || config.MetadataPermitIssuer != nil || config.MetadataTransitionWriter != nil
	if !configured {
		return nil
	}
	if config.MetadataRecordReader == nil || config.MetadataPermitIssuer == nil || config.MetadataTransitionWriter == nil ||
		validateControllerTransitionText(config.MetadataTransitionScope, controllerTransitionMaxScope) != nil ||
		strings.TrimSpace(config.MetadataTransitionScope) != config.MetadataTransitionScope || hasControllerTransitionControl(config.MetadataTransitionScope) ||
		validateControllerTransitionText(config.MetadataTransitionKind, controllerTransitionMaxKind) != nil ||
		strings.TrimSpace(config.MetadataTransitionKind) != config.MetadataTransitionKind || hasControllerTransitionControl(config.MetadataTransitionKind) {
		return ErrRemoteDecisionFrontierWriterUnavailable
	}
	return nil
}

func (w *RemoteDecisionFrontierRecordWriter) compareAndSetDecisionFrontierRecordMetadataKey(id, key, expected, next string) (bool, error) {
	before, err := w.metadataRecordReader.Get(id)
	if err != nil {
		return false, fmt.Errorf("read decision-frontier record %q before metadata CAS: %w", id, err)
	}
	// Keep the exact read snapshot stable even when an injected reader reuses
	// mutable maps in its returned Bead value.
	before = cloneBead(before)
	if err := w.validateMetadataTransitionRecord(before, id); err != nil {
		return false, err
	}
	if before.Metadata[key] != expected {
		return false, nil
	}
	if err := validateDecisionFrontierRecordCAS(before, key, expected, next); err != nil {
		return false, err
	}

	request, replayID, err := w.protectedMetadataTransitionRequest(id, key, next, before)
	if err != nil {
		return false, err
	}
	if controllerTransitionValuesEqual(request.Expected, request.Value) {
		// The Q43 plan rejects identical markers. Treat an already-present
		// identical reason as a no-op instead of issuing a request it cannot
		// represent. An absent empty reason remains a real presence change.
		return false, nil
	}
	permitRequest := ControllerProtectedMutationRequest{
		Operation:   controllerDecisionFrontierMetadataCASOperation,
		ResourceIDs: []string{id},
	}
	permitRequest.RequestDigest, err = controllerBeadsProtectedRevisionTransitionDigest(id, request)
	if err != nil {
		return false, fmt.Errorf("digest protected decision-frontier metadata transition: %w", err)
	}
	permit, err := w.metadataPermitIssuer.IssueProtectedMutation(permitRequest, replayID)
	if err != nil {
		return false, fmt.Errorf("issue protected decision-frontier metadata permit: %w", err)
	}
	if !validControllerBatchApplyText(permit, controllerBatchApplyMaxPermitBytes) ||
		len(permit) > controllerBatchApplyMaxPermitBytes || strings.TrimSpace(permit) != permit || controllerBatchApplyHasControl(permit) {
		return false, ErrControllerBeadsPermitRequest
	}
	request.ProtectedPermit = permit

	result, err := w.metadataTransitionWriter.TransitionMetadata(id, request)
	if err != nil {
		return false, fmt.Errorf("apply protected decision-frontier metadata transition %q: %w", id, err)
	}
	if !result.Applied {
		return w.verifyMetadataTransitionRefusal(before, id, key, expected, request, result)
	}
	return w.verifyMetadataTransitionApplied(before, id, key, expected, next, request, result)
}

func (w *RemoteDecisionFrontierRecordWriter) protectedMetadataTransitionRequest(id, key, next string, before Bead) (ControllerMetadataTransitionRequest, string, error) {
	request := ControllerMetadataTransitionRequest{
		Scope: w.metadataTransitionScope, Kind: w.metadataTransitionKind, Actor: w.actor,
		ExpectedVersion: before.Revision, Key: key, Payload: json.RawMessage(`{}`),
	}
	if current, present := before.Metadata[key]; present {
		encoded, err := json.Marshal(current)
		if err != nil {
			return ControllerMetadataTransitionRequest{}, "", fmt.Errorf("encode expected decision-frontier marker: %w", err)
		}
		value := json.RawMessage(encoded)
		request.Expected = &value
	}
	encodedNext, err := json.Marshal(next)
	if err != nil {
		return ControllerMetadataTransitionRequest{}, "", fmt.Errorf("encode next decision-frontier marker: %w", err)
	}
	encodedValue := json.RawMessage(encodedNext)
	request.Value = &encodedValue
	seedDigest, err := controllerBeadsProtectedRevisionTransitionDigest(id, request)
	if err != nil {
		return ControllerMetadataTransitionRequest{}, "", err
	}
	request.ReceiptID = controllerDecisionFrontierMetadataCASReceiptPrefix + seedDigest
	digest, err := controllerBeadsProtectedRevisionTransitionDigest(id, request)
	if err != nil {
		return ControllerMetadataTransitionRequest{}, "", err
	}
	return request, controllerDecisionFrontierMetadataCASReplayPrefix + digest, nil
}

func (w *RemoteDecisionFrontierRecordWriter) verifyMetadataTransitionRefusal(before Bead, id, key, expected string, request ControllerMetadataTransitionRequest, result ControllerMetadataTransitionResult) (bool, error) {
	if result.Replayed || result.Receipt != nil || len(result.Current) == 0 {
		return false, ErrRemoteDecisionFrontierMetadataCASProtocol
	}
	after, err := w.metadataRecordReader.Get(id)
	if err != nil {
		return false, fmt.Errorf("read decision-frontier record %q after refused metadata CAS: %w", id, err)
	}
	if err := w.validateMetadataTransitionRecord(after, id); err != nil {
		return false, err
	}
	if after.Revision == before.Revision || !remoteDecisionFrontierMetadataCurrentMatchesSource(result.Current, after, key) ||
		remoteDecisionFrontierMetadataPointerMatchesSource(request.Expected, after, key) {
		return false, ErrRemoteDecisionFrontierMetadataCASProtocol
	}
	if !controllerTransitionSourceContentMatches(before, after, key, expected, after.Metadata[key]) {
		return false, ErrRemoteDecisionFrontierMetadataCASProtocol
	}
	return false, nil
}

func (w *RemoteDecisionFrontierRecordWriter) verifyMetadataTransitionApplied(before Bead, id, key, expected, next string, request ControllerMetadataTransitionRequest, result ControllerMetadataTransitionResult) (bool, error) {
	receipt := result.Receipt
	if receipt == nil || receipt.ReceiptID != request.ReceiptID || receipt.IssueID != id || receipt.Scope != request.Scope ||
		receipt.Kind != request.Kind || receipt.Actor != request.Actor || receipt.ExpectedVersion != request.ExpectedVersion ||
		receipt.Key != request.Key || !bytes.Equal(nilSafeRaw(rawControllerTransitionPointer(receipt.Expected)), nilSafeRaw(request.Expected)) ||
		!bytes.Equal(nilSafeRaw(rawControllerTransitionPointer(receipt.Value)), nilSafeRaw(request.Value)) ||
		!bytes.Equal(receipt.Payload, request.Payload) || receipt.ToVersion <= 0 || receipt.ToVersion == request.ExpectedVersion {
		return false, ErrRemoteDecisionFrontierMetadataCASProtocol
	}
	after, err := w.metadataRecordReader.Get(id)
	if err != nil {
		return false, fmt.Errorf("read decision-frontier record %q after metadata CAS: %w", id, err)
	}
	if err := w.validateMetadataTransitionRecord(after, id); err != nil {
		return false, err
	}
	value, present := after.Metadata[key]
	if after.Revision != receipt.ToVersion || !present || value != next ||
		!controllerTransitionSourceContentMatches(before, after, key, expected, next) {
		return false, ErrRemoteDecisionFrontierMetadataCASProtocol
	}
	if len(result.Current) != 0 && !remoteDecisionFrontierMetadataCurrentMatchesSource(result.Current, after, key) {
		return false, ErrRemoteDecisionFrontierMetadataCASProtocol
	}
	return true, nil
}

func (w *RemoteDecisionFrontierRecordWriter) validateMetadataTransitionRecord(record Bead, id string) error {
	if record.ID != id || record.Revision <= 0 {
		return ErrRemoteDecisionFrontierMetadataCASProtocol
	}
	doc, _, err := decisionFrontierLinkRecord(record)
	if err != nil {
		return err
	}
	if doc.StoreRef != w.metadataTransitionScope {
		return ErrDecisionFrontierLinkConflict
	}
	return nil
}

func remoteDecisionFrontierMetadataCurrentMatchesSource(raw json.RawMessage, current Bead, key string) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return false
	}
	value, present := current.Metadata[key]
	if bytes.Equal(trimmed, []byte(`null`)) {
		return !present
	}
	var decoded string
	if err := json.Unmarshal(trimmed, &decoded); err != nil {
		return false
	}
	return present && value == decoded
}

func remoteDecisionFrontierMetadataPointerMatchesSource(expected *json.RawMessage, current Bead, key string) bool {
	value, present := current.Metadata[key]
	if !present {
		return expected == nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return false
	}
	actual := json.RawMessage(encoded)
	return controllerTransitionValuesEqual(expected, &actual)
}

// controllerBeadsDigestRevisionTransitionRequest mirrors
// issueops.RevisionTransitionRequest at Beads commit
// 0206d40b71634b7cfc3f025e90e39a3b63f6d846. Keep field order and zero-value
// emission aligned with that type because ProtectedMutationDigest hashes its
// encoding/json output after clearing ProtectedPermit.
type controllerBeadsDigestRevisionTransitionRequest struct {
	ProtectedPermit string
	ReceiptID       string
	IssueID         string
	Scope           string
	Kind            string
	Actor           string
	ExpectedVersion int64
	Key             string
	Expected        *json.RawMessage
	Value           *json.RawMessage
	Payload         json.RawMessage
}

func controllerBeadsProtectedRevisionTransitionDigest(id string, request ControllerMetadataTransitionRequest) (string, error) {
	if id == "" || request.ExpectedVersion == 0 || request.ProtectedPermit != "" {
		return "", ErrControllerBeadsPermitRequest
	}
	expected, err := canonicalControllerTransitionPointer(request.Expected)
	if err != nil {
		return "", err
	}
	value, err := canonicalControllerTransitionPointer(request.Value)
	if err != nil {
		return "", err
	}
	payload := json.RawMessage(`{}`)
	if len(request.Payload) != 0 {
		payload, err = canonicalControllerTransitionJSON(request.Payload)
		if err != nil {
			return "", err
		}
	}
	encoded, err := json.Marshal(controllerBeadsDigestRevisionTransitionRequest{
		ProtectedPermit: "", ReceiptID: request.ReceiptID, IssueID: id, Scope: request.Scope,
		Kind: request.Kind, Actor: request.Actor, ExpectedVersion: request.ExpectedVersion,
		Key: request.Key, Expected: expected, Value: value, Payload: payload,
	})
	if err != nil {
		return "", fmt.Errorf("encode pinned Beads revision-transition digest input: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
