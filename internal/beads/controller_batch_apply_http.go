package beads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	controllerBatchApplyTimeout          = 5 * time.Second
	controllerBatchApplyMaxRequest       = 4 << 20
	controllerBatchApplyMaxSuccess       = 1 << 20
	controllerBatchApplyMaxProblem       = 64 << 10
	controllerBatchApplyMaxRecordIDRunes = 255
	controllerBatchApplyMaxActorBytes    = 256
	controllerBatchApplyMaxTextRunes     = 255
	controllerBatchApplyMaxClassBytes    = 64
	controllerBatchApplyMaxReceiptBytes  = 191
	controllerBatchApplyMaxPermitBytes   = 64 << 10
	controllerBatchApplyMaxDepTypeBytes  = 32
	controllerBatchApplyMaxItems         = 100
	controllerBatchApplyPath             = "/v0/beads/issues:batchApply"
	controllerBatchApplyContextPath      = "/v0/beads/context"
)

var (
	// ErrControllerBatchApplyUnavailable reports unavailable client transport or credentials.
	ErrControllerBatchApplyUnavailable = errors.New("controller batchApply HTTP transport unavailable")
	// ErrControllerBatchApplyIdentity reports that the server is not the configured workspace.
	ErrControllerBatchApplyIdentity = errors.New("controller batchApply Beads workspace identity mismatch")
	// ErrControllerBatchApplyProtocol reports a wire response outside the supported contract.
	ErrControllerBatchApplyProtocol = errors.New("controller batchApply HTTP protocol unsupported")
	// ErrControllerBatchApplyOutcomeUnknown reports that a write may have landed but its durable result was not established.
	ErrControllerBatchApplyOutcomeUnknown = errors.New("controller batchApply outcome could not be established")
)

// ControllerBatchApplyHTTPConfig selects one authenticated Beads HTTP workspace.
// This client is an inert contract adapter; no store or command path installs it.
type ControllerBatchApplyHTTPConfig struct {
	Endpoint  string
	ProjectID string
	Database  string
	TokenFile string
}

// ControllerProtectedRecord is the deliberately narrow create payload
// accepted by ControllerProtectedCreateAndLinkRequest. An explicit ID lets
// Beads verify a supplied permit against the exact create and link resources
// before writing. Type, description, labels, and string metadata let a caller
// create the decision-frontier gate records without a follow-up update.
type ControllerProtectedRecord struct {
	ID              string
	Type            string
	Title           string
	Description     string
	Labels          []string
	Metadata        map[string]string
	ProtectionClass string
}

// ControllerDependencyLink is one exact-ID dependency edge in a protected
// batchApply request. Lists preserve caller order.
type ControllerDependencyLink struct {
	SourceID string
	TargetID string
	Type     string
}

// ControllerProtectedCreateAndLinkRequest creates one exact-ID record and
// atomically adds zero to 99 exact-ID dependency links. A protected create
// supplies both ProtectionClass and ProtectedPermit; an ordinary create omits
// both. Every link must include Record.ID as one endpoint. This narrow contract
// has no update, close, generated-ID, or arbitrary metadata operation.
type ControllerProtectedCreateAndLinkRequest struct {
	Actor           string
	Record          ControllerProtectedRecord
	Links           []ControllerDependencyLink
	ReceiptID       string
	ProtectedPermit string
}

// ControllerProtectedLinkRequest adds one exact-ID dependency edge between
// existing records. A protected permit is required for this link-only batch.
type ControllerProtectedLinkRequest struct {
	Actor           string
	Link            ControllerDependencyLink
	ReceiptID       string
	ProtectedPermit string
}

// ControllerProtectedCreateAndLinkResult reports whether Beads returned the
// original durable receipt result for an exact retry.
type ControllerProtectedCreateAndLinkResult struct {
	Replayed bool
}

// ControllerProtectedLinkResult reports whether Beads returned the original
// durable receipt result for an exact retry.
type ControllerProtectedLinkResult struct {
	Replayed bool
}

// ControllerProtectedCreateAndLinkWriter is the narrow batchApply capability
// exposed by this HTTP contract client.
type ControllerProtectedCreateAndLinkWriter interface {
	ApplyProtectedCreateAndLink(context.Context, ControllerProtectedCreateAndLinkRequest) (ControllerProtectedCreateAndLinkResult, error)
}

// ControllerProtectedLinkWriter exposes a protected exact dependency link.
type ControllerProtectedLinkWriter interface {
	ApplyProtectedLink(context.Context, ControllerProtectedLinkRequest) (ControllerProtectedLinkResult, error)
}

