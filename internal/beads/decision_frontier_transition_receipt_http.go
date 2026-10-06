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
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

const controllerDecisionFrontierTransitionKind = "decision-frontier-source-transition"

var (
	// ErrDecisionFrontierReceiptReadUnavailable reports an unavailable exact
	// receipt route or an incomplete transport context.
	ErrDecisionFrontierReceiptReadUnavailable = errors.New("decision-frontier transition receipt HTTP transport unavailable")
	// ErrDecisionFrontierReceiptReadProtocol reports an unsupported or malformed
	// exact receipt response.
	ErrDecisionFrontierReceiptReadProtocol                                                         = errors.New("decision-frontier transition receipt HTTP protocol unsupported")
	_                                      RevisionTransitionReceiptReader                         = (*BdStore)(nil)
	_                                      ControllerMetadataTransitionReceiptReader               = (*BdStore)(nil)
	_                                      ControllerMetadataTransitionReceiptReaderHandleProvider = (*BdStore)(nil)
)

// ControllerMetadataTransitionReceiptReaderHandle exposes full durable Q43
// envelopes only when the revision-transition transport is configured.
func (s *BdStore) ControllerMetadataTransitionReceiptReaderHandle() (ControllerMetadataTransitionReceiptReader, bool) {
	if s == nil || s.privateEvidenceHTTP == nil || s.privateEvidenceHTTPInitErr != nil || !s.privateEvidenceHTTP.revisionTransitions {
		return nil, false
	}
	return s, true
}

// DecisionFrontierRevisionTransitionReceipt reads and validates one exact
// Q43 receipt for the requested source issue. Receipt payload identity and the
// hold-marker transition are checked before exposing its revision tokens.
func (s *BdStore) DecisionFrontierRevisionTransitionReceipt(issueID, receiptID string) (RevisionTransitionReceipt, bool, error) {
	remote, found, err := s.ControllerMetadataTransitionReceipt(issueID, receiptID)
	if err != nil || !found {
		return RevisionTransitionReceipt{}, found, err
	}
	receipt, err := decodeDecisionFrontierControllerReceipt(remote)
	if err != nil {
		return RevisionTransitionReceipt{}, false, fmt.Errorf("%w: invalid Q43 source-transition receipt", ErrDecisionFrontierTransitionReceiptCorrupt)
	}
	return receipt, true, nil
}

// ControllerMetadataTransitionReceipt reads the complete durable Q43 receipt
// envelope. The caller can bind its expected/value/payload request semantics
// to the exact envelope rather than relying on the projected source receipt.
func (s *BdStore) ControllerMetadataTransitionReceipt(issueID, receiptID string) (ControllerMetadataTransitionReceipt, bool, error) {
	if s == nil || s.privateEvidenceHTTP == nil || s.privateEvidenceHTTPInitErr != nil || !s.privateEvidenceHTTP.revisionTransitions {
		return ControllerMetadataTransitionReceipt{}, false, ErrDecisionFrontierReceiptReadUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*privateEvidenceHTTPTimeout)
	defer cancel()
	return s.privateEvidenceHTTP.controllerMetadataTransitionReceipt(ctx, issueID, receiptID)
}

func (c *privateEvidenceHTTPClient) controllerMetadataTransitionReceipt(ctx context.Context, issueID, receiptID string) (ControllerMetadataTransitionReceipt, bool, error) {
	if !c.revisionTransitions {
		return ControllerMetadataTransitionReceipt{}, false, ErrDecisionFrontierReceiptReadUnavailable
	}
	if err := validateControllerTransitionText(issueID, controllerTransitionMaxIssueID); err != nil || hasControllerTransitionControl(issueID) ||
		validateControllerTransitionText(receiptID, controllerTransitionMaxReceiptID) != nil || len(receiptID) > controllerTransitionMaxReceiptID || strings.ContainsRune(receiptID, '\x00') {
		return ControllerMetadataTransitionReceipt{}, false, fmt.Errorf("%w: issue or receipt identity is invalid", ErrDecisionFrontierReceiptReadProtocol)
	}
	if err := c.verifyRevisionTransitionContext(ctx, false); err != nil {
		return ControllerMetadataTransitionReceipt{}, false, fmt.Errorf("%w: transition receipt context is unavailable", ErrDecisionFrontierReceiptReadUnavailable)
	}
	path := controllerTransitionReceiptPath + url.PathEscape(receiptID)
	body, status, err := c.requestWithResponseCaps(ctx, http.MethodGet, path, nil,
		controllerTransitionMaxSuccessBody, controllerTransitionMaxProblemBody)
	if err != nil {
		return ControllerMetadataTransitionReceipt{}, false, fmt.Errorf("%w: receipt request failed", ErrDecisionFrontierReceiptReadUnavailable)
	}
	if status == http.StatusNotFound {
		var problem struct {
			Code string `json:"code"`
		}
		if decodePrivateEvidenceJSON(body, &problem) == nil && problem.Code == "not_found" {
			return ControllerMetadataTransitionReceipt{}, false, nil
		}
		return ControllerMetadataTransitionReceipt{}, false, fmt.Errorf("%w: unrecognized missing-receipt response", ErrDecisionFrontierReceiptReadProtocol)
	}
	if status != http.StatusOK {
		return ControllerMetadataTransitionReceipt{}, false, fmt.Errorf("%w: receipt lookup returned HTTP %d", ErrDecisionFrontierReceiptReadProtocol, status)
	}
	remote, err := decodeControllerMetadataTransitionReceipt(body)
	if err != nil || remote.ReceiptID != receiptID || remote.IssueID != issueID ||
		!c.metadataTransitionScopeAllowed(remote.Scope, remote.Kind) {
		return ControllerMetadataTransitionReceipt{}, false, fmt.Errorf("%w: receipt identity does not match the query", ErrDecisionFrontierTransitionReceiptCorrupt)
	}
	if _, err := decodeDecisionFrontierControllerReceipt(remote); err != nil {
		return ControllerMetadataTransitionReceipt{}, false, fmt.Errorf("%w: invalid Q43 source-transition receipt", ErrDecisionFrontierTransitionReceiptCorrupt)
	}
	return cloneControllerMetadataTransitionReceipt(remote), true, nil
}

