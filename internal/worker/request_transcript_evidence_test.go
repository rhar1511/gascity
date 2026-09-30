package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

func requestTranscriptFixture() (*HistorySnapshot, sessionpkg.RequestReceipt, string) {
	const (
		path              = "/private/transcripts/session.jsonl"
		providerSessionID = "provider-session"
		sessionID         = "gc-session"
		requestID         = "request-evidence"
		message           = "run the checked command"
		instruction       = "Acknowledge receipt before acting by running the command in acknowledge_with."
	)
	digest := sha256.Sum256([]byte(message))
	receipt := sessionpkg.RequestReceipt{
		RequestID: requestID, SessionID: sessionID, Generation: 4, MessageDigest: hex.EncodeToString(digest[:]),
	}
	envelope, _ := json.Marshal(struct {
		RequestID       string `json:"request_id"`
		SessionID       string `json:"session_id"`
		Generation      int    `json:"generation"`
		Instruction     string `json:"instruction"`
		AcknowledgeWith string `json:"acknowledge_with"`
		Message         string `json:"message"`
	}{requestID, sessionID, receipt.Generation, instruction, `gc session request ack "request-evidence"`, message})
	newEntry := func(id, parent, kind string, actor Actor, text string, blocks []HistoryBlock, root bool) HistoryEntry {
		return HistoryEntry{
			ID: id, Kind: kind, Actor: actor, Text: text, Blocks: blocks,
			Provenance:    Provenance{Provider: "claude", TranscriptPath: path, ProviderSessionID: providerSessionID, RawEntryID: id},
			parentEntryID: parent, parentKnown: root || parent != "",
		}
	}
	snapshot := &HistorySnapshot{
		ProviderSessionID:  providerSessionID,
		TranscriptStreamID: path,
		Generation:         Generation{ID: "1780000000000000000:900"},
		Continuity:         Continuity{Status: ContinuityStatusContinuous},
		Entries: []HistoryEntry{
			newEntry("user-envelope", "", "user", ActorUser, string(envelope), nil, true),
			newEntry("assistant-tool", "user-envelope", "assistant", ActorAssistant, "", []HistoryBlock{{Kind: BlockKindToolUse, ToolUseID: "tool-1"}}, false),
			newEntry("tool-result", "assistant-tool", "user", ActorTool, "", []HistoryBlock{{Kind: BlockKindToolResult, ToolUseID: "tool-1"}}, false),
			newEntry("assistant-done", "tool-result", "assistant", ActorAssistant, "finished", nil, false),
		},
	}
	return snapshot, receipt, path
}

func TestDeriveRequestTranscriptEvidenceIsOpaqueReplayStableAndExact(t *testing.T) {
	snapshot, receipt, sourcePath := requestTranscriptFixture()
	evidence := deriveRequestTranscriptEvidence(snapshot, receipt, "claude", true)
	if evidence.Status != sessionpkg.RequestTranscriptEvidenceAvailable || evidence.ToolStatus != sessionpkg.RequestTranscriptEvidenceAvailable || evidence.UnavailableReason != "" || evidence.ToolUnavailableReason != "" {
		t.Fatalf("evidence status = %+v", evidence)
	}
	if evidence.TranscriptStreamID == sourcePath || evidence.TranscriptGenerationID == snapshot.Generation.ID || len(evidence.TranscriptStreamID) != 64 || len(evidence.TranscriptGenerationID) != 64 {
		t.Fatalf("transcript identity was not opaque: %+v", evidence)
	}
	if len(evidence.References) != 3 {
		t.Fatalf("references = %+v, want envelope, tool use, and tool result", evidence.References)
	}
	for _, reference := range evidence.References {
		if reference.SessionID != receipt.SessionID || reference.RequestID != receipt.RequestID || reference.Generation != receipt.Generation ||
			reference.TranscriptStreamID != evidence.TranscriptStreamID || reference.TranscriptGenerationID != evidence.TranscriptGenerationID {
			t.Fatalf("reference lost exact request/transcript binding: %+v", reference)
		}
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), sourcePath) || strings.Contains(string(encoded), snapshot.Generation.ID) || strings.Contains(string(encoded), "run the checked command") {
		t.Fatalf("evidence stored a path, raw generation, or payload: %s", encoded)
	}
	regenerated := *snapshot
	regenerated.Entries = append([]HistoryEntry(nil), snapshot.Entries...)
	again := deriveRequestTranscriptEvidence(&regenerated, receipt, "claude", true)
	if !reflect.DeepEqual(again, evidence) {
		t.Fatalf("same full transcript did not regenerate stable evidence: first=%+v again=%+v", evidence, again)
	}
}