// ControllerProtectedBatchApplyWriter exposes the narrow protected batchApply
// operations supported by this contract client.
type ControllerProtectedBatchApplyWriter interface {
	ControllerProtectedCreateAndLinkWriter
	ControllerProtectedLinkWriter
}

// ControllerProtectedBatchApplyTransportValidator is implemented by the
// controller HTTP client so a long-running owner can verify its workspace and
// required protected routes before publishing a store that depends on them.
type ControllerProtectedBatchApplyTransportValidator interface {
	ValidateProtectedBatchApplyTransport(context.Context) error
}

// ControllerBatchApplyProblem preserves only Beads' stable HTTP status and
// machine-readable code. It never retains caller-controlled response detail.
type ControllerBatchApplyProblem struct {
	Status int
	Code   string
}

func (p *ControllerBatchApplyProblem) Error() string {
	if p == nil {
		return "controller batchApply problem: <nil>"
	}
	return fmt.Sprintf("controller batchApply refused with HTTP %d (%s)", p.Status, p.Code)
}

type controllerBatchApplyHTTPClient struct {
	endpoint  string
	projectID string
	database  string
	token     string
	client    *http.Client
}

type controllerBatchApplyServerContext struct {
	APIVersion   string   `json:"api_version"`
	Backend      string   `json:"backend"`
	BdVersion    string   `json:"bd_version"`
	Capabilities []string `json:"capabilities"`
	Database     string   `json:"database"`
	DoltMode     string   `json:"dolt_mode"`
	ProjectID    string   `json:"project_id"`
}

type controllerBatchApplyWireRequest struct {
	Actor           string                         `json:"actor"`
	Items           []controllerBatchApplyWireItem `json:"items"`
	ReceiptID       *string                        `json:"receipt_id,omitempty"`
	ProtectedPermit *string                        `json:"protected_permit,omitempty"`
}

type controllerBatchApplyWireItem struct {
	Kind   string                          `json:"kind"`
	Create *controllerBatchApplyWireCreate `json:"create,omitempty"`
	DepAdd *controllerBatchApplyWireDepAdd `json:"dep_add,omitempty"`
}

type controllerBatchApplyWireCreate struct {
	ID              string             `json:"id"`
	IssueType       string             `json:"issue_type,omitempty"`
	Title           string             `json:"title"`
	Description     string             `json:"description,omitempty"`
	Labels          *[]string          `json:"labels,omitempty"`
	Metadata        *map[string]string `json:"metadata,omitempty"`
	ProtectionClass string             `json:"protection_class,omitempty"`
}

type controllerBatchApplyWireDepAdd struct {
	Source controllerBatchApplyWireRef `json:"source"`
	Target controllerBatchApplyWireRef `json:"target"`
	Type   string                      `json:"type"`
}

type controllerBatchApplyWireRef struct {
	ID string `json:"id"`
}

// NewControllerBatchApplyHTTPClient validates a trusted controller endpoint and
// reads its bearer token from the same protected token-file contract used by the
// existing controller-owned Beads transports. It does not reuse or install the
// private-evidence transport.
func NewControllerBatchApplyHTTPClient(config ControllerBatchApplyHTTPConfig) (ControllerProtectedBatchApplyWriter, error) {
	return newControllerBatchApplyHTTPClient(config)
}

func newControllerBatchApplyHTTPClient(config ControllerBatchApplyHTTPConfig) (*controllerBatchApplyHTTPClient, error) {
	endpoint := strings.TrimSpace(config.Endpoint)
	projectID := strings.TrimSpace(config.ProjectID)
	database := strings.TrimSpace(config.Database)
	tokenFile := strings.TrimSpace(config.TokenFile)
	if endpoint == "" || projectID == "" || database == "" || tokenFile == "" || !validControllerBatchApplyText(projectID, 255) || !validControllerBatchApplyText(database, 255) {
		return nil, fmt.Errorf("%w: endpoint, project, database, and token file are required", ErrControllerBatchApplyUnavailable)
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed == nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, fmt.Errorf("%w: endpoint must be an origin URL", ErrControllerBatchApplyUnavailable)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
	case "http":
		ip := net.ParseIP(parsed.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("%w: cleartext endpoint must use a loopback IP", ErrControllerBatchApplyUnavailable)
		}
	default:
		return nil, fmt.Errorf("%w: endpoint scheme must be https or loopback http", ErrControllerBatchApplyUnavailable)
	}
	if len(endpoint) > 2048 {
		return nil, fmt.Errorf("%w: endpoint is too long", ErrControllerBatchApplyUnavailable)
	}
	// The existing token-file reader enforces a regular, non-symlinked, private
	// file and never includes credential contents in its errors.
	token, err := readPrivateEvidenceToken(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("%w: controller token file is unavailable", ErrControllerBatchApplyUnavailable)
	}
	transport := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          4,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   controllerBatchApplyTimeout,
		ResponseHeaderTimeout: controllerBatchApplyTimeout,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   controllerBatchApplyTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &controllerBatchApplyHTTPClient{
		endpoint: strings.TrimRight(endpoint, "/"), projectID: projectID,
		database: database, token: token, client: client,
	}, nil
}

