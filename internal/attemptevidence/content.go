package attemptevidence

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// PayloadRead is the verified result of resolving one content-addressed
// attempt artifact. Missing and corrupt payloads are states, not empty success.
type PayloadRead struct {
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	SHA256  string `json:"sha256"`
	Bytes   int64  `json:"bytes"`
	Content []byte `json:"content"`
}

// ReadPayload resolves an artifact reference and verifies its address and
// recorded size. Store failures are returned as errors; missing or corrupt
// content is returned as an explicit state.
func ReadPayload(store beads.Store, artifact PayloadReference) (PayloadRead, error) {
	if store == nil {
		return PayloadRead{}, errors.New("reading attempt evidence payload: bead store is unavailable")
	}
	if artifact.Bytes < 0 {
		return PayloadRead{}, errors.New("reading attempt evidence payload: expected byte count cannot be negative")
	}
	if artifact.Status != "" && artifact.Status != StatusAvailable {
		return PayloadRead{
			Status: artifact.Status, Reason: artifact.Reason,
			SHA256: artifact.SHA256, Bytes: artifact.Bytes,
		}, nil
	}
	content, status, reason, err := readPayloadBytes(store, artifact.SHA256, artifact.Bytes)
	if err != nil {
		return PayloadRead{}, err
	}
	return PayloadRead{
		Status: status, Reason: reason, SHA256: artifact.SHA256,
		Bytes: artifact.Bytes, Content: content,
	}, nil
}

func compactEvidencePayloads(store beads.Store, evidence Evidence) (Evidence, error) {
	compact := evidence
	compact.Artifacts = append([]PayloadArtifact(nil), evidence.Artifacts...)
	if err := compactDiffSnapshot(store, &compact.Diff, "candidate diff"); err != nil {
		return Evidence{}, err
	}
	if err := compactDiffSnapshot(store, &compact.WorkspaceDiff, "workspace diff"); err != nil {
		return Evidence{}, err
	}
	for i := range compact.Artifacts {
		artifact := &compact.Artifacts[i]
		if artifact.Content != nil {
			if len(artifact.Content) > maxPayloadBytes {
				return Evidence{}, fmt.Errorf("attempt artifact %q: %w", artifact.Name, ErrCaptureTooLarge)
			}
			digest := payloadDigest(artifact.Content)
			if artifact.SHA256 != "" && artifact.SHA256 != digest {
				return Evidence{}, fmt.Errorf("attempt artifact %q digest does not match its content", artifact.Name)
			}
			if artifact.Bytes != 0 && artifact.Bytes != int64(len(artifact.Content)) {
				return Evidence{}, fmt.Errorf("attempt artifact %q size does not match its content", artifact.Name)
			}
			if err := storePayloadBytes(store, digest, artifact.Content); err != nil {
				return Evidence{}, fmt.Errorf("storing attempt artifact %q: %w", artifact.Name, err)
			}
			artifact.SHA256 = digest
			artifact.Bytes = int64(len(artifact.Content))
			artifact.Content = nil
			continue
		}
		if artifact.Status != StatusAvailable {
			continue
		}
		if _, status, reason, err := readPayloadBytes(store, artifact.SHA256, artifact.Bytes); err != nil {
			return Evidence{}, err
		} else if status != StatusAvailable {
			return Evidence{}, fmt.Errorf("sealing attempt artifact %q: %s: %s", artifact.Name, status, reason)
		}
	}
	return compact, nil
}

func compactDiffSnapshot(store beads.Store, diff *DiffSnapshot, label string) error {
	if diff.Status != StatusAvailable {
		return nil
	}
	if len(diff.Payload) != 0 {
		if _, _, err := DecodeDiff(*diff); err != nil {
			return fmt.Errorf("validating %s payload before seal: %w", label, err)
		}
		if err := storePayloadBytes(store, diff.SHA256, diff.Payload); err != nil {
			return fmt.Errorf("storing %s payload %s: %w", label, diff.SHA256, err)
		}
		diff.Payload = nil
		return nil
	}
	if _, status, reason, err := readPayloadBytes(store, diff.SHA256, -1); err != nil {
		return err
	} else if status != StatusAvailable {
		return fmt.Errorf("sealing %s reference %s: %s: %s", label, diff.SHA256, status, reason)
	}
	return nil
}