func TestNormalizeEntryKeepsOnlyRawExplicitParentIdentity(t *testing.T) {
	for _, test := range []struct {
		name       string
		raw        string
		parent     string
		logical    string
		wantParent string
		wantKnown  bool
	}{
		{name: "explicit parent", raw: `{"uuid":"child","parentUuid":"parent","type":"assistant"}`, parent: "parent", wantParent: "parent", wantKnown: true},
		{name: "reader inferred parent", raw: `{"uuid":"child","type":"assistant"}`, parent: "parent"},
		{name: "explicit root", raw: `{"uuid":"root","parentUuid":null,"type":"user"}`, wantKnown: true},
		{name: "ambiguous raw links", raw: `{"uuid":"child","parentUuid":"parent-a","message":{"parentId":"parent-b"}}`, parent: "parent-a"},
		{name: "explicit logical parent", raw: `{"uuid":"child","logicalParentUuid":"parent"}`, logical: "parent", wantParent: "parent", wantKnown: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			entry := &sessionlog.Entry{UUID: "child", ParentUUID: test.parent, LogicalParentUUID: test.logical, Type: "assistant", Raw: json.RawMessage(test.raw)}
			if test.name == "explicit root" {
				entry.UUID = "root"
			}
			normalized := normalizeEntry("claude", "/transcript.jsonl", "provider-session", 0, entry)
			if normalized.parentEntryID != test.wantParent || normalized.parentKnown != test.wantKnown {
				t.Fatalf("normalized parent = %q known=%t, want %q known=%t", normalized.parentEntryID, normalized.parentKnown, test.wantParent, test.wantKnown)
			}
		})
	}
}

func TestDeriveRequestTranscriptEvidenceFailsClosed(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*HistorySnapshot)
		full     bool
		toolOnly bool
		want     sessionpkg.RequestTranscriptEvidenceReason
	}{
		{name: "partial page", full: false, want: sessionpkg.RequestTranscriptIncompleteView},
		{name: "pagination marker", full: true, want: sessionpkg.RequestTranscriptIncompleteView, mutate: func(snapshot *HistorySnapshot) { snapshot.Pagination = &TranscriptPagination{HasOlderMessages: true} }},
		{name: "degraded source", full: true, want: sessionpkg.RequestTranscriptDegraded, mutate: func(snapshot *HistorySnapshot) { snapshot.Diagnostics = []HistoryDiagnostic{{Code: "malformed_tail"}} }},
		{name: "branched source", full: true, want: sessionpkg.RequestTranscriptBranched, mutate: func(snapshot *HistorySnapshot) { snapshot.Continuity.HasBranches = true }},
		{name: "missing envelope", full: true, want: sessionpkg.RequestTranscriptNoExactEnvelope, mutate: func(snapshot *HistorySnapshot) { snapshot.Entries[0].Text = "request_id request-evidence generation 4" }},
		{name: "duplicate envelope", full: true, want: sessionpkg.RequestTranscriptAmbiguousEnvelope, mutate: func(snapshot *HistorySnapshot) { snapshot.Entries = append(snapshot.Entries, snapshot.Entries[0]) }},
		{name: "derived envelope id", full: true, want: sessionpkg.RequestTranscriptUnstableEntryID, mutate: func(snapshot *HistorySnapshot) { snapshot.Entries[0].Provenance.Derived = true }},
		{name: "wrong source stream", full: true, want: sessionpkg.RequestTranscriptStreamMismatch, mutate: func(snapshot *HistorySnapshot) { snapshot.Entries[0].Provenance.TranscriptPath = "/other/source.jsonl" }},
		{name: "missing parent evidence", full: true, want: sessionpkg.RequestTranscriptNoToolLineage, toolOnly: true, mutate: func(snapshot *HistorySnapshot) { snapshot.Entries[1].parentKnown = false }},
		{name: "tool event without id", full: true, want: sessionpkg.RequestTranscriptMissingToolID, toolOnly: true, mutate: func(snapshot *HistorySnapshot) { snapshot.Entries[1].Blocks[0].ToolUseID = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snapshot, receipt, _ := requestTranscriptFixture()
			if tt.mutate != nil {
				tt.mutate(snapshot)
			}
			evidence := deriveRequestTranscriptEvidence(snapshot, receipt, "claude", tt.full)
			if tt.toolOnly {
				if evidence.Status != sessionpkg.RequestTranscriptEvidenceAvailable || evidence.ToolStatus != sessionpkg.RequestTranscriptEvidenceUnavailable || evidence.ToolUnavailableReason != tt.want || len(evidence.References) != 1 {
					t.Fatalf("tool evidence = %+v, want only the exact envelope reference and unavailable tools %q", evidence, tt.want)
				}
				return
			}
			if evidence.Status != sessionpkg.RequestTranscriptEvidenceUnavailable || evidence.UnavailableReason != tt.want || len(evidence.References) != 0 || evidence.ToolStatus != sessionpkg.RequestTranscriptEvidenceUnavailable {
				t.Fatalf("evidence = %+v, want unavailable %q with no references", evidence, tt.want)
			}
		})
	}

	t.Run("tool lineage does not use entry order", func(t *testing.T) {
		snapshot, receipt, _ := requestTranscriptFixture()
		snapshot.Entries[1].parentEntryID = ""
		snapshot.Entries[1].parentKnown = true
		evidence := deriveRequestTranscriptEvidence(snapshot, receipt, "claude", true)
		if evidence.Status != sessionpkg.RequestTranscriptEvidenceAvailable || evidence.ToolStatus != sessionpkg.RequestTranscriptEvidenceUnavailable || evidence.ToolUnavailableReason != sessionpkg.RequestTranscriptNoToolLineage || len(evidence.References) != 1 {
			t.Fatalf("order-only tool association was accepted: %+v", evidence)
		}
	})
}