// ValidateProtectedBatchApplyTransport checks the configured workspace and
// the batchApply, durable-receipt, and protected-mutation capabilities without
// submitting a write.
func (c *controllerBatchApplyHTTPClient) ValidateProtectedBatchApplyTransport(ctx context.Context) error {
	if c == nil || c.client == nil || ctx == nil {
		return ErrControllerBatchApplyUnavailable
	}
	return c.verifyContext(ctx, true, true)
}

// ApplyProtectedCreateAndLink submits one protected create followed by its
// ordered exact-ID links. A durable receipt makes one identical recovery POST
// safe after an ambiguous result because batchApply itself is the receipt read.
func (c *controllerBatchApplyHTTPClient) ApplyProtectedCreateAndLink(ctx context.Context, request ControllerProtectedCreateAndLinkRequest) (ControllerProtectedCreateAndLinkResult, error) {
	if c == nil || c.client == nil || ctx == nil {
		return ControllerProtectedCreateAndLinkResult{}, ErrControllerBatchApplyUnavailable
	}
	wire, err := planControllerProtectedCreateAndLink(request)
	if err != nil {
		return ControllerProtectedCreateAndLinkResult{}, err
	}
	var result ControllerProtectedCreateAndLinkResult
	err = c.apply(ctx, wire, request.ReceiptID, request.ProtectedPermit != "", func(response []byte) error {
		var decodeErr error
		result, decodeErr = decodeControllerBatchApplyResponse(response, request)
		return decodeErr
	})
	return result, err
}

// ApplyProtectedLink submits one protected exact-ID dependency edge between
// records that already exist. A durable receipt makes one identical recovery
// POST safe after an ambiguous result.
func (c *controllerBatchApplyHTTPClient) ApplyProtectedLink(ctx context.Context, request ControllerProtectedLinkRequest) (ControllerProtectedLinkResult, error) {
	if c == nil || c.client == nil || ctx == nil {
		return ControllerProtectedLinkResult{}, ErrControllerBatchApplyUnavailable
	}
	wire, err := planControllerProtectedLink(request)
	if err != nil {
		return ControllerProtectedLinkResult{}, err
	}
	var result ControllerProtectedLinkResult
	err = c.apply(ctx, wire, request.ReceiptID, true, func(response []byte) error {
		var decodeErr error
		result, decodeErr = decodeControllerBatchApplyLinkResponse(response, request)
		return decodeErr
	})
	return result, err
}

func (c *controllerBatchApplyHTTPClient) apply(ctx context.Context, wire controllerBatchApplyWireRequest, receiptID string, requireProtected bool, decode func([]byte) error) error {
	if c == nil || c.client == nil || ctx == nil {
		return ErrControllerBatchApplyUnavailable
	}
	body, err := json.Marshal(wire)
	if err != nil || len(body) > controllerBatchApplyMaxRequest {
		return controllerBatchApplyProtocolError("request is too large or could not be encoded")
	}

	for attempt := 0; attempt < 2; attempt++ {
		if err := c.verifyContext(ctx, receiptID != "", requireProtected); err != nil {
			if attempt == 0 {
				return err
			}
			return controllerBatchApplyUnknownOutcome(err)
		}
		response, status, requestErr := c.request(ctx, http.MethodPost, controllerBatchApplyPath, body)
		if attempt == 1 {
			if requestErr == nil && status == http.StatusOK {
				if decodeErr := decode(response); decodeErr == nil {
					return nil
				} else {
					return controllerBatchApplyUnknownOutcome(decodeErr)
				}
			}
			var recoveryErr error
			switch {
			case status >= 300 && status < 400:
				recoveryErr = controllerBatchApplyProtocolError("redirect refused")
			case status >= 400 && status < 500 && requestErr != nil:
				recoveryErr = controllerBatchApplyProtocolError("Beads error response could not be read")
			case status >= 400 && status < 500:
				recoveryErr = controllerBatchApplyStatusError(status, response)
			case requestErr != nil:
				recoveryErr = requestErr
			default:
				recoveryErr = controllerBatchApplyProtocolError("recovery response did not establish the batchApply result")
			}
			return controllerBatchApplyUnknownOutcome(recoveryErr)
		}
		var ambiguousErr error
		if status >= 300 && status < 400 {
			ambiguousErr = controllerBatchApplyProtocolError("redirect refused")
		}
		if status >= 400 && status < 500 {
			if requestErr != nil {
				return controllerBatchApplyProtocolError("Beads error response could not be read")
			}
			return controllerBatchApplyStatusError(status, response)
		}
		if ambiguousErr == nil && requestErr == nil && status == http.StatusOK {
			if decodeErr := decode(response); decodeErr == nil {
				return nil
			} else {
				ambiguousErr = decodeErr
			}
		} else if ambiguousErr == nil {
			ambiguousErr = requestErr
		}
		if receiptID == "" {
			return controllerBatchApplyUnknownOutcome(ambiguousErr)
		}
	}
	return ErrControllerBatchApplyOutcomeUnknown
}

