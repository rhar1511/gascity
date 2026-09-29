package beads

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

type remoteDecisionFrontierTestPermitIssuer struct {
	requests []ControllerProtectedCreateAndLinkRequest
	replayID []string
	token    string
	err      error
}

func (issuer *remoteDecisionFrontierTestPermitIssuer) IssueProtectedCreateAndLink(request ControllerProtectedCreateAndLinkRequest, replayID string) (string, error) {
	issuer.requests = append(issuer.requests, request)
	issuer.replayID = append(issuer.replayID, replayID)
	return issuer.token, issuer.err
}

type remoteDecisionFrontierTestBatchWriter struct {
	requests []ControllerProtectedCreateAndLinkRequest
	result   ControllerProtectedCreateAndLinkResult
	err      error
	deadline bool
}

func (writer *remoteDecisionFrontierTestBatchWriter) ApplyProtectedCreateAndLink(ctx context.Context, request ControllerProtectedCreateAndLinkRequest) (ControllerProtectedCreateAndLinkResult, error) {
	if ctx == nil {
		return ControllerProtectedCreateAndLinkResult{}, errors.New("nil context")
	}
	_, writer.deadline = ctx.Deadline()
	writer.requests = append(writer.requests, request)
	return writer.result, writer.err
}

type remoteDecisionFrontierTestLinkWriter struct {
	sourceID string
	targetID string
	depType  string
	calls    int
	err      error
}

func (writer *remoteDecisionFrontierTestLinkWriter) EnsureDecisionFrontierLink(sourceID, targetID, depType string) error {
	writer.sourceID = sourceID
	writer.targetID = targetID
	writer.depType = depType
	writer.calls++
	return writer.err
}

func newRemoteDecisionFrontierTestWriter(t *testing.T, issuer *remoteDecisionFrontierTestPermitIssuer, batch *remoteDecisionFrontierTestBatchWriter, link *remoteDecisionFrontierTestLinkWriter) *RemoteDecisionFrontierRecordWriter {
	t.Helper()
	const actor = "controller-test-actor"
	const protectionClass = "frontier-records"
	writer, err := NewRemoteDecisionFrontierRecordWriter(RemoteDecisionFrontierRecordWriterConfig{
		Actor:           actor,
		ProtectionClass: protectionClass,
		RequestTimeout:  3 * time.Second,
		PermitIssuer:    issuer,
		BatchWriter:     batch,
		LinkWriter:      link,
	})
	if err != nil {
		t.Fatalf("NewRemoteDecisionFrontierRecordWriter: %v", err)
	}
	return writer
}

func remoteDecisionFrontierTestMapRecord() Bead {
	return Bead{
		ID:          "gc-map-1",
		Type:        "gate",
		Title:       "Decision map for wrk-1",
		Description: `{"schema_version":1,"city_ref":"city-a","store_ref":"store-a","work_id":"wrk-1","work_revision":"7","map_id":"gc-map-1"}`,
		Metadata: StringMap{
			beadmeta.DecisionFrontierRecordMetadataKey: "decision-frontier/map/v1",
			beadmeta.DecisionFrontierStateMetadataKey:  "pending",
		},
	}
}

func TestRemoteDecisionFrontierRecordWriterCreatesProtectedExactRecord(t *testing.T) {
	issuer := &remoteDecisionFrontierTestPermitIssuer{token: "opaque-permit"}
	batch := &remoteDecisionFrontierTestBatchWriter{}
	link := &remoteDecisionFrontierTestLinkWriter{}
	writer := newRemoteDecisionFrontierTestWriter(t, issuer, batch, link)
	record := remoteDecisionFrontierTestMapRecord()
	record.Labels = []string{"decision-frontier"}

	created, err := writer.CreateDecisionFrontierRecord(record)
	if err != nil {
		t.Fatalf("CreateDecisionFrontierRecord: %v", err)
	}
	if created.ID != record.ID || created.Type != record.Type || created.Title != record.Title || created.Description != record.Description ||
		!reflect.DeepEqual(created.Metadata, record.Metadata) || !reflect.DeepEqual(created.Labels, record.Labels) {
		t.Fatalf("created projection = %#v, want input record %#v", created, record)
	}
	if len(issuer.requests) != 1 || len(batch.requests) != 1 {
		t.Fatalf("issuer/batch calls = %d/%d, want 1/1", len(issuer.requests), len(batch.requests))
	}
	issued := issuer.requests[0]
	if issued.ProtectedPermit != "" || issued.Record.ProtectionClass != "frontier-records" || len(issued.Links) != 0 {
		t.Fatalf("permit input = %#v, want create-only protected request without an existing permit", issued)
	}
	if issued.Actor != "controller-test-actor" || issued.Record.ID != record.ID || issued.Record.Type != record.Type ||
		issued.Record.Title != record.Title || issued.Record.Description != record.Description ||
		!reflect.DeepEqual(issued.Record.Labels, record.Labels) || !reflect.DeepEqual(issued.Record.Metadata, map[string]string(record.Metadata)) {
		t.Fatalf("permit record = %#v, want exact immutable record payload", issued.Record)
	}
	if issued.ReceiptID == "" || issuer.replayID[0] == "" {
		t.Fatalf("receipt/replay IDs = %q/%q, want both present", issued.ReceiptID, issuer.replayID[0])
	}
	if batch.requests[0].ProtectedPermit != "opaque-permit" || batch.requests[0].ReceiptID != issued.ReceiptID || !reflect.DeepEqual(batch.requests[0].Record, issued.Record) {
		t.Fatalf("batch request = %#v, want issued request with the opaque permit", batch.requests[0])
	}
	if !batch.deadline {
		t.Fatal("batch writer context has no deadline")
	}
	if link.calls != 0 {
		t.Fatalf("link writer calls = %d, want none for create-only operation", link.calls)
	}

	if _, err := writer.CreateDecisionFrontierRecord(record); err != nil {
		t.Fatalf("repeat exact CreateDecisionFrontierRecord: %v", err)
	}
	if issuer.replayID[1] != issuer.replayID[0] || issuer.requests[1].ReceiptID != issuer.requests[0].ReceiptID {
		t.Fatalf("exact retry changed replay/receipt identity: %q/%q then %q/%q", issuer.replayID[0], issuer.requests[0].ReceiptID, issuer.replayID[1], issuer.requests[1].ReceiptID)
	}
	changed := record
	changed.Title = "Decision map with changed request content"
	if _, err := writer.CreateDecisionFrontierRecord(changed); err != nil {
		t.Fatalf("CreateDecisionFrontierRecord with changed payload: %v", err)
	}
	if issuer.replayID[2] == issuer.replayID[0] || issuer.requests[2].ReceiptID == issuer.requests[0].ReceiptID {
		t.Fatalf("changed payload reused replay/receipt identity: %q/%q", issuer.replayID[2], issuer.requests[2].ReceiptID)
	}
}

