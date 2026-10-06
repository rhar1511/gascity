package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"sort"
	"strings"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// deriveRequestTranscriptEvidence binds a request only to IDs from one full,
// healthy provider snapshot. The persisted identities are opaque hashes of the
// source stream and generation, never transcript paths or provider payloads.
func deriveRequestTranscriptEvidence(snapshot *HistorySnapshot, receipt sessionpkg.RequestReceipt, provider string, fullView bool) sessionpkg.RequestTranscriptEvidence {
	if !fullView {
		return unavailableTranscriptEvidence(sessionpkg.RequestTranscriptIncompleteView, "", "")
	}
	if snapshot == nil || strings.TrimSpace(snapshot.TranscriptStreamID) == "" {
		return unavailableTranscriptEvidence(sessionpkg.RequestTranscriptUnavailable, "", "")
	}
	if strings.TrimSpace(snapshot.Generation.ID) == "" {
		return unavailableTranscriptEvidence(sessionpkg.RequestTranscriptGenerationUnavailable, "", "")
	}
	if strings.TrimSpace(snapshot.ProviderSessionID) == "" || strings.TrimSpace(provider) == "" || receipt.SessionID == "" {
		return unavailableTranscriptEvidence(sessionpkg.RequestTranscriptUnavailable, "", "")
	}
	streamID := requestTranscriptStreamIdentity(receipt.SessionID, provider, snapshot.ProviderSessionID, snapshot.TranscriptStreamID)
	generationID := requestTranscriptGenerationIdentity(streamID, snapshot.Generation.ID)
	if snapshot.Pagination != nil {
		return unavailableTranscriptEvidence(sessionpkg.RequestTranscriptIncompleteView, streamID, generationID)
	}
	if snapshot.Continuity.Status == ContinuityStatusDegraded || snapshot.TailState.Degraded || len(snapshot.Diagnostics) > 0 {
		return unavailableTranscriptEvidence(sessionpkg.RequestTranscriptDegraded, streamID, generationID)
	}
	if snapshot.Continuity.HasBranches {
		return unavailableTranscriptEvidence(sessionpkg.RequestTranscriptBranched, streamID, generationID)
	}

	var exact []*HistoryEntry
	for index := range snapshot.Entries {
		entry := &snapshot.Entries[index]
		if entry.Kind != "user" || entry.Actor != ActorUser || !sessionpkg.RequestEnvelopeMatchesReceipt(entry.Text, receipt) {
			continue
		}
		exact = append(exact, entry)
	}
	if len(exact) == 0 {
		return unavailableTranscriptEvidence(sessionpkg.RequestTranscriptNoExactEnvelope, streamID, generationID)
	}
	if len(exact) != 1 {
		return unavailableTranscriptEvidence(sessionpkg.RequestTranscriptAmbiguousEnvelope, streamID, generationID)
	}
	envelope := exact[0]
	if filepath.Clean(envelope.Provenance.TranscriptPath) != filepath.Clean(snapshot.TranscriptStreamID) ||
		envelope.Provenance.ProviderSessionID != snapshot.ProviderSessionID {
		return unavailableTranscriptEvidence(sessionpkg.RequestTranscriptStreamMismatch, streamID, generationID)
	}
	if !stableHistoryEntryID(envelope) {
		return unavailableTranscriptEvidence(sessionpkg.RequestTranscriptUnstableEntryID, streamID, generationID)
	}
	if historyEntryIDCount(snapshot.Entries, envelope.ID) != 1 {
		return unavailableTranscriptEvidence(sessionpkg.RequestTranscriptAmbiguousEnvelope, streamID, generationID)
	}

	evidence := sessionpkg.RequestTranscriptEvidence{
		Status:                 sessionpkg.RequestTranscriptEvidenceAvailable,
		ToolStatus:             sessionpkg.RequestTranscriptEvidenceUnavailable,
		ToolUnavailableReason:  sessionpkg.RequestTranscriptNoToolLineage,
		TranscriptStreamID:     streamID,
		TranscriptGenerationID: generationID,
		References: []sessionpkg.RequestTranscriptReference{{
			SessionID: receipt.SessionID, Generation: receipt.Generation, RequestID: receipt.RequestID,
			TranscriptStreamID: streamID, TranscriptGenerationID: generationID,
			EntryID: envelope.ID, Kind: sessionpkg.RequestTranscriptEntryReference,
		}},
	}
	toolReferences, reason := requestDescendantToolReferences(snapshot.Entries, envelope.ID)
	if reason != "" {
		evidence.ToolUnavailableReason = reason
		return evidence
	}
	evidence.ToolStatus = sessionpkg.RequestTranscriptEvidenceAvailable
	evidence.ToolUnavailableReason = ""
	for _, reference := range toolReferences {
		reference.SessionID = receipt.SessionID
		reference.Generation = receipt.Generation
		reference.RequestID = receipt.RequestID
		reference.TranscriptStreamID = streamID
		reference.TranscriptGenerationID = generationID
		evidence.References = append(evidence.References, reference)
	}
	return evidence
}

