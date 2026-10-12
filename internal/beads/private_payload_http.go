package beads

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// CreatePrivatePayloadValue uses stock Beads' transactional create-only HTTP
// operation. The deterministic ID is never replaced with a minted ID, and a
// duplicate or ambiguous response remains an error for exact caller readback.
// The transport's 4 MiB request limit also bounds the encoded payload.
func (s *BdStore) CreatePrivatePayloadValue(b Bead) (Bead, error) {
	if err := validatePrivatePayloadCreate(b); err != nil {
		return Bead{}, err
	}
	if s == nil || s.privateEvidenceHTTP == nil || s.privateEvidenceHTTPInitErr != nil {
		return Bead{}, ErrPrivatePayloadCreateUnsupported
	}
	if b.Type != "molecule" || b.Status != "closed" || b.Ephemeral || b.NoHistory {
		return Bead{}, fmt.Errorf("%w: payload must be a closed durable molecule", ErrPrivateEvidenceHTTPProtocol)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*privateEvidenceHTTPTimeout)
	defer cancel()
	client := s.privateEvidenceHTTP
	if err := client.verifyContext(ctx); err != nil {
		return Bead{}, err
	}
	request := struct {
		Actor       string            `json:"actor"`
		ID          string            `json:"id"`
		ForcePrefix bool              `json:"force_id_prefix"`
		Title       string            `json:"title"`
		IssueType   string            `json:"issue_type"`
		Status      string            `json:"status"`
		Metadata    map[string]string `json:"metadata"`
	}{
		Actor: privateEvidenceActor, ID: b.ID, ForcePrefix: true, Title: b.Title,
		IssueType: b.Type, Status: b.Status, Metadata: map[string]string(b.Metadata),
	}
	body, err := json.Marshal(request)
	if err != nil {
		return Bead{}, fmt.Errorf("%w: payload create could not be encoded", ErrPrivateEvidenceHTTPProtocol)
	}
	response, status, err := client.request(ctx, http.MethodPost, "/v0/beads/issues", body)
	if err != nil {
		return Bead{}, err
	}
	if status != http.StatusOK {
		return Bead{}, fmt.Errorf("%w: payload create returned HTTP %d", ErrPrivateEvidenceHTTPProtocol, status)
	}
	var issue bdIssue
	if err := decodePrivateEvidenceJSON(response, &issue); err != nil || issue.ID != b.ID || issue.Status != "closed" || issue.IssueType != "molecule" || issue.Ephemeral || issue.NoHistory {
		return Bead{}, fmt.Errorf("%w: payload create returned an invalid identity or storage shape", ErrPrivateEvidenceHTTPProtocol)
	}
	created := issue.toBead()
	if err := validatePrivatePayloadCreate(created); err != nil {
		return Bead{}, fmt.Errorf("%w: payload create returned invalid content", ErrPrivateEvidenceHTTPProtocol)
	}
	for key, value := range b.Metadata {
		if created.Metadata[key] != value {
			return Bead{}, fmt.Errorf("%w: payload create did not preserve metadata", ErrPrivateEvidenceHTTPProtocol)
		}
	}
	return created, nil
}

// ListPrivatePayloadValues reads every row for a digest, including historical
// duplicates. HTTP-backed stores keep payload bytes out of the bd subprocess
// and its telemetry; a failed HTTP read never falls back to the CLI.
func ListPrivatePayloadValues(store Store, digest string) ([]Bead, error) {
	if AttemptEvidencePayloadID(digest) == "" {
		return nil, fmt.Errorf("%w: payload digest is invalid", ErrPrivateEvidenceHTTPProtocol)
	}
	backend, ok := PrivatePayloadValueBackend(store)
	if !ok {
		return nil, ErrPrivateEvidenceTransportUnsupported
	}
	filters := map[string]string{beadmeta.AttemptEvidencePayloadDigestMetadataKey: digest}
	if bd, ok := backend.(*BdStore); ok {
		return bd.privateEvidenceHTTP.listPrivateEvidenceRows(context.Background(), filters)
	}
	return backend.ListByMetadata(filters, 0, IncludeClosed)
}
