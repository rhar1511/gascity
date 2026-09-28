package beads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const sourceSnapshotIssuePath = "/v0/beads/issues/"

var (
	// ErrDecisionFrontierSourceSnapshotUnavailable reports that the configured
	// private-evidence transport cannot serve authoritative source snapshots.
	ErrDecisionFrontierSourceSnapshotUnavailable = errors.New("decision-frontier source snapshot HTTP transport unavailable")
	// ErrDecisionFrontierSourceSnapshotProtocol reports an unsupported or
	// malformed response from the source-snapshot endpoint.
	ErrDecisionFrontierSourceSnapshotProtocol = errors.New("decision-frontier source snapshot HTTP protocol unsupported")
)

var (
	_ DecisionFrontierSourceReader               = (*BdStore)(nil)
	_ DecisionFrontierSourceReaderHandleProvider = (*BdStore)(nil)
)

// DecisionFrontierSourceReaderHandle exposes the bounded source-snapshot role
// only when this store has an initialized private-evidence transport and its
// configured revision-transition scope is enabled.
func (s *BdStore) DecisionFrontierSourceReaderHandle() (DecisionFrontierSourceReader, bool) {
	if s == nil || s.privateEvidenceHTTP == nil || s.privateEvidenceHTTPInitErr != nil || !s.privateEvidenceHTTP.revisionTransitions {
		return nil, false
	}
	return s, true
}

// DecisionFrontierSourceSnapshot reads one issue and its exact outgoing edge
// set through the configured body-only HTTP transport.
func (s *BdStore) DecisionFrontierSourceSnapshot(id string) (Bead, error) {
	if s == nil || s.privateEvidenceHTTP == nil || s.privateEvidenceHTTPInitErr != nil || !s.privateEvidenceHTTP.revisionTransitions {
		return Bead{}, ErrDecisionFrontierSourceSnapshotUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*privateEvidenceHTTPTimeout)
	defer cancel()
	return s.privateEvidenceHTTP.sourceSnapshot(ctx, id)
}

func (c *privateEvidenceHTTPClient) sourceSnapshot(ctx context.Context, id string) (Bead, error) {
	if !c.revisionTransitions {
		return Bead{}, ErrDecisionFrontierSourceSnapshotUnavailable
	}
	if err := validateControllerTransitionText(id, controllerTransitionMaxIssueID); err != nil || hasControllerTransitionControl(id) {
		return Bead{}, sourceSnapshotProtocolError("issue ID is invalid")
	}
	if err := c.verifySourceSnapshotContext(ctx); err != nil {
		return Bead{}, err
	}
	path := sourceSnapshotIssuePath + url.PathEscape(id) + "/sourceSnapshot"
	body, status, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return Bead{}, fmt.Errorf("%w: source snapshot request failed: %w", ErrDecisionFrontierSourceSnapshotUnavailable, err)
	}
	if status == http.StatusNotFound {
		return Bead{}, ErrNotFound
	}
	if status != http.StatusOK {
		return Bead{}, sourceSnapshotProtocolError("source snapshot route returned HTTP %d", status)
	}
	return decodeDecisionFrontierSourceSnapshot(body, id)
}

func (c *privateEvidenceHTTPClient) verifySourceSnapshotContext(ctx context.Context) error {
	response, err := c.readContext(ctx)
	if err != nil {
		if errors.Is(err, ErrPrivateEvidenceHTTPUnavailable) {
			return fmt.Errorf("%w: source snapshot context request failed: %w", ErrDecisionFrontierSourceSnapshotUnavailable, err)
		}
		return sourceSnapshotProtocolError("source snapshot context response is unsupported")
	}
	if err := c.verifyContextIdentity(response); err != nil {
		return fmt.Errorf("%w: configured workspace identity mismatch: %w", ErrDecisionFrontierSourceSnapshotProtocol, err)
	}
	for _, required := range []string{"issues.sourceSnapshot", "project.enforce"} {
		if !containsString(response.Capabilities, required) {
			return sourceSnapshotProtocolError("Beads server lacks required capability %q", required)
		}
	}
	return nil
}