func unavailableTranscriptEvidence(reason sessionpkg.RequestTranscriptEvidenceReason, streamID, generationID string) sessionpkg.RequestTranscriptEvidence {
	return sessionpkg.RequestTranscriptEvidence{
		Status:                 sessionpkg.RequestTranscriptEvidenceUnavailable,
		UnavailableReason:      reason,
		ToolStatus:             sessionpkg.RequestTranscriptEvidenceUnavailable,
		ToolUnavailableReason:  reason,
		TranscriptStreamID:     streamID,
		TranscriptGenerationID: generationID,
	}
}

func requestTranscriptStreamIdentity(sessionID, provider, providerSessionID, stream string) string {
	return requestTranscriptHash("gc.request.transcript.stream.v1", sessionID, provider, providerSessionID, filepath.Clean(stream))
}

func requestTranscriptGenerationIdentity(streamID, generation string) string {
	return requestTranscriptHash("gc.request.transcript.generation.v1", streamID, generation)
}

func requestTranscriptHash(domain string, fields ...string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(domain))
	for _, field := range fields {
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(field))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func stableHistoryEntryID(entry *HistoryEntry) bool {
	return entry != nil && !entry.Provenance.Derived && entry.ID != "" && strings.TrimSpace(entry.ID) == entry.ID &&
		entry.ID == entry.Provenance.RawEntryID
}

func historyEntryIDCount(entries []HistoryEntry, id string) int {
	count := 0
	for _, entry := range entries {
		if entry.ID == id {
			count++
		}
	}
	return count
}

