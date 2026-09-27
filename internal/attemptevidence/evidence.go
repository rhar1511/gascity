// Package attemptevidence captures immutable evidence for one exact execution
// attempt. A first-write owner index binds the attempt ID to one payload; a
// separate, non-ephemeral archive bead preserves that payload if the work or
// controller bead is later removed.
package attemptevidence

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

const (
	// SchemaVersion is the version of the persisted attempt-evidence record.
	SchemaVersion = 1

	// KindWorkbench identifies a Workbench execution attempt.
	KindWorkbench = "workbench"
	// KindRetry identifies a formula retry attempt.
	KindRetry = "retry"
	// KindRalph identifies a Ralph loop attempt.
	KindRalph = "ralph"

	// StatusAvailable means the evidence payload was captured and sealed.
	StatusAvailable = "available"
	// StatusUnavailable means capture could not establish the requested evidence.
	StatusUnavailable = "unavailable"
	// StatusMissing means the source did not contain the requested evidence.
	StatusMissing = "missing"
	// StatusPending means a capture reservation exists but is not yet sealed.
	StatusPending = "pending"

	// WorkingTreeClean means no mutable worktree changes were found.
	WorkingTreeClean = "clean"
	// WorkingTreeDirty means mutable worktree changes were captured separately.
	WorkingTreeDirty = "dirty"
	// WorkingTreeUnknown means the worktree status could not be established.
	WorkingTreeUnknown = "unavailable"

	// DiffSourceCandidateCommitDelta identifies an immutable base-to-candidate commit diff.
	DiffSourceCandidateCommitDelta = "candidate_commit_delta"
	// DiffSourceWorkingTree identifies mutable tracked or untracked worktree content.
	DiffSourceWorkingTree = "working_tree"

	// DiffEncoding identifies the compressed JSON encoding used for stored diffs.
	DiffEncoding = "gzip+json"

	archiveLabel      = "gc:attempt-evidence"
	maxDiffInputBytes = 16 << 20
	maxEvidenceBytes  = 64 << 10
)

var (
	// ErrNotFound indicates that the exact owner/attempt pair has no archive.
	ErrNotFound = errors.New("attempt evidence not found")
	// ErrPending indicates that capture was reserved but has not been sealed.
	ErrPending = errors.New("attempt evidence capture is pending")
	// ErrIdentityIncomplete indicates that an execution lacks a stable identity.
	ErrIdentityIncomplete = errors.New("attempt identity is incomplete")
	// ErrArchiveConflict indicates that the store contains conflicting payloads.
	ErrArchiveConflict = errors.New("conflicting attempt evidence archive records")
	// ErrCaptureTooLarge indicates that evidence exceeds the record size limit.
	ErrCaptureTooLarge = errors.New("attempt evidence exceeds the supported durable record size")
	// ErrPrivatePayloadTransportUnsupported indicates that a store cannot safely
	// persist private evidence bytes.
	ErrPrivatePayloadTransportUnsupported = errors.New("store does not support private attempt-evidence payload transport")
	// ErrConditionalWriteUnsupported indicates that the store cannot seal a
	// first-write-wins evidence record.
	ErrConditionalWriteUnsupported = beads.ErrConditionalWriteUnsupported
)

// Identity is the immutable key for one execution, independent of the
// current branch or whichever attempt is newest. Workbench identities include
// the exact session incarnation and claim generation. Retry and ralph
// identities include their exact attempt bead ID.
type Identity struct {
	Kind              string `json:"kind"`
	OwnerBeadID       string `json:"owner_bead_id"`
	ExecutionBeadID   string `json:"execution_bead_id"`
	SessionID         string `json:"session_id,omitempty"`
	SessionGeneration string `json:"session_generation,omitempty"`
	ClaimGeneration   string `json:"claim_generation,omitempty"`
}

// PermissionScope is the original access boundary of an attempt. It is sealed
// with the archive so removing or relocating the owner bead cannot widen who
// may read its private diff. RepositoryRoot identifies the shared Git common
// repository; WorkspaceRoot pins the exact worktree that was captured.
type PermissionScope struct {
	StoreRef       string `json:"store_ref"`
	WorkID         string `json:"work_id"`
	RepositoryRoot string `json:"repository_root,omitempty"`
	WorkspaceRoot  string `json:"workspace_root,omitempty"`
}

// ReadAuthorizationRequest binds a read decision to one immutable attempt and
// its archived original permission scope.
type ReadAuthorizationRequest struct {
	Scope     PermissionScope `json:"scope"`
	AttemptID string          `json:"attempt_id"`
}

// Reference is the compact cross-surface reference stored by policy/action
// records. It binds those records to one immutable attempt and its exact diff.
type Reference struct {
	StoreRef          string `json:"store_ref"`
	WorkID            string `json:"work_id"`
	AttemptID         string `json:"attempt_id"`
	BaseSHA           string `json:"base_sha,omitempty"`
	CandidateSHA      string `json:"candidate_sha,omitempty"`
	DiffSHA256        string `json:"diff_sha256,omitempty"`
	DiffSource        string `json:"diff_source"`
	WorkingTreeStatus string `json:"working_tree_status"`
}