func decodeDecisionFrontierSourceSnapshot(body []byte, requestedID string) (Bead, error) {
	var object map[string]json.RawMessage
	if err := decodePrivateEvidenceJSON(body, &object); err != nil || object == nil {
		return Bead{}, sourceSnapshotProtocolError("source snapshot is not a JSON object")
	}
	issue, ok := object["issue"]
	if !ok || len(bytes.TrimSpace(issue)) == 0 || bytes.Equal(bytes.TrimSpace(issue), []byte("null")) {
		return Bead{}, sourceSnapshotProtocolError("source snapshot is missing its issue")
	}
	edgesRaw, ok := object["outgoing_dependencies"]
	trimmedEdges := bytes.TrimSpace(edgesRaw)
	if !ok || len(trimmedEdges) == 0 || bytes.Equal(trimmedEdges, []byte("null")) || trimmedEdges[0] != '[' {
		return Bead{}, sourceSnapshotProtocolError("source snapshot is missing its outgoing dependency array")
	}

	var issueIdentity struct {
		ID       string          `json:"id"`
		Revision json.RawMessage `json:"revision"`
	}
	if err := decodePrivateEvidenceJSON(issue, &issueIdentity); err != nil || issueIdentity.ID != requestedID {
		return Bead{}, sourceSnapshotProtocolError("source snapshot issue ID does not match the requested ID")
	}
	revision, err := canonicalSourceSnapshotRevision(issueIdentity.Revision)
	if err != nil {
		return Bead{}, sourceSnapshotProtocolError("source snapshot revision is not a nonzero canonical signed decimal string")
	}

	var bdRow bdIssue
	if err := decodePrivateEvidenceJSON(issue, &bdRow); err != nil {
		return Bead{}, sourceSnapshotProtocolError("source snapshot issue is malformed")
	}
	if bdRow.ID != requestedID {
		return Bead{}, sourceSnapshotProtocolError("source snapshot issue ID does not match the requested ID")
	}
	bead := bdRow.toBead()
	bead.Revision = revision

	var rawEdges []json.RawMessage
	if err := json.Unmarshal(trimmedEdges, &rawEdges); err != nil || rawEdges == nil {
		return Bead{}, sourceSnapshotProtocolError("source snapshot outgoing dependency list is malformed")
	}
	bead.Dependencies = make([]Dep, 0, len(rawEdges))
	for index, rawEdge := range rawEdges {
		var edge bdIssueDep
		if err := decodePrivateEvidenceJSON(rawEdge, &edge); err != nil || edge.IssueID != requestedID ||
			strings.TrimSpace(edge.DependsOnID) == "" || strings.TrimSpace(edge.Type) == "" {
			return Bead{}, sourceSnapshotProtocolError("source snapshot outgoing dependency %d is invalid", index)
		}
		bead.Dependencies = append(bead.Dependencies, Dep{
			IssueID:     edge.IssueID,
			DependsOnID: edge.DependsOnID,
			Type:        edge.Type,
		})
		if bead.ParentID == "" && edge.Type == "parent-child" {
			bead.ParentID = edge.DependsOnID
		}
	}
	return bead, nil
}

func canonicalSourceSnapshotRevision(raw json.RawMessage) (int64, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) < 2 || trimmed[0] != '"' {
		return 0, errors.New("revision must be a decimal string")
	}
	var token string
	if err := json.Unmarshal(trimmed, &token); err != nil {
		return 0, err
	}
	revision, err := strconv.ParseInt(token, 10, 64)
	if err != nil || revision == 0 || strconv.FormatInt(revision, 10) != token {
		return 0, errors.New("revision token is not canonical")
	}
	return revision, nil
}

func sourceSnapshotProtocolError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrDecisionFrontierSourceSnapshotProtocol, fmt.Sprintf(format, args...))
}