func cloneControllerMetadataTransitionReceipt(receipt ControllerMetadataTransitionReceipt) ControllerMetadataTransitionReceipt {
	receipt.Expected = append(json.RawMessage(nil), receipt.Expected...)
	receipt.Value = append(json.RawMessage(nil), receipt.Value...)
	receipt.Payload = append(json.RawMessage(nil), receipt.Payload...)
	return receipt
}

func decodeDecisionFrontierControllerReceipt(remote ControllerMetadataTransitionReceipt) (RevisionTransitionReceipt, error) {
	var result RevisionTransitionReceipt
	var members map[string]json.RawMessage
	if err := json.Unmarshal(remote.Payload, &members); err != nil || members == nil ||
		!controllerTransitionOnlyMembers(members, "id", "city_ref", "store_ref", "work_id", "map_id", "operation", "from_revision", "to_revision") {
		return result, errors.New("receipt payload shape is invalid")
	}
	for _, name := range []string{"id", "city_ref", "store_ref", "work_id", "map_id", "operation", "from_revision", "to_revision"} {
		if _, ok := members[name]; !ok {
			return result, errors.New("receipt payload is incomplete")
		}
	}
	if err := json.Unmarshal(remote.Payload, &result); err != nil || result.ID == "" || result.CityRef == "" ||
		result.StoreRef == "" || result.WorkID == "" || result.MapID == "" || result.Operation == "" ||
		(result.Operation != "reserve" && result.Operation != "release") || result.FromRevision == 0 || result.ToRevision != 0 {
		return RevisionTransitionReceipt{}, errors.New("receipt payload identity is invalid")
	}
	if remote.ReceiptID != result.ID || remote.IssueID != result.WorkID || remote.Scope != result.StoreRef ||
		remote.Kind != controllerDecisionFrontierTransitionKind || remote.Actor != privateEvidenceActor ||
		remote.ExpectedVersion != result.FromRevision || remote.Key != beadmeta.DecisionFrontierHoldMetadataKey ||
		remote.ToVersion == 0 || remote.ToVersion == result.FromRevision {
		return RevisionTransitionReceipt{}, errors.New("Q43 receipt envelope does not match the source receipt")
	}
	var marker string
	switch result.Operation {
	case "reserve":
		if len(remote.Expected) != 0 || len(remote.Value) == 0 || result.ID == "" {
			return RevisionTransitionReceipt{}, errors.New("reservation receipt has invalid marker values")
		}
		if err := json.Unmarshal(remote.Value, &marker); err != nil || marker == "" {
			return RevisionTransitionReceipt{}, errors.New("reservation receipt has invalid hold marker")
		}
	case "release":
		if len(remote.Expected) == 0 || len(remote.Value) != 0 {
			return RevisionTransitionReceipt{}, errors.New("release receipt has invalid marker values")
		}
		if err := json.Unmarshal(remote.Expected, &marker); err != nil || marker == "" {
			return RevisionTransitionReceipt{}, errors.New("release receipt has invalid hold marker")
		}
	}
	var hold decisionFrontierHoldBinding
	if err := json.Unmarshal([]byte(marker), &hold); err != nil || hold.SchemaVersion != 1 || hold.CityRef != result.CityRef ||
		hold.StoreRef != result.StoreRef || hold.WorkID != result.WorkID || hold.MapID != result.MapID ||
		hold.WorkDigest == "" || hold.ProposalHash == "" || hold.ReservationID == "" ||
		hold.MapID != DecisionFrontierMapRecordID(hold.CityRef, hold.StoreRef, hold.WorkID, hold.WorkRevision) {
		return RevisionTransitionReceipt{}, errors.New("hold marker does not match the receipt payload")
	}
	canonicalMarker, err := json.Marshal(hold)
	if err != nil || !bytes.Equal(canonicalMarker, []byte(marker)) {
		return RevisionTransitionReceipt{}, errors.New("hold marker is not canonical")
	}
	baseRevision, err := strconv.ParseInt(hold.WorkRevision, 10, 64)
	if err != nil || baseRevision == 0 || strconv.FormatInt(baseRevision, 10) != hold.WorkRevision {
		return RevisionTransitionReceipt{}, errors.New("hold marker revision is invalid")
	}
	wantReservationID := decisionFrontierStableID("frontier-transition", result.CityRef, result.StoreRef, result.MapID, "reserve")
	wantReceiptID := decisionFrontierStableID("frontier-transition", result.CityRef, result.StoreRef, result.MapID, result.Operation)
	if hold.ReservationID != wantReservationID || result.ID != wantReceiptID ||
		result.Operation == "reserve" && (result.ID != hold.ReservationID || result.FromRevision != baseRevision) ||
		result.Operation == "release" && result.ID == hold.ReservationID {
		return RevisionTransitionReceipt{}, errors.New("source receipt identity does not match the hold marker")
	}
	result.ToRevision = remote.ToVersion
	return result, nil
}