// Facet records whether a related policy/action/acknowledgement fact could be
// attributed to this attempt. Empty lists are never treated as proof that no
// action or acknowledgement occurred.
type Facet struct {
	Status string   `json:"status"`
	Reason string   `json:"reason,omitempty"`
	Refs   []string `json:"refs,omitempty"`
}

// Evidence is one immutable attempt snapshot. Diff.Payload is the immutable
// base-commit-to-candidate-commit delta. WorkspaceDiff separately retains
// mutable tracked edits and sorted untracked file bytes from the worktree.
type Evidence struct {
	SchemaVersion int             `json:"schema_version"`
	AttemptID     string          `json:"attempt_id"`
	Identity      Identity        `json:"identity"`
	StoreRef      string          `json:"store_ref,omitempty"`
	Permission    PermissionScope `json:"permission_scope"`
	CapturedAt    time.Time       `json:"captured_at"`
	Outcome       string          `json:"outcome,omitempty"`

	SourceStatus      string `json:"source_status"`
	SourceReason      string `json:"source_reason,omitempty"`
	BaseSHA           string `json:"base_sha,omitempty"`
	BaseStatus        string `json:"base_status"`
	BaseReason        string `json:"base_reason,omitempty"`
	CandidateSHA      string `json:"candidate_sha,omitempty"`
	CandidateStatus   string `json:"candidate_status"`
	WorkingTreeStatus string `json:"working_tree_status"`
	CandidateReason   string `json:"candidate_reason,omitempty"`

	Diff          DiffSnapshot `json:"diff"`
	WorkspaceDiff DiffSnapshot `json:"workspace_diff"`

	Policy           Facet `json:"policy"`
	Actions          Facet `json:"actions"`
	Acknowledgements Facet `json:"acknowledgements"`
	Redaction        Facet `json:"redaction"`
}

// Reader is the exact attempt-evidence read port used by API and policy
// services. Callers resolve the authoritative beads.Store from trusted server
// state, then pass that store here; the browser never supplies a store
// reference. Every method addresses an owner and exact attempt ID, with no
// latest-attempt fallback.
type Reader interface {
	Read(store beads.Store, ownerBeadID, attemptID string) (Evidence, error)
	List(store beads.Store, ownerBeadID string) ([]Evidence, error)
	References(store beads.Store, storeRef, ownerBeadID string) ([]Reference, error)
}

// StoreReader adapts the package's validated archive reads to Reader.
type StoreReader struct{}

func (StoreReader) Read(store beads.Store, ownerBeadID, attemptID string) (Evidence, error) {
	return Read(store, ownerBeadID, attemptID)
}

// List returns all validated archives for one owner in deterministic order.
func (StoreReader) List(store beads.Store, ownerBeadID string) ([]Evidence, error) {
	return List(store, ownerBeadID)
}

// References returns exact archive references for one owner and store scope.
func (StoreReader) References(store beads.Store, storeRef, ownerBeadID string) ([]Reference, error) {
	return References(store, storeRef, ownerBeadID)
}

// DiffSnapshot describes exactly what was retained. A missing base revision or
// workspace is a sealed unavailable result. Capture/read/write failures return
// errors and do not authorize the caller to close, delete, or advance work.
type DiffSnapshot struct {
	Status            string `json:"status"`
	Reason            string `json:"reason,omitempty"`
	Source            string `json:"source"`
	Encoding          string `json:"encoding,omitempty"`
	SHA256            string `json:"sha256,omitempty"`
	UncompressedBytes int64  `json:"uncompressed_bytes,omitempty"`
	Payload           []byte `json:"payload,omitempty"`
}

type diffBundle struct {
	TrackedPatch []byte          `json:"tracked_patch"`
	Untracked    []UntrackedFile `json:"untracked,omitempty"`
}

// UntrackedFile is a file that was not in git's index at capture time. Its
// path is relative to the captured worktree and its content is base64 encoded
// by JSON serialization.
type UntrackedFile struct {
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Bytes   []byte `json:"bytes"`
	Mode    uint32 `json:"mode,omitempty"`
	Symlink bool   `json:"symlink,omitempty"`
}

// CaptureSpec supplies the immutable execution identity and the source facts
// gathered by the caller at the lifecycle boundary.
type CaptureSpec struct {
	Identity    Identity
	StoreRef    string
	Permission  PermissionScope
	WorkDir     string
	BaseSHA     string
	Outcome     string
	RequestRefs []string
	Now         func() time.Time
}