func TestDeriveRequestTranscriptEvidenceStopsAtLaterUserTurn(t *testing.T) {
	snapshot, receipt, _ := requestTranscriptFixture()
	newEntry := func(id, parent, kind string, actor Actor, blocks []HistoryBlock) HistoryEntry {
		return HistoryEntry{
			ID: id, Kind: kind, Actor: actor, Blocks: blocks,
			Provenance:    Provenance{Provider: "claude", TranscriptPath: snapshot.TranscriptStreamID, ProviderSessionID: snapshot.ProviderSessionID, RawEntryID: id},
			parentEntryID: parent, parentKnown: true,
		}
	}
	snapshot.Entries = append(snapshot.Entries,
		newEntry("later-user", "assistant-done", "user", ActorUser, nil),
		newEntry("later-tool", "later-user", "assistant", ActorAssistant, []HistoryBlock{{Kind: BlockKindToolUse, ToolUseID: "tool-later"}}),
		newEntry("later-result", "later-tool", "user", ActorTool, []HistoryBlock{{Kind: BlockKindToolResult, ToolUseID: "tool-later"}}),
	)

	evidence := deriveRequestTranscriptEvidence(snapshot, receipt, "claude", true)
	if evidence.Status != sessionpkg.RequestTranscriptEvidenceAvailable || evidence.ToolStatus != sessionpkg.RequestTranscriptEvidenceAvailable {
		t.Fatalf("evidence status = %+v", evidence)
	}
	if len(evidence.References) != 3 {
		t.Fatalf("references = %+v, want only tracked envelope and its tool pair", evidence.References)
	}
	for _, reference := range evidence.References {
		if reference.EntryID == "later-tool" || reference.EntryID == "later-result" || reference.ToolID == "tool-later" {
			t.Fatalf("later turn was attributed to earlier request: %+v", evidence.References)
		}
	}
}