func hydrateEvidencePayloads(store beads.Store, evidence Evidence) (Evidence, error) {
	if err := hydrateDiffSnapshot(store, &evidence.Diff); err != nil {
		return Evidence{}, err
	}
	if err := hydrateDiffSnapshot(store, &evidence.WorkspaceDiff); err != nil {
		return Evidence{}, err
	}
	return evidence, nil
}

func hydrateCaptureEvidencePayloads(store beads.Store, evidence Evidence) (Evidence, error) {
	hydrated, err := hydrateEvidencePayloads(store, evidence)
	if err != nil {
		return Evidence{}, err
	}
	for _, pair := range []struct {
		name   string
		before DiffSnapshot
		after  DiffSnapshot
	}{
		{name: "candidate diff", before: evidence.Diff, after: hydrated.Diff},
		{name: "workspace diff", before: evidence.WorkspaceDiff, after: hydrated.WorkspaceDiff},
	} {
		if pair.before.Status == StatusAvailable && pair.after.Status != StatusAvailable {
			return Evidence{}, fmt.Errorf("reusing sealed attempt with unavailable %s payload: %s: %s", pair.name, pair.after.Status, pair.after.Reason)
		}
	}
	for _, before := range evidence.Artifacts {
		if before.Status != StatusAvailable {
			continue
		}
		_, status, reason, err := readPayloadBytes(store, before.SHA256, before.Bytes)
		if err != nil {
			return Evidence{}, err
		}
		if status != StatusAvailable {
			return Evidence{}, fmt.Errorf("reusing sealed attempt with unavailable artifact %q: %s: %s", before.Name, status, reason)
		}
	}
	return hydrated, nil
}

func hydrateDiffSnapshot(store beads.Store, diff *DiffSnapshot) error {
	if diff.Status != StatusAvailable || len(diff.Payload) != 0 {
		return nil
	}
	content, status, reason, err := readPayloadBytes(store, diff.SHA256, -1)
	if err != nil {
		return err
	}
	if status != StatusAvailable {
		diff.Status = status
		diff.Reason = reason
		diff.Payload = nil
	} else {
		diff.Payload = content
	}
	return nil
}

func storePayloadBytes(store beads.Store, digest string, content []byte) error {
	if len(content) > maxStoredPayloadBytes {
		return ErrCaptureTooLarge
	}
	if !isPayloadDigest(digest) || digest != payloadDigest(content) {
		return errors.New("attempt evidence payload digest does not match its content")
	}
	backend, supported := beads.PrivatePayloadValueBackend(store)
	if !supported {
		return ErrPrivatePayloadTransportUnsupported
	}
	rowID := beads.AttemptEvidencePayloadID(digest)
	if rowID == "" {
		return errors.New("attempt evidence payload digest is not canonical")
	}
	// Scan every row carrying this digest before accepting an existing value.
	// Historical stores may contain non-canonical duplicates; an exact-ID fast
	// path would let a valid canonical row mask a corrupt duplicate even though
	// reads correctly reject the ambiguous payload set.
	if existing, status, reason, err := readPayloadBytes(backend, digest, int64(len(content))); err != nil {
		return err
	} else if status == StatusAvailable {
		if !equalBytes(existing, content) {
			return fmt.Errorf("content-addressed payload %s conflicts with supplied bytes", digest)
		}
		return nil
	} else if status == StatusCorrupt {
		return fmt.Errorf("content-addressed payload %s is corrupt: %s", digest, reason)
	}

	_, err := beads.CreatePrivatePayloadValue(backend, beads.Bead{
		ID:     rowID,
		Title:  "Immutable attempt evidence content",
		Type:   "molecule",
		Status: "closed",
		Metadata: beads.StringMap{
			beadmeta.GCExemptMetadataKey:                     "true",
			beadmeta.AttemptEvidencePayloadDigestMetadataKey: digest,
			beadmeta.AttemptEvidencePayloadDataMetadataKey:   base64.StdEncoding.EncodeToString(content),
		},
	})
	if err != nil {
		// Concurrent writers use the same deterministic ID. One insert wins;
		// any duplicate-ID or ambiguous-commit result is resolved by exact ID
		// readback and digest verification.
		row, readErr := backend.Get(rowID)
		if readErr == nil && validatePayloadRow(row, digest, int64(len(content)), content) == nil {
			return nil
		}
		if readErr != nil && !errors.Is(readErr, beads.ErrNotFound) {
			return errors.Join(fmt.Errorf("creating content-addressed payload %s: %w", digest, err), readErr)
		}
		return fmt.Errorf("creating content-addressed payload %s: %w (exact readback failed: %w)", digest, err, readErr)
	}
	row, err := backend.Get(rowID)
	if err != nil {
		return fmt.Errorf("verifying content-addressed payload %s: %w", digest, err)
	}
	if err := validatePayloadRow(row, digest, int64(len(content)), content); err != nil {
		return fmt.Errorf("verifying content-addressed payload %s: %w", digest, err)
	}
	if _, status, reason, err := readPayloadBytes(backend, digest, int64(len(content))); err != nil {
		return err
	} else if status != StatusAvailable {
		return fmt.Errorf("verifying content-addressed payload %s: %s: %s", digest, status, reason)
	}
	return nil
}