func planControllerProtectedCreateAndLink(request ControllerProtectedCreateAndLinkRequest) (controllerBatchApplyWireRequest, error) {
	if !validControllerBatchApplyText(request.Actor, controllerBatchApplyMaxActorBytes) || len(request.Actor) > controllerBatchApplyMaxActorBytes || strings.TrimSpace(request.Actor) != request.Actor || controllerBatchApplyHasControl(request.Actor) {
		return controllerBatchApplyWireRequest{}, controllerBatchApplyProtocolError("actor is invalid")
	}
	if !validControllerBatchApplyText(request.Record.ID, controllerBatchApplyMaxRecordIDRunes) || strings.TrimSpace(request.Record.ID) != request.Record.ID || controllerBatchApplyHasControl(request.Record.ID) {
		return controllerBatchApplyWireRequest{}, controllerBatchApplyProtocolError("record ID is invalid")
	}
	if request.Record.Type != "" && (!validControllerBatchApplyText(request.Record.Type, controllerBatchApplyMaxTextRunes) || strings.TrimSpace(request.Record.Type) != request.Record.Type || controllerBatchApplyHasControl(request.Record.Type)) {
		return controllerBatchApplyWireRequest{}, controllerBatchApplyProtocolError("record type is invalid")
	}
	if !validControllerBatchApplyText(request.Record.Title, controllerBatchApplyMaxTextRunes) || strings.TrimSpace(request.Record.Title) == "" || controllerBatchApplyHasControl(request.Record.Title) {
		return controllerBatchApplyWireRequest{}, controllerBatchApplyProtocolError("record title is invalid")
	}
	if request.Record.Description != "" &&
		(!validControllerBatchApplyText(request.Record.Description, controllerBatchApplyMaxRequest) ||
			len(request.Record.Description) > controllerBatchApplyMaxRequest || controllerBatchApplyHasControl(request.Record.Description)) {
		return controllerBatchApplyWireRequest{}, controllerBatchApplyProtocolError("record description is invalid")
	}
	if len(request.Record.Labels) > controllerBatchApplyMaxItems {
		return controllerBatchApplyWireRequest{}, controllerBatchApplyProtocolError("record labels exceed the supported bound")
	}
	seenLabels := make(map[string]struct{}, len(request.Record.Labels))
	for _, label := range request.Record.Labels {
		if !validControllerBatchApplyText(label, controllerBatchApplyMaxTextRunes) || strings.TrimSpace(label) != label || controllerBatchApplyHasControl(label) {
			return controllerBatchApplyWireRequest{}, controllerBatchApplyProtocolError("record label is invalid")
		}
		if _, duplicate := seenLabels[label]; duplicate {
			return controllerBatchApplyWireRequest{}, controllerBatchApplyProtocolError("record labels contain a duplicate")
		}
		seenLabels[label] = struct{}{}
	}
	if len(request.Record.Metadata) > controllerBatchApplyMaxItems {
		return controllerBatchApplyWireRequest{}, controllerBatchApplyProtocolError("record metadata exceeds the supported bound")
	}
	metadataBytes := 0
	for key, value := range request.Record.Metadata {
		if !validControllerBatchApplyText(key, controllerBatchApplyMaxTextRunes) || strings.TrimSpace(key) != key || controllerBatchApplyHasControl(key) {
			return controllerBatchApplyWireRequest{}, controllerBatchApplyProtocolError("record metadata key is invalid")
		}
		if !utf8.ValidString(value) || len(value) > controllerBatchApplyMaxRequest || controllerBatchApplyHasControl(value) {
			return controllerBatchApplyWireRequest{}, controllerBatchApplyProtocolError("record metadata value is invalid")
		}
		entryBytes := len(key) + len(value)
		if entryBytes > controllerBatchApplyMaxRequest || metadataBytes > controllerBatchApplyMaxRequest-entryBytes {
			return controllerBatchApplyWireRequest{}, controllerBatchApplyProtocolError("record metadata exceeds the request size limit")
		}
		metadataBytes += entryBytes
	}
	if request.Record.ProtectionClass != "" && (!validControllerBatchApplyText(request.Record.ProtectionClass, controllerBatchApplyMaxTextRunes) || len(request.Record.ProtectionClass) > controllerBatchApplyMaxClassBytes || strings.TrimSpace(request.Record.ProtectionClass) != request.Record.ProtectionClass || controllerBatchApplyHasControl(request.Record.ProtectionClass)) {
		return controllerBatchApplyWireRequest{}, controllerBatchApplyProtocolError("protection class is invalid")
	}
	if (request.Record.ProtectionClass == "") != (request.ProtectedPermit == "") {
		return controllerBatchApplyWireRequest{}, controllerBatchApplyProtocolError("protected create requires both a protection class and permit")
	}
	if len(request.Links)+1 > controllerBatchApplyMaxItems {
		return controllerBatchApplyWireRequest{}, controllerBatchApplyProtocolError("link count is outside the supported batch bound")
	}
	if request.ReceiptID != "" && (!utf8.ValidString(request.ReceiptID) || len(request.ReceiptID) > controllerBatchApplyMaxReceiptBytes || strings.ContainsRune(request.ReceiptID, '\x00')) {
		return controllerBatchApplyWireRequest{}, controllerBatchApplyProtocolError("receipt ID is invalid")
	}
	if request.ProtectedPermit != "" && (!utf8.ValidString(request.ProtectedPermit) || len(request.ProtectedPermit) > controllerBatchApplyMaxPermitBytes || strings.TrimSpace(request.ProtectedPermit) != request.ProtectedPermit || controllerBatchApplyHasControl(request.ProtectedPermit)) {
		return controllerBatchApplyWireRequest{}, controllerBatchApplyProtocolError("protected permit is invalid")
	}

	wire := controllerBatchApplyWireRequest{Actor: request.Actor}
	wire.Items = make([]controllerBatchApplyWireItem, 0, len(request.Links)+1)
	create := &controllerBatchApplyWireCreate{
		ID: request.Record.ID, IssueType: request.Record.Type, Title: request.Record.Title,
		Description: request.Record.Description, ProtectionClass: request.Record.ProtectionClass,
	}
	if request.Record.Labels != nil {
		labels := append(make([]string, 0, len(request.Record.Labels)), request.Record.Labels...)
		create.Labels = &labels
	}
	if request.Record.Metadata != nil {
		metadata := make(map[string]string, len(request.Record.Metadata))
		for key, value := range request.Record.Metadata {
			metadata[key] = value
		}
		create.Metadata = &metadata
	}
	wire.Items = append(wire.Items, controllerBatchApplyWireItem{
		Kind:   "create",
		Create: create,
	})
	for _, link := range request.Links {
		if !validControllerDependencyLink(link) {
			return controllerBatchApplyWireRequest{}, controllerBatchApplyProtocolError("link is invalid")
		}
		if link.SourceID == link.TargetID || (link.SourceID != request.Record.ID && link.TargetID != request.Record.ID) {
			return controllerBatchApplyWireRequest{}, controllerBatchApplyProtocolError("each link must name the new record and a distinct exact-ID endpoint")
		}
		wire.Items = append(wire.Items, controllerBatchApplyWireItem{
			Kind: "dep_add",
			DepAdd: &controllerBatchApplyWireDepAdd{
				Source: controllerBatchApplyWireRef{ID: link.SourceID},
				Target: controllerBatchApplyWireRef{ID: link.TargetID},
				Type:   link.Type,
			},
		})
	}
	if request.ReceiptID != "" {
		value := request.ReceiptID
		wire.ReceiptID = &value
	}
	if request.ProtectedPermit != "" {
		value := request.ProtectedPermit
		wire.ProtectedPermit = &value
	}
	return wire, nil
}