func TestRemoteDecisionFrontierRecordWriterRejectsUnrepresentableRecordBeforeIssuingPermit(t *testing.T) {
	issuer := &remoteDecisionFrontierTestPermitIssuer{token: "opaque-permit"}
	batch := &remoteDecisionFrontierTestBatchWriter{}
	writer := newRemoteDecisionFrontierTestWriter(t, issuer, batch, &remoteDecisionFrontierTestLinkWriter{})
	record := remoteDecisionFrontierTestMapRecord()
	record.Status = "closed"

	if _, err := writer.CreateDecisionFrontierRecord(record); !errors.Is(err, ErrRemoteDecisionFrontierRecordShape) {
		t.Fatalf("CreateDecisionFrontierRecord error = %v, want ErrRemoteDecisionFrontierRecordShape", err)
	}
	if len(issuer.requests) != 0 || len(batch.requests) != 0 {
		t.Fatalf("invalid record reached permit issuer or batch writer: %d/%d", len(issuer.requests), len(batch.requests))
	}
}

func TestRemoteDecisionFrontierRecordWriterCASUnavailableAndNotAdvertised(t *testing.T) {
	writer := newRemoteDecisionFrontierTestWriter(t,
		&remoteDecisionFrontierTestPermitIssuer{token: "opaque-permit"},
		&remoteDecisionFrontierTestBatchWriter{},
		&remoteDecisionFrontierTestLinkWriter{},
	)
	if applied, err := writer.CompareAndSetDecisionFrontierRecordMetadataKey("gc-question-1", beadmeta.DecisionFrontierStateMetadataKey, "pending", "answering:gc-answer-1"); applied || !errors.Is(err, ErrConditionalWriteUnsupported) || !errors.Is(err, ErrRemoteDecisionFrontierCASUnsupported) {
		t.Fatalf("record CAS = (%t, %v), want fail-closed unsupported", applied, err)
	}
	if handle, ok := writer.DecisionFrontierRecordWriterHandle(); ok || handle != nil {
		t.Fatalf("record capability handle = (%T, %t), want absent until remote CAS is implemented", handle, ok)
	}
}

func TestRemoteDecisionFrontierRecordWriterUsesInjectedNarrowLinkWriter(t *testing.T) {
	linkErr := errors.New("remote link writer refused")
	link := &remoteDecisionFrontierTestLinkWriter{err: linkErr}
	batch := &remoteDecisionFrontierTestBatchWriter{}
	writer := newRemoteDecisionFrontierTestWriter(t, &remoteDecisionFrontierTestPermitIssuer{token: "opaque-permit"}, batch, link)

	err := writer.EnsureDecisionFrontierLink("gc-question-1", "gc-map-1", "relates-to")
	if !errors.Is(err, linkErr) {
		t.Fatalf("EnsureDecisionFrontierLink error = %v, want injected link writer error", err)
	}
	if link.calls != 1 || link.sourceID != "gc-question-1" || link.targetID != "gc-map-1" || link.depType != "relates-to" {
		t.Fatalf("link writer call = (%q, %q, %q) x%d, want exact delegated request", link.sourceID, link.targetID, link.depType, link.calls)
	}
	if len(batch.requests) != 0 {
		t.Fatalf("protected batch writer calls = %d, want none for link operation", len(batch.requests))
	}
}

func TestNewRemoteDecisionFrontierRecordWriterRequiresAllInjectedCapabilities(t *testing.T) {
	_, err := NewRemoteDecisionFrontierRecordWriter(RemoteDecisionFrontierRecordWriterConfig{
		Actor:           "controller-test-actor",
		ProtectionClass: "frontier-records",
	})
	if !errors.Is(err, ErrRemoteDecisionFrontierWriterUnavailable) {
		t.Fatalf("constructor error = %v, want ErrRemoteDecisionFrontierWriterUnavailable", err)
	}
}