func TestSessionHistoryPersistsExactRequestTranscriptReferencesAcrossHandleRecreation(t *testing.T) {
	workDir := t.TempDir()
	searchRoot := t.TempDir()
	handle, backing, sp, manager := newTestSessionHandle(t, SessionSpec{
		Profile: ProfileClaudeTmuxCLI, Template: "probe", Title: "Probe", Command: "claude", WorkDir: workDir, Provider: "claude",
	})
	handle.adapter.SearchPaths = []string{searchRoot}
	if err := handle.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, err := manager.Get(handle.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.PersistSessionKey(info.ID, "provider-session"); err != nil {
		t.Fatal(err)
	}
	generation, err := strconv.Atoi(info.Generation)
	if err != nil {
		t.Fatalf("parse current generation %q: %v", info.Generation, err)
	}
	if _, err := handle.Message(context.Background(), MessageRequest{Text: "run the checked command", RequestID: "request-integration", Generation: generation}); err != nil {
		t.Fatal(err)
	}
	var envelope string
	for _, call := range sp.SnapshotCalls() {
		if call.Method == "Nudge" || call.Method == "NudgeNow" {
			envelope = call.Message
		}
	}
	if envelope == "" {
		t.Fatal("tracked message did not deliver a canonical envelope")
	}
	transcriptPath := writeClaudeTrackedTranscript(t, searchRoot, workDir, "provider-session", envelope)

	first, err := handle.History(context.Background(), HistoryRequest{})
	if err != nil || first.RequestLedger == nil || first.RequestLedger.Status != sessionpkg.RequestLedgerAvailable {
		t.Fatalf("first History = %+v, %v", first, err)
	}
	evidence := first.RequestLedger.Requests[0].Ledger.TranscriptEvidence
	if evidence == nil || evidence.Status != sessionpkg.RequestTranscriptEvidenceAvailable || evidence.ToolStatus != sessionpkg.RequestTranscriptEvidenceAvailable || len(evidence.References) != 3 {
		t.Fatalf("persisted evidence = %+v", evidence)
	}
	bead, err := backing.Get(handle.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	storedReceipt := ""
	for key, value := range bead.Metadata {
		if strings.HasPrefix(key, beadmeta.SessionRequestReceiptPrefix) && strings.Contains(key, "request-integration") {
			storedReceipt = value
		}
	}
	if storedReceipt == "" || strings.Contains(storedReceipt, transcriptPath) || strings.Contains(storedReceipt, first.Generation.ID) || strings.Contains(storedReceipt, envelope) {
		t.Fatalf("session bead stored a transcript path, raw generation, or envelope payload: %s", storedReceipt)
	}

	restarted, err := NewSessionHandle(SessionHandleConfig{Manager: manager, Session: SessionSpec{ID: info.ID, Profile: ProfileClaudeTmuxCLI, Template: "probe", Command: "claude", WorkDir: workDir, Provider: "claude"}})
	if err != nil {
		t.Fatal(err)
	}
	restarted.adapter.SearchPaths = []string{searchRoot}
	beforeRevision := bead.Revision
	regenerated, err := restarted.History(context.Background(), HistoryRequest{})
	if err != nil || regenerated.RequestLedger == nil || len(regenerated.RequestLedger.Requests) != 1 {
		t.Fatalf("recreated handle History = %+v, %v", regenerated, err)
	}
	regeneratedEvidence := regenerated.RequestLedger.Requests[0].Ledger.TranscriptEvidence
	if !reflect.DeepEqual(regeneratedEvidence, evidence) {
		t.Fatalf("recreated handle changed transcript references: before=%+v after=%+v", evidence, regeneratedEvidence)
	}
	afterBead, err := backing.Get(handle.sessionID)
	if err != nil || beforeRevision != afterBead.Revision {
		t.Fatalf("regenerating exact evidence changed the session row: revisions=%d/%d err=%v", beforeRevision, afterBead.Revision, err)
	}
	if _, err := manager.PersistedStore().AcceptRequest(handle.sessionID, "request-never-sent", generation, "accepted but not delivered", time.Now().UTC()); err != nil {
		t.Fatalf("accept pending request: %v", err)
	}
	if err := backing.Update(handle.sessionID, beads.UpdateOpts{Metadata: map[string]string{"generation": "2", "instance_token": "next-execution"}}); err != nil {
		t.Fatal(err)
	}
	beforeHistoricalRead, _ := backing.Get(handle.sessionID)
	historical, err := restarted.History(context.Background(), HistoryRequest{})
	if err != nil {
		t.Fatal(err)
	}
	foundHistorical := false
	for _, diagnostic := range historical.Diagnostics {
		if diagnostic.Code == "request_transcript_historical_generation" && diagnostic.Count == 2 {
			foundHistorical = true
		}
	}
	if !foundHistorical {
		t.Fatalf("stale request generation was not surfaced as historical: %+v", historical.Diagnostics)
	}
	afterHistoricalRead, _ := backing.Get(handle.sessionID)
	if beforeHistoricalRead.Revision != afterHistoricalRead.Revision {
		t.Fatal("current transcript evidence was written for a stale receipt generation")
	}
}

type transcriptEvidenceWriteFailureStore struct {
	*beads.MemStore
	failTranscriptEvidence bool
}

func (s *transcriptEvidenceWriteFailureStore) UpdateIfMatch(id string, revision int64, opts beads.UpdateOpts) error {
	if s.failTranscriptEvidence {
		for _, value := range opts.Metadata {
			if strings.Contains(value, `"kind":"transcript_evidence"`) {
				return errors.New("injected transcript evidence write failure")
			}
		}
	}
	return s.MemStore.UpdateIfMatch(id, revision, opts)
}

func TestSessionHistoryReportsTranscriptEvidenceWriteFailure(t *testing.T) {
	backing := beads.NewMemStore()
	store := &transcriptEvidenceWriteFailureStore{MemStore: backing}
	sp := runtime.NewFake()
	manager := sessionpkg.NewManagerWithOptions(store, sp)
	workDir, searchRoot := t.TempDir(), t.TempDir()
	handle, err := NewSessionHandle(SessionHandleConfig{
		Manager: manager,
		Session: SessionSpec{Profile: ProfileClaudeTmuxCLI, Template: "probe", Title: "Probe", Command: "claude", WorkDir: workDir, Provider: "claude"},
	})
	if err != nil {
		t.Fatal(err)
	}
	handle.adapter.SearchPaths = []string{searchRoot}
	if err := handle.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, err := manager.Get(handle.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.PersistSessionKey(info.ID, "provider-session"); err != nil {
		t.Fatal(err)
	}
	generation, err := strconv.Atoi(info.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Message(context.Background(), MessageRequest{Text: "report progress", RequestID: "request-write-failure", Generation: generation}); err != nil {
		t.Fatal(err)
	}
	var envelope string
	for _, call := range sp.SnapshotCalls() {
		if call.Method == "Nudge" || call.Method == "NudgeNow" {
			envelope = call.Message
		}
	}
	if envelope == "" {
		t.Fatal("tracked delivery did not provide an envelope")
	}
	writeClaudeTrackedTranscript(t, searchRoot, workDir, "provider-session", envelope)
	store.failTranscriptEvidence = true
	before, _ := backing.Get(handle.sessionID)
	history, err := handle.History(context.Background(), HistoryRequest{})
	if err != nil || history.RequestLedger == nil || history.RequestLedger.Status != sessionpkg.RequestLedgerAvailable {
		t.Fatalf("History after write failure = %+v, %v", history, err)
	}
	foundFailure := false
	for _, diagnostic := range history.Diagnostics {
		if diagnostic.Code == "request_transcript_evidence_write_failed" && diagnostic.Count == 1 {
			foundFailure = true
		}
	}
	if !foundFailure {
		t.Fatalf("evidence write failure was hidden: %+v", history.Diagnostics)
	}
	evidence := history.RequestLedger.Requests[0].Ledger.TranscriptEvidence
	if evidence == nil || evidence.Status != sessionpkg.RequestTranscriptEvidenceUnavailable || evidence.UnavailableReason != sessionpkg.RequestTranscriptNotObserved || len(evidence.References) != 0 {
		t.Fatalf("failed write was presented as durable evidence: %+v", evidence)
	}
	after, _ := backing.Get(handle.sessionID)
	if before.Revision != after.Revision {
		t.Fatal("failed transcript evidence write changed the session row")
	}
}

func writeClaudeTrackedTranscript(t *testing.T, searchRoot, workDir, providerSessionID, envelope string) string {
	t.Helper()
	transcriptPath := filepath.Join(searchRoot, sessionlog.ProjectSlug(workDir), providerSessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o755); err != nil {
		t.Fatal(err)
	}
	writeWorkerTestJSONL(t, transcriptPath, []map[string]any{
		{"uuid": "entry-envelope", "parentUuid": nil, "type": "user", "sessionId": providerSessionID, "message": map[string]any{"role": "user", "content": envelope}},
		{"uuid": "entry-assistant", "parentUuid": "entry-envelope", "type": "assistant", "sessionId": providerSessionID, "message": map[string]any{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "tool-1", "name": "Bash", "input": map[string]string{"command": "true"}}}}},
		{"uuid": "entry-result", "parentUuid": "entry-assistant", "type": "user", "sessionId": providerSessionID, "message": map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "tool-1", "content": "ok"}}}},
	})
	return transcriptPath
}

func TestFullHistoryRequestRecognizesOnlyUnpagedViews(t *testing.T) {
	for _, req := range []HistoryRequest{
		{},
		{LogicalID: "caller-label"},
	} {
		if !fullHistoryRequest(req) {
			t.Fatalf("full history request rejected: %+v", req)
		}
	}
	for _, req := range []HistoryRequest{
		{TailCompactions: 1},
		{BeforeEntryID: "entry-1"},
		{AfterEntryID: "entry-1"},
	} {
		if fullHistoryRequest(req) {
			t.Fatalf("partial history request accepted as full: %+v", req)
		}
	}
}