// AttemptID returns a stable ID for the complete execution identity. It never
// uses a branch name, current checkout HEAD, or a latest-attempt pointer.
func AttemptID(identity Identity) (string, error) {
	if err := validateIdentity(identity); err != nil {
		return "", err
	}
	parts := []string{
		identity.Kind,
		identity.OwnerBeadID,
		identity.ExecutionBeadID,
		identity.SessionID,
		identity.SessionGeneration,
		identity.ClaimGeneration,
	}
	material := strings.Join(parts, "\x00")
	digest := sha256.Sum256([]byte(material))
	return "ae-" + hex.EncodeToString(digest[:]), nil
}

// EvidenceReference extracts the exact digest/revision tuple needed by a
// policy/action record to bind itself to this snapshot.
func EvidenceReference(e Evidence, storeRef string) Reference {
	return Reference{
		StoreRef: storeRef, WorkID: e.Identity.OwnerBeadID, AttemptID: e.AttemptID,
		BaseSHA: e.BaseSHA, CandidateSHA: e.CandidateSHA, DiffSHA256: e.Diff.SHA256,
		DiffSource: e.Diff.Source, WorkingTreeStatus: e.WorkingTreeStatus,
	}
}

// References returns every exact immutable reference for an owner, in the
// same deterministic order as List. storeRef is supplied by the server after
// resolving the authoritative store; it must never come from an untrusted
// request. WorkID is the exact evidence owner bead ID.
func References(store beads.Store, storeRef, ownerBeadID string) ([]Reference, error) {
	if strings.TrimSpace(storeRef) == "" {
		return nil, errors.New("attempt evidence store reference is missing")
	}
	evidence, err := List(store, ownerBeadID)
	if err != nil {
		return nil, err
	}
	refs := make([]Reference, 0, len(evidence))
	for _, item := range evidence {
		refs = append(refs, EvidenceReference(item, storeRef))
	}
	return refs, nil
}

// Capture snapshots a worktree and seals the resulting record. If this exact
// attempt is already sealed, it repairs a missing archive copy and returns the
// first payload without reading the mutable worktree again.
func Capture(ctx context.Context, store beads.Store, spec CaptureSpec) (Evidence, error) {
	attemptID, err := AttemptID(spec.Identity)
	if err != nil {
		return Evidence{}, err
	}
	if store == nil {
		return Evidence{}, errors.New("capturing attempt evidence: bead store is unavailable")
	}
	if prior, found, err := readOwnerIndex(store, spec.Identity.OwnerBeadID, attemptID); err != nil {
		return Evidence{}, err
	} else if found {
		if err := ensureArchive(store, prior); err != nil {
			return Evidence{}, err
		}
		return prior, nil
	}
	if prior, found, err := readArchive(store, spec.Identity.OwnerBeadID, attemptID); err != nil {
		return Evidence{}, err
	} else if found {
		return prior, nil
	}
	if _, err := store.Get(spec.Identity.OwnerBeadID); err != nil {
		return Evidence{}, fmt.Errorf("capturing attempt evidence owner %q: %w", spec.Identity.OwnerBeadID, err)
	}

	evidence, err := snapshot(ctx, spec, attemptID)
	if err != nil {
		return Evidence{}, err
	}
	return Seal(store, evidence)
}

// Seal binds a caller-supplied, complete or explicitly unavailable snapshot to
// the owner's first-write metadata index, then copies it to an independent
// non-ephemeral archive bead. The owner CAS is the uniqueness fence: concurrent
// callers may create duplicate archive rows, but every row carries the same
// winning immutable bytes and Read deduplicates those copies.
func Seal(store beads.Store, proposed Evidence) (Evidence, error) {
	if store == nil {
		return Evidence{}, errors.New("sealing attempt evidence: bead store is unavailable")
	}
	if err := validateEvidence(proposed); err != nil {
		return Evidence{}, err
	}
	if !beads.SupportsPrivatePayloadValues(store) {
		return Evidence{}, ErrPrivatePayloadTransportUnsupported
	}
	encoded, err := json.Marshal(proposed)
	if err != nil {
		return Evidence{}, fmt.Errorf("marshal attempt evidence %s: %w", proposed.AttemptID, err)
	}
	if len(encoded) > maxEvidenceBytes {
		return Evidence{}, fmt.Errorf("attempt %s: %w (%d > %d bytes)", proposed.AttemptID, ErrCaptureTooLarge, len(encoded), maxEvidenceBytes)
	}
	key := ownerIndexKey(proposed.AttemptID)
	owner, err := store.Get(proposed.Identity.OwnerBeadID)
	if err != nil {
		return Evidence{}, fmt.Errorf("reading attempt evidence owner %q: %w", proposed.Identity.OwnerBeadID, err)
	}
	if existing := strings.TrimSpace(owner.Metadata[key]); existing != "" {
		sealed, err := decodeEvidence([]byte(existing))
		if err != nil {
			return Evidence{}, fmt.Errorf("reading sealed attempt evidence %s: %w", proposed.AttemptID, err)
		}
		if err := sameIdentity(sealed, proposed); err != nil {
			return Evidence{}, err
		}
		if err := ensureArchive(store, sealed); err != nil {
			return Evidence{}, err
		}
		return sealed, nil
	}

	encodedValue := string(encoded)
	outcome, casErr := beads.ApplyMetadataCAS(store, proposed.Identity.OwnerBeadID, key, "", encodedValue)
	if casErr != nil || outcome == beads.MetadataCASConflict {
		// A backend error may have happened after commit. Readback resolves that
		// ambiguity; an unavailable/empty read remains a hard failure.
		sealed, found, readErr := readOwnerIndex(store, proposed.Identity.OwnerBeadID, proposed.AttemptID)
		if readErr != nil {
			return Evidence{}, errors.Join(fmt.Errorf("sealing attempt evidence %s: %w", proposed.AttemptID, casErr), readErr)
		}
		if !found {
			if casErr != nil {
				return Evidence{}, fmt.Errorf("sealing attempt evidence %s: %w", proposed.AttemptID, casErr)
			}
			return Evidence{}, fmt.Errorf("sealing attempt evidence %s: metadata CAS conflict without a readable winner", proposed.AttemptID)
		}
		if err := sameIdentity(sealed, proposed); err != nil {
			return Evidence{}, err
		}
		if err := ensureArchive(store, sealed); err != nil {
			return Evidence{}, err
		}
		return sealed, nil
	}

	if err := ensureArchive(store, proposed); err != nil {
		return Evidence{}, err
	}
	return proposed, nil
}

