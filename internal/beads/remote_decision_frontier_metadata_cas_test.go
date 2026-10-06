package beads

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

type remoteDecisionFrontierMetadataPermitIssuerStub struct {
	requests  []ControllerProtectedMutationRequest
	replayIDs []string
	token     string
	err       error
}

func (issuer *remoteDecisionFrontierMetadataPermitIssuerStub) IssueProtectedMutation(request ControllerProtectedMutationRequest, replayID string) (string, error) {
	request.ResourceIDs = append([]string(nil), request.ResourceIDs...)
	issuer.requests = append(issuer.requests, request)
	issuer.replayIDs = append(issuer.replayIDs, replayID)
	return issuer.token, issuer.err
}

type remoteDecisionFrontierMetadataTransitionWriterStub struct {
	reader              *remoteDecisionFrontierLinkRecordReaderStub
	requests            []ControllerMetadataTransitionRequest
	toRevision          int64
	replayed            bool
	refuseWith          *string
	corruptReceipt      bool
	mutateTitleAfterCAS bool
	omitDurableReceipt  bool
	dropAfterWrite      bool
	receipts            map[string]ControllerMetadataTransitionReceipt
	err                 error
}

func (writer *remoteDecisionFrontierMetadataTransitionWriterStub) TransitionMetadata(issueID string, request ControllerMetadataTransitionRequest) (ControllerMetadataTransitionResult, error) {
	request = cloneControllerMetadataTransitionRequest(request)
	writer.requests = append(writer.requests, request)
	if writer.err != nil {
		return ControllerMetadataTransitionResult{}, writer.err
	}
	current, ok := writer.reader.records[issueID]
	if !ok || current.ID != issueID {
		return ControllerMetadataTransitionResult{}, ErrNotFound
	}
	if writer.refuseWith != nil {
		current.Metadata[request.Key] = *writer.refuseWith
		current.Revision = writer.toRevision
		writer.reader.records[issueID] = current
		encoded, _ := json.Marshal(*writer.refuseWith)
		return ControllerMetadataTransitionResult{Current: encoded}, nil
	}
	if request.ExpectedVersion != current.Revision {
		return ControllerMetadataTransitionResult{}, errors.New("unexpected test revision")
	}
	var expected *json.RawMessage
	if value, present := current.Metadata[request.Key]; present {
		encoded, _ := json.Marshal(value)
		raw := json.RawMessage(encoded)
		expected = &raw
	}
	if !controllerTransitionValuesEqual(expected, request.Expected) {
		return ControllerMetadataTransitionResult{}, errors.New("unexpected test marker")
	}
	var value string
	if request.Value == nil || json.Unmarshal(*request.Value, &value) != nil {
		return ControllerMetadataTransitionResult{}, errors.New("invalid test next marker")
	}
	if current.Metadata == nil {
		current.Metadata = make(StringMap)
	}
	current.Metadata[request.Key] = value
	current.Revision = writer.toRevision
	if writer.mutateTitleAfterCAS {
		current.Title += " changed"
	}
	writer.reader.records[issueID] = current
	receipt := &ControllerMetadataTransitionReceipt{
		ReceiptID: request.ReceiptID, IssueID: issueID, Scope: request.Scope, Kind: request.Kind,
		Actor: request.Actor, ExpectedVersion: request.ExpectedVersion, ToVersion: writer.toRevision,
		Key: request.Key, Payload: append(json.RawMessage(nil), request.Payload...),
	}
	if request.Expected != nil {
		receipt.Expected = append(json.RawMessage(nil), (*request.Expected)...)
	}
	if request.Value != nil {
		receipt.Value = append(json.RawMessage(nil), (*request.Value)...)
	}
	if writer.corruptReceipt {
		receipt.ReceiptID += "-changed"
	}
	if !writer.omitDurableReceipt {
		if writer.receipts == nil {
			writer.receipts = make(map[string]ControllerMetadataTransitionReceipt)
		}
		writer.receipts[request.ReceiptID] = cloneControllerMetadataTransitionReceipt(*receipt)
	}
	if writer.dropAfterWrite {
		return ControllerMetadataTransitionResult{}, errors.New("simulated lost metadata transition response")
	}
	return ControllerMetadataTransitionResult{Applied: true, Replayed: writer.replayed, Receipt: receipt}, nil
}

