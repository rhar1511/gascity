package beads

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// DecisionFrontierAnswerSlot is an independently authenticated immutable answer
// slot that must still match the protected ticket at the eligibility check.
type DecisionFrontierAnswerSlot struct {
	TicketID string
	AnswerID string
}

// DecisionFrontierTargetBinding selects one persisted session incarnation.
type DecisionFrontierTargetBinding struct {
	SessionID  string
	Generation int64
	TargetName string
}

// DecisionFrontierResumeCheck binds eligibility to original and physical revisions.
type DecisionFrontierResumeCheck struct {
	CityRef, StoreRef, WorkID, MapID, FrontierRevision string
	PhysicalRevision                                   int64
	Answers                                            []DecisionFrontierAnswerSlot
}

// DecisionFrontierResumeResult is a no-op guarded observation, not admission.
type DecisionFrontierResumeResult struct {
	Eligible bool
	Reason   string
}

// DecisionFrontierAtomicBackend owns target-fenced creation and guarded resume
// reads in the existing physical ledger. A callback runs on an isolated backend
// transaction view; it must not perform provider I/O or escape that view.
type DecisionFrontierAtomicBackend interface {
	EnsureDecisionFrontierTargetBound(DecisionFrontierTargetBinding, func(Store) error) error
	CheckDecisionFrontierResume(DecisionFrontierResumeCheck) (DecisionFrontierResumeResult, error)
}

// DecisionFrontierAtomicBackendHandleProvider forwards a qualified backend's
// transaction semantics through a controller-owned wrapper.
type DecisionFrontierAtomicBackendHandleProvider interface {
	DecisionFrontierAtomicBackendHandle() (DecisionFrontierAtomicBackend, bool)
}

// DecisionFrontierAtomicBackendFor deliberately exposes only qualified native
// memory transactions. SQLite, remote, caching and proxy adapters require their
// own atomic contract implementation; row CAS alone is insufficient.
func DecisionFrontierAtomicBackendFor(store Store) (DecisionFrontierAtomicBackend, bool) {
	if p, ok := store.(DecisionFrontierAtomicBackendHandleProvider); ok {
		backend, available := p.DecisionFrontierAtomicBackendHandle()
		return backend, available && capabilityValuePresent(backend)
	}
	m, ok := store.(*MemStore)
	if !ok || m == nil {
		return nil, false
	}
	writable, _ := m.probeConditionalWriteCapability()
	if !writable {
		return nil, false
	}
	return m, true
}