// Read returns exactly attemptID for ownerBeadID. It consults the owner's
// first-write index first and the independent archive second, including after
// the source owner has been deleted. It never follows a latest pointer.
func Read(store beads.Store, ownerBeadID, attemptID string) (Evidence, error) {
	if store == nil {
		return Evidence{}, errors.New("reading attempt evidence: bead store is unavailable")
	}
	if ownerBeadID == "" || attemptID == "" {
		return Evidence{}, ErrNotFound
	}
	if evidence, found, err := readOwnerIndex(store, ownerBeadID, attemptID); err != nil {
		return Evidence{}, err
	} else if found {
		return evidence, nil
	}
	if evidence, found, err := readArchive(store, ownerBeadID, attemptID); err != nil {
		return Evidence{}, err
	} else if found {
		return evidence, nil
	}
	return Evidence{}, ErrNotFound
}

// List returns all retained attempts for an owner in deterministic capture
// order. Duplicate archive rows are collapsed only when their payloads match.
func List(store beads.Store, ownerBeadID string) ([]Evidence, error) {
	if store == nil {
		return nil, errors.New("listing attempt evidence: bead store is unavailable")
	}
	if strings.TrimSpace(ownerBeadID) == "" {
		return nil, ErrNotFound
	}
	byID := make(map[string]Evidence)
	owner, ownerErr := store.Get(ownerBeadID)
	if ownerErr == nil {
		for key, value := range owner.Metadata {
			if !strings.HasPrefix(key, beadmeta.AttemptEvidenceIndexPrefix) || value == "" {
				continue
			}
			evidence, err := decodeEvidence([]byte(value))
			if err != nil {
				return nil, fmt.Errorf("reading attempt evidence index on %s: %w", ownerBeadID, err)
			}
			if evidence.Identity.OwnerBeadID != ownerBeadID {
				return nil, fmt.Errorf("attempt evidence index on %s points to owner %s", ownerBeadID, evidence.Identity.OwnerBeadID)
			}
			byID[evidence.AttemptID] = evidence
		}
	} else if !errors.Is(ownerErr, beads.ErrNotFound) {
		return nil, fmt.Errorf("reading attempt evidence owner %s: %w", ownerBeadID, ownerErr)
	}

	archives, err := store.ListByMetadata(map[string]string{beadmeta.AttemptEvidenceArchiveOwnerIDMetadataKey: ownerBeadID}, 0, beads.IncludeClosed)
	if err != nil {
		return nil, fmt.Errorf("listing attempt evidence archive for %s: %w", ownerBeadID, err)
	}
	for _, row := range archives {
		if !IsArchiveRecord(row) {
			continue
		}
		evidence, err := evidenceFromArchive(row)
		if err != nil {
			return nil, err
		}
		if previous, exists := byID[evidence.AttemptID]; exists {
			if !samePayload(previous, evidence) {
				return nil, fmt.Errorf("attempt %s: %w", evidence.AttemptID, ErrArchiveConflict)
			}
			continue
		}
		byID[evidence.AttemptID] = evidence
	}

	out := make([]Evidence, 0, len(byID))
	for _, evidence := range byID {
		out = append(out, evidence)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CapturedAt.Equal(out[j].CapturedAt) {
			return out[i].CapturedAt.Before(out[j].CapturedAt)
		}
		return out[i].AttemptID < out[j].AttemptID
	})
	return out, nil
}

// IsArchiveRecord identifies the durable copy so generic read/delete routes can
// keep its payload behind the attempt-evidence access path.
func IsArchiveRecord(b beads.Bead) bool {
	return beads.IsAttemptEvidenceArchive(b)
}