func planControllerProtectedLink(request ControllerProtectedLinkRequest) (controllerBatchApplyWireRequest, error) {
	if !validControllerBatchApplyText(request.Actor, controllerBatchApplyMaxActorBytes) || len(request.Actor) > controllerBatchApplyMaxActorBytes || strings.TrimSpace(request.Actor) != request.Actor || controllerBatchApplyHasControl(request.Actor) {
		return controllerBatchApplyWireRequest{}, controllerBatchApplyProtocolError("actor is invalid")
	}
	if !validControllerDependencyLink(request.Link) {
		return controllerBatchApplyWireRequest{}, controllerBatchApplyProtocolError("link is invalid")
	}
	if request.Link.SourceID == request.Link.TargetID {
		return controllerBatchApplyWireRequest{}, controllerBatchApplyProtocolError("link endpoints must be distinct exact IDs")
	}
	if request.ReceiptID != "" && (!utf8.ValidString(request.ReceiptID) || len(request.ReceiptID) > controllerBatchApplyMaxReceiptBytes || strings.ContainsRune(request.ReceiptID, '\x00')) {
		return controllerBatchApplyWireRequest{}, controllerBatchApplyProtocolError("receipt ID is invalid")
	}
	if request.ProtectedPermit == "" || !utf8.ValidString(request.ProtectedPermit) || len(request.ProtectedPermit) > controllerBatchApplyMaxPermitBytes || strings.TrimSpace(request.ProtectedPermit) != request.ProtectedPermit || controllerBatchApplyHasControl(request.ProtectedPermit) {
		return controllerBatchApplyWireRequest{}, controllerBatchApplyProtocolError("protected link requires a valid permit")
	}
	wire := controllerBatchApplyWireRequest{
		Actor: request.Actor,
		Items: []controllerBatchApplyWireItem{{
			Kind: "dep_add",
			DepAdd: &controllerBatchApplyWireDepAdd{
				Source: controllerBatchApplyWireRef{ID: request.Link.SourceID},
				Target: controllerBatchApplyWireRef{ID: request.Link.TargetID},
				Type:   request.Link.Type,
			},
		}},
	}
	if request.ReceiptID != "" {
		value := request.ReceiptID
		wire.ReceiptID = &value
	}
	permit := request.ProtectedPermit
	wire.ProtectedPermit = &permit
	return wire, nil
}

