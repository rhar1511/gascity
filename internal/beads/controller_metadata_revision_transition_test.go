package beads

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

type controllerRevisionTransitionFixture struct {
	bead                  Bead
	receipts              map[string]RevisionTransitionReceipt
	receiptEnvelopes      map[string]ControllerMetadataTransitionReceipt
	requests              map[string]ControllerMetadataTransitionRequest
	calls                 []ControllerMetadataTransitionRequest
	nextRevision          int64
	dropAfterWrite        bool
	mutateReceiptEnvelope func(*ControllerMetadataTransitionReceipt)
}

func (f *controllerRevisionTransitionFixture) TransitionMetadata(issueID string, request ControllerMetadataTransitionRequest) (ControllerMetadataTransitionResult, error) {
	f.calls = append(f.calls, cloneControllerMetadataTransitionRequest(request))
	if issueID != f.bead.ID {
		return ControllerMetadataTransitionResult{Current: json.RawMessage(`null`)}, nil
	}
	if prior, found := f.receipts[request.ReceiptID]; found {
		if !sameTestControllerTransitionRequest(f.requests[request.ReceiptID], request) {
			return ControllerMetadataTransitionResult{}, errors.New("receipt ID replay changed its request")
		}
		return ControllerMetadataTransitionResult{
			Applied: true, Replayed: true,
			Receipt: fixtureControllerTransitionReceipt(issueID, request, prior.ToRevision),
		}, nil
	}
	if request.ExpectedVersion != f.bead.Revision {
		return ControllerMetadataTransitionResult{Current: json.RawMessage(`null`)}, nil
	}
	expected, err := controllerRequestMarker(request.Expected)
	if err != nil {
		return ControllerMetadataTransitionResult{}, err
	}
	value, err := controllerRequestMarker(request.Value)
	if err != nil {
		return ControllerMetadataTransitionResult{}, err
	}
	if f.bead.Metadata[request.Key] != expected {
		current, _ := json.Marshal(f.bead.Metadata[request.Key])
		return ControllerMetadataTransitionResult{Current: current}, nil
	}
	var sourceReceipt RevisionTransitionReceipt
	if err := json.Unmarshal(request.Payload, &sourceReceipt); err != nil || sourceReceipt.ToRevision != 0 {
		return ControllerMetadataTransitionResult{}, errors.New("invalid source receipt payload")
	}
	if f.nextRevision == 0 || f.nextRevision == f.bead.Revision {
		return ControllerMetadataTransitionResult{}, errors.New("invalid fixture result revision")
	}
	sourceReceipt.ToRevision = f.nextRevision
	if f.receipts == nil {
		f.receipts = make(map[string]RevisionTransitionReceipt)
	}
	f.receipts[sourceReceipt.ID] = sourceReceipt
	if f.receiptEnvelopes == nil {
		f.receiptEnvelopes = make(map[string]ControllerMetadataTransitionReceipt)
	}
	envelope := *fixtureControllerTransitionReceipt(issueID, request, f.nextRevision)
	if f.mutateReceiptEnvelope != nil {
		f.mutateReceiptEnvelope(&envelope)
	}
	f.receiptEnvelopes[sourceReceipt.ID] = cloneControllerMetadataTransitionReceipt(envelope)
	if f.requests == nil {
		f.requests = make(map[string]ControllerMetadataTransitionRequest)
	}
	f.requests[sourceReceipt.ID] = cloneControllerMetadataTransitionRequest(request)
	if f.bead.Metadata == nil {
		f.bead.Metadata = make(StringMap)
	}
	if value == "" {
		delete(f.bead.Metadata, request.Key)
	} else {
		f.bead.Metadata[request.Key] = value
	}
	f.bead.Revision = f.nextRevision
	if f.dropAfterWrite {
		f.dropAfterWrite = false
		return ControllerMetadataTransitionResult{}, errors.New("simulated lost transition response")
	}
	return ControllerMetadataTransitionResult{Applied: true, Receipt: fixtureControllerTransitionReceipt(issueID, request, f.nextRevision)}, nil
}

func fixtureControllerTransitionReceipt(issueID string, request ControllerMetadataTransitionRequest, toRevision int64) *ControllerMetadataTransitionReceipt {
	remoteReceipt := &ControllerMetadataTransitionReceipt{
		ReceiptID: request.ReceiptID, IssueID: issueID,
		Scope: request.Scope, Kind: request.Kind, Actor: request.Actor,
		ExpectedVersion: request.ExpectedVersion, ToVersion: toRevision, Key: request.Key,
		Payload: append(json.RawMessage(nil), request.Payload...),
	}
	if request.Expected != nil {
		remoteReceipt.Expected = append(json.RawMessage(nil), (*request.Expected)...)
	}
	if request.Value != nil {
		remoteReceipt.Value = append(json.RawMessage(nil), (*request.Value)...)
	}
	return remoteReceipt
}