func (writer *remoteDecisionFrontierMetadataTransitionWriterStub) ControllerMetadataTransitionReceipt(issueID, receiptID string) (ControllerMetadataTransitionReceipt, bool, error) {
	receipt, found := writer.receipts[receiptID]
	if !found || receipt.IssueID != issueID {
		return ControllerMetadataTransitionReceipt{}, false, nil
	}
	return cloneControllerMetadataTransitionReceipt(receipt), true, nil
}

func newRemoteDecisionFrontierMetadataCASFixture(t *testing.T, record Bead, issuer *remoteDecisionFrontierMetadataPermitIssuerStub, transition *remoteDecisionFrontierMetadataTransitionWriterStub) (*RemoteDecisionFrontierRecordWriter, *remoteDecisionFrontierLinkRecordReaderStub) {
	t.Helper()
	reader := remoteDecisionFrontierLinkTestRecords(t, "wrk-1")
	reader.records[record.ID] = cloneBead(record)
	if transition != nil {
		transition.reader = reader
	}
	writer, err := NewRemoteDecisionFrontierRecordWriter(RemoteDecisionFrontierRecordWriterConfig{
		Actor: "controller-test-actor", ProtectionClass: "frontier-records",
		PermitIssuer: &remoteDecisionFrontierTestPermitIssuer{token: "unused-create-permit"},
		BatchWriter:  &remoteDecisionFrontierTestBatchWriter{}, LinkWriter: &remoteDecisionFrontierTestLinkWriter{},
		MetadataTransitionScope: "store-test", MetadataTransitionKind: "decision-frontier-record-metadata",
		MetadataRecordReader: reader, MetadataPermitIssuer: issuer, MetadataTransitionWriter: transition,
		MetadataReceiptReader: transition,
	})
	if err != nil {
		t.Fatalf("NewRemoteDecisionFrontierRecordWriter: %v", err)
	}
	return writer, reader
}

func remoteDecisionFrontierMetadataCASMapRecord(t *testing.T, revision int64) Bead {
	t.Helper()
	reader := remoteDecisionFrontierLinkTestRecords(t, "wrk-1")
	mapID := DecisionFrontierMapRecordID("city-test", "store-test", "wrk-1", "7")
	record := cloneBead(reader.records[mapID])
	record.Revision = revision
	return record
}

func remoteDecisionFrontierMetadataCASPromptRecord(t *testing.T, revision int64) Bead {
	t.Helper()
	const cityRef, storeRef, workID, workRevision = "city-test", "store-test", "wrk-1", "7"
	mapID := DecisionFrontierMapRecordID(cityRef, storeRef, workID, workRevision)
	promptID := DecisionFrontierPromptRecordID(cityRef, storeRef, mapID)
	doc := decisionFrontierLinkDocument{
		SchemaVersion: 1, CityRef: cityRef, StoreRef: storeRef, WorkID: workID,
		WorkRevision: workRevision, WorkDigest: "work-digest", MapID: mapID, ID: promptID,
	}
	description, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal prompt document: %v", err)
	}
	return Bead{
		ID: promptID, Type: "gate", Title: "Prompt for wrk-1", Description: string(description), Revision: revision,
		Metadata: StringMap{
			beadmeta.DecisionFrontierRecordMetadataKey: decisionFrontierPromptKind,
			beadmeta.DecisionFrontierStateMetadataKey:  "unconfigured",
		},
	}
}

