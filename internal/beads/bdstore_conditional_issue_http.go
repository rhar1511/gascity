package beads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const conditionalIssueUpdateActor = "gascity-controller"

// Conditional issue-update errors distinguish unavailable transport, wrong
// workspace identity, malformed protocol, and a missing server capability.
var (
	ErrConditionalIssueUpdateHTTPUnavailable = errors.New("conditional issue-update HTTP transport unavailable")
	ErrConditionalIssueUpdateHTTPIdentity    = errors.New("conditional issue-update HTTP workspace identity mismatch")
	ErrConditionalIssueUpdateHTTPProtocol    = errors.New("conditional issue-update HTTP protocol unsupported")
	ErrConditionalIssueUpdateHTTPCapability  = errors.New("conditional issue-update HTTP capability unavailable")
)

type conditionalIssueUpdateRequest struct {
	Actor           string                      `json:"actor"`
	ExpectedVersion string                      `json:"expected_version"`
	Patch           conditionalIssueUpdatePatch `json:"patch"`
}

type conditionalIssueUpdatePatch struct {
	Title        *string                         `json:"title,omitempty"`
	Status       *string                         `json:"status,omitempty"`
	IssueType    *string                         `json:"issue_type,omitempty"`
	Priority     *int                            `json:"priority,omitempty"`
	Description  *string                         `json:"description,omitempty"`
	ParentID     *string                         `json:"parent_id,omitempty"`
	Assignee     *string                         `json:"assignee,omitempty"`
	AddLabels    []string                        `json:"add_labels,omitempty"`
	RemoveLabels []string                        `json:"remove_labels,omitempty"`
	Metadata     *conditionalIssueUpdateMetadata `json:"metadata,omitempty"`
}

type conditionalIssueUpdateMetadata struct {
	Set map[string]string `json:"set,omitempty"`
}

type conditionalIssueUpdateResponseWire struct {
	Issue    json.RawMessage `json:"issue"`
	Changed  *bool           `json:"changed"`
	Revision json.RawMessage `json:"revision"`
}

// updateIssueIfMatchHTTP preserves the caller-owned aggregate revision over
// the authenticated HTTP route. Callers have already validated the current
// lifecycle snapshot. No PATCH outcome may fall back to a CLI mutation.
func (s *BdStore) updateIssueIfMatchHTTP(id string, expectedRevision int64, opts UpdateOpts) error {
	if err := rejectPrivateEvidenceArgvMetadata("bd conditional update", opts.Metadata); err != nil {
		return err
	}
	if isEmptyUpdateOpts(opts) {
		return ErrEmptyConditionalUpdate
	}
	if !validConditionalIssueUpdateID(id) {
		return fmt.Errorf("%w: issue ID is invalid", ErrConditionalIssueUpdateHTTPProtocol)
	}
	if s == nil || s.privateEvidenceHTTP == nil || s.privateEvidenceHTTPInitErr != nil {
		return ErrConditionalWriteUnsupported
	}

	ctx, cancel := context.WithTimeout(context.Background(), privateEvidenceHTTPTimeout)
	defer cancel()
	client := s.privateEvidenceHTTP
	if err := client.verifyConditionalIssueUpdateContext(ctx); err != nil {
		return err
	}
	return client.updateIssueIfMatch(ctx, id, expectedRevision, opts)
}

func validConditionalIssueUpdateID(id string) bool {
	return id != "" && strings.TrimSpace(id) == id && len(id) <= controllerTransitionMaxIssueID && !hasControllerTransitionControl(id)
}

func (c *privateEvidenceHTTPClient) verifyConditionalIssueUpdateContext(ctx context.Context) error {
	body, status, err := c.requestWithResponseCaps(ctx, http.MethodGet, "/v0/beads/context", nil, 1<<20, 1<<20)
	response, decodeErr := decodePrivateEvidenceContext(body, status, err)
	if decodeErr != nil {
		if errors.Is(decodeErr, ErrPrivateEvidenceHTTPUnavailable) {
			return fmt.Errorf("%w: Beads context handshake failed", ErrConditionalIssueUpdateHTTPUnavailable)
		}
		return fmt.Errorf("%w: Beads context handshake is malformed", ErrConditionalIssueUpdateHTTPProtocol)
	}
	if response.ProjectID != c.projectID || response.Database != c.database || response.Backend != "dolt" || (response.DoltMode != "server" && response.DoltMode != "proxied-server") {
		return fmt.Errorf("%w: configured scope %q does not match the served Beads workspace", ErrConditionalIssueUpdateHTTPIdentity, c.scopeRef)
	}
	// The stable v1.3.1 HTTP lifecycle API fences updates with expected_version.
	// Gate on its advertised operation and workspace enforcement capabilities.
	for _, required := range []string{"issues.update", "project.enforce"} {
		if !containsString(response.Capabilities, required) {
			return fmt.Errorf("%w: %w: Beads server lacks required capability %q",
				ErrConditionalWriteUnsupported, ErrConditionalIssueUpdateHTTPCapability, required)
		}
	}
	return nil
}