func validControllerDependencyLink(link ControllerDependencyLink) bool {
	return validControllerBatchApplyText(link.SourceID, controllerBatchApplyMaxRecordIDRunes) && strings.TrimSpace(link.SourceID) == link.SourceID && !controllerBatchApplyHasControl(link.SourceID) &&
		validControllerBatchApplyText(link.TargetID, controllerBatchApplyMaxRecordIDRunes) && strings.TrimSpace(link.TargetID) == link.TargetID && !controllerBatchApplyHasControl(link.TargetID) &&
		validControllerBatchApplyText(link.Type, controllerBatchApplyMaxTextRunes) && len(link.Type) <= controllerBatchApplyMaxDepTypeBytes && !controllerBatchApplyHasControl(link.Type)
}

func (c *controllerBatchApplyHTTPClient) verifyContext(ctx context.Context, requireReceipt, requireProtected bool) error {
	body, status, err := c.request(ctx, http.MethodGet, controllerBatchApplyContextPath, nil)
	if err != nil {
		return fmt.Errorf("%w: context handshake failed", ErrControllerBatchApplyUnavailable)
	}
	if status != http.StatusOK {
		return controllerBatchApplyProtocolError("context handshake returned an unsupported status")
	}
	var response controllerBatchApplyServerContext
	if decodeControllerBatchApplyJSON(body, &response) != nil || response.APIVersion != "v0" || response.BdVersion == "" || response.ProjectID == "" || response.Database == "" || response.Capabilities == nil {
		return controllerBatchApplyProtocolError("context handshake is malformed")
	}
	if response.ProjectID != c.projectID || response.Database != c.database || response.Backend != "dolt" || response.DoltMode != "server" {
		return ErrControllerBatchApplyIdentity
	}
	required := []string{"issues.batchApply", "project.enforce"}
	if requireReceipt {
		required = append(required, "issues.batchApplyReceipt")
	}
	if requireProtected {
		required = append(required, "issues.protectedMutation")
	}
	for _, capability := range required {
		if !controllerBatchApplyContains(response.Capabilities, capability) {
			return controllerBatchApplyProtocolError("Beads server lacks a required capability")
		}
	}
	return nil
}

