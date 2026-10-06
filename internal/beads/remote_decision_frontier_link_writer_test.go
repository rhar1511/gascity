package beads

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

type remoteDecisionFrontierLinkPermitIssuerStub struct {
	requests  []ControllerProtectedLinkRequest
	replayIDs []string
	token     string
	err       error
}

func (issuer *remoteDecisionFrontierLinkPermitIssuerStub) IssueProtectedLink(request ControllerProtectedLinkRequest, replayID string) (string, error) {
	issuer.requests = append(issuer.requests, request)
	issuer.replayIDs = append(issuer.replayIDs, replayID)
	return issuer.token, issuer.err
}

type remoteDecisionFrontierProtectedLinkWriterStub struct {
	requests  []ControllerProtectedLinkRequest
	deadlines []time.Time
	err       error
}

type remoteDecisionFrontierLinkRecordReaderStub struct {
	records map[string]Bead
	calls   []string
}

func (reader *remoteDecisionFrontierLinkRecordReaderStub) Get(id string) (Bead, error) {
	reader.calls = append(reader.calls, id)
	record, ok := reader.records[id]
	if !ok {
		return Bead{}, ErrNotFound
	}
	return record, nil
}

func (writer *remoteDecisionFrontierProtectedLinkWriterStub) ApplyProtectedLink(ctx context.Context, request ControllerProtectedLinkRequest) (ControllerProtectedLinkResult, error) {
	if deadline, ok := ctx.Deadline(); ok {
		writer.deadlines = append(writer.deadlines, deadline)
	} else {
		writer.deadlines = append(writer.deadlines, time.Time{})
	}
	writer.requests = append(writer.requests, request)
	return ControllerProtectedLinkResult{}, writer.err
}

func newRemoteDecisionFrontierLinkTestWriter(t *testing.T, issuer ControllerProtectedLinkPermitIssuer, linkWriter ControllerProtectedLinkWriter) *RemoteDecisionFrontierLinkWriter {
	t.Helper()
	return newRemoteDecisionFrontierLinkTestWriterWithReader(t, issuer, linkWriter, remoteDecisionFrontierLinkTestRecords(t, "wrk-1"))
}

func newRemoteDecisionFrontierLinkTestWriterWithReader(t *testing.T, issuer ControllerProtectedLinkPermitIssuer, linkWriter ControllerProtectedLinkWriter, reader DecisionFrontierLinkRecordReader) *RemoteDecisionFrontierLinkWriter {
	t.Helper()
	writer, err := NewRemoteDecisionFrontierLinkWriter(RemoteDecisionFrontierLinkWriterConfig{
		Actor: "controller-test-actor", AllowedDependencyTypes: []string{"blocks", "relates-to"},
		BatchTimeout: 2 * time.Second, RecordReader: reader,
		PermitIssuer: issuer, ProtectedLinkWriter: linkWriter,
	})
	if err != nil {
		t.Fatalf("NewRemoteDecisionFrontierLinkWriter: %v", err)
	}
	return writer
}