// IsExecutionRecord reports whether a bead has been stamped as a Workbench
// execution source, rather than treating every arbitrary work bead as one.
func IsExecutionRecord(b beads.Bead) bool {
	return strings.TrimSpace(b.Metadata[beadmeta.SessionIDMetadataKey]) != "" &&
		strings.TrimSpace(b.Metadata[beadmeta.ClaimGenerationMetadataKey]) != ""
}

// DecodeDiff decompresses the stored canonical diff bundle and verifies its
// content digest before returning it.
func DecodeDiff(snapshot DiffSnapshot) (trackedPatch []byte, untracked []UntrackedFile, err error) {
	if snapshot.Status != StatusAvailable || snapshot.Encoding != DiffEncoding || len(snapshot.Payload) == 0 {
		return nil, nil, fmt.Errorf("diff is not available: %s", snapshot.Reason)
	}
	compressedDigest := sha256.Sum256(snapshot.Payload)
	if got := hex.EncodeToString(compressedDigest[:]); got != snapshot.SHA256 {
		return nil, nil, fmt.Errorf("diff digest mismatch: got %s, recorded %s", got, snapshot.SHA256)
	}
	reader, err := gzip.NewReader(bytes.NewReader(snapshot.Payload))
	if err != nil {
		return nil, nil, fmt.Errorf("opening compressed diff: %w", err)
	}
	defer reader.Close() //nolint:errcheck
	limited := io.LimitReader(reader, maxDiffInputBytes+1)
	decoded, err := io.ReadAll(limited)
	if err != nil {
		return nil, nil, fmt.Errorf("reading compressed diff: %w", err)
	}
	if len(decoded) > maxDiffInputBytes {
		return nil, nil, ErrCaptureTooLarge
	}
	if int64(len(decoded)) != snapshot.UncompressedBytes {
		return nil, nil, fmt.Errorf("diff size mismatch: got %d, recorded %d", len(decoded), snapshot.UncompressedBytes)
	}
	var bundle diffBundle
	if err := json.Unmarshal(decoded, &bundle); err != nil {
		return nil, nil, fmt.Errorf("decoding diff bundle: %w", err)
	}
	return bundle.TrackedPatch, bundle.Untracked, nil
}

func ownerIndexKey(attemptID string) string {
	digest := sha256.Sum256([]byte(attemptID))
	return beadmeta.AttemptEvidenceIndexPrefix + hex.EncodeToString(digest[:])
}

func readOwnerIndex(store beads.Store, ownerID, attemptID string) (Evidence, bool, error) {
	owner, err := store.Get(ownerID)
	if errors.Is(err, beads.ErrNotFound) {
		return Evidence{}, false, nil
	}
	if err != nil {
		return Evidence{}, false, fmt.Errorf("reading attempt evidence owner %s: %w", ownerID, err)
	}
	value := strings.TrimSpace(owner.Metadata[ownerIndexKey(attemptID)])
	if value == "" {
		return Evidence{}, false, nil
	}
	evidence, err := decodeEvidence([]byte(value))
	if err != nil {
		return Evidence{}, false, fmt.Errorf("decoding owner attempt index %s: %w", attemptID, err)
	}
	if evidence.AttemptID != attemptID || evidence.Identity.OwnerBeadID != ownerID {
		return Evidence{}, false, fmt.Errorf("owner attempt index mismatch for %s", attemptID)
	}
	return evidence, true, nil
}

func readArchive(store beads.Store, ownerID, attemptID string) (Evidence, bool, error) {
	rows, err := store.ListByMetadata(map[string]string{
		beadmeta.AttemptEvidenceArchiveOwnerIDMetadataKey:   ownerID,
		beadmeta.AttemptEvidenceArchiveAttemptIDMetadataKey: attemptID,
	}, 0, beads.IncludeClosed)
	if err != nil {
		return Evidence{}, false, fmt.Errorf("listing archive for attempt %s: %w", attemptID, err)
	}
	var found *Evidence
	for _, row := range rows {
		if !IsArchiveRecord(row) {
			continue
		}
		evidence, err := evidenceFromArchive(row)
		if err != nil {
			return Evidence{}, false, err
		}
		if evidence.AttemptID != attemptID || evidence.Identity.OwnerBeadID != ownerID {
			return Evidence{}, false, fmt.Errorf("archive row %s has a mismatched attempt reference", row.ID)
		}
		if found != nil && !samePayload(*found, evidence) {
			return Evidence{}, false, fmt.Errorf("attempt %s: %w", attemptID, ErrArchiveConflict)
		}
		evidenceCopy := evidence
		found = &evidenceCopy
	}
	if found == nil {
		return Evidence{}, false, nil
	}
	return *found, true, nil
}

