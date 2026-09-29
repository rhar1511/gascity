package beads

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// ErrControllerMetadataRevisionTransitionUnavailable reports an incomplete
// or contradictory receipt/snapshot proof for a Q43 source transition.
var ErrControllerMetadataRevisionTransitionUnavailable = errors.New("controller metadata revision transition cannot prove its durable receipt")

type controllerMetadataRevisionTransitionWriter struct {
	writer         ControllerMetadataTransitionWriter
	sourceReader   DecisionFrontierSourceReader
	receiptReader  RevisionTransitionReceiptReader
	envelopeReader ControllerMetadataTransitionReceiptReader
}

var _ RevisionTransitionWriterHandleProvider = (*BdStore)(nil)

// RevisionTransitionWriterHandle composes the controller-backed source
// transition only when this BdStore exposes every required transport and
// authoritative proof reader. A partial configuration stays unavailable.
func (s *BdStore) RevisionTransitionWriterHandle() (RevisionTransitionWriter, bool) {
	writer, writerOK := ControllerMetadataTransitionWriterFor(s)
	sourceReader, sourceOK := DecisionFrontierSourceReaderFor(s)
	receiptReader, receiptOK := RevisionTransitionReceiptReaderFor(s)
	envelopeReader, envelopeOK := ControllerMetadataTransitionReceiptReaderFor(s)
	if !writerOK || !sourceOK || !receiptOK || !envelopeOK {
		return nil, false
	}
	adapted, err := NewControllerMetadataRevisionTransitionWriter(writer, sourceReader, receiptReader, envelopeReader)
	return adapted, err == nil
}

// NewControllerMetadataRevisionTransitionWriter adapts the Q43 metadata
// transition route to the decision-frontier source-transition contract. The
// adapter is intentionally explicit: callers must provide the authoritative
// source reader, projected source receipt reader, and full durable Q43 receipt
// reader alongside the Q43 writer.
func NewControllerMetadataRevisionTransitionWriter(writer ControllerMetadataTransitionWriter, sourceReader DecisionFrontierSourceReader, receiptReader RevisionTransitionReceiptReader, envelopeReader ControllerMetadataTransitionReceiptReader) (RevisionTransitionWriter, error) {
	if writer == nil || sourceReader == nil || receiptReader == nil || envelopeReader == nil {
		return nil, ErrControllerMetadataRevisionTransitionUnavailable
	}
	return controllerMetadataRevisionTransitionWriter{
		writer: writer, sourceReader: sourceReader, receiptReader: receiptReader, envelopeReader: envelopeReader,
	}, nil
}

