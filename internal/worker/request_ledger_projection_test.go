package worker

import (
	"testing"

	"github.com/gastownhall/gascity/internal/session"
)

func TestHistoryCacheRefreshesRequestLedgerWithoutTranscriptChange(t *testing.T) {
	handle := &SessionHandle{}
	first := &HistorySnapshot{
		GCSessionID:           "gc-session",
		LogicalConversationID: "conversation",
		TranscriptStreamID:    "same-transcript-stream",
		Generation:            Generation{ID: "same-file-generation"},
		RequestLedger: &session.RequestLedgerProjection{
			SessionID: "gc-session", Status: session.RequestLedgerAvailable, Digest: "ledger-before",
		},
	}
	if got := handle.mergeLoadedHistorySnapshot(first); got == nil || got.RequestLedger == nil || got.RequestLedger.Digest != "ledger-before" {
		t.Fatalf("initial history projection = %+v", got)
	}
	updated := *first
	updated.RequestLedger = &session.RequestLedgerProjection{
		SessionID: "gc-session", Status: session.RequestLedgerAvailable, Digest: "ledger-after",
		Requests: []session.RequestReceipt{{RequestID: "request-1", Ledger: &session.RequestLedger{
			Events: []session.RequestEvent{{TranscriptEvidence: &session.RequestTranscriptEvidence{
				Status:     session.RequestTranscriptEvidenceAvailable,
				ToolStatus: session.RequestTranscriptEvidenceAvailable,
				References: []session.RequestTranscriptReference{{EntryID: "entry-original"}},
			}}},
			TranscriptEvidence: &session.RequestTranscriptEvidence{
				Status:     session.RequestTranscriptEvidenceAvailable,
				ToolStatus: session.RequestTranscriptEvidenceAvailable,
				References: []session.RequestTranscriptReference{{EntryID: "entry-original"}},
			},
		}}},
	}
	got := handle.mergeLoadedHistorySnapshot(&updated)
	if got == nil || got.RequestLedger == nil || got.RequestLedger.Digest != "ledger-after" {
		t.Fatalf("cached transcript masked the current ledger: %+v", got)
	}

	// The cache and caller receive separate ledger slices, so a consumer cannot
	// alter the next regenerated projection in memory.
	got = handle.mergeLoadedHistorySnapshot(&updated)
	got.RequestLedger.Requests[0].RequestID = "changed-by-caller"
	got.RequestLedger.Requests[0].Ledger.TranscriptEvidence.References[0].EntryID = "changed-evidence"
	got.RequestLedger.Requests[0].Ledger.Events[0].TranscriptEvidence.References[0].EntryID = "changed-event-evidence"
	got = handle.mergeLoadedHistorySnapshot(&updated)
	if got.RequestLedger.Requests[0].RequestID != "request-1" {
		t.Fatalf("cached request ledger aliased caller data: %+v", got.RequestLedger.Requests)
	}
	if got.RequestLedger.Requests[0].Ledger.TranscriptEvidence.References[0].EntryID != "entry-original" || got.RequestLedger.Requests[0].Ledger.Events[0].TranscriptEvidence.References[0].EntryID != "entry-original" {
		t.Fatalf("cached transcript evidence aliased caller data: %+v", got.RequestLedger.Requests[0].Ledger)
	}
}