func ensureArchive(store beads.Store, evidence Evidence) error {
	if !beads.SupportsPrivatePayloadValues(store) {
		return ErrPrivatePayloadTransportUnsupported
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		return fmt.Errorf("marshal archive for %s: %w", evidence.AttemptID, err)
	}
	if len(encoded) > maxEvidenceBytes {
		return fmt.Errorf("attempt %s archive: %w", evidence.AttemptID, ErrCaptureTooLarge)
	}
	archiveDigest := sha256.Sum256(encoded)
	digest := hex.EncodeToString(archiveDigest[:])
	rows, err := store.ListByMetadata(map[string]string{
		beadmeta.AttemptEvidenceArchiveOwnerIDMetadataKey:   evidence.Identity.OwnerBeadID,
		beadmeta.AttemptEvidenceArchiveAttemptIDMetadataKey: evidence.AttemptID,
	}, 0, beads.IncludeClosed)
	if err != nil {
		return fmt.Errorf("checking archive for attempt %s: %w", evidence.AttemptID, err)
	}
	for _, row := range rows {
		if !IsArchiveRecord(row) {
			continue
		}
		have, decodeErr := evidenceFromArchive(row)
		if decodeErr != nil {
			return decodeErr
		}
		if !samePayload(have, evidence) {
			return fmt.Errorf("attempt %s: %w", evidence.AttemptID, ErrArchiveConflict)
		}
		return nil
	}

	row, err := store.Create(beads.Bead{
		Title:  "Immutable execution attempt evidence",
		Type:   "molecule",
		Status: "open",
		Labels: []string{archiveLabel},
		Metadata: beads.StringMap{
			beadmeta.GCExemptMetadataKey:                        "true",
			beadmeta.AttemptEvidenceArchiveOwnerIDMetadataKey:   evidence.Identity.OwnerBeadID,
			beadmeta.AttemptEvidenceArchiveAttemptIDMetadataKey: evidence.AttemptID,
			beadmeta.AttemptEvidenceArchiveDigestMetadataKey:    digest,
			beadmeta.AttemptEvidenceArchivePayloadMetadataKey:   string(encoded),
		},
	})
	if err != nil {
		// Create may have committed before the provider reported an ambiguous
		// transport failure. Exact lookup makes retry safe and never substitutes
		// another attempt.
		if archived, found, readErr := readArchive(store, evidence.Identity.OwnerBeadID, evidence.AttemptID); readErr == nil && found && samePayload(archived, evidence) {
			return nil
		} else if readErr != nil {
			return errors.Join(fmt.Errorf("creating archive for attempt %s: %w", evidence.AttemptID, err), readErr)
		}
		return fmt.Errorf("creating archive for attempt %s: %w", evidence.AttemptID, err)
	}
	// Type=molecule keeps archive rows out of Ready on every supported beads
	// backend. They deliberately carry no workflow/control markers and no parent
	// edge, so dispatch and wisp GC do not own them. Closing them is followed by
	// a retention exemption in SQLite; they remain open on providers whose create
	// contract cannot set status atomically.
	if row.Status != "open" {
		return nil
	}
	if err := store.Close(row.ID); err != nil {
		return fmt.Errorf("closing non-actionable evidence archive %s: %w", row.ID, err)
	}
	return nil
}

func evidenceFromArchive(row beads.Bead) (Evidence, error) {
	if !IsArchiveRecord(row) {
		return Evidence{}, fmt.Errorf("bead %s is not an attempt evidence archive", row.ID)
	}
	encoded := row.Metadata[beadmeta.AttemptEvidenceArchivePayloadMetadataKey]
	digest := sha256.Sum256([]byte(encoded))
	if got := hex.EncodeToString(digest[:]); got != row.Metadata[beadmeta.AttemptEvidenceArchiveDigestMetadataKey] {
		return Evidence{}, fmt.Errorf("attempt evidence archive %s payload digest mismatch", row.ID)
	}
	evidence, err := decodeEvidence([]byte(encoded))
	if err != nil {
		return Evidence{}, fmt.Errorf("decoding attempt evidence archive %s: %w", row.ID, err)
	}
	if evidence.Identity.OwnerBeadID != row.Metadata[beadmeta.AttemptEvidenceArchiveOwnerIDMetadataKey] ||
		evidence.AttemptID != row.Metadata[beadmeta.AttemptEvidenceArchiveAttemptIDMetadataKey] {
		return Evidence{}, fmt.Errorf("attempt evidence archive %s identity mismatch", row.ID)
	}
	return evidence, nil
}

func decodeEvidence(raw []byte) (Evidence, error) {
	var evidence Evidence
	if err := json.Unmarshal(raw, &evidence); err != nil {
		return Evidence{}, err
	}
	if err := validateEvidence(evidence); err != nil {
		return Evidence{}, err
	}
	return evidence, nil
}