func remoteDecisionFrontierLinkTestRecords(t *testing.T, workID string) *remoteDecisionFrontierLinkRecordReaderStub {
	t.Helper()
	const cityRef = "city-test"
	const storeRef = "store-test"
	const workRevision = "7"
	mapID := DecisionFrontierMapRecordID(cityRef, storeRef, workID, workRevision)
	promptID := DecisionFrontierPromptRecordID(cityRef, storeRef, mapID)
	questionA := decisionFrontierLinkQuestion{ID: "question-a", Title: "Question A", Prompt: "Choose A", DependsOn: []string{"question-b"}}
	questionB := decisionFrontierLinkQuestion{ID: "question-b", Title: "Question B", Prompt: "Choose B"}
	mapDoc := decisionFrontierLinkDocument{
		SchemaVersion: 1, CityRef: cityRef, StoreRef: storeRef, WorkID: workID,
		WorkRevision: workRevision, WorkDigest: "work-digest", MapID: mapID,
		PromptID: promptID, Questions: []decisionFrontierLinkQuestion{questionA, questionB},
	}
	mapDescription, err := json.Marshal(mapDoc)
	if err != nil {
		t.Fatalf("marshal map document: %v", err)
	}
	mapRecord := Bead{
		ID: mapID, Type: "gate", Title: "Decision map for " + workID, Description: string(mapDescription),
		Metadata: StringMap{
			beadmeta.DecisionFrontierRecordMetadataKey: decisionFrontierMapKind,
			beadmeta.DecisionFrontierStateMetadataKey:  "pending",
		},
	}
	questionRecord := func(question decisionFrontierLinkQuestion) Bead {
		id := DecisionFrontierQuestionRecordID(cityRef, storeRef, mapID, question.ID)
		doc := decisionFrontierLinkDocument{
			SchemaVersion: 1, CityRef: cityRef, StoreRef: storeRef, WorkID: workID,
			WorkRevision: workRevision, WorkDigest: "work-digest", MapID: mapID, Question: question,
		}
		description, marshalErr := json.Marshal(doc)
		if marshalErr != nil {
			t.Fatalf("marshal question document: %v", marshalErr)
		}
		return Bead{
			ID: id, Type: "gate", Title: question.Title, Description: string(description),
			Metadata: StringMap{
				beadmeta.DecisionFrontierRecordMetadataKey: decisionFrontierQuestionKind,
				beadmeta.DecisionFrontierStateMetadataKey:  "pending",
			},
		}
	}
	questionARecord := questionRecord(questionA)
	questionBRecord := questionRecord(questionB)
	return &remoteDecisionFrontierLinkRecordReaderStub{records: map[string]Bead{
		mapRecord.ID: mapRecord, questionARecord.ID: questionARecord, questionBRecord.ID: questionBRecord,
	}}
}

func TestRemoteDecisionFrontierLinkWriterIssuesExactProtectedLinkAndPreservesRetries(t *testing.T) {
	issuer := &remoteDecisionFrontierLinkPermitIssuerStub{token: "opaque-protected-link-permit"}
	protectedWriter := &remoteDecisionFrontierProtectedLinkWriterStub{}
	reader := remoteDecisionFrontierLinkTestRecords(t, "wrk-1")
	writer := newRemoteDecisionFrontierLinkTestWriterWithReader(t, issuer, protectedWriter, reader)

	questionA := DecisionFrontierQuestionRecordID("city-test", "store-test", DecisionFrontierMapRecordID("city-test", "store-test", "wrk-1", "7"), "question-a")
	questionB := DecisionFrontierQuestionRecordID("city-test", "store-test", DecisionFrontierMapRecordID("city-test", "store-test", "wrk-1", "7"), "question-b")
	mapID := DecisionFrontierMapRecordID("city-test", "store-test", "wrk-1", "7")
	if err := writer.EnsureDecisionFrontierLink(questionA, questionB, "blocks"); err != nil {
		t.Fatalf("EnsureDecisionFrontierLink: %v", err)
	}
	if err := writer.EnsureDecisionFrontierLink(questionA, questionB, "blocks"); err != nil {
		t.Fatalf("exact retry EnsureDecisionFrontierLink: %v", err)
	}
	if len(issuer.requests) != 2 || len(protectedWriter.requests) != 2 {
		t.Fatalf("permit/link calls = %d/%d, want 2/2", len(issuer.requests), len(protectedWriter.requests))
	}
	for index, issued := range issuer.requests {
		if issued.Actor != "controller-test-actor" || issued.Link != (ControllerDependencyLink{SourceID: questionA, TargetID: questionB, Type: "blocks"}) {
			t.Fatalf("permit request %d = %#v, want trusted actor and exact link", index, issued)
		}
		if issued.ProtectedPermit != "" {
			t.Fatalf("permit request %d already contains permit %q", index, issued.ProtectedPermit)
		}
		if !strings.HasPrefix(issued.ReceiptID, controllerDecisionFrontierLinkReceiptPrefix) ||
			!strings.HasPrefix(issuer.replayIDs[index], controllerDecisionFrontierLinkPermitReplayPrefix) {
			t.Fatalf("receipt/replay IDs = %q/%q, want decision-frontier link prefixes", issued.ReceiptID, issuer.replayIDs[index])
		}
		digest, err := controllerBeadsProtectedLinkDigest(issued)
		if err != nil {
			t.Fatalf("controllerBeadsProtectedLinkDigest: %v", err)
		}
		if issued.ReceiptID != controllerDecisionFrontierLinkReceiptPrefix+digest ||
			issuer.replayIDs[index] != controllerDecisionFrontierLinkPermitReplayPrefix+digest {
			t.Fatalf("receipt/replay IDs are not digest-bound: %q/%q for digest %q", issued.ReceiptID, issuer.replayIDs[index], digest)
		}
	}
	if issuer.requests[0].ReceiptID != issuer.requests[1].ReceiptID || issuer.replayIDs[0] != issuer.replayIDs[1] {
		t.Fatalf("exact retry changed receipt/replay IDs: %q/%q then %q/%q", issuer.requests[0].ReceiptID, issuer.replayIDs[0], issuer.requests[1].ReceiptID, issuer.replayIDs[1])
	}
	wantApplied := issuer.requests[0]
	wantApplied.ProtectedPermit = "opaque-protected-link-permit"
	for index, applied := range protectedWriter.requests {
		if !reflect.DeepEqual(applied, wantApplied) {
			t.Fatalf("protected request %d = %#v, want exact permitted request %#v", index, applied, wantApplied)
		}
	}
	for index, deadline := range protectedWriter.deadlines {
		remaining := time.Until(deadline)
		if deadline.IsZero() || remaining <= 0 || remaining > 2*time.Second {
			t.Fatalf("protected request %d deadline = %v (remaining %v), want bounded 2s context", index, deadline, remaining)
		}
	}

	if err := writer.EnsureDecisionFrontierLink(questionA, mapID, "relates-to"); err != nil {
		t.Fatalf("EnsureDecisionFrontierLink with changed source: %v", err)
	}
	if issuer.requests[2].ReceiptID == issuer.requests[0].ReceiptID || issuer.replayIDs[2] == issuer.replayIDs[0] {
		t.Fatalf("changed link reused receipt/replay identity: %q/%q", issuer.requests[2].ReceiptID, issuer.replayIDs[2])
	}
}