func requestDescendantToolReferences(entries []HistoryEntry, requestEntryID string) ([]sessionpkg.RequestTranscriptReference, sessionpkg.RequestTranscriptEvidenceReason) {
	byID := make(map[string]*HistoryEntry, len(entries))
	positions := make(map[string]int, len(entries))
	children := make(map[string][]string, len(entries))
	for index := range entries {
		entry := &entries[index]
		if !stableHistoryEntryID(entry) {
			return nil, sessionpkg.RequestTranscriptUnstableEntryID
		}
		if !entry.parentKnown {
			return nil, sessionpkg.RequestTranscriptNoToolLineage
		}
		if _, exists := byID[entry.ID]; exists {
			return nil, sessionpkg.RequestTranscriptAmbiguousToolLineage
		}
		if filepath.Clean(entry.Provenance.TranscriptPath) != filepath.Clean(entries[0].Provenance.TranscriptPath) ||
			entry.Provenance.ProviderSessionID != entries[0].Provenance.ProviderSessionID {
			return nil, sessionpkg.RequestTranscriptStreamMismatch
		}
		byID[entry.ID] = entry
		positions[entry.ID] = index
	}
	for _, entry := range byID {
		if entry.parentEntryID == "" {
			continue
		}
		if byID[entry.parentEntryID] == nil {
			return nil, sessionpkg.RequestTranscriptNoToolLineage
		}
		children[entry.parentEntryID] = append(children[entry.parentEntryID], entry.ID)
	}
	for parentID := range children {
		sort.Strings(children[parentID])
	}
	if byID[requestEntryID] == nil {
		return nil, sessionpkg.RequestTranscriptAmbiguousToolLineage
	}
	for _, entry := range byID {
		seen := make(map[string]bool)
		current := entry
		for current != nil {
			if seen[current.ID] {
				return nil, sessionpkg.RequestTranscriptAmbiguousToolLineage
			}
			seen[current.ID] = true
			if current.parentEntryID == "" {
				break
			}
			current = byID[current.parentEntryID]
		}
	}

	descendants := map[string]bool{requestEntryID: true}
	laterTurnRoots := make(map[string]bool)
	queue := []string{requestEntryID}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, childID := range children[parent] {
			if isLaterUserTurn(byID[childID]) {
				laterTurnRoots[childID] = true
				continue
			}
			if descendants[childID] {
				return nil, sessionpkg.RequestTranscriptAmbiguousToolLineage
			}
			descendants[childID] = true
			queue = append(queue, childID)
		}
	}
	// A later user turn ends the tracked turn. Its explicitly linked subtree is
	// separate evidence and must never be attributed to this request.
	laterTurnEntries := make(map[string]bool)
	laterQueue := make([]string, 0, len(laterTurnRoots))
	for id := range laterTurnRoots {
		laterQueue = append(laterQueue, id)
	}
	for len(laterQueue) > 0 {
		id := laterQueue[0]
		laterQueue = laterQueue[1:]
		if laterTurnEntries[id] {
			return nil, sessionpkg.RequestTranscriptAmbiguousToolLineage
		}
		laterTurnEntries[id] = true
		laterQueue = append(laterQueue, children[id]...)
	}
	requestPosition := positions[requestEntryID]
	// A tool event after the envelope but outside both the proven request turn
	// and a later explicitly linked user turn may still belong to this request.
	// Without a parent chain we cannot safely claim otherwise.
	for id, entry := range byID {
		if descendants[id] || laterTurnEntries[id] || positions[id] < requestPosition {
			continue
		}
		for _, block := range entry.Blocks {
			if block.Kind == BlockKindToolUse || block.Kind == BlockKindToolResult {
				return nil, sessionpkg.RequestTranscriptNoToolLineage
			}
		}
	}

	toolUses := make(map[string]string)
	toolResults := make(map[string]int)
	var references []sessionpkg.RequestTranscriptReference
	descendantIDs := make([]string, 0, len(descendants))
	for id := range descendants {
		descendantIDs = append(descendantIDs, id)
	}
	sort.Strings(descendantIDs)
	for _, id := range descendantIDs {
		entry := byID[id]
		for _, block := range entry.Blocks {
			if block.Kind != BlockKindToolUse && block.Kind != BlockKindToolResult {
				continue
			}
			toolID := strings.TrimSpace(block.ToolUseID)
			if toolID == "" || toolID != block.ToolUseID {
				return nil, sessionpkg.RequestTranscriptMissingToolID
			}
			kind := sessionpkg.RequestTranscriptToolUseReference
			if block.Kind == BlockKindToolUse {
				if _, exists := toolUses[toolID]; exists {
					return nil, sessionpkg.RequestTranscriptAmbiguousToolLineage
				}
				toolUses[toolID] = id
			} else {
				kind = sessionpkg.RequestTranscriptToolResultReference
				toolResults[toolID]++
				if toolResults[toolID] > 1 {
					return nil, sessionpkg.RequestTranscriptAmbiguousToolLineage
				}
			}
			references = append(references, sessionpkg.RequestTranscriptReference{EntryID: id, Kind: kind, ToolID: toolID})
		}
	}
	for _, id := range descendantIDs {
		entry := byID[id]
		for _, block := range entry.Blocks {
			if block.Kind != BlockKindToolResult {
				continue
			}
			ancestor := entry.parentEntryID
			matchedUse := false
			for ancestor != "" {
				parent := byID[ancestor]
				if parent == nil {
					return nil, sessionpkg.RequestTranscriptNoToolLineage
				}
				for _, parentBlock := range parent.Blocks {
					if parentBlock.Kind == BlockKindToolUse && parentBlock.ToolUseID == block.ToolUseID {
						matchedUse = true
					}
				}
				ancestor = parent.parentEntryID
			}
			if !matchedUse || toolUses[block.ToolUseID] == "" {
				return nil, sessionpkg.RequestTranscriptNoToolLineage
			}
		}
	}
	return references, ""
}

func isLaterUserTurn(entry *HistoryEntry) bool {
	if entry == nil || entry.Actor != ActorUser {
		return false
	}
	for _, block := range entry.Blocks {
		if block.Kind == BlockKindToolResult {
			return false
		}
	}
	return true
}