func validateIdentity(identity Identity) error {
	if identity.Kind != KindWorkbench && identity.Kind != KindRetry && identity.Kind != KindRalph {
		return fmt.Errorf("unsupported attempt evidence kind %q", identity.Kind)
	}
	if strings.TrimSpace(identity.OwnerBeadID) == "" || strings.TrimSpace(identity.ExecutionBeadID) == "" {
		return ErrIdentityIncomplete
	}
	if identity.Kind == KindWorkbench && (strings.TrimSpace(identity.SessionID) == "" ||
		strings.TrimSpace(identity.SessionGeneration) == "" || strings.TrimSpace(identity.ClaimGeneration) == "") {
		return fmt.Errorf("workbench attempt needs session ID, session generation, and claim generation: %w", ErrIdentityIncomplete)
	}
	return nil
}

func validateEvidence(e Evidence) error {
	if e.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported attempt evidence schema version %d", e.SchemaVersion)
	}
	if err := validateIdentity(e.Identity); err != nil {
		return err
	}
	wantID, err := AttemptID(e.Identity)
	if err != nil {
		return err
	}
	if e.AttemptID != wantID {
		return fmt.Errorf("attempt evidence ID mismatch: got %q, want %q", e.AttemptID, wantID)
	}
	if e.CapturedAt.IsZero() {
		return errors.New("attempt evidence capture time is missing")
	}
	if strings.TrimSpace(e.StoreRef) == "" {
		return errors.New("attempt evidence store reference is missing")
	}
	if e.Permission.StoreRef != "" && e.Permission.StoreRef != e.StoreRef {
		return errors.New("attempt evidence permission store does not match its storage reference")
	}
	if e.Permission.WorkID != "" && e.Permission.WorkID != e.Identity.OwnerBeadID {
		return errors.New("attempt evidence permission work ID does not match its owner")
	}
	if e.SourceStatus != StatusAvailable && e.SourceStatus != StatusUnavailable && e.SourceStatus != StatusMissing {
		return fmt.Errorf("invalid attempt evidence source status %q", e.SourceStatus)
	}
	if e.BaseStatus != StatusAvailable && e.BaseStatus != StatusUnavailable && e.BaseStatus != StatusMissing {
		return fmt.Errorf("invalid attempt evidence base status %q", e.BaseStatus)
	}
	if e.CandidateStatus != StatusAvailable && e.CandidateStatus != StatusUnavailable && e.CandidateStatus != StatusMissing {
		return fmt.Errorf("invalid attempt evidence candidate status %q", e.CandidateStatus)
	}
	if e.BaseStatus == StatusAvailable && strings.TrimSpace(e.BaseSHA) == "" {
		return errors.New("available attempt base revision is missing")
	}
	if e.CandidateStatus == StatusAvailable && strings.TrimSpace(e.CandidateSHA) == "" {
		return errors.New("available attempt candidate revision is missing")
	}
	if e.BaseStatus != StatusAvailable && strings.TrimSpace(e.BaseReason) == "" {
		return errors.New("unavailable attempt base revision is missing a reason")
	}
	if e.CandidateStatus != StatusAvailable && strings.TrimSpace(e.CandidateReason) == "" {
		return errors.New("unavailable attempt candidate revision is missing a reason")
	}
	if e.WorkingTreeStatus != WorkingTreeClean && e.WorkingTreeStatus != WorkingTreeDirty && e.WorkingTreeStatus != WorkingTreeUnknown {
		return fmt.Errorf("invalid attempt working-tree status %q", e.WorkingTreeStatus)
	}
	if err := validateDiffSnapshot(e.Diff, DiffSourceCandidateCommitDelta, e.BaseStatus == StatusAvailable && e.CandidateStatus == StatusAvailable); err != nil {
		return fmt.Errorf("validating candidate diff: %w", err)
	}
	if err := validateDiffSnapshot(e.WorkspaceDiff, DiffSourceWorkingTree, e.SourceStatus == StatusAvailable); err != nil {
		return fmt.Errorf("validating workspace diff: %w", err)
	}
	for name, facet := range map[string]Facet{
		"policy": e.Policy, "actions": e.Actions,
		"acknowledgements": e.Acknowledgements, "redaction": e.Redaction,
	} {
		if facet.Status != StatusAvailable && facet.Status != StatusUnavailable && facet.Status != StatusMissing && facet.Status != StatusPending {
			return fmt.Errorf("invalid attempt evidence %s status %q", name, facet.Status)
		}
		if facet.Status != StatusAvailable && strings.TrimSpace(facet.Reason) == "" {
			return fmt.Errorf("attempt evidence %s status %q is missing a reason", name, facet.Status)
		}
	}
	return nil
}