func sameTestControllerTransitionRequest(left, right ControllerMetadataTransitionRequest) bool {
	return left.ReceiptID == right.ReceiptID && left.Scope == right.Scope && left.Kind == right.Kind && left.Actor == right.Actor &&
		left.ExpectedVersion == right.ExpectedVersion && left.Key == right.Key && left.ProtectedPermit == right.ProtectedPermit &&
		bytes.Equal(nilSafeRaw(left.Expected), nilSafeRaw(right.Expected)) && bytes.Equal(nilSafeRaw(left.Value), nilSafeRaw(right.Value)) &&
		bytes.Equal(left.Payload, right.Payload)
}

func (f *controllerRevisionTransitionFixture) DecisionFrontierSourceSnapshot(issueID string) (Bead, error) {
	if issueID != f.bead.ID {
		return Bead{}, ErrNotFound
	}
	return cloneBead(f.bead), nil
}

func (f *controllerRevisionTransitionFixture) DecisionFrontierRevisionTransitionReceipt(issueID, receiptID string) (RevisionTransitionReceipt, bool, error) {
	if issueID != f.bead.ID {
		return RevisionTransitionReceipt{}, false, ErrNotFound
	}
	receipt, found := f.receipts[receiptID]
	return receipt, found, nil
}

func (f *controllerRevisionTransitionFixture) ControllerMetadataTransitionReceipt(issueID, receiptID string) (ControllerMetadataTransitionReceipt, bool, error) {
	if issueID != f.bead.ID {
		return ControllerMetadataTransitionReceipt{}, false, ErrNotFound
	}
	receipt, found := f.receiptEnvelopes[receiptID]
	if !found {
		return ControllerMetadataTransitionReceipt{}, false, nil
	}
	return cloneControllerMetadataTransitionReceipt(receipt), true, nil
}

func cloneControllerMetadataTransitionRequest(request ControllerMetadataTransitionRequest) ControllerMetadataTransitionRequest {
	if request.Expected != nil {
		copy := append(json.RawMessage(nil), (*request.Expected)...)
		request.Expected = &copy
	}
	if request.Value != nil {
		copy := append(json.RawMessage(nil), (*request.Value)...)
		request.Value = &copy
	}
	request.Payload = append(json.RawMessage(nil), request.Payload...)
	return request
}

func TestControllerMetadataRevisionTransitionRereadsAndBindsReserveAndRelease(t *testing.T) {
	const (
		issueID  = "work-transition-adapter"
		cityRef  = "city:adapter"
		storeRef = "city:adapter"
	)
	baseRevision := int64(7)
	mapID := DecisionFrontierMapRecordID(cityRef, storeRef, issueID, "7")
	reservationID := decisionFrontierStableID("frontier-transition", cityRef, storeRef, mapID, "reserve")
	markerBytes, err := json.Marshal(decisionFrontierHoldBinding{
		SchemaVersion: 1, CityRef: cityRef, StoreRef: storeRef, WorkID: issueID, MapID: mapID,
		WorkRevision: "7", WorkDigest: "work-digest", ProposalHash: "proposal-hash", ReservationID: reservationID,
	})
	if err != nil {
		t.Fatal(err)
	}
	marker := string(markerBytes)
	fixture := &controllerRevisionTransitionFixture{
		bead: Bead{ID: issueID, Type: "task", Title: "source", Revision: baseRevision}, nextRevision: 23,
	}
	writer, err := NewControllerMetadataRevisionTransitionWriter(fixture, fixture, fixture, fixture)
	if err != nil {
		t.Fatal(err)
	}
	reservation := RevisionTransitionReceipt{
		ID: reservationID, CityRef: cityRef, StoreRef: storeRef, WorkID: issueID, MapID: mapID,
		Operation: "reserve", FromRevision: baseRevision,
	}
	reserved, won, err := writer.CompareAndSetMetadataKeyWithReceipt(issueID, beadmeta.DecisionFrontierHoldMetadataKey, "", marker, baseRevision, reservation)
	if err != nil || !won {
		t.Fatalf("reserve transition: won=%v err=%v", won, err)
	}
	if reserved.Revision != 23 || reserved.Metadata[beadmeta.DecisionFrontierHoldMetadataKey] != marker {
		t.Fatalf("returned reserve row = %+v, want authoritative revision 23 and exact marker", reserved)
	}
	if len(fixture.calls) != 1 || fixture.calls[0].ExpectedVersion != baseRevision || fixture.calls[0].Key != beadmeta.DecisionFrontierHoldMetadataKey ||
		fixture.calls[0].ReceiptID != reservation.ID || fixture.calls[0].ProtectedPermit != "" {
		t.Fatalf("Q43 reservation request = %+v", fixture.calls)
	}
	if receipt, found, err := fixture.DecisionFrontierRevisionTransitionReceipt(issueID, reservation.ID); err != nil || !found || receipt.ToRevision != 23 {
		t.Fatalf("persisted reservation receipt = %+v found=%v err=%v", receipt, found, err)
	}

	fixture.nextRevision = 41
	release := RevisionTransitionReceipt{
		ID: decisionFrontierStableID("frontier-transition", cityRef, storeRef, mapID, "release"), CityRef: cityRef, StoreRef: storeRef, WorkID: issueID, MapID: mapID,
		Operation: "release", FromRevision: reserved.Revision,
	}
	released, won, err := writer.CompareAndSetMetadataKeyWithReceipt(issueID, beadmeta.DecisionFrontierHoldMetadataKey, marker, "", reserved.Revision, release)
	if err != nil || !won {
		t.Fatalf("release transition: won=%v err=%v", won, err)
	}
	if released.Revision != 41 || released.Metadata[beadmeta.DecisionFrontierHoldMetadataKey] != "" {
		t.Fatalf("returned release row = %+v, want authoritative revision 41 and no marker", released)
	}
	if len(fixture.calls) != 2 || fixture.calls[1].ExpectedVersion != 23 || fixture.calls[1].Expected == nil || fixture.calls[1].Value != nil ||
		fixture.calls[1].ReceiptID != release.ID {
		t.Fatalf("Q43 release request = %+v", fixture.calls)
	}
	if receipt, found, err := fixture.DecisionFrontierRevisionTransitionReceipt(issueID, release.ID); err != nil || !found || receipt.FromRevision != 23 || receipt.ToRevision != 41 {
		t.Fatalf("persisted release receipt = %+v found=%v err=%v", receipt, found, err)
	}
}