func (w controllerMetadataRevisionTransitionWriter) CompareAndSetMetadataKeyWithReceipt(id, key, expected, next string, expectedRevision int64, receipt RevisionTransitionReceipt) (Bead, bool, error) {
	if w.writer == nil || w.sourceReader == nil || w.receiptReader == nil || w.envelopeReader == nil {
		return Bead{}, false, ErrControllerMetadataRevisionTransitionUnavailable
	}
	if err := validateControllerMetadataRevisionTransitionInput(id, key, expected, next, expectedRevision, receipt); err != nil {
		return Bead{}, false, err
	}
	before, err := w.sourceReader.DecisionFrontierSourceSnapshot(id)
	if err != nil {
		return Bead{}, false, fmt.Errorf("read decision-frontier source before Q43 transition: %w", err)
	}
	if before.ID != id {
		return Bead{}, false, ErrControllerMetadataRevisionTransitionUnavailable
	}
	request, err := controllerMetadataRequestForRevisionTransition(key, expected, next, expectedRevision, receipt)
	if err != nil {
		return Bead{}, false, err
	}

	if _, found, readErr := controllerMetadataTransitionReceiptForRequest(w.envelopeReader, id, receipt, request, 0); readErr != nil {
		return Bead{}, false, fmt.Errorf("read prior decision-frontier transition receipt: %w", readErr)
	} else if found {
		return w.replayAndVerify(before, id, key, next, expectedRevision, receipt, request)
	}
	if err := validateControllerMetadataRevisionTransitionSource(before, key, expected, next, expectedRevision, receipt, w.receiptReader, w.envelopeReader); err != nil {
		return before, false, err
	}

	result, transitionErr := w.writer.TransitionMetadata(id, request)
	if transitionErr != nil {
		committed, recovered, recoveryErr := w.recoverAmbiguous(before, id, key, expected, next, expectedRevision, receipt, request)
		if recoveryErr == nil && recovered {
			return committed, true, nil
		}
		if recoveryErr != nil {
			return Bead{}, false, errors.Join(fmt.Errorf("apply Q43 decision-frontier source transition: %w", transitionErr), recoveryErr)
		}
		return Bead{}, false, transitionErr
	}
	if !result.Applied {
		if result.Replayed || result.Receipt != nil {
			return Bead{}, false, fmt.Errorf("%w: refused transition returned an applied receipt", ErrControllerMetadataTransitionProtocol)
		}
		_, found, receiptErr := controllerMetadataTransitionReceiptForRequest(w.envelopeReader, id, receipt, request, 0)
		if receiptErr != nil {
			return Bead{}, false, fmt.Errorf("reread refused Q43 decision-frontier receipt: %w", receiptErr)
		}
		current, readErr := w.sourceReader.DecisionFrontierSourceSnapshot(id)
		if readErr != nil {
			return Bead{}, false, fmt.Errorf("read refused decision-frontier source: %w", readErr)
		}
		if found {
			return w.replayAndVerify(before, id, key, next, expectedRevision, receipt, request)
		}
		if current.ID != id || !controllerTransitionCurrentMatchesSource(result.Current, current, key) {
			return Bead{}, false, ErrControllerMetadataRevisionTransitionUnavailable
		}
		return current, false, nil
	}
	if !controllerMetadataRevisionTransitionResultMatches(result, id, request, receipt) {
		committed, recovered, recoveryErr := w.recoverAmbiguous(before, id, key, expected, next, expectedRevision, receipt, request)
		if recoveryErr == nil && recovered {
			return committed, true, nil
		}
		return Bead{}, false, errors.Join(
			fmt.Errorf("%w: applied result does not match the source transition", ErrControllerMetadataTransitionProtocol), recoveryErr)
	}
	current, readErr := w.sourceReader.DecisionFrontierSourceSnapshot(id)
	if readErr != nil {
		return Bead{}, false, fmt.Errorf("reread decision-frontier source after Q43 transition: %w", readErr)
	}
	committed, verifyErr := w.verifyCommitted(&before, current, id, key, next, expectedRevision, receipt, request, result.Receipt.ToVersion, &result.Current)
	if verifyErr != nil {
		return Bead{}, false, verifyErr
	}
	return committed, true, nil
}

func (w controllerMetadataRevisionTransitionWriter) replayAndVerify(before Bead, id, key, next string, expectedRevision int64, expected RevisionTransitionReceipt, request ControllerMetadataTransitionRequest) (Bead, bool, error) {
	replay, replayErr := w.writer.TransitionMetadata(id, request)
	if replayErr == nil && controllerMetadataRevisionTransitionResultMatches(replay, id, request, expected) {
		current, readErr := w.sourceReader.DecisionFrontierSourceSnapshot(id)
		if readErr != nil {
			return Bead{}, false, fmt.Errorf("reread decision-frontier source after Q43 receipt replay: %w", readErr)
		}
		committed, verifyErr := w.verifyCommitted(nil, current, id, key, next, expectedRevision, expected, request, replay.Receipt.ToVersion, &replay.Current)
		return committed, verifyErr == nil, verifyErr
	}

	_, found, receiptErr := controllerMetadataTransitionReceiptForRequest(w.envelopeReader, id, expected, request, 0)
	if receiptErr != nil {
		return Bead{}, false, errors.Join(replayErr, fmt.Errorf("reread Q43 decision-frontier receipt: %w", receiptErr))
	}
	current, sourceErr := w.sourceReader.DecisionFrontierSourceSnapshot(id)
	if sourceErr != nil {
		return Bead{}, false, errors.Join(replayErr, fmt.Errorf("reread decision-frontier source after Q43 receipt replay: %w", sourceErr))
	}
	if found {
		return Bead{}, false, fmt.Errorf("%w: exact Q43 receipt content could not be re-established", ErrControllerMetadataRevisionTransitionUnavailable)
	}
	if current.ID != id || current.Revision != expectedRevision || current.Metadata[key] != requestMarkerValue(request.Expected) ||
		!controllerTransitionSourceContentMatches(before, current, key, requestMarkerValue(request.Expected), requestMarkerValue(request.Expected)) {
		return Bead{}, false, ErrControllerMetadataRevisionTransitionUnavailable
	}
	if replayErr != nil {
		return Bead{}, false, replayErr
	}
	return Bead{}, false, fmt.Errorf("%w: Q43 replay did not return the exact applied receipt", ErrControllerMetadataRevisionTransitionUnavailable)
}