func (m *MemStore) independentFrontierReadyLocked(mapID string) (bool, error) {
	i := m.indexOfLocked(mapID)
	if i < 0 {
		return true, nil
	}
	doc, _, err := decisionFrontierLinkRecord(m.beads[i])
	if err != nil {
		return false, err
	}
	if doc.DeliveryContract != DecisionFrontierIndependentQuestionsContract {
		return true, nil
	}
	if doc.PromptBinding == nil || doc.PromptTarget == "" || doc.PromptBinding.RequestID != doc.PromptID {
		return false, ErrDecisionFrontierLinkConflict
	}
	si := m.indexOfLocked(doc.PromptBinding.SessionID)
	if si < 0 {
		return false, nil
	}
	target := m.beads[si]
	if target.Status != "open" || target.Metadata["generation"] != fmt.Sprint(doc.PromptBinding.ExecutionGeneration) ||
		target.Metadata["configured_named_session"] != "true" || target.Metadata["configured_named_identity"] != doc.PromptTarget ||
		(target.Metadata["state"] != "active" && target.Metadata["state"] != "awake") {
		return false, nil
	}
	for _, other := range m.beads {
		if other.ID != target.ID && other.Status == "open" && other.Metadata["configured_named_identity"] == doc.PromptTarget {
			return false, nil
		}
	}
	covered := make(map[string]bool, len(doc.Questions))
	for _, edge := range m.deps {
		if edge.DependsOnID != doc.MapID || edge.Type != "relates-to" {
			continue
		}
		i := m.indexOfLocked(edge.IssueID)
		if i < 0 {
			return false, ErrNotFound
		}
		intent := m.beads[i]
		if intent.Metadata[beadmeta.DecisionFrontierRecordMetadataKey] != decisionFrontierPromptKind {
			continue
		}
		state := intent.Metadata[beadmeta.DecisionFrontierStateMetadataKey]
		if state != "accepted" && state != "acknowledged" {
			continue
		}
		round, kind, err := decisionFrontierLinkRecord(intent)
		if err != nil || kind != decisionFrontierPromptKind || round.PresentationVersion != 2 || !sameDecisionFrontierLinkIdentity(doc.identity(), round.identity()) ||
			!decisionFrontierPromptTicketsMatch(round, doc) || round.PromptBinding == nil || round.PromptBinding.SessionID != doc.PromptBinding.SessionID ||
			round.PromptBinding.ExecutionGeneration != doc.PromptBinding.ExecutionGeneration || round.PromptBinding.RequestID != round.ID {
			return false, ErrDecisionFrontierLinkConflict
		}
		for _, id := range round.TicketIDs {
			covered[id] = true
		}
	}
	for _, q := range doc.Questions {
		id := DecisionFrontierQuestionRecordID(doc.CityRef, doc.StoreRef, doc.MapID, q.ID)
		i := m.indexOfLocked(id)
		if i < 0 {
			return false, ErrNotFound
		}
		ticket := m.beads[i]
		if ticket.Status != "open" || hasDecisionHoldLabel(ticket) || HasDecisionFrontierHold(ticket) || !covered[id] || !strings.HasPrefix(ticket.Metadata[beadmeta.DecisionFrontierStateMetadataKey], "answered:") {
			return false, nil
		}
	}
	return true, nil
}

// EnsureDecisionFrontierTargetBound compares the authoritative session row and
// commits source reservation and immutable records under the same backend lock.
func (m *MemStore) EnsureDecisionFrontierTargetBound(target DecisionFrontierTargetBinding, operation func(Store) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.DisableConditionalWrites || operation == nil {
		return ErrConditionalWriteUnsupported
	}
	i := m.indexOfLocked(target.SessionID)
	if i < 0 {
		return fmt.Errorf("target session %q: %w", target.SessionID, ErrNotFound)
	}
	row := m.beads[i]
	if target.Generation <= 0 || row.Metadata["generation"] != fmt.Sprint(target.Generation) ||
		target.TargetName == "" || strings.TrimSpace(target.TargetName) != target.TargetName || row.Status != "open" ||
		row.Metadata["configured_named_session"] != "true" || row.Metadata["configured_named_identity"] != target.TargetName {
		return ErrDecisionFrontierLinkConflict
	}
	// This is an ephemeral transaction image of the same ledger, not another
	// durable authority store. All mutations commit together or none do.
	view := NewMemStore()
	view.HonorExplicitIDs, view.IDPrefix, view.seq = m.HonorExplicitIDs, m.IDPrefix, m.seq
	view.deps = slices.Clone(m.deps)
	for _, b := range m.beads {
		view.beads = append(view.beads, cloneBead(b))
	}
	if err := operation(view); err != nil {
		return err
	}
	m.beads, m.deps, m.seq = view.beads, view.deps, view.seq
	return nil
}