func TestBdStoreRevisionTransitionWriterRequiresCompleteControllerTransport(t *testing.T) {
	unconfigured := NewBdStoreWithPrefix(t.TempDir(), nil, "gc")
	if reader, ok := ControllerMetadataTransitionReceiptReaderFor(unconfigured); ok || reader != nil {
		t.Fatal("unconfigured BdStore advertised full controller receipt envelopes")
	}
	if writer, ok := RevisionTransitionWriterFor(unconfigured); ok || writer != nil {
		t.Fatal("unconfigured BdStore advertised controller-backed source transitions")
	}

	configured := controllerTransitionTestStore(t, &controllerTransitionTestTransport{
		handler: controllerTransitionTestHandler(nil, true),
	})
	writer, ok := RevisionTransitionWriterFor(configured)
	if !ok || writer == nil {
		t.Fatal("fully configured BdStore did not compose the controller-backed source transition")
	}
	if reader, ok := ControllerMetadataTransitionReceiptReaderFor(configured); !ok || reader == nil {
		t.Fatal("configured BdStore hid full controller receipt envelopes")
	}
}

func TestControllerMetadataRevisionTransitionRejectsPersistedEnvelopeMarkerConflict(t *testing.T) {
	const (
		issueID  = "work-transition-envelope-conflict"
		cityRef  = "city:adapter"
		storeRef = "city:adapter"
	)
	const baseRevision = int64(7)
	mapID := DecisionFrontierMapRecordID(cityRef, storeRef, issueID, "7")
	reservationID := decisionFrontierStableID("frontier-transition", cityRef, storeRef, mapID, "reserve")
	hold := decisionFrontierHoldBinding{
		SchemaVersion: 1, CityRef: cityRef, StoreRef: storeRef, WorkID: issueID, MapID: mapID,
		WorkRevision: "7", WorkDigest: "work-digest", ProposalHash: "proposal-hash", ReservationID: reservationID,
	}
	markerBytes, err := json.Marshal(hold)
	if err != nil {
		t.Fatal(err)
	}
	tamperedHold := hold
	tamperedHold.ProposalHash = "different-proposal-hash"
	tamperedMarkerBytes, err := json.Marshal(tamperedHold)
	if err != nil {
		t.Fatal(err)
	}
	tamperedMarker, err := json.Marshal(string(tamperedMarkerBytes))
	if err != nil {
		t.Fatal(err)
	}
	fixture := &controllerRevisionTransitionFixture{
		bead: Bead{ID: issueID, Type: "task", Title: "source", Revision: baseRevision}, nextRevision: 23,
		mutateReceiptEnvelope: func(receipt *ControllerMetadataTransitionReceipt) {
			receipt.Value = append(json.RawMessage(nil), tamperedMarker...)
		},
	}
	writer, err := NewControllerMetadataRevisionTransitionWriter(fixture, fixture, fixture, fixture)
	if err != nil {
		t.Fatal(err)
	}
	requestReceipt := RevisionTransitionReceipt{
		ID: reservationID, CityRef: cityRef, StoreRef: storeRef, WorkID: issueID, MapID: mapID,
		Operation: "reserve", FromRevision: baseRevision,
	}
	_, won, err := writer.CompareAndSetMetadataKeyWithReceipt(issueID, beadmeta.DecisionFrontierHoldMetadataKey, "", string(markerBytes), baseRevision, requestReceipt)
	if won || !errors.Is(err, ErrDecisionFrontierTransitionReceiptCorrupt) {
		t.Fatalf("conflicting persisted hold marker won=%v err=%v, want corrupt durable receipt", won, err)
	}
	projected, found, readErr := fixture.DecisionFrontierRevisionTransitionReceipt(issueID, reservationID)
	if readErr != nil || !found || projected.ID != requestReceipt.ID || projected.ToRevision != 23 {
		t.Fatalf("projected receipt = %+v found=%v err=%v, want same projected identity and revision", projected, found, readErr)
	}
}