func (c *controllerBatchApplyHTTPClient) request(ctx context.Context, method, path string, body []byte) ([]byte, int, error) {
	if len(body) > controllerBatchApplyMaxRequest {
		return nil, 0, controllerBatchApplyProtocolError("request exceeds size limit")
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, controllerBatchApplyProtocolError("request could not be formed")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Bd-Project-Id", c.projectID)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.Do(req)
	if err != nil {
		if response != nil {
			status := response.StatusCode
			_ = response.Body.Close()
			if status >= 300 && status < 400 {
				return nil, status, controllerBatchApplyProtocolError("redirect refused")
			}
			return nil, status, fmt.Errorf("%w: %s request failed", ErrControllerBatchApplyUnavailable, method)
		}
		return nil, 0, fmt.Errorf("%w: %s request failed", ErrControllerBatchApplyUnavailable, method)
	}
	responseCap := controllerBatchApplyMaxSuccess
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		responseCap = controllerBatchApplyMaxProblem
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, int64(responseCap)+1))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		return nil, response.StatusCode, fmt.Errorf("%w: %s response could not be read", ErrControllerBatchApplyUnavailable, method)
	}
	if len(data) > responseCap {
		return nil, response.StatusCode, controllerBatchApplyProtocolError("response exceeds size limit")
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return nil, response.StatusCode, controllerBatchApplyProtocolError("redirect refused")
	}
	return data, response.StatusCode, nil
}

func decodeControllerBatchApplyResponse(raw []byte, request ControllerProtectedCreateAndLinkRequest) (ControllerProtectedCreateAndLinkResult, error) {
	members, err := controllerBatchApplyObject(raw)
	if err != nil || !controllerBatchApplyOnlyMembers(members, "keys", "items", "replayed") {
		return ControllerProtectedCreateAndLinkResult{}, controllerBatchApplyProtocolError("batchApply response has unsupported members")
	}
	var keys map[string]string
	if rawKeys, ok := members["keys"]; !ok || json.Unmarshal(rawKeys, &keys) != nil || keys == nil || len(keys) != 0 {
		return ControllerProtectedCreateAndLinkResult{}, controllerBatchApplyProtocolError("batchApply response keys are inconsistent")
	}
	var replayed *bool
	if rawReplayed, ok := members["replayed"]; !ok || json.Unmarshal(rawReplayed, &replayed) != nil || replayed == nil {
		return ControllerProtectedCreateAndLinkResult{}, controllerBatchApplyProtocolError("batchApply response replay flag is malformed")
	}
	if *replayed && request.ReceiptID == "" {
		return ControllerProtectedCreateAndLinkResult{}, controllerBatchApplyProtocolError("batchApply response replay flag is inconsistent")
	}
	var items []json.RawMessage
	if rawItems, ok := members["items"]; !ok || json.Unmarshal(rawItems, &items) != nil || items == nil || len(items) != len(request.Links)+1 {
		return ControllerProtectedCreateAndLinkResult{}, controllerBatchApplyProtocolError("batchApply response item count is inconsistent")
	}
	if err := validateControllerBatchApplyResultItem(items[0], "create", request.Record.ID, "", true); err != nil {
		return ControllerProtectedCreateAndLinkResult{}, err
	}
	for i, link := range request.Links {
		if err := validateControllerBatchApplyResultItem(items[i+1], "dep_add", link.SourceID, link.TargetID, false); err != nil {
			return ControllerProtectedCreateAndLinkResult{}, err
		}
	}
	return ControllerProtectedCreateAndLinkResult{Replayed: *replayed}, nil
}

func decodeControllerBatchApplyLinkResponse(raw []byte, request ControllerProtectedLinkRequest) (ControllerProtectedLinkResult, error) {
	members, err := controllerBatchApplyObject(raw)
	if err != nil || !controllerBatchApplyOnlyMembers(members, "keys", "items", "replayed") {
		return ControllerProtectedLinkResult{}, controllerBatchApplyProtocolError("batchApply response has unsupported members")
	}
	var keys map[string]string
	if rawKeys, ok := members["keys"]; !ok || json.Unmarshal(rawKeys, &keys) != nil || keys == nil || len(keys) != 0 {
		return ControllerProtectedLinkResult{}, controllerBatchApplyProtocolError("batchApply response keys are inconsistent")
	}
	var replayed *bool
	if rawReplayed, ok := members["replayed"]; !ok || json.Unmarshal(rawReplayed, &replayed) != nil || replayed == nil {
		return ControllerProtectedLinkResult{}, controllerBatchApplyProtocolError("batchApply response replay flag is malformed")
	}
	if *replayed && request.ReceiptID == "" {
		return ControllerProtectedLinkResult{}, controllerBatchApplyProtocolError("batchApply response replay flag is inconsistent")
	}
	var items []json.RawMessage
	if rawItems, ok := members["items"]; !ok || json.Unmarshal(rawItems, &items) != nil || items == nil || len(items) != 1 {
		return ControllerProtectedLinkResult{}, controllerBatchApplyProtocolError("batchApply response item count is inconsistent")
	}
	if err := validateControllerBatchApplyResultItem(items[0], "dep_add", request.Link.SourceID, request.Link.TargetID, false); err != nil {
		return ControllerProtectedLinkResult{}, err
	}
	return ControllerProtectedLinkResult{Replayed: *replayed}, nil
}

