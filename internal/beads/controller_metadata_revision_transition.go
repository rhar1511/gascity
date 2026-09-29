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
	writer        ControllerMetadataTransitionWriter
	sourceReader  DecisionFrontierSourceReader
	receiptReader RevisionTransitionReceiptReader
}

// NewControllerMetadataRevisionTransitionWriter adapts the Q43 metadata
// transition route to the decision-frontier source-transition contract. The
// adapter is intentionally explicit: callers must provide the authoritative
// source reader and exact durable receipt reader alongside the Q43 writer.
func NewControllerMetadataRevisionTransitionWriter(writer ControllerMetadataTransitionWriter, sourceReader DecisionFrontierSourceReader, receiptReader RevisionTransitionReceiptReader) (RevisionTransitionWriter, error) {
	if writer == nil || sourceReader == nil || receiptReader == nil {
		return nil, ErrControllerMetadataRevisionTransitionUnavailable
	}
	return controllerMetadataRevisionTransitionWriter{
		writer: writer, sourceReader: sourceReader, receiptReader: receiptReader,
	}, nil
}

func (w controllerMetadataRevisionTransitionWriter) CompareAndSetMetadataKeyWithReceipt(id, key, expected, next string, expectedRevision int64, receipt RevisionTransitionReceipt) (Bead, bool, error) {
	if w.writer == nil || w.sourceReader == nil || w.receiptReader == nil {
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

	if prior, found, readErr := w.receiptReader.DecisionFrontierRevisionTransitionReceipt(id, receipt.ID); readErr != nil {
		return Bead{}, false, fmt.Errorf("read prior decision-frontier transition receipt: %w", readErr)
	} else if found {
		if !sameRevisionTransitionIdentity(prior, receipt) || prior.ToRevision == 0 {
			return Bead{}, false, ErrDecisionFrontierTransitionReceiptCorrupt
		}
		return w.replayAndVerify(before, id, key, next, expectedRevision, receipt, request)
	}
	if err := validateControllerMetadataRevisionTransitionSource(before, key, expected, next, expectedRevision, receipt, w.receiptReader); err != nil {
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
		prior, found, receiptErr := w.receiptReader.DecisionFrontierRevisionTransitionReceipt(id, receipt.ID)
		if receiptErr != nil {
			return Bead{}, false, fmt.Errorf("reread refused Q43 decision-frontier receipt: %w", receiptErr)
		}
		current, readErr := w.sourceReader.DecisionFrontierSourceSnapshot(id)
		if readErr != nil {
			return Bead{}, false, fmt.Errorf("read refused decision-frontier source: %w", readErr)
		}
		if found {
			if !sameRevisionTransitionIdentity(prior, receipt) || prior.ToRevision == 0 {
				return Bead{}, false, ErrDecisionFrontierTransitionReceiptCorrupt
			}
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
	committed, verifyErr := w.verifyCommitted(&before, current, id, key, next, expectedRevision, receipt, result.Receipt.ToVersion, &result.Current)
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
		committed, verifyErr := w.verifyCommitted(nil, current, id, key, next, expectedRevision, expected, replay.Receipt.ToVersion, &replay.Current)
		return committed, verifyErr == nil, verifyErr
	}

	actual, found, receiptErr := w.receiptReader.DecisionFrontierRevisionTransitionReceipt(id, expected.ID)
	if receiptErr != nil {
		return Bead{}, false, errors.Join(replayErr, fmt.Errorf("reread Q43 decision-frontier receipt: %w", receiptErr))
	}
	current, sourceErr := w.sourceReader.DecisionFrontierSourceSnapshot(id)
	if sourceErr != nil {
		return Bead{}, false, errors.Join(replayErr, fmt.Errorf("reread decision-frontier source after Q43 receipt replay: %w", sourceErr))
	}
	if found {
		if !sameRevisionTransitionIdentity(actual, expected) || actual.ToRevision == 0 {
			return Bead{}, false, ErrDecisionFrontierTransitionReceiptCorrupt
		}
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
	actual, found, receiptErr := w.receiptReader.DecisionFrontierRevisionTransitionReceipt(id, expected.ID)
	if receiptErr != nil {
		return Bead{}, false, fmt.Errorf("reread Q43 decision-frontier receipt: %w", receiptErr)
	}
	if found {
		if !sameRevisionTransitionIdentity(actual, expected) || actual.ToRevision == 0 {
			return Bead{}, false, ErrDecisionFrontierTransitionReceiptCorrupt
		}
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

func (w controllerMetadataRevisionTransitionWriter) verifyCommitted(before *Bead, current Bead, id, key, next string, expectedRevision int64, expected RevisionTransitionReceipt, toRevision int64, responseCurrent *json.RawMessage) (Bead, error) {
	if current.ID != id || current.Revision != toRevision || current.Metadata[key] != next || toRevision == expectedRevision {
		return Bead{}, fmt.Errorf("%w: source snapshot does not match the committed Q43 revision", ErrControllerMetadataRevisionTransitionUnavailable)
	}
	if before != nil && !controllerTransitionSourceContentMatches(*before, current, key, before.Metadata[key], next) {
		return Bead{}, fmt.Errorf("%w: source content changed outside the requested metadata transition", ErrControllerMetadataRevisionTransitionUnavailable)
	}
	if responseCurrent != nil && !controllerTransitionCurrentMatchesSource(*responseCurrent, current, key) {
		return Bead{}, fmt.Errorf("%w: Q43 result marker differs from the authoritative source snapshot", ErrControllerMetadataTransitionProtocol)
	}
	actual, found, err := w.receiptReader.DecisionFrontierRevisionTransitionReceipt(id, expected.ID)
	if err != nil {
		return Bead{}, fmt.Errorf("reread exact decision-frontier receipt: %w", err)
	}
	if !found || !sameRevisionTransitionIdentity(actual, expected) || actual.ToRevision != toRevision {
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

func validateControllerMetadataRevisionTransitionSource(current Bead, key, expected, next string, expectedRevision int64, receipt RevisionTransitionReceipt, receiptReader RevisionTransitionReceiptReader) error {
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
	reserved, found, err := receiptReader.DecisionFrontierRevisionTransitionReceipt(current.ID, hold.ReservationID)
	if err != nil {
		return fmt.Errorf("read matching reservation receipt: %w", err)
	}
	if !found || reserved.ID != hold.ReservationID || reserved.CityRef != receipt.CityRef || reserved.StoreRef != receipt.StoreRef ||
		reserved.WorkID != receipt.WorkID || reserved.MapID != receipt.MapID || reserved.Operation != "reserve" ||
		reserved.FromRevision == 0 || strconv.FormatInt(reserved.FromRevision, 10) != hold.WorkRevision ||
		reserved.ToRevision != expectedRevision || reserved.ToRevision == reserved.FromRevision {
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