func TestRemoteDecisionFrontierLinkWriterEnforcesConfiguredPolicyBeforePermit(t *testing.T) {
	issuer := &remoteDecisionFrontierLinkPermitIssuerStub{token: "opaque-protected-link-permit"}
	protectedWriter := &remoteDecisionFrontierProtectedLinkWriterStub{}
	writer := newRemoteDecisionFrontierLinkTestWriter(t, issuer, protectedWriter)

	err := writer.EnsureDecisionFrontierLink("gc-question-a", "gc-map-a", "relates-to-extra")
	if !errors.Is(err, ErrRemoteDecisionFrontierLinkPolicy) {
		t.Fatalf("EnsureDecisionFrontierLink error = %v, want configured-policy refusal", err)
	}
	if len(issuer.requests) != 0 || len(protectedWriter.requests) != 0 {
		t.Fatalf("policy refusal reached permit/link writer: %d/%d calls", len(issuer.requests), len(protectedWriter.requests))
	}
}

func TestRemoteDecisionFrontierLinkWriterRejectsRelationshipsNotAuthorizedByDocuments(t *testing.T) {
	firstMap := remoteDecisionFrontierLinkTestRecords(t, "wrk-1")
	secondMap := remoteDecisionFrontierLinkTestRecords(t, "wrk-2")
	for id, record := range secondMap.records {
		firstMap.records[id] = record
	}
	firstMapID := DecisionFrontierMapRecordID("city-test", "store-test", "wrk-1", "7")
	secondMapID := DecisionFrontierMapRecordID("city-test", "store-test", "wrk-2", "7")
	questionA := DecisionFrontierQuestionRecordID("city-test", "store-test", firstMapID, "question-a")
	questionB := DecisionFrontierQuestionRecordID("city-test", "store-test", firstMapID, "question-b")
	issuer := &remoteDecisionFrontierLinkPermitIssuerStub{token: "opaque-protected-link-permit"}
	protectedWriter := &remoteDecisionFrontierProtectedLinkWriterStub{}
	writer := newRemoteDecisionFrontierLinkTestWriterWithReader(t, issuer, protectedWriter, firstMap)

	for _, test := range []struct {
		name     string
		sourceID string
		targetID string
		depType  string
	}{
		{name: "cross-map edge", sourceID: questionA, targetID: secondMapID, depType: "relates-to"},
		{name: "undeclared prerequisite", sourceID: questionB, targetID: questionA, depType: "blocks"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := writer.EnsureDecisionFrontierLink(test.sourceID, test.targetID, test.depType)
			if !errors.Is(err, ErrDecisionFrontierLinkConflict) {
				t.Fatalf("EnsureDecisionFrontierLink error = %v, want immutable-document conflict", err)
			}
			if len(issuer.requests) != 0 || len(protectedWriter.requests) != 0 {
				t.Fatalf("unauthorized relationship reached permit/link writer: %d/%d calls", len(issuer.requests), len(protectedWriter.requests))
			}
		})
	}
}