func validatePayloadRow(row beads.Bead, digest string, expectedBytes int64, expectedContent []byte) error {
	if !beads.IsAttemptEvidencePayload(row) || row.Metadata[beadmeta.AttemptEvidencePayloadDigestMetadataKey] != digest {
		return errors.New("row has malformed payload metadata")
	}
	content, err := base64.StdEncoding.DecodeString(row.Metadata[beadmeta.AttemptEvidencePayloadDataMetadataKey])
	if err != nil {
		return errors.New("payload data is not valid base64")
	}
	if payloadDigest(content) != digest || int64(len(content)) != expectedBytes || !equalBytes(content, expectedContent) {
		return errors.New("payload content, digest, or size does not match reference")
	}
	return nil
}

func readPayloadBytes(store beads.Store, digest string, expectedBytes int64) ([]byte, string, string, error) {
	backend, supported := beads.PrivatePayloadValueBackend(store)
	if !supported {
		return nil, "", "", ErrPrivatePayloadTransportUnsupported
	}
	store = backend
	if !isPayloadDigest(digest) {
		return nil, StatusCorrupt, "payload_reference_invalid", nil
	}
	rows, err := store.ListByMetadata(map[string]string{beadmeta.AttemptEvidencePayloadDigestMetadataKey: digest}, 0, beads.IncludeClosed)
	if err != nil {
		return nil, "", "", fmt.Errorf("reading content-addressed payload %s: %w", digest, err)
	}
	if len(rows) == 0 {
		return nil, StatusUnavailable, "payload_missing", nil
	}
	var verified []byte
	haveVerified := false
	corruptReason := "payload_malformed_duplicate"
	for _, row := range rows {
		if !beads.IsAttemptEvidencePayload(row) {
			return nil, StatusCorrupt, "payload_malformed_duplicate", nil
		}
		content, decodeErr := base64.StdEncoding.DecodeString(row.Metadata[beadmeta.AttemptEvidencePayloadDataMetadataKey])
		if decodeErr != nil {
			return nil, StatusCorrupt, "payload_encoding_mismatch", nil
		}
		if payloadDigest(content) != digest {
			return nil, StatusCorrupt, "payload_digest_mismatch", nil
		}
		if expectedBytes >= 0 && int64(len(content)) != expectedBytes {
			return nil, StatusCorrupt, "payload_size_mismatch", nil
		}
		if haveVerified && !equalBytes(verified, content) {
			return nil, StatusCorrupt, "payload_conflicting_duplicates", nil
		}
		verified = content
		haveVerified = true
	}
	if haveVerified {
		content := make([]byte, len(verified))
		copy(content, verified)
		return content, StatusAvailable, "", nil
	}
	return nil, StatusCorrupt, corruptReason, nil
}

func payloadDigest(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

func isPayloadDigest(digest string) bool {
	if len(digest) != sha256.Size*2 || strings.ToLower(digest) != digest {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