func TestControllerBeadsProtectedRevisionTransitionDigestMatchesPinnedGolden(t *testing.T) {
	expected, err := json.Marshal("pending")
	if err != nil {
		t.Fatal(err)
	}
	value, err := json.Marshal("resolved")
	if err != nil {
		t.Fatal(err)
	}
	expectedRaw, valueRaw := json.RawMessage(expected), json.RawMessage(value)
	digest, err := controllerBeadsProtectedRevisionTransitionDigest("gc-record-1", ControllerMetadataTransitionRequest{
		ReceiptID: "receipt-test", Scope: "rig:fixture", Kind: "decision-frontier-record-metadata",
		Actor: "controller-test-actor", ExpectedVersion: 7, Key: beadmeta.DecisionFrontierStateMetadataKey,
		Expected: &expectedRaw, Value: &valueRaw, Payload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("controllerBeadsProtectedRevisionTransitionDigest: %v", err)
	}
	const want = "9dc1875d200aa59ac356d13e9ab59e6999c4789f3c9f7823ddcf347405f9dde0"
	if digest != want {
		t.Fatalf("pinned revision-transition digest = %q, want %q", digest, want)
	}
}

func TestRemoteDecisionFrontierMetadataCASAcceptsExactReplayReceiptAndAuthoritativeRecord(t *testing.T) {
	issuer := &remoteDecisionFrontierMetadataPermitIssuerStub{token: "opaque-transition-permit"}
	transition := &remoteDecisionFrontierMetadataTransitionWriterStub{toRevision: 47, replayed: true}
	record := remoteDecisionFrontierMetadataCASMapRecord(t, 7)
	writer, reader := newRemoteDecisionFrontierMetadataCASFixture(t, record, issuer, transition)

	won, err := writer.CompareAndSetDecisionFrontierRecordMetadataKey(record.ID, beadmeta.DecisionFrontierStateMetadataKey, "pending", "resolved")
	if err != nil || !won {
		t.Fatalf("CompareAndSetDecisionFrontierRecordMetadataKey = %v, %v; want applied", won, err)
	}
	if len(issuer.requests) != 1 || len(transition.requests) != 1 {
		t.Fatalf("permit/transition calls = %d/%d, want 1/1", len(issuer.requests), len(transition.requests))
	}
	request := transition.requests[0]
	unsigned := cloneControllerMetadataTransitionRequest(request)
	unsigned.ProtectedPermit = ""
	digest, err := controllerBeadsProtectedRevisionTransitionDigest(record.ID, unsigned)
	if err != nil {
		t.Fatalf("digest applied transition: %v", err)
	}
	seed := unsigned
	seed.ReceiptID = ""
	seedDigest, err := controllerBeadsProtectedRevisionTransitionDigest(record.ID, seed)
	if err != nil {
		t.Fatalf("digest receipt seed: %v", err)
	}
	if request.ReceiptID != controllerDecisionFrontierMetadataCASReceiptPrefix+seedDigest ||
		issuer.replayIDs[0] != controllerDecisionFrontierMetadataCASReplayPrefix+digest {
		t.Fatalf("receipt/replay identity = %q/%q, want seed/final digest bindings", request.ReceiptID, issuer.replayIDs[0])
	}
	wantPermit := ControllerProtectedMutationRequest{
		Operation: controllerDecisionFrontierMetadataCASOperation, ResourceIDs: []string{record.ID}, RequestDigest: digest,
	}
	if !reflect.DeepEqual(issuer.requests[0], wantPermit) || request.ProtectedPermit != issuer.token ||
		request.ExpectedVersion != record.Revision || request.Scope != "store-test" || request.Kind != "decision-frontier-record-metadata" ||
		request.Actor != "controller-test-actor" || request.Key != beadmeta.DecisionFrontierStateMetadataKey {
		t.Fatalf("permit/request binding = %#v / %#v, want %#v and explicit Q43 fields", issuer.requests[0], request, wantPermit)
	}
	if got := reader.records[record.ID]; got.Revision != 47 || got.Metadata[beadmeta.DecisionFrontierStateMetadataKey] != "resolved" {
		t.Fatalf("authoritative record after transition = revision %d state %q, want 47/resolved", got.Revision, got.Metadata[beadmeta.DecisionFrontierStateMetadataKey])
	}
	if handle, ok := writer.DecisionFrontierRecordWriterHandle(); !ok || handle != writer {
		t.Fatal("complete protected create/link/CAS writer did not advertise its record-writer capability")
	}
}

func TestRemoteDecisionFrontierMetadataCASRecoversLostResponseFromDurableReceipt(t *testing.T) {
	issuer := &remoteDecisionFrontierMetadataPermitIssuerStub{token: "opaque-transition-permit"}
	transition := &remoteDecisionFrontierMetadataTransitionWriterStub{toRevision: 47, dropAfterWrite: true}
	record := remoteDecisionFrontierMetadataCASMapRecord(t, 7)
	writer, _ := newRemoteDecisionFrontierMetadataCASFixture(t, record, issuer, transition)
	if won, err := writer.CompareAndSetDecisionFrontierRecordMetadataKey(record.ID, beadmeta.DecisionFrontierStateMetadataKey, "pending", "resolved"); err != nil || !won {
		t.Fatalf("lost-response recovery = %v, %v; want durable receipt proof", won, err)
	}
}

func TestBdStoreDecisionFrontierWriterOptionAdvertisesOnlyCompleteRemoteWriter(t *testing.T) {
	incomplete := newRemoteDecisionFrontierTestWriter(t,
		&remoteDecisionFrontierTestPermitIssuer{token: "create-permit"},
		&remoteDecisionFrontierTestBatchWriter{}, &remoteDecisionFrontierTestLinkWriter{})
	incompleteStore := NewBdStore("/city", nil, WithBdStoreDecisionFrontierRecordWriter(incomplete))
	if writer, ok := DecisionFrontierRecordWriterFor(incompleteStore); ok || writer != nil {
		t.Fatalf("incomplete remote writer was advertised through BdStore: (%T, %t)", writer, ok)
	}

	record := remoteDecisionFrontierMetadataCASMapRecord(t, 7)
	complete, _ := newRemoteDecisionFrontierMetadataCASFixture(t, record,
		&remoteDecisionFrontierMetadataPermitIssuerStub{token: "metadata-permit"},
		&remoteDecisionFrontierMetadataTransitionWriterStub{toRevision: 47})
	completeStore := NewBdStore("/city", nil, WithBdStoreDecisionFrontierRecordWriter(complete))
	writer, ok := DecisionFrontierRecordWriterFor(completeStore)
	if !ok || writer != complete {
		t.Fatalf("complete remote writer handle = (%T, %t), want configured writer", writer, ok)
	}

	plainStore := NewBdStore("/city", nil)
	if writer, ok := DecisionFrontierRecordWriterFor(plainStore); ok || writer != nil {
		t.Fatalf("unconfigured BdStore advertised a record writer: (%T, %t)", writer, ok)
	}
}

func TestRemoteDecisionFrontierMetadataCASIsDeterministicAndPreservesReasonPresence(t *testing.T) {
	firstIssuer := &remoteDecisionFrontierMetadataPermitIssuerStub{token: "opaque-transition-permit"}
	firstTransition := &remoteDecisionFrontierMetadataTransitionWriterStub{toRevision: 47}
	firstRecord := remoteDecisionFrontierMetadataCASPromptRecord(t, 7)
	firstWriter, _ := newRemoteDecisionFrontierMetadataCASFixture(t, firstRecord, firstIssuer, firstTransition)
	if won, err := firstWriter.CompareAndSetDecisionFrontierRecordMetadataKey(firstRecord.ID, beadmeta.DecisionFrontierReasonMetadataKey, "", "reviewed"); err != nil || !won {
		t.Fatalf("first prompt reason CAS = %v, %v; want applied", won, err)
	}

	secondIssuer := &remoteDecisionFrontierMetadataPermitIssuerStub{token: "opaque-transition-permit"}
	secondTransition := &remoteDecisionFrontierMetadataTransitionWriterStub{toRevision: 47}
	secondRecord := remoteDecisionFrontierMetadataCASPromptRecord(t, 7)
	secondWriter, _ := newRemoteDecisionFrontierMetadataCASFixture(t, secondRecord, secondIssuer, secondTransition)
	if won, err := secondWriter.CompareAndSetDecisionFrontierRecordMetadataKey(secondRecord.ID, beadmeta.DecisionFrontierReasonMetadataKey, "", "reviewed"); err != nil || !won {
		t.Fatalf("retry prompt reason CAS = %v, %v; want applied", won, err)
	}
	if !reflect.DeepEqual(firstIssuer.requests, secondIssuer.requests) || !reflect.DeepEqual(firstIssuer.replayIDs, secondIssuer.replayIDs) ||
		!reflect.DeepEqual(firstTransition.requests, secondTransition.requests) {
		t.Fatal("same exact prompt reason CAS changed its durable receipt, replay, permit, or Q43 request identity")
	}
	if firstTransition.requests[0].Expected != nil || firstTransition.requests[0].Value == nil || string(*firstTransition.requests[0].Value) != `"reviewed"` {
		t.Fatalf("absent-to-present reason markers = %s / %v, want absent / JSON string", firstTransition.requests[0].Expected, firstTransition.requests[0].Value)
	}

	presentIssuer := &remoteDecisionFrontierMetadataPermitIssuerStub{token: "opaque-transition-permit"}
	presentTransition := &remoteDecisionFrontierMetadataTransitionWriterStub{toRevision: 47}
	presentRecord := remoteDecisionFrontierMetadataCASPromptRecord(t, 7)
	presentRecord.Metadata[beadmeta.DecisionFrontierReasonMetadataKey] = ""
	presentWriter, _ := newRemoteDecisionFrontierMetadataCASFixture(t, presentRecord, presentIssuer, presentTransition)
	if won, err := presentWriter.CompareAndSetDecisionFrontierRecordMetadataKey(presentRecord.ID, beadmeta.DecisionFrontierReasonMetadataKey, "", "reviewed"); err != nil || !won {
		t.Fatalf("present-empty prompt reason CAS = %v, %v; want applied", won, err)
	}
	presentExpected := presentTransition.requests[0].Expected
	if presentExpected == nil || string(*presentExpected) != `""` ||
		presentTransition.requests[0].ReceiptID == firstTransition.requests[0].ReceiptID ||
		presentIssuer.replayIDs[0] == firstIssuer.replayIDs[0] {
		t.Fatalf("present-empty reason identity = expected %v, receipt %q, replay %q; want explicit empty marker and distinct identities", presentExpected, presentTransition.requests[0].ReceiptID, presentIssuer.replayIDs[0])
	}
}

func TestRemoteDecisionFrontierMetadataCASRefusesInvalidAndStaleTransitionsBeforePermit(t *testing.T) {
	t.Run("direct writer transition policy", func(t *testing.T) {
		issuer := &remoteDecisionFrontierMetadataPermitIssuerStub{token: "opaque-transition-permit"}
		transition := &remoteDecisionFrontierMetadataTransitionWriterStub{toRevision: 47}
		record := remoteDecisionFrontierMetadataCASMapRecord(t, 7)
		writer, _ := newRemoteDecisionFrontierMetadataCASFixture(t, record, issuer, transition)
		if _, err := writer.CompareAndSetDecisionFrontierRecordMetadataKey(record.ID, beadmeta.DecisionFrontierStateMetadataKey, "pending", "answered:question-a"); !errors.Is(err, ErrDecisionFrontierMutationBlocked) {
			t.Fatalf("invalid map state transition error = %v, want ErrDecisionFrontierMutationBlocked", err)
		}
		if len(issuer.requests) != 0 || len(transition.requests) != 0 {
			t.Fatalf("invalid transition reached permit/Q43: %d/%d calls", len(issuer.requests), len(transition.requests))
		}
	})
	t.Run("stale expected marker", func(t *testing.T) {
		issuer := &remoteDecisionFrontierMetadataPermitIssuerStub{token: "opaque-transition-permit"}
		transition := &remoteDecisionFrontierMetadataTransitionWriterStub{toRevision: 47}
		record := remoteDecisionFrontierMetadataCASMapRecord(t, 7)
		writer, _ := newRemoteDecisionFrontierMetadataCASFixture(t, record, issuer, transition)
		if won, err := writer.CompareAndSetDecisionFrontierRecordMetadataKey(record.ID, beadmeta.DecisionFrontierStateMetadataKey, "unresolved", "resolved"); err != nil || won {
			t.Fatalf("stale CAS = %v, %v; want false without error", won, err)
		}
		if len(issuer.requests) != 0 || len(transition.requests) != 0 {
			t.Fatalf("stale CAS reached permit/Q43: %d/%d calls", len(issuer.requests), len(transition.requests))
		}
	})
	t.Run("malformed immutable record", func(t *testing.T) {
		issuer := &remoteDecisionFrontierMetadataPermitIssuerStub{token: "opaque-transition-permit"}
		transition := &remoteDecisionFrontierMetadataTransitionWriterStub{toRevision: 47}
		record := remoteDecisionFrontierMetadataCASMapRecord(t, 7)
		record.Description = `{"schema_version":1,"city_ref":"city-test","store_ref":"store-test","work_id":"wrk-1","work_revision":"7","map_id":"wrong"}`
		writer, _ := newRemoteDecisionFrontierMetadataCASFixture(t, record, issuer, transition)
		if _, err := writer.CompareAndSetDecisionFrontierRecordMetadataKey(record.ID, beadmeta.DecisionFrontierStateMetadataKey, "stale", "resolved"); !errors.Is(err, ErrDecisionFrontierLinkConflict) {
			t.Fatalf("malformed record error = %v, want immutable-record conflict", err)
		}
		if len(issuer.requests) != 0 || len(transition.requests) != 0 {
			t.Fatalf("malformed record reached permit/Q43: %d/%d calls", len(issuer.requests), len(transition.requests))
		}
	})
	t.Run("reader returned another ID", func(t *testing.T) {
		issuer := &remoteDecisionFrontierMetadataPermitIssuerStub{token: "opaque-transition-permit"}
		transition := &remoteDecisionFrontierMetadataTransitionWriterStub{toRevision: 47}
		record := remoteDecisionFrontierMetadataCASMapRecord(t, 7)
		writer, reader := newRemoteDecisionFrontierMetadataCASFixture(t, record, issuer, transition)
		wrong := reader.records[record.ID]
		wrong.ID = record.ID + "-other"
		reader.records[record.ID] = wrong
		if _, err := writer.CompareAndSetDecisionFrontierRecordMetadataKey(record.ID, beadmeta.DecisionFrontierStateMetadataKey, "pending", "resolved"); !errors.Is(err, ErrRemoteDecisionFrontierMetadataCASProtocol) {
			t.Fatalf("mismatched read ID error = %v, want protocol refusal", err)
		}
		if len(issuer.requests) != 0 || len(transition.requests) != 0 {
			t.Fatalf("mismatched read reached permit/Q43: %d/%d calls", len(issuer.requests), len(transition.requests))
		}
	})
	t.Run("invalid current revision", func(t *testing.T) {
		issuer := &remoteDecisionFrontierMetadataPermitIssuerStub{token: "opaque-transition-permit"}
		transition := &remoteDecisionFrontierMetadataTransitionWriterStub{toRevision: 47}
		record := remoteDecisionFrontierMetadataCASMapRecord(t, 0)
		writer, _ := newRemoteDecisionFrontierMetadataCASFixture(t, record, issuer, transition)
		if _, err := writer.CompareAndSetDecisionFrontierRecordMetadataKey(record.ID, beadmeta.DecisionFrontierStateMetadataKey, "pending", "resolved"); !errors.Is(err, ErrRemoteDecisionFrontierMetadataCASProtocol) {
			t.Fatalf("invalid current revision error = %v, want protocol refusal", err)
		}
		if len(issuer.requests) != 0 || len(transition.requests) != 0 {
			t.Fatalf("invalid revision reached permit/Q43: %d/%d calls", len(issuer.requests), len(transition.requests))
		}
	})
}

func TestRemoteDecisionFrontierMetadataCASValidatesRefusalAndRejectsUnprovedCommit(t *testing.T) {
	t.Run("authoritative marker refusal", func(t *testing.T) {
		issuer := &remoteDecisionFrontierMetadataPermitIssuerStub{token: "opaque-transition-permit"}
		externalState := "resolved"
		transition := &remoteDecisionFrontierMetadataTransitionWriterStub{toRevision: 48, refuseWith: &externalState}
		record := remoteDecisionFrontierMetadataCASMapRecord(t, 7)
		writer, _ := newRemoteDecisionFrontierMetadataCASFixture(t, record, issuer, transition)
		if won, err := writer.CompareAndSetDecisionFrontierRecordMetadataKey(record.ID, beadmeta.DecisionFrontierStateMetadataKey, "pending", "resolved"); err != nil || won {
			t.Fatalf("concurrent marker refusal = %v, %v; want false without error", won, err)
		}
	})
	t.Run("receipt mismatch", func(t *testing.T) {
		issuer := &remoteDecisionFrontierMetadataPermitIssuerStub{token: "opaque-transition-permit"}
		transition := &remoteDecisionFrontierMetadataTransitionWriterStub{toRevision: 48, corruptReceipt: true}
		record := remoteDecisionFrontierMetadataCASMapRecord(t, 7)
		writer, _ := newRemoteDecisionFrontierMetadataCASFixture(t, record, issuer, transition)
		if _, err := writer.CompareAndSetDecisionFrontierRecordMetadataKey(record.ID, beadmeta.DecisionFrontierStateMetadataKey, "pending", "resolved"); !errors.Is(err, ErrRemoteDecisionFrontierMetadataCASProtocol) {
			t.Fatalf("mismatched receipt error = %v, want protocol refusal", err)
		}
	})
	t.Run("response without durable receipt", func(t *testing.T) {
		issuer := &remoteDecisionFrontierMetadataPermitIssuerStub{token: "opaque-transition-permit"}
		transition := &remoteDecisionFrontierMetadataTransitionWriterStub{toRevision: 48, omitDurableReceipt: true}
		record := remoteDecisionFrontierMetadataCASMapRecord(t, 7)
		writer, _ := newRemoteDecisionFrontierMetadataCASFixture(t, record, issuer, transition)
		if _, err := writer.CompareAndSetDecisionFrontierRecordMetadataKey(record.ID, beadmeta.DecisionFrontierStateMetadataKey, "pending", "resolved"); !errors.Is(err, ErrRemoteDecisionFrontierMetadataCASProtocol) {
			t.Fatalf("missing durable receipt error = %v, want protocol refusal", err)
		}
	})
	t.Run("authoritative record mismatch", func(t *testing.T) {
		issuer := &remoteDecisionFrontierMetadataPermitIssuerStub{token: "opaque-transition-permit"}
		transition := &remoteDecisionFrontierMetadataTransitionWriterStub{toRevision: 48, mutateTitleAfterCAS: true}
		record := remoteDecisionFrontierMetadataCASMapRecord(t, 7)
		writer, _ := newRemoteDecisionFrontierMetadataCASFixture(t, record, issuer, transition)
		if _, err := writer.CompareAndSetDecisionFrontierRecordMetadataKey(record.ID, beadmeta.DecisionFrontierStateMetadataKey, "pending", "resolved"); !errors.Is(err, ErrRemoteDecisionFrontierMetadataCASProtocol) {
			t.Fatalf("changed immutable record error = %v, want protocol refusal", err)
		}
	})
}

func TestNewRemoteDecisionFrontierRecordWriterRequiresCompleteMetadataCASConfiguration(t *testing.T) {
	_, err := NewRemoteDecisionFrontierRecordWriter(RemoteDecisionFrontierRecordWriterConfig{
		Actor: "controller-test-actor", ProtectionClass: "frontier-records",
		PermitIssuer: &remoteDecisionFrontierTestPermitIssuer{token: "unused-create-permit"},
		BatchWriter:  &remoteDecisionFrontierTestBatchWriter{}, LinkWriter: &remoteDecisionFrontierTestLinkWriter{},
		MetadataTransitionScope: "store-test",
	})
	if !errors.Is(err, ErrRemoteDecisionFrontierWriterUnavailable) {
		t.Fatalf("partial metadata CAS config error = %v, want ErrRemoteDecisionFrontierWriterUnavailable", err)
	}

	var nilExactReader *remoteDecisionFrontierLinkRecordReaderStub
	transition := &remoteDecisionFrontierMetadataTransitionWriterStub{}
	_, err = NewRemoteDecisionFrontierRecordWriter(RemoteDecisionFrontierRecordWriterConfig{
		Actor: "controller-test-actor", ProtectionClass: "frontier-records",
		PermitIssuer: &remoteDecisionFrontierTestPermitIssuer{token: "unused-create-permit"},
		BatchWriter:  &remoteDecisionFrontierTestBatchWriter{}, LinkWriter: &remoteDecisionFrontierTestLinkWriter{},
		MetadataTransitionScope: "store-test", MetadataTransitionKind: "decision-frontier-record-metadata",
		MetadataRecordReader: nilExactReader, MetadataPermitIssuer: &remoteDecisionFrontierMetadataPermitIssuerStub{token: "metadata-permit"},
		MetadataTransitionWriter: transition, MetadataReceiptReader: transition,
	})
	if !errors.Is(err, ErrRemoteDecisionFrontierWriterUnavailable) {
		t.Fatalf("typed-nil exact record reader error = %v, want ErrRemoteDecisionFrontierWriterUnavailable", err)
	}
}