func validateDiffSnapshot(diff DiffSnapshot, wantSource string, expectedAvailable bool) error {
	if diff.Status != StatusAvailable && diff.Status != StatusUnavailable && diff.Status != StatusMissing {
		return fmt.Errorf("invalid attempt evidence diff status %q", diff.Status)
	}
	if expectedAvailable && diff.Status != StatusAvailable {
		return errors.New("diff is unavailable despite its source being available")
	}
	if !expectedAvailable && diff.Status == StatusAvailable {
		return errors.New("diff is available although its source is unavailable")
	}
	if diff.Status == StatusAvailable {
		if diff.Source != wantSource {
			return fmt.Errorf("diff source is %q, want %q", diff.Source, wantSource)
		}
		if diff.Encoding != DiffEncoding || diff.SHA256 == "" || len(diff.Payload) == 0 {
			return errors.New("available attempt diff is missing its encoding, digest, or payload")
		}
		if _, _, err := DecodeDiff(diff); err != nil {
			return fmt.Errorf("validating attempt diff: %w", err)
		}
	} else if strings.TrimSpace(diff.Reason) == "" {
		return errors.New("unavailable attempt diff is missing a reason")
	}
	return nil
}

func sameIdentity(a, b Evidence) error {
	if a.AttemptID != b.AttemptID || a.Identity != b.Identity {
		return fmt.Errorf("attempt %s conflicts with the already-sealed execution identity", b.AttemptID)
	}
	return nil
}

func samePayload(a, b Evidence) bool {
	aBytes, aErr := json.Marshal(a)
	bBytes, bErr := json.Marshal(b)
	return aErr == nil && bErr == nil && bytes.Equal(aBytes, bBytes)
}

func compressDiff(bundle diffBundle, source string) (DiffSnapshot, error) {
	encoded, err := json.Marshal(bundle)
	if err != nil {
		return DiffSnapshot{}, fmt.Errorf("marshal diff bundle: %w", err)
	}
	if len(encoded) > maxDiffInputBytes {
		return DiffSnapshot{}, ErrCaptureTooLarge
	}
	var compressed bytes.Buffer
	writer, err := gzip.NewWriterLevel(&compressed, gzip.BestCompression)
	if err != nil {
		return DiffSnapshot{}, fmt.Errorf("create compressed diff writer: %w", err)
	}
	writer.ModTime = time.Time{}
	if _, err := writer.Write(encoded); err != nil {
		return DiffSnapshot{}, fmt.Errorf("compress diff bundle: %w", err)
	}
	if err := writer.Close(); err != nil {
		return DiffSnapshot{}, fmt.Errorf("finish compressed diff bundle: %w", err)
	}
	if compressed.Len() > maxEvidenceBytes/2 {
		return DiffSnapshot{}, fmt.Errorf("compressed diff: %w", ErrCaptureTooLarge)
	}
	digest := sha256.Sum256(compressed.Bytes())
	return DiffSnapshot{
		Status: StatusAvailable, Source: source, Encoding: DiffEncoding,
		SHA256:            hex.EncodeToString(digest[:]),
		UncompressedBytes: int64(len(encoded)),
		Payload:           append([]byte(nil), compressed.Bytes()...),
	}, nil
}

func nowUTC(spec CaptureSpec) time.Time {
	clock := spec.Now
	if clock == nil {
		clock = time.Now
	}
	return clock().UTC()
}

func unavailableFacet(reason string) Facet {
	return Facet{Status: StatusUnavailable, Reason: reason}
}

func availableFacet(refs []string) Facet {
	if len(refs) == 0 {
		return unavailableFacet("no_server_proven_attempt_attribution")
	}
	return Facet{Status: StatusAvailable, Refs: append([]string(nil), refs...)}
}

// MakeUnavailable builds an explicit snapshot for a source known to be absent
// before capture starts. Callers must use an error, not this function, for
// permission, subprocess, transport, or persistence failures.
func MakeUnavailable(spec CaptureSpec, attemptID, reason string) Evidence {
	status := StatusMissing
	if reason == "" {
		reason = "source_absent_before_capture"
	}
	permission := spec.Permission
	if permission.StoreRef == "" {
		permission.StoreRef = strings.TrimSpace(spec.StoreRef)
	}
	if permission.WorkID == "" {
		permission.WorkID = spec.Identity.OwnerBeadID
	}
	return Evidence{
		SchemaVersion:     SchemaVersion,
		AttemptID:         attemptID,
		Identity:          spec.Identity,
		StoreRef:          spec.StoreRef,
		Permission:        permission,
		CapturedAt:        nowUTC(spec),
		Outcome:           spec.Outcome,
		SourceStatus:      status,
		SourceReason:      reason,
		BaseStatus:        StatusUnavailable,
		BaseReason:        "workspace_absent_before_capture",
		CandidateStatus:   StatusUnavailable,
		WorkingTreeStatus: WorkingTreeUnknown,
		CandidateReason:   "workspace_absent_before_capture",
		Diff:              DiffSnapshot{Status: StatusUnavailable, Reason: reason},
		WorkspaceDiff:     DiffSnapshot{Status: StatusUnavailable, Reason: reason},
		Policy:            unavailableFacet("policy_verdict_not_linked_to_attempt"),
		Actions:           availableFacet(spec.RequestRefs),
		Acknowledgements:  unavailableFacet("no_server_proven_attempt_attribution"),
		Redaction:         unavailableFacet("redaction_not_performed"),
	}
}