func (c *privateEvidenceHTTPClient) updateIssueIfMatch(ctx context.Context, id string, expectedRevision int64, opts UpdateOpts) error {
	patch := conditionalIssueUpdatePatch{
		Title: opts.Title, Status: opts.Status, IssueType: opts.Type, Priority: opts.Priority,
		Description: opts.Description, ParentID: opts.ParentID, Assignee: opts.Assignee,
		AddLabels: append([]string(nil), opts.Labels...), RemoveLabels: append([]string(nil), opts.RemoveLabels...),
	}
	if len(opts.Metadata) > 0 {
		patch.Metadata = &conditionalIssueUpdateMetadata{Set: opts.Metadata}
	}
	body, err := json.Marshal(conditionalIssueUpdateRequest{
		Actor: conditionalIssueUpdateActor, ExpectedVersion: strconv.FormatInt(expectedRevision, 10), Patch: patch,
	})
	if err != nil || len(body) > privateEvidenceHTTPMaxRequestBody {
		return fmt.Errorf("%w: update request cannot be encoded", ErrConditionalIssueUpdateHTTPProtocol)
	}
	path := "/v0/beads/issues/" + url.PathEscape(id)
	responseBody, status, err := c.requestWithResponseCaps(ctx, http.MethodPatch, path, body,
		controllerTransitionMaxSuccessBody, controllerTransitionMaxProblemBody)
	if err != nil {
		if errors.Is(err, ErrPrivateEvidenceHTTPProtocol) {
			return fmt.Errorf("%w: update response is invalid", ErrConditionalIssueUpdateHTTPProtocol)
		}
		return fmt.Errorf("%w: PATCH request failed", ErrConditionalIssueUpdateHTTPUnavailable)
	}
	if status == http.StatusOK {
		if err := validateConditionalIssueUpdateResponse(responseBody, id, expectedRevision); err != nil {
			return err
		}
		return nil
	}
	return conditionalIssueUpdateHTTPError(status, responseBody, id, expectedRevision)
}

func validateConditionalIssueUpdateResponse(body []byte, id string, expectedRevision int64) error {
	var response conditionalIssueUpdateResponseWire
	if err := decodePrivateEvidenceJSON(body, &response); err != nil || len(response.Issue) == 0 || response.Changed == nil || len(response.Revision) == 0 {
		return fmt.Errorf("%w: update response is incomplete", ErrConditionalIssueUpdateHTTPProtocol)
	}
	var issue struct {
		ID        string  `json:"id"`
		Title     *string `json:"title"`
		Priority  *int    `json:"priority"`
		CreatedAt *string `json:"created_at"`
		UpdatedAt *string `json:"updated_at"`
	}
	if err := json.Unmarshal(response.Issue, &issue); err != nil || issue.ID != id || issue.Title == nil || issue.Priority == nil || issue.CreatedAt == nil || issue.UpdatedAt == nil {
		return fmt.Errorf("%w: update response issue is malformed or has the wrong identity", ErrConditionalIssueUpdateHTTPProtocol)
	}
	if _, err := time.Parse(time.RFC3339Nano, *issue.CreatedAt); err != nil {
		return fmt.Errorf("%w: update response created_at is malformed", ErrConditionalIssueUpdateHTTPProtocol)
	}
	if _, err := time.Parse(time.RFC3339Nano, *issue.UpdatedAt); err != nil {
		return fmt.Errorf("%w: update response updated_at is malformed", ErrConditionalIssueUpdateHTTPProtocol)
	}
	var revision string
	if err := json.Unmarshal(response.Revision, &revision); err != nil {
		return fmt.Errorf("%w: update response revision is not a string", ErrConditionalIssueUpdateHTTPProtocol)
	}
	parsedRevision, err := strconv.ParseInt(revision, 10, 64)
	if err != nil || strconv.FormatInt(parsedRevision, 10) != revision {
		return fmt.Errorf("%w: update response revision is not a canonical signed decimal", ErrConditionalIssueUpdateHTTPProtocol)
	}
	if *response.Changed && parsedRevision == expectedRevision {
		return fmt.Errorf("%w: changed update response preserved the expected revision", ErrConditionalIssueUpdateHTTPProtocol)
	}
	if !*response.Changed && parsedRevision != expectedRevision {
		return fmt.Errorf("%w: unchanged update response did not preserve the expected revision", ErrConditionalIssueUpdateHTTPProtocol)
	}
	return nil
}

func conditionalIssueUpdateHTTPError(status int, body []byte, id string, expectedRevision int64) error {
	switch status {
	case http.StatusNotFound:
		return fmt.Errorf("conditional HTTP issue update %q: %w", id, ErrNotFound)
	case http.StatusNotImplemented, http.StatusMethodNotAllowed:
		return fmt.Errorf("%w: issues.update route is unavailable", ErrConditionalWriteUnsupported)
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusServiceUnavailable:
		return fmt.Errorf("%w: issues.update route refused the configured request", ErrConditionalIssueUpdateHTTPUnavailable)
	case http.StatusConflict:
		var problem struct {
			Code  string `json:"code"`
			Param string `json:"param"`
		}
		if err := decodePrivateEvidenceJSON(body, &problem); err != nil || problem.Code == "" {
			return fmt.Errorf("%w: conflict response is malformed", ErrConditionalIssueUpdateHTTPProtocol)
		}
		if problem.Code == "precondition_failed" && problem.Param == "expected_version" {
			return &PreconditionFailedError{
				ID: id, Expected: expectedRevision,
				Raw: fmt.Sprintf(`{"code":%q,"param":%q}`, problem.Code, problem.Param),
			}
		}
		return &GateRefusalError{
			ID: id, Verb: "update", Code: problem.Code,
			Raw: fmt.Sprintf(`{"code":%q,"param":%q}`, problem.Code, problem.Param),
		}
	default:
		return fmt.Errorf("%w: issues.update returned HTTP %d", ErrConditionalIssueUpdateHTTPProtocol, status)
	}
}