// CheckDecisionFrontierResume checks source, release chain, tickets, holds and
// source dependencies from one memory-ledger critical section without writing.
func (m *MemStore) CheckDecisionFrontierResume(check DecisionFrontierResumeCheck) (DecisionFrontierResumeResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.DisableConditionalWrites {
		return DecisionFrontierResumeResult{}, ErrConditionalWriteUnsupported
	}
	wi, mi := m.indexOfLocked(check.WorkID), m.indexOfLocked(check.MapID)
	if wi < 0 || mi < 0 {
		return DecisionFrontierResumeResult{}, ErrNotFound
	}
	work, mapBead := m.beads[wi], m.beads[mi]
	if work.Revision != check.PhysicalRevision {
		return DecisionFrontierResumeResult{}, &PreconditionFailedError{ID: work.ID, Expected: check.PhysicalRevision, Current: work.Revision}
	}
	doc, kind, err := decisionFrontierLinkRecord(mapBead)
	if err != nil || kind != decisionFrontierMapKind || doc.CityRef != check.CityRef || doc.StoreRef != check.StoreRef || doc.WorkID != work.ID || doc.WorkRevision != check.FrontierRevision || doc.DeliveryContract != DecisionFrontierIndependentQuestionsContract {
		return DecisionFrontierResumeResult{}, ErrDecisionFrontierLinkConflict
	}
	pending := func(reason string) DecisionFrontierResumeResult {
		return DecisionFrontierResumeResult{Reason: reason}
	}
	if HasDecisionFrontierHold(work) || mapBead.Metadata[beadmeta.DecisionFrontierStateMetadataKey] != "resolved" {
		return pending("human decisions remain pending"), nil
	}
	ready, err := m.independentFrontierReadyLocked(check.MapID)
	if err != nil {
		return DecisionFrontierResumeResult{}, err
	}
	if !ready {
		return pending("human decisions or delivery evidence remain pending"), nil
	}
	var receipts []RevisionTransitionReceipt
	if json.Unmarshal([]byte(work.Metadata[beadmeta.DecisionFrontierRevisionReceiptsMetadataKey]), &receipts) != nil {
		return DecisionFrontierResumeResult{}, ErrDecisionFrontierLinkConflict
	}
	var reserved, released *RevisionTransitionReceipt
	for i := range receipts {
		r := &receipts[i]
		if r.CityRef == check.CityRef && r.StoreRef == check.StoreRef && r.WorkID == work.ID && r.MapID == check.MapID {
			if r.Operation == "reserve" {
				reserved = r
			}
			if r.Operation == "release" {
				released = r
			}
		}
	}
	if reserved == nil || released == nil || fmt.Sprint(reserved.FromRevision) != check.FrontierRevision || released.FromRevision != reserved.ToRevision || released.ToRevision != work.Revision {
		return DecisionFrontierResumeResult{}, ErrDecisionFrontierLinkConflict
	}
	if len(check.Answers) != len(doc.Questions) {
		return pending("human decisions remain pending"), nil
	}
	for i, q := range doc.Questions {
		id := DecisionFrontierQuestionRecordID(doc.CityRef, doc.StoreRef, doc.MapID, q.ID)
		ti := m.indexOfLocked(id)
		if ti < 0 || check.Answers[i].TicketID != id {
			return DecisionFrontierResumeResult{}, ErrDecisionFrontierLinkConflict
		}
		ticket := m.beads[ti]
		if ticket.Metadata[beadmeta.DecisionFrontierStateMetadataKey] != "answered:"+check.Answers[i].AnswerID || hasDecisionHoldLabel(ticket) {
			return pending("human decisions remain pending"), nil
		}
	}
	if hasDecisionHoldLabel(work) {
		return pending("source work has an independent hold"), nil
	}
	if work.Status != "open" && work.Status != "in_progress" || IsDeferred(work, time.Now()) {
		return pending("source work is not open"), nil
	}
	for _, dep := range m.deps {
		if dep.IssueID != work.ID || !IsReadyBlockingDependencyType(dep.Type) {
			continue
		}
		i := m.indexOfLocked(dep.DependsOnID)
		if i < 0 || !DependencySatisfied(m.beads[i].Status, m.beads[i].Metadata[beadmeta.WorkOutcomeMetadataKey]) {
			return pending("source work has blocking dependencies"), nil
		}
	}
	return DecisionFrontierResumeResult{Eligible: true}, nil
}

func hasDecisionHoldLabel(b Bead) bool {
	for _, label := range b.Labels {
		if strings.HasPrefix(label, "hold:") {
			return true
		}
	}
	return false
}