func validateControllerBatchApplyResultItem(raw []byte, wantKind, wantIssueID, wantDependsOnID string, create bool) error {
	members, err := controllerBatchApplyObject(raw)
	allowed := []string{"kind", "issue_id", "changed", "revision"}
	if !create {
		allowed = append(allowed, "depends_on_id")
	}
	if err != nil || !controllerBatchApplyOnlyMembers(members, allowed...) {
		return controllerBatchApplyProtocolError("batchApply response item has unsupported members")
	}
	var kind, issueID, revision string
	var changed *bool
	if json.Unmarshal(members["kind"], &kind) != nil || json.Unmarshal(members["issue_id"], &issueID) != nil ||
		json.Unmarshal(members["changed"], &changed) != nil || changed == nil || json.Unmarshal(members["revision"], &revision) != nil {
		return controllerBatchApplyProtocolError("batchApply response item is malformed")
	}
	if kind != wantKind || issueID != wantIssueID || (create && !*changed) || !validControllerBatchApplyRevision(revision) {
		return controllerBatchApplyProtocolError("batchApply response item does not match the request")
	}
	if create {
		if _, present := members["depends_on_id"]; present {
			return controllerBatchApplyProtocolError("create result unexpectedly carries a dependency target")
		}
		return nil
	}
	var dependsOnID string
	if rawID, ok := members["depends_on_id"]; !ok || json.Unmarshal(rawID, &dependsOnID) != nil || dependsOnID != wantDependsOnID {
		return controllerBatchApplyProtocolError("dependency result does not match the request")
	}
	return nil
}

func validControllerBatchApplyRevision(value string) bool {
	parsed, err := strconv.ParseInt(value, 10, 64)
	return err == nil && strconv.FormatInt(parsed, 10) == value
}

func controllerBatchApplyStatusError(status int, body []byte) error {
	var problem struct {
		Code string `json:"code"`
	}
	if decodeControllerBatchApplyJSON(body, &problem) != nil || !validControllerBatchApplyProblemCode(problem.Code) {
		return controllerBatchApplyProtocolError("Beads returned a malformed problem response")
	}
	return &ControllerBatchApplyProblem{Status: status, Code: problem.Code}
}

func controllerBatchApplyObject(raw []byte) (map[string]json.RawMessage, error) {
	var members map[string]json.RawMessage
	if err := decodeControllerBatchApplyJSON(raw, &members); err != nil || members == nil {
		return nil, errors.New("invalid object")
	}
	return members, nil
}

func decodeControllerBatchApplyJSON(raw []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

func controllerBatchApplyOnlyMembers(members map[string]json.RawMessage, allowed ...string) bool {
	if len(members) != len(allowed) {
		return false
	}
	for name := range members {
		found := false
		for _, candidate := range allowed {
			if name == candidate {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func validControllerBatchApplyText(value string, maxRunes int) bool {
	return value != "" && utf8.ValidString(value) && utf8.RuneCountInString(value) <= maxRunes
}

func controllerBatchApplyHasControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) || r == '\u2028' || r == '\u2029' {
			return true
		}
	}
	return false
}

func controllerBatchApplyContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func validControllerBatchApplyProblemCode(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, r := range value {
		if r != '_' && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func controllerBatchApplyProtocolError(reason string) error {
	return fmt.Errorf("%w: %s", ErrControllerBatchApplyProtocol, reason)
}

func controllerBatchApplyUnknownOutcome(cause error) error {
	if cause == nil {
		return ErrControllerBatchApplyOutcomeUnknown
	}
	return errors.Join(ErrControllerBatchApplyOutcomeUnknown, cause)
}