func (w controllerMetadataRevisionTransitionWriter) recoverAmbiguous(before Bead, id, key, expectedMarker, next string, expectedRevision int64, expected RevisionTransitionReceipt, request ControllerMetadataTransitionRequest) (Bead, bool, error) {
	_, found, receiptErr := controllerMetadataTransitionReceiptForRequest(w.envelopeReader, id, expected, request, 0)
	if receiptErr != nil {
		return Bead{}, false, fmt.Errorf("reread Q43 decision-frontier receipt: %w", receiptErr)
	}
	if found {
		return w.replayAndVerify(before, id, key, next, expectedRevision, expected, request)
	}
	current, sourceErr := w.sourceReader.DecisionFrontierSourceSnapshot(id)
	if sourceErr != nil {
		return Bead{}, false, fmt.Errorf("reread decision-frontier source after ambiguous Q43 result: %w", sourceErr)
	}
	if current.ID != id || current.Revision != expectedRevision ||
		!controllerTransitionSourceContentMatches(before, current, key, expectedMarker, expectedMarker) {
		return Bead{}, false, ErrControllerMetadataRevisionTransitionUnavailable
	}
	return Bead{}, false, nil
}

func controllerRequestMarker(raw *json.RawMessage) (string, error) {
	if raw == nil {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(*raw, &value); err != nil {
		return "", err
	}
	return value, nil
}

func requestMarkerValue(raw *json.RawMessage) string {
	value, _ := controllerRequestMarker(raw)
	return value
}

func (w controllerMetadataRevisionTransitionWriter) verifyCommitted(before *Bead, current Bead, id, key, next string, expectedRevision int64, expected RevisionTransitionReceipt, request ControllerMetadataTransitionRequest, toRevision int64, responseCurrent *json.RawMessage) (Bead, error) {
	if current.ID != id || current.Revision != toRevision || current.Metadata[key] != next || toRevision == expectedRevision {
		return Bead{}, fmt.Errorf("%w: source snapshot does not match the committed Q43 revision", ErrControllerMetadataRevisionTransitionUnavailable)
	}
	if before != nil && !controllerTransitionSourceContentMatches(*before, current, key, before.Metadata[key], next) {
		return Bead{}, fmt.Errorf("%w: source content changed outside the requested metadata transition", ErrControllerMetadataRevisionTransitionUnavailable)
	}
	if responseCurrent != nil && !controllerTransitionCurrentMatchesSource(*responseCurrent, current, key) {
		return Bead{}, fmt.Errorf("%w: Q43 result marker differs from the authoritative source snapshot", ErrControllerMetadataTransitionProtocol)
	}
	_, found, err := controllerMetadataTransitionReceiptForRequest(w.envelopeReader, id, expected, request, toRevision)
	if err != nil {
		return Bead{}, fmt.Errorf("reread exact Q43 decision-frontier receipt: %w", err)
	}
	if !found {
		return Bead{}, ErrDecisionFrontierTransitionReceiptCorrupt
	}
	return cloneBead(current), nil
}

func validateControllerMetadataRevisionTransitionInput(id, key, expected, next string, expectedRevision int64, receipt RevisionTransitionReceipt) error {
	if key != beadmeta.DecisionFrontierHoldMetadataKey || id == "" || id != receipt.WorkID ||
		receipt.ID == "" || receipt.CityRef == "" || receipt.StoreRef == "" || receipt.MapID == "" ||
		receipt.FromRevision == 0 || receipt.FromRevision != expectedRevision || receipt.ToRevision != 0 ||
		(receipt.Operation != "reserve" && receipt.Operation != "release") {
		return fmt.Errorf("%w: invalid source-transition identity", ErrControllerMetadataTransitionProtocol)
	}
	if receipt.Operation == "reserve" {
		if expected != "" || next == "" {
			return fmt.Errorf("%w: invalid source reservation markers", ErrControllerMetadataTransitionProtocol)
		}
	} else if expected == "" || next != "" {
		return fmt.Errorf("%w: invalid source release markers", ErrControllerMetadataTransitionProtocol)
	}
	marker := next
	if receipt.Operation == "release" {
		marker = expected
	}
	if _, err := controllerTransitionHoldForReceipt(marker, receipt); err != nil {
		return err
	}
	return nil
}

func validateControllerMetadataRevisionTransitionSource(current Bead, key, expected, next string, expectedRevision int64, receipt RevisionTransitionReceipt, receiptReader RevisionTransitionReceiptReader, envelopeReader ControllerMetadataTransitionReceiptReader) error {
	if current.Revision != expectedRevision || current.Metadata[key] != expected {
		return &PreconditionFailedError{ID: current.ID, Expected: expectedRevision, Current: current.Revision}
	}
	hold, err := controllerTransitionHoldForReceipt(func() string {
		if receipt.Operation == "reserve" {
			return next
		}
		return expected
	}(), receipt)
	if err != nil {
		return err
	}
	if receipt.Operation == "reserve" {
		if current.Metadata[key] != "" || hold.WorkRevision != strconv.FormatInt(expectedRevision, 10) {
			return fmt.Errorf("%w: reservation source does not match its base revision", ErrControllerMetadataTransitionProtocol)
		}
		if _, found, readErr := receiptReader.DecisionFrontierRevisionTransitionReceipt(current.ID, receipt.ID); readErr != nil {
			return readErr
		} else if found {
			return ErrDecisionFrontierTransitionReceiptExists
		}
		return nil
	}
	if current.Metadata[key] != expected || receipt.ID == hold.ReservationID {
		return fmt.Errorf("%w: release source identity does not match its hold", ErrControllerMetadataTransitionProtocol)
	}
	baseRevision, err := strconv.ParseInt(hold.WorkRevision, 10, 64)
	if err != nil {
		return fmt.Errorf("parse matching reservation revision: %w", err)
	}
	reservation := RevisionTransitionReceipt{
		ID: hold.ReservationID, CityRef: hold.CityRef, StoreRef: hold.StoreRef, WorkID: hold.WorkID,
		MapID: hold.MapID, Operation: "reserve", FromRevision: baseRevision,
	}
	reservationRequest, err := controllerMetadataRequestForRevisionTransition(key, "", expected, baseRevision, reservation)
	if err != nil {
		return err
	}
	reserved, found, err := controllerMetadataTransitionReceiptForRequest(envelopeReader, current.ID, reservation, reservationRequest, expectedRevision)
	if err != nil {
		return fmt.Errorf("read matching reservation receipt: %w", err)
	}
	if !found || reserved.ToRevision != expectedRevision || reserved.ToRevision == reserved.FromRevision {
		return fmt.Errorf("%w: release has no exact matching reservation receipt", ErrDecisionFrontierTransitionReceiptCorrupt)
	}
	return nil
}

func controllerTransitionHoldForReceipt(marker string, receipt RevisionTransitionReceipt) (decisionFrontierHoldBinding, error) {
	var hold decisionFrontierHoldBinding
	if err := json.Unmarshal([]byte(marker), &hold); err != nil || marker == "" || hold.SchemaVersion != 1 ||
		hold.CityRef != receipt.CityRef || hold.StoreRef != receipt.StoreRef || hold.WorkID != receipt.WorkID ||
		hold.MapID != receipt.MapID || hold.WorkDigest == "" || hold.ProposalHash == "" ||
		hold.ReservationID != decisionFrontierStableID("frontier-transition", receipt.CityRef, receipt.StoreRef, receipt.MapID, "reserve") ||
		receipt.ID != decisionFrontierStableID("frontier-transition", receipt.CityRef, receipt.StoreRef, receipt.MapID, receipt.Operation) {
		return decisionFrontierHoldBinding{}, fmt.Errorf("%w: hold marker does not match the source receipt", ErrControllerMetadataTransitionProtocol)
	}
	canonical, err := json.Marshal(hold)
	if err != nil || !bytes.Equal(canonical, []byte(marker)) {
		return decisionFrontierHoldBinding{}, fmt.Errorf("%w: hold marker is not canonical", ErrControllerMetadataTransitionProtocol)
	}
	baseRevision, err := strconv.ParseInt(hold.WorkRevision, 10, 64)
	if err != nil || baseRevision == 0 || strconv.FormatInt(baseRevision, 10) != hold.WorkRevision ||
		hold.MapID != DecisionFrontierMapRecordID(hold.CityRef, hold.StoreRef, hold.WorkID, hold.WorkRevision) {
		return decisionFrontierHoldBinding{}, fmt.Errorf("%w: hold marker revision or map identity is invalid", ErrControllerMetadataTransitionProtocol)
	}
	if receipt.Operation == "reserve" && (receipt.ID != hold.ReservationID || receipt.FromRevision != baseRevision) ||
		receipt.Operation == "release" && receipt.ID == hold.ReservationID {
		return decisionFrontierHoldBinding{}, fmt.Errorf("%w: receipt ID does not match the hold marker", ErrControllerMetadataTransitionProtocol)
	}
	return hold, nil
}

func controllerMetadataRequestForRevisionTransition(key, expected, next string, expectedRevision int64, receipt RevisionTransitionReceipt) (ControllerMetadataTransitionRequest, error) {
	payload, err := json.Marshal(receipt)
	if err != nil {
		return ControllerMetadataTransitionRequest{}, fmt.Errorf("encode decision-frontier Q43 receipt payload: %w", err)
	}
	request := ControllerMetadataTransitionRequest{
		ReceiptID: receipt.ID, Scope: receipt.StoreRef, Kind: controllerDecisionFrontierTransitionKind,
		Actor: privateEvidenceActor, ExpectedVersion: expectedRevision, Key: key, Payload: payload,
	}
	request.Payload, err = canonicalControllerTransitionJSON(request.Payload)
	if err != nil {
		return ControllerMetadataTransitionRequest{}, fmt.Errorf("canonicalize decision-frontier Q43 receipt payload: %w", err)
	}
	if expected != "" {
		raw, err := json.Marshal(expected)
		if err != nil {
			return ControllerMetadataTransitionRequest{}, err
		}
		value := json.RawMessage(raw)
		request.Expected = &value
	}
	if next != "" {
		raw, err := json.Marshal(next)
		if err != nil {
			return ControllerMetadataTransitionRequest{}, err
		}
		value := json.RawMessage(raw)
		request.Value = &value
	}
	return request, nil
}

func controllerMetadataRevisionTransitionResultMatches(result ControllerMetadataTransitionResult, id string, request ControllerMetadataTransitionRequest, expected RevisionTransitionReceipt) bool {
	if !result.Applied || result.Receipt == nil || result.Receipt.ReceiptID != expected.ID || result.Receipt.IssueID != id ||
		result.Receipt.Scope != request.Scope || result.Receipt.Kind != request.Kind || result.Receipt.Actor != request.Actor ||
		result.Receipt.ExpectedVersion != request.ExpectedVersion || result.Receipt.Key != request.Key ||
		!bytes.Equal(nilSafeRaw(rawControllerTransitionPointer(result.Receipt.Expected)), nilSafeRaw(request.Expected)) ||
		!bytes.Equal(nilSafeRaw(rawControllerTransitionPointer(result.Receipt.Value)), nilSafeRaw(request.Value)) ||
		!bytes.Equal(result.Receipt.Payload, request.Payload) || result.Receipt.ToVersion == request.ExpectedVersion || result.Receipt.ToVersion == 0 {
		return false
	}
	return true
}

func controllerMetadataTransitionReceiptForRequest(reader ControllerMetadataTransitionReceiptReader, issueID string, expected RevisionTransitionReceipt, request ControllerMetadataTransitionRequest, expectedToVersion int64) (RevisionTransitionReceipt, bool, error) {
	if reader == nil {
		return RevisionTransitionReceipt{}, false, ErrControllerMetadataRevisionTransitionUnavailable
	}
	actual, found, err := reader.ControllerMetadataTransitionReceipt(issueID, expected.ID)
	if err != nil || !found {
		return RevisionTransitionReceipt{}, found, err
	}
	projected, decodeErr := decodeDecisionFrontierControllerReceipt(actual)
	if decodeErr != nil || !controllerMetadataTransitionReceiptMatchesRequest(actual, issueID, request) ||
		!sameRevisionTransitionIdentity(projected, expected) || projected.ToRevision == 0 ||
		expectedToVersion != 0 && projected.ToRevision != expectedToVersion {
		return RevisionTransitionReceipt{}, true, ErrDecisionFrontierTransitionReceiptCorrupt
	}
	return projected, true, nil
}

func controllerMetadataTransitionReceiptMatchesRequest(actual ControllerMetadataTransitionReceipt, issueID string, request ControllerMetadataTransitionRequest) bool {
	return actual.ReceiptID == request.ReceiptID && actual.IssueID == issueID && actual.Scope == request.Scope &&
		actual.Kind == request.Kind && actual.Actor == request.Actor && actual.ExpectedVersion == request.ExpectedVersion &&
		actual.ToVersion != 0 && actual.ToVersion != request.ExpectedVersion && actual.Key == request.Key &&
		bytes.Equal(nilSafeRaw(rawControllerTransitionPointer(actual.Expected)), nilSafeRaw(request.Expected)) &&
		bytes.Equal(nilSafeRaw(rawControllerTransitionPointer(actual.Value)), nilSafeRaw(request.Value)) &&
		bytes.Equal(actual.Payload, request.Payload)
}

func controllerTransitionCurrentMatchesSource(raw json.RawMessage, current Bead, key string) bool {
	if len(raw) == 0 {
		return true
	}
	trimmed := bytes.TrimSpace(raw)
	if bytes.Equal(trimmed, []byte("null")) {
		return current.Metadata[key] == ""
	}
	var value string
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return false
	}
	return value == current.Metadata[key]
}

func sameRevisionTransitionIdentity(actual, expected RevisionTransitionReceipt) bool {
	return actual.ID == expected.ID && actual.CityRef == expected.CityRef && actual.StoreRef == expected.StoreRef &&
		actual.WorkID == expected.WorkID && actual.MapID == expected.MapID && actual.Operation == expected.Operation &&
		actual.FromRevision == expected.FromRevision
}

func controllerTransitionSourceContentMatches(before, after Bead, key, expected, next string) bool {
	before = cloneBead(before)
	after = cloneBead(after)
	if before.Metadata[key] != expected || after.Metadata[key] != next {
		return false
	}
	before.Revision = after.Revision
	before.UpdatedAt = after.UpdatedAt
	if before.Metadata == nil {
		before.Metadata = make(StringMap)
	}
	if after.Metadata == nil {
		after.Metadata = make(StringMap)
	}
	delete(before.Metadata, key)
	delete(after.Metadata, key)
	return reflect.DeepEqual(before, after)
}

var _ RevisionTransitionWriter = controllerMetadataRevisionTransitionWriter{}