func TestRemoteDecisionFrontierLinkWriterFailsClosedOnPermitErrorOrInvalidPermit(t *testing.T) {
	permitErr := errors.New("issuer unavailable")
	for _, test := range []struct {
		name   string
		issuer *remoteDecisionFrontierLinkPermitIssuerStub
	}{
		{name: "issuer error", issuer: &remoteDecisionFrontierLinkPermitIssuerStub{err: permitErr}},
		{name: "empty permit", issuer: &remoteDecisionFrontierLinkPermitIssuerStub{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			protectedWriter := &remoteDecisionFrontierProtectedLinkWriterStub{}
			writer := newRemoteDecisionFrontierLinkTestWriter(t, test.issuer, protectedWriter)
			mapID := DecisionFrontierMapRecordID("city-test", "store-test", "wrk-1", "7")
			questionA := DecisionFrontierQuestionRecordID("city-test", "store-test", mapID, "question-a")
			err := writer.EnsureDecisionFrontierLink(questionA, mapID, "relates-to")
			if err == nil {
				t.Fatal("EnsureDecisionFrontierLink returned nil, want fail-closed error")
			}
			if test.issuer.err != nil && !errors.Is(err, permitErr) {
				t.Fatalf("EnsureDecisionFrontierLink error = %v, want wrapped issuer error", err)
			}
			if test.issuer.err == nil && !errors.Is(err, ErrControllerBeadsPermitRequest) {
				t.Fatalf("EnsureDecisionFrontierLink error = %v, want invalid permit error", err)
			}
			if len(protectedWriter.requests) != 0 {
				t.Fatalf("invalid permit reached protected writer %d times", len(protectedWriter.requests))
			}
		})
	}
}

func TestNewRemoteDecisionFrontierLinkWriterRequiresExplicitCapabilitiesAndPolicy(t *testing.T) {
	issuer := &remoteDecisionFrontierLinkPermitIssuerStub{token: "opaque-protected-link-permit"}
	protectedWriter := &remoteDecisionFrontierProtectedLinkWriterStub{}
	base := RemoteDecisionFrontierLinkWriterConfig{
		Actor: "controller-test-actor", AllowedDependencyTypes: []string{"blocks"},
		RecordReader: remoteDecisionFrontierLinkTestRecords(t, "wrk-1"),
		PermitIssuer: issuer, ProtectedLinkWriter: protectedWriter,
	}
	for _, mutate := range []func(*RemoteDecisionFrontierLinkWriterConfig){
		func(config *RemoteDecisionFrontierLinkWriterConfig) { config.Actor = " " },
		func(config *RemoteDecisionFrontierLinkWriterConfig) { config.AllowedDependencyTypes = nil },
		func(config *RemoteDecisionFrontierLinkWriterConfig) {
			config.AllowedDependencyTypes = []string{"blocks", "blocks"}
		},
		func(config *RemoteDecisionFrontierLinkWriterConfig) {
			config.AllowedDependencyTypes = []string{"blocks\x00"}
		},
		func(config *RemoteDecisionFrontierLinkWriterConfig) { config.BatchTimeout = -time.Second },
		func(config *RemoteDecisionFrontierLinkWriterConfig) {
			config.BatchTimeout = controllerDecisionFrontierLinkMaxBatchTimeout + time.Nanosecond
		},
		func(config *RemoteDecisionFrontierLinkWriterConfig) { config.RecordReader = nil },
		func(config *RemoteDecisionFrontierLinkWriterConfig) { config.PermitIssuer = nil },
		func(config *RemoteDecisionFrontierLinkWriterConfig) { config.ProtectedLinkWriter = nil },
	} {
		config := base
		mutate(&config)
		if _, err := NewRemoteDecisionFrontierLinkWriter(config); !errors.Is(err, ErrRemoteDecisionFrontierLinkWriterUnavailable) {
			t.Fatalf("NewRemoteDecisionFrontierLinkWriter error = %v, want ErrRemoteDecisionFrontierLinkWriterUnavailable", err)
		}
	}
}