func TestControllerMetadataRevisionTransitionRecoversLostReplyFromExactReceiptAndSnapshot(t *testing.T) {
	const (
		issueID  = "work-transition-recovery"
		cityRef  = "city:adapter"
		storeRef = "city:adapter"
	)
	mapID := DecisionFrontierMapRecordID(cityRef, storeRef, issueID, "5")
	reservationID := decisionFrontierStableID("frontier-transition", cityRef, storeRef, mapID, "reserve")
	markerBytes, err := json.Marshal(decisionFrontierHoldBinding{
		SchemaVersion: 1, CityRef: cityRef, StoreRef: storeRef, WorkID: issueID, MapID: mapID,
		WorkRevision: "5", WorkDigest: "work-digest", ProposalHash: "proposal-hash", ReservationID: reservationID,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture := &controllerRevisionTransitionFixture{
		bead: Bead{ID: issueID, Type: "task", Title: "source", Revision: 5}, nextRevision: 19, dropAfterWrite: true,
	}
	writer, err := NewControllerMetadataRevisionTransitionWriter(fixture, fixture, fixture, fixture)
	if err != nil {
		t.Fatal(err)
	}
	receipt := RevisionTransitionReceipt{
		ID: reservationID, CityRef: cityRef, StoreRef: storeRef, WorkID: issueID, MapID: mapID,
		Operation: "reserve", FromRevision: 5,
	}
	bead, won, err := writer.CompareAndSetMetadataKeyWithReceipt(issueID, beadmeta.DecisionFrontierHoldMetadataKey, "", string(markerBytes), 5, receipt)
	if err != nil || !won {
		t.Fatalf("lost-reply recovery: won=%v err=%v", won, err)
	}
	if bead.Revision != 19 || bead.Metadata[beadmeta.DecisionFrontierHoldMetadataKey] != string(markerBytes) || len(fixture.calls) != 2 ||
		!sameTestControllerTransitionRequest(fixture.calls[0], fixture.calls[1]) {
		t.Fatalf("recovered row = %+v calls=%d, want committed opaque revision 19 after one exact replay", bead, len(fixture.calls))
	}
}

func TestControllerMetadataRevisionTransitionRejectsStaleRevisionBeforeWrite(t *testing.T) {
	const (
		issueID  = "work-stale"
		cityRef  = "city:adapter"
		storeRef = "city:adapter"
	)
	mapID := DecisionFrontierMapRecordID(cityRef, storeRef, issueID, "7")
	reservationID := decisionFrontierStableID("frontier-transition", cityRef, storeRef, mapID, "reserve")
	marker, err := json.Marshal(decisionFrontierHoldBinding{
		SchemaVersion: 1, CityRef: cityRef, StoreRef: storeRef, WorkID: issueID, MapID: mapID,
		WorkRevision: "7", WorkDigest: "digest", ProposalHash: "proposal", ReservationID: reservationID,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture := &controllerRevisionTransitionFixture{bead: Bead{ID: issueID, Revision: 8}}
	writer, err := NewControllerMetadataRevisionTransitionWriter(fixture, fixture, fixture, fixture)
	if err != nil {
		t.Fatal(err)
	}
	_, won, err := writer.CompareAndSetMetadataKeyWithReceipt(issueID, beadmeta.DecisionFrontierHoldMetadataKey, "", string(marker), 7,
		RevisionTransitionReceipt{ID: reservationID, CityRef: cityRef, StoreRef: storeRef, WorkID: issueID, MapID: mapID, Operation: "reserve", FromRevision: 7})
	if err == nil || won || len(fixture.calls) != 0 {
		t.Fatalf("stale transition result won=%v err=%v Q43 calls=%d, want fail closed before write", won, err, len(fixture.calls))
	}
}
