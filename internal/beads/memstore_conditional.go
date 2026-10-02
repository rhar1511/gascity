package beads

import (
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

var (
	_ ConditionalWriter                = (*MemStore)(nil)
	_ RevisionTransitionWriter         = (*MemStore)(nil)
	_ DecisionFrontierSourceReader     = (*MemStore)(nil)
	_ DecisionFrontierRecordWriter     = (*MemStore)(nil)
	_ conditionalWritesModeCarrier     = (*MemStore)(nil)
	_ conditionalWriteCapabilityProber = (*MemStore)(nil)

	// FileStore inherits the stamp and the prober through its embedded
	// *MemStore: DisableConditionalWrites is ONE field stored on the embedded
	// MemStore (FileStore's CAS shadows read the same storage through
	// promotion), so a promoted prober answers identically and FileStore
	// needs no shadow of its own.
	_ conditionalWritesModeCarrier     = (*FileStore)(nil)
	_ conditionalWriteCapabilityProber = (*FileStore)(nil)
)

// DecisionFrontierSourceSnapshot returns the current source row together with
// its persisted outgoing edges from one memory-store critical section.
func (m *MemStore) DecisionFrontierSourceSnapshot(id string) (Bead, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	index := m.indexOfLocked(id)
	if index < 0 {
		return Bead{}, fmt.Errorf("reading decision-frontier source %q: %w", id, ErrNotFound)
	}
	bead := cloneBead(m.beads[index])
	bead.Dependencies = nil
	for _, dep := range m.deps {
		if dep.IssueID == id {
			bead.Dependencies = append(bead.Dependencies, dep)
		}
	}
	return bead, nil
}

// probeConditionalWriteCapability reports the instance toggle: a MemStore is
// natively capable unless DisableConditionalWrites is set (the deterministic
// auto-degrade / require-fail-closed matrix cell, §7.3).
func (m *MemStore) probeConditionalWriteCapability() (bool, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.DisableConditionalWrites {
		return false, "conditional writes disabled on this store instance"
	}
	return true, ""
}

// CreateDecisionFrontierRecord creates one validated controller-owned record.
// Generic Create rejects this metadata namespace.
func (m *MemStore) CreateDecisionFrontierRecord(b Bead) (Bead, error) {
	if err := validateDecisionFrontierRecordCreate(b); err != nil {
		return Bead{}, err
	}
	return m.create(b)
}

// CompareAndSetDecisionFrontierRecordMetadataKey advances only the state or
// prompt-reason fields allowed by the controller record state machine.
func (m *MemStore) CompareAndSetDecisionFrontierRecordMetadataKey(id, key, expected, next string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.DisableConditionalWrites {
		return false, ErrConditionalWriteUnsupported
	}
	i := m.indexOfLocked(id)
	if i < 0 {
		return false, fmt.Errorf("compare-and-set decision record %q: %w", id, ErrNotFound)
	}
	current := m.beads[i]
	if current.Metadata[key] != expected {
		return false, nil
	}
	if err := validateDecisionFrontierRecordCAS(current, key, expected, next); err != nil {
		return false, err
	}
	if current.Metadata == nil {
		current.Metadata = make(StringMap, 1)
	}
	current.Metadata[key] = next
	current.UpdatedAt = time.Now()
	current.Revision++
	m.beads[i] = current
	return true, nil
}

// EnsureDecisionFrontierLink adds only a relationship authorized by the
// immutable endpoint documents. Exact retries are no-ops.
func (m *MemStore) EnsureDecisionFrontierLink(sourceID, targetID, depType string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.DisableConditionalWrites {
		return ErrConditionalWriteUnsupported
	}
	sourceIndex := m.indexOfLocked(sourceID)
	if sourceIndex < 0 {
		return fmt.Errorf("decision-frontier link source %q: %w", sourceID, ErrNotFound)
	}
	targetIndex := m.indexOfLocked(targetID)
	if targetIndex < 0 {
		return fmt.Errorf("decision-frontier link target %q: %w", targetID, ErrNotFound)
	}
	source := m.beads[sourceIndex]
	target := m.beads[targetIndex]
	sourceDoc, _, err := decisionFrontierLinkRecord(source)
	if err != nil {
		return err
	}
	mapBead := target
	if targetID != sourceDoc.MapID {
		mapIndex := m.indexOfLocked(sourceDoc.MapID)
		if mapIndex < 0 {
			return fmt.Errorf("decision-frontier link map %q: %w", sourceDoc.MapID, ErrNotFound)
		}
		mapBead = m.beads[mapIndex]
	}
	if err := validateDecisionFrontierLink(source, target, mapBead, depType); err != nil {
		return err
	}
	for _, existing := range m.deps {
		if existing.IssueID != sourceID || existing.DependsOnID != targetID {
			continue
		}
		if existing.Type == depType {
			return nil
		}
		return ErrDecisionFrontierLinkConflict
	}
	m.deps = append(m.deps, Dep{IssueID: sourceID, DependsOnID: targetID, Type: depType})
	m.bumpDependencySourceRevisionLocked(sourceID)
	return nil
}

// UpdateIfMatch applies opts only when the bead's current revision equals
// expectedRevision, otherwise it returns *PreconditionFailedError. When the
// instance has DisableConditionalWrites set it returns ErrConditionalWriteUnsupported.
func (m *MemStore) UpdateIfMatch(id string, expectedRevision int64, opts UpdateOpts) error {
	if err := rejectAttemptEvidencePayloadMetadataWrite(opts.Metadata); err != nil {
		return err
	}
	if err := validateConditionalUpdateOpts(opts); err != nil {
		return fmt.Errorf("conditional update %s: %w", id, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.DisableConditionalWrites {
		return ErrConditionalWriteUnsupported
	}
	i := m.indexOfLocked(id)
	if i < 0 {
		return fmt.Errorf("updating bead %q: %w", id, ErrNotFound)
	}
	if m.beads[i].Revision != expectedRevision {
		return &PreconditionFailedError{ID: id, Expected: expectedRevision, Current: m.beads[i].Revision}
	}
	if err := protectAttemptEvidenceUpdate(m.beads[i], opts); err != nil {
		return err
	}
	if err := ValidateLifecycleMutation(m.beads[i], opts); err != nil {
		return fmt.Errorf("conditional update lifecycle bead %q: %w", id, err)
	}
	m.applyUpdateLocked(i, opts)
	return nil
}

// CloseIfMatch closes the bead only when its current revision equals
// expectedRevision. Closing an already-closed bead is a no-op (matching Close).
func (m *MemStore) CloseIfMatch(id string, expectedRevision int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.DisableConditionalWrites {
		return ErrConditionalWriteUnsupported
	}
	i := m.indexOfLocked(id)
	if i < 0 {
		return fmt.Errorf("closing bead %q: %w", id, ErrNotFound)
	}
	if m.beads[i].Revision != expectedRevision {
		return &PreconditionFailedError{ID: id, Expected: expectedRevision, Current: m.beads[i].Revision}
	}
	if HasLifecycleRecoveryIntent(m.beads[i]) {
		return ErrLifecycleIntentImmutable
	}
	if err := protectAttemptEvidencePayloadMutation(m.beads[i]); err != nil {
		return err
	}
	if err := ValidateDecisionFrontierClose(m.beads[i]); err != nil {
		return err
	}
	if m.beads[i].Status == "closed" {
		return nil
	}
	setBeadStatus(&m.beads[i], "closed")
	m.beads[i].UpdatedAt = time.Now()
	m.beads[i].Revision++
	return nil
}

// DeleteIfMatch removes the bead only when its current revision equals
// expectedRevision.
func (m *MemStore) DeleteIfMatch(id string, expectedRevision int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.DisableConditionalWrites {
		return ErrConditionalWriteUnsupported
	}
	i := m.indexOfLocked(id)
	if i < 0 {
		return fmt.Errorf("deleting bead %q: %w", id, ErrNotFound)
	}
	if m.beads[i].Revision != expectedRevision {
		return &PreconditionFailedError{ID: id, Expected: expectedRevision, Current: m.beads[i].Revision}
	}
	if err := ValidateDecisionFrontierDelete(m.beads[i]); err != nil {
		return err
	}
	if err := protectRetainedEvidenceDelete(m.beads[i]); err != nil {
		return err
	}
	if err := ValidateLifecycleDelete(m.beads[i]); err != nil {
		return err
	}
	// Like the pinned SQL backend's ON DELETE CASCADE, remove references only
	// after the row fence and guards succeed, in the same critical section.
	// Never make a caller remove edges before attempting its conditional delete.
	if err := m.deleteDependencyReferencesLocked(m.beads[i]); err != nil {
		return err
	}
	m.beads = append(m.beads[:i], m.beads[i+1:]...)
	delete(m.localStrings, id)
	return nil
}

// CompareAndSetMetadataKey atomically sets metadata[key] = next when the current
// value equals expected. expected == "" matches an absent or empty-valued key.
// Reading a key from a nil metadata map yields "", so the absent case falls out
// naturally. Returns (true, nil) on swap, (false, nil) on a genuine mismatch.
func (m *MemStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	if isDecisionFrontierControlKey(key) {
		return false, ErrDecisionFrontierMutationBlocked
	}
	if err := rejectAttemptEvidencePayloadMetadataKeyWrite(key); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.DisableConditionalWrites {
		return false, ErrConditionalWriteUnsupported
	}
	i := m.indexOfLocked(id)
	if i < 0 {
		return false, fmt.Errorf("compare-and-set metadata on %q: %w", id, ErrNotFound)
	}
	if IsDecisionFrontierRecord(m.beads[i]) {
		return false, ErrDecisionFrontierMutationBlocked
	}
	if err := protectAttemptEvidenceRecordMutation(m.beads[i]); err != nil {
		return false, err
	}
	if m.beads[i].Metadata[key] != expected {
		return false, nil
	}
	if err := ValidateLifecycleMutation(m.beads[i], UpdateOpts{Metadata: map[string]string{key: next}}); err != nil {
		return false, err
	}
	if m.beads[i].Metadata == nil {
		m.beads[i].Metadata = make(StringMap)
	}
	m.beads[i].Metadata[key] = next
	m.beads[i].UpdatedAt = time.Now()
	m.beads[i].Revision++
	return true, nil
}

// CompareAndSetMetadataKeyWithReceipt performs a revision-fenced decision
// source transition and stores its exact post-write revision in the same
// in-memory critical section.
func (m *MemStore) CompareAndSetMetadataKeyWithReceipt(id, key, expected, next string, expectedRevision int64, receipt RevisionTransitionReceipt) (Bead, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.DisableConditionalWrites {
		return Bead{}, false, ErrConditionalWriteUnsupported
	}
	i := m.indexOfLocked(id)
	if i < 0 {
		return Bead{}, false, fmt.Errorf("decision-frontier transition on %q: %w", id, ErrNotFound)
	}
	current := m.beads[i]
	if current.Revision != expectedRevision {
		return Bead{}, false, &PreconditionFailedError{ID: id, Expected: expectedRevision, Current: current.Revision}
	}
	if err := validateDecisionFrontierTransition(current, key, expected, next, expectedRevision, receipt); err != nil {
		return Bead{}, false, err
	}
	if current.Metadata[key] != expected {
		return Bead{}, false, nil
	}
	if receipt.Operation == "release" {
		ready, err := m.independentFrontierReadyLocked(receipt.MapID)
		if err != nil {
			return Bead{}, false, err
		}
		if !ready {
			return Bead{}, false, ErrDecisionFrontierMutationBlocked
		}
	}
	if current.Metadata == nil {
		current.Metadata = make(StringMap)
	}
	if expectedRevision == int64(^uint64(0)>>1) {
		return Bead{}, false, fmt.Errorf("decision-frontier transition exhausted revision token")
	}
	toRevision := expectedRevision + 1
	receipts, err := appendRevisionTransitionReceipt(current.Metadata[beadmeta.DecisionFrontierRevisionReceiptsMetadataKey], receipt, toRevision)
	if err != nil {
		return Bead{}, false, err
	}
	current.Metadata[key] = next
	current.Metadata[beadmeta.DecisionFrontierRevisionReceiptsMetadataKey] = receipts
	current.UpdatedAt = time.Now()
	current.Revision = toRevision
	m.beads[i] = cloneBead(current)
	return cloneBead(current), true, nil
}
