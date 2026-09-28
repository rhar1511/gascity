package beads

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

const (
	privateEvidenceHTTPTimeout         = 5 * time.Second
	privateEvidenceHTTPMaxRequestBody  = 4 << 20
	privateEvidenceHTTPMaxResponseBody = 32 << 20
	controllerTransitionMaxSuccessBody = 4 << 20
	controllerTransitionMaxProblemBody = 1 << 20
	privateEvidenceTokenMaxBytes       = 4 << 10
	privateEvidenceActor               = "gascity-controller"
	privateEvidenceRequiredScope       = "private attempt evidence"
	controllerTransitionMaxReceiptID   = 191
	controllerTransitionMaxIssueID     = 255
	controllerTransitionMaxScope       = 255
	controllerTransitionMaxKind        = 64
	controllerTransitionMaxActor       = 255
	controllerTransitionMaxKey         = 255
	controllerTransitionMaxRequests    = 8
)

var (
	// ErrPrivateEvidenceHTTPUnavailable reports that no usable private-evidence
	// HTTP transport is configured or reachable.
	ErrPrivateEvidenceHTTPUnavailable = errors.New("private evidence HTTP transport unavailable")
	// ErrPrivateEvidenceHTTPIdentity reports a mismatch with the configured
	// Beads workspace identity.
	ErrPrivateEvidenceHTTPIdentity = errors.New("private evidence HTTP workspace identity mismatch")
	// ErrPrivateEvidenceHTTPProtocol reports an unsupported or malformed
	// private-evidence HTTP response.
	ErrPrivateEvidenceHTTPProtocol = errors.New("private evidence HTTP protocol unsupported")
	// ErrControllerMetadataTransitionUnavailable reports a missing or unusable
	// controller transition transport.
	ErrControllerMetadataTransitionUnavailable = errors.New("controller metadata transition HTTP transport unavailable")
	// ErrControllerMetadataTransitionProtocol reports an unsupported or
	// malformed response from the controller transition API.
	ErrControllerMetadataTransitionProtocol = errors.New("controller metadata transition HTTP protocol unsupported")
)

const redactedPrivateEvidenceDiagnostic = "[private attempt-evidence output redacted]"

func redactPrivateEvidenceDiagnostic(value string) string {
	if strings.Contains(value, beadmeta.AttemptEvidenceArchivePayloadMetadataKey) ||
		strings.Contains(value, beadmeta.AttemptEvidenceIndexPrefix) {
		return redactedPrivateEvidenceDiagnostic
	}
	return value
}

// PrivateEvidenceHTTPConfig is trusted controller configuration for one
// canonical Beads store scope. A zero value leaves the HTTP transport disabled.
// TokenFile contains only a filesystem path; the token itself is loaded into
// this process and is never passed through a bd argument or child environment.
type PrivateEvidenceHTTPConfig struct {
	Endpoint            string
	ProjectID           string
	Database            string
	ScopeRef            string
	TokenFile           string
	RevisionTransitions bool
}

// PrivateEvidenceMetadataCASWriter is deliberately separate from
// MetadataCASWriter and ConditionalWriter. The Beads HTTP route fences exactly
// one metadata key; it cannot satisfy the whole-row revision contract.
type PrivateEvidenceMetadataCASWriter interface {
	CompareAndSetPrivateEvidenceMetadataKey(id, key, expected, next string) (bool, error)
	ReadPrivateEvidenceMetadataKey(id, key string) (value string, present bool, err error)
}

// PrivateEvidenceMetadataCASWriterHandleProvider preserves wrapper-owned write
// behavior. ProxiedStore brackets it with its generation check and CachingStore
// evicts its snapshot after the write.
type PrivateEvidenceMetadataCASWriterHandleProvider interface {
	PrivateEvidenceMetadataCASWriterHandle() (PrivateEvidenceMetadataCASWriter, bool)
}

// PrivateEvidenceMetadataCASWriterFor resolves only the dedicated private
// evidence capability. Unlike generic metadata CAS resolution, it never follows
// ConditionalWritesResolveTarget: doing so would bypass wrappers such as
// ProxiedStore that own mutation bracketing.
func PrivateEvidenceMetadataCASWriterFor(store Store) (PrivateEvidenceMetadataCASWriter, bool) {
	if store == nil {
		return nil, false
	}
	if provider, ok := store.(PrivateEvidenceMetadataCASWriterHandleProvider); ok {
		return provider.PrivateEvidenceMetadataCASWriterHandle()
	}
	writer, ok := store.(PrivateEvidenceMetadataCASWriter)
	return writer, ok
}

// PrivateEvidencePayloadTransportReady verifies the configured service scope
// and all routes needed to read, create, and CAS immutable evidence. It is
// implemented only by stores with a body-only private transport.
type PrivateEvidencePayloadTransportReady interface {
	PrivateEvidencePayloadTransportReady() bool
}

// PrivateEvidenceArchiveReader reads owner-index and archive values without
// routing their payloads through generic CLI output.
type PrivateEvidenceArchiveReader interface {
	ListPrivateEvidenceOwnerIndexes(ownerID string) (map[string]string, error)
	ListPrivateEvidenceArchives(ownerID, attemptID string) ([]Bead, error)
}

// PrivateEvidenceArchiveReaderHandleProvider exposes a body-only reader when
// the wrapped store has a configured private-evidence transport.
type PrivateEvidenceArchiveReaderHandleProvider interface {
	PrivateEvidenceArchiveReaderHandle() (PrivateEvidenceArchiveReader, bool)
}

var (
	_ PrivateEvidenceMetadataCASWriter                 = (*BdStore)(nil)
	_ PrivateEvidenceMetadataCASWriterHandleProvider   = (*BdStore)(nil)
	_ PrivateEvidencePayloadTransportReady             = (*BdStore)(nil)
	_ PrivateEvidenceArchiveReader                     = (*BdStore)(nil)
	_ PrivateEvidenceArchiveReaderHandleProvider       = (*BdStore)(nil)
	_ ControllerMetadataTransitionWriter               = (*BdStore)(nil)
	_ ControllerMetadataTransitionWriterHandleProvider = (*BdStore)(nil)
)

type privateEvidenceHTTPClient struct {
	endpoint            string
	projectID           string
	database            string
	scopeRef            string
	token               string
	revisionTransitions bool
	client              *http.Client
}

// WithBdStorePrivateEvidenceHTTP installs the explicitly configured private
// evidence transport on a BdStore. Configuration and credentials are checked
// here; the server's identity and route capabilities are verified lazily before
// the store advertises the private-payload capability and again before writes.
func WithBdStorePrivateEvidenceHTTP(config PrivateEvidenceHTTPConfig) BdStoreOption {
	client, err := newPrivateEvidenceHTTPClient(config)
	return func(store *BdStore) {
		store.privateEvidenceHTTP = client
		store.privateEvidenceHTTPInitErr = err
		store.privateEvidenceHTTPConfigured = true
	}
}

// PrivateEvidenceMetadataCASWriterHandle exposes this store's configured
// body-based owner-index compare-and-set capability.
func (s *BdStore) PrivateEvidenceMetadataCASWriterHandle() (PrivateEvidenceMetadataCASWriter, bool) {
	if s == nil || s.privateEvidenceHTTP == nil || s.privateEvidenceHTTPInitErr != nil {
		return nil, false
	}
	return s, true
}

// PrivateEvidencePayloadTransportReady reports whether this BdStore has a
// configured body transport whose authenticated context matches the trusted
// workspace and advertises all required issue routes.
func (s *BdStore) PrivateEvidencePayloadTransportReady() bool {
	if s == nil || s.privateEvidenceHTTP == nil || s.privateEvidenceHTTPInitErr != nil {
		return false
	}
	ctx := context.Background()
	return s.privateEvidenceHTTP.verifyContext(ctx) == nil && s.privateEvidenceHTTP.verifyListRoute(ctx) == nil
}

// PrivateEvidenceArchiveReaderHandle exposes this store's configured
// body-based owner-index and archive reader.
func (s *BdStore) PrivateEvidenceArchiveReaderHandle() (PrivateEvidenceArchiveReader, bool) {
	if s == nil || s.privateEvidenceHTTP == nil || s.privateEvidenceHTTPInitErr != nil {
		return nil, false
	}
	return s, true
}

// ControllerMetadataTransitionWriterHandle exposes the Q43 transition route
// only for a scope whose trusted transport explicitly enables it.
func (s *BdStore) ControllerMetadataTransitionWriterHandle() (ControllerMetadataTransitionWriter, bool) {
	if s == nil || s.privateEvidenceHTTP == nil || s.privateEvidenceHTTPInitErr != nil || !s.privateEvidenceHTTP.revisionTransitions {
		return nil, false
	}
	return s, true
}

// TransitionMetadata applies one exact Q43 revision-fenced metadata
// transition through the bounded controller HTTP transport.
func (s *BdStore) TransitionMetadata(issueID string, request ControllerMetadataTransitionRequest) (ControllerMetadataTransitionResult, error) {
	if s == nil || s.privateEvidenceHTTP == nil || s.privateEvidenceHTTPInitErr != nil || !s.privateEvidenceHTTP.revisionTransitions {
		return ControllerMetadataTransitionResult{}, ErrControllerMetadataTransitionUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(controllerTransitionMaxRequests)*privateEvidenceHTTPTimeout)
	defer cancel()
	return s.privateEvidenceHTTP.transitionMetadata(ctx, issueID, request)
}

// PrivateEvidenceArchiveReaderFor resolves the body-only reader through store
// wrappers without using a generic read or conditional-write target resolver.
// The reader is deliberately not cached because its rows contain private
// payloads.
func PrivateEvidenceArchiveReaderFor(store Store) (PrivateEvidenceArchiveReader, bool) {
	if store == nil {
		return nil, false
	}
	if provider, ok := store.(PrivateEvidenceArchiveReaderHandleProvider); ok {
		return provider.PrivateEvidenceArchiveReaderHandle()
	}
	reader, ok := store.(PrivateEvidenceArchiveReader)
	return reader, ok
}

// ListPrivateEvidenceOwnerIndexes reads all owner-index entries through the
// body-only path when configured. Existing private-safe stores may use their
// normal typed read path; unsupported stores receive an error instead of a
// generic CLI fallback.
func ListPrivateEvidenceOwnerIndexes(store Store, ownerID string) (map[string]string, bool, error) {
	reader, ok := PrivateEvidenceArchiveReaderFor(store)
	if !ok {
		if !SupportsPrivatePayloadValues(store) {
			return nil, false, ErrPrivateEvidenceTransportUnsupported
		}
		return nil, false, nil
	}
	indexes, err := reader.ListPrivateEvidenceOwnerIndexes(ownerID)
	return indexes, true, err
}

// ListPrivateEvidenceArchives reads payload-bearing rows through the
// authenticated HTTP body path, never through bd stdout or telemetry.
func (s *BdStore) ListPrivateEvidenceArchives(ownerID, attemptID string) ([]Bead, error) {
	if s == nil || s.privateEvidenceHTTP == nil || s.privateEvidenceHTTPInitErr != nil {
		return nil, ErrPrivateEvidenceHTTPUnavailable
	}
	return s.privateEvidenceHTTP.listEvidenceArchives(context.Background(), ownerID, attemptID)
}

// ListPrivateEvidenceOwnerIndexes reads all private owner-index values through
// the authenticated HTTP response body.
func (s *BdStore) ListPrivateEvidenceOwnerIndexes(ownerID string) (map[string]string, error) {
	if s == nil || s.privateEvidenceHTTP == nil || s.privateEvidenceHTTPInitErr != nil {
		return nil, ErrPrivateEvidenceHTTPUnavailable
	}
	return s.privateEvidenceHTTP.listOwnerIndexes(context.Background(), ownerID)
}

// CompareAndSetPrivateEvidenceMetadataKey stores private owner-index bytes over
// the authenticated JSON API. The bd CLI path is intentionally never used.
func (s *BdStore) CompareAndSetPrivateEvidenceMetadataKey(id, key, expected, next string) (bool, error) {
	if s == nil || s.privateEvidenceHTTP == nil || s.privateEvidenceHTTPInitErr != nil {
		return false, ErrPrivateEvidenceHTTPUnavailable
	}
	return s.privateEvidenceHTTP.compareAndSetMetadata(context.Background(), id, key, expected, next)
}

// ReadPrivateEvidenceMetadataKey reads one opaque owner-index value without
// passing private bytes through bd stdout or telemetry.
func (s *BdStore) ReadPrivateEvidenceMetadataKey(id, key string) (string, bool, error) {
	if s == nil || s.privateEvidenceHTTP == nil || s.privateEvidenceHTTPInitErr != nil {
		return "", false, ErrPrivateEvidenceHTTPUnavailable
	}
	if !strings.HasPrefix(key, beadmeta.AttemptEvidenceIndexPrefix) || len(key) == len(beadmeta.AttemptEvidenceIndexPrefix) {
		return "", false, fmt.Errorf("%w: invalid owner-index key", ErrPrivateEvidenceHTTPProtocol)
	}
	if err := s.privateEvidenceHTTP.verifyContext(context.Background()); err != nil {
		return "", false, err
	}
	metadata, err := s.privateEvidenceHTTP.readMetadata(context.Background(), id)
	if err != nil {
		return "", false, err
	}
	raw, present := metadata[key]
	if !present {
		return "", false, nil
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '"' {
		return "", true, fmt.Errorf("%w: owner-index value is not a JSON string", ErrPrivateEvidenceHTTPProtocol)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", true, fmt.Errorf("%w: owner-index value is not a string", ErrPrivateEvidenceHTTPProtocol)
	}
	return value, true, nil
}

func isPrivateEvidenceValueKey(key string) bool {
	return strings.HasPrefix(key, beadmeta.AttemptEvidenceIndexPrefix) || key == beadmeta.AttemptEvidenceArchivePayloadMetadataKey
}

func hasPrivateEvidenceValueMetadata(metadata map[string]string) bool {
	for key := range metadata {
		if isPrivateEvidenceValueKey(key) {
			return true
		}
	}
	return false
}

func rejectPrivateEvidenceArgvMetadata(op string, metadata map[string]string) error {
	for key := range metadata {
		if isPrivateEvidenceValueKey(key) {
			return fmt.Errorf("%s of private attempt-evidence metadata requires the configured HTTP body transport", op)
		}
	}
	return nil
}

func validatePrivateEvidenceArchiveCreate(bead Bead, ephemeral, noHistory bool) error {
	if bead.ID != "" || bead.Title != "Immutable execution attempt evidence" || bead.Type != "molecule" ||
		(bead.Status != "" && bead.Status != "open" && bead.Status != "closed") || bead.Assignee != "" ||
		bead.ParentID != "" || len(bead.Needs) != 0 || bead.Description != "" || bead.From != "" ||
		bead.Priority != nil || bead.DeferUntil != nil || ephemeral || noHistory || len(bead.Labels) != 1 ||
		bead.Labels[0] != "gc:attempt-evidence" {
		return fmt.Errorf("%w: private payload is restricted to immutable evidence archive rows", ErrPrivateEvidenceHTTPProtocol)
	}
	wantKeys := map[string]bool{
		beadmeta.GCExemptMetadataKey:                        true,
		beadmeta.AttemptEvidenceArchiveOwnerIDMetadataKey:   true,
		beadmeta.AttemptEvidenceArchiveAttemptIDMetadataKey: true,
		beadmeta.AttemptEvidenceArchiveDigestMetadataKey:    true,
		beadmeta.AttemptEvidenceArchivePayloadMetadataKey:   true,
	}
	if len(bead.Metadata) != len(wantKeys) || bead.Metadata[beadmeta.GCExemptMetadataKey] != "true" {
		return fmt.Errorf("%w: evidence archive metadata is incomplete", ErrPrivateEvidenceHTTPProtocol)
	}
	for key := range bead.Metadata {
		if !wantKeys[key] {
			return fmt.Errorf("%w: evidence archive has unsupported metadata", ErrPrivateEvidenceHTTPProtocol)
		}
	}
	ownerID := strings.TrimSpace(bead.Metadata[beadmeta.AttemptEvidenceArchiveOwnerIDMetadataKey])
	attemptID := strings.TrimSpace(bead.Metadata[beadmeta.AttemptEvidenceArchiveAttemptIDMetadataKey])
	payload := bead.Metadata[beadmeta.AttemptEvidenceArchivePayloadMetadataKey]
	digest := bead.Metadata[beadmeta.AttemptEvidenceArchiveDigestMetadataKey]
	if ownerID == "" || attemptID == "" || payload == "" {
		return fmt.Errorf("%w: evidence archive identity or payload is missing", ErrPrivateEvidenceHTTPProtocol)
	}
	computed := sha256.Sum256([]byte(payload))
	if digest != hex.EncodeToString(computed[:]) {
		return fmt.Errorf("%w: evidence archive digest does not match payload", ErrPrivateEvidenceHTTPProtocol)
	}
	var identity struct {
		AttemptID string `json:"attempt_id"`
		Identity  struct {
			OwnerBeadID string `json:"owner_bead_id"`
		} `json:"identity"`
	}
	if err := json.Unmarshal([]byte(payload), &identity); err != nil || identity.AttemptID != attemptID || identity.Identity.OwnerBeadID != ownerID {
		return fmt.Errorf("%w: evidence archive payload identity does not match metadata", ErrPrivateEvidenceHTTPProtocol)
	}
	return nil
}

func newPrivateEvidenceHTTPClient(config PrivateEvidenceHTTPConfig) (*privateEvidenceHTTPClient, error) {
	endpoint := strings.TrimSpace(config.Endpoint)
	projectID := strings.TrimSpace(config.ProjectID)
	database := strings.TrimSpace(config.Database)
	scopeRef := strings.TrimSpace(config.ScopeRef)
	tokenFile := strings.TrimSpace(config.TokenFile)
	if endpoint == "" || projectID == "" || database == "" || tokenFile == "" || !validPrivateEvidenceScopeRef(scopeRef) {
		return nil, fmt.Errorf("%w: endpoint, project, database, scope, and token file are required", ErrPrivateEvidenceHTTPUnavailable)
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed == nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, fmt.Errorf("%w: endpoint must be an origin URL without credentials, path, query, or fragment", ErrPrivateEvidenceHTTPUnavailable)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
	case "http":
		ip := net.ParseIP(parsed.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("%w: cleartext endpoint must use a literal loopback IP", ErrPrivateEvidenceHTTPUnavailable)
		}
	default:
		return nil, fmt.Errorf("%w: endpoint scheme must be https or loopback http", ErrPrivateEvidenceHTTPUnavailable)
	}
	if len(endpoint) > 2048 {
		return nil, fmt.Errorf("%w: endpoint is too long", ErrPrivateEvidenceHTTPUnavailable)
	}
	token, err := readPrivateEvidenceToken(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("%w: controller token file is unavailable", ErrPrivateEvidenceHTTPUnavailable)
	}
	transport := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          4,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   privateEvidenceHTTPTimeout,
		ResponseHeaderTimeout: privateEvidenceHTTPTimeout,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   privateEvidenceHTTPTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &privateEvidenceHTTPClient{
		endpoint:            strings.TrimRight(endpoint, "/"),
		projectID:           projectID,
		database:            database,
		scopeRef:            scopeRef,
		token:               token,
		revisionTransitions: config.RevisionTransitions,
		client:              client,
	}, nil
}

func validPrivateEvidenceScopeRef(scope string) bool {
	if scope == "" || strings.TrimSpace(scope) != scope || strings.ContainsAny(scope, "\r\n\t") {
		return false
	}
	for _, prefix := range []string{"city:", "rig:"} {
		if strings.HasPrefix(scope, prefix) && strings.TrimSpace(strings.TrimPrefix(scope, prefix)) != "" {
			return true
		}
	}
	return false
}

func readPrivateEvidenceToken(path string) (token string, resultErr error) {
	cleanPath := filepath.Clean(path)
	initialInfo, err := os.Lstat(cleanPath)
	if err != nil || !privateEvidenceTokenFileInfoSafe(initialInfo) {
		return "", ErrPrivateEvidenceHTTPUnavailable
	}
	file, err := os.OpenFile(cleanPath, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", ErrPrivateEvidenceHTTPUnavailable
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			token = ""
			resultErr = ErrPrivateEvidenceHTTPUnavailable
		}
	}()
	openedInfo, err := file.Stat()
	if err != nil || !privateEvidenceTokenFileInfoSafe(openedInfo) || !samePrivateEvidenceTokenFileInfo(initialInfo, openedInfo) {
		return "", ErrPrivateEvidenceHTTPUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(file, privateEvidenceTokenMaxBytes+1))
	if err != nil {
		return "", ErrPrivateEvidenceHTTPUnavailable
	}
	readInfo, statErr := file.Stat()
	pathInfo, pathErr := os.Lstat(cleanPath)
	if statErr != nil || pathErr != nil || !privateEvidenceTokenFileInfoSafe(readInfo) ||
		!samePrivateEvidenceTokenFileInfo(openedInfo, readInfo) || !samePrivateEvidenceTokenFileInfo(initialInfo, pathInfo) ||
		int64(len(data)) != readInfo.Size() {
		return "", ErrPrivateEvidenceHTTPUnavailable
	}
	if len(data) > privateEvidenceTokenMaxBytes {
		return "", ErrPrivateEvidenceHTTPUnavailable
	}
	token = strings.TrimSpace(string(data))
	if token == "" || !utf8.ValidString(token) || strings.ContainsAny(token, "\r\n\t ") {
		return "", ErrPrivateEvidenceHTTPUnavailable
	}
	for _, r := range token {
		if r < 0x21 || r > 0x7e {
			return "", ErrPrivateEvidenceHTTPUnavailable
		}
	}
	return token, nil
}

func privateEvidenceTokenFileInfoSafe(info os.FileInfo) bool {
	return info != nil && info.Mode().IsRegular() && info.Mode().Perm()&0o077 == 0 &&
		info.Size() >= 0 && info.Size() <= privateEvidenceTokenMaxBytes
}

func samePrivateEvidenceTokenFileInfo(left, right os.FileInfo) bool {
	return left != nil && right != nil && os.SameFile(left, right) && left.Mode() == right.Mode() &&
		left.Size() == right.Size() && left.ModTime().Equal(right.ModTime())
}

func (c *privateEvidenceHTTPClient) verifyContext(ctx context.Context) error {
	response, err := c.readContext(ctx)
	if err != nil {
		return err
	}
	if err := c.verifyContextIdentity(response); err != nil {
		return err
	}
	for _, required := range []string{"issues.casMetadata", "issues.create", "issues.get", "project.enforce"} {
		if !containsString(response.Capabilities, required) {
			return fmt.Errorf("%w: Beads server lacks required capability %q", ErrPrivateEvidenceHTTPProtocol, required)
		}
	}
	return nil
}

type privateEvidenceServerContext struct {
	APIVersion   string   `json:"api_version"`
	Backend      string   `json:"backend"`
	BdVersion    string   `json:"bd_version"`
	Capabilities []string `json:"capabilities"`
	Database     string   `json:"database"`
	DoltMode     string   `json:"dolt_mode"`
	ProjectID    string   `json:"project_id"`
}

func (c *privateEvidenceHTTPClient) readContext(ctx context.Context) (privateEvidenceServerContext, error) {
	body, status, err := c.request(ctx, http.MethodGet, "/v0/beads/context", nil)
	return decodePrivateEvidenceContext(body, status, err)
}

func (c *privateEvidenceHTTPClient) readRevisionTransitionContext(ctx context.Context) (privateEvidenceServerContext, error) {
	body, status, err := c.requestWithResponseCaps(ctx, http.MethodGet, "/v0/beads/context", nil,
		controllerTransitionMaxSuccessBody, controllerTransitionMaxProblemBody)
	return decodePrivateEvidenceContext(body, status, err)
}

func decodePrivateEvidenceContext(body []byte, status int, err error) (privateEvidenceServerContext, error) {
	if err != nil {
		return privateEvidenceServerContext{}, err
	}
	if status != http.StatusOK {
		return privateEvidenceServerContext{}, fmt.Errorf("%w: context handshake returned HTTP %d", ErrPrivateEvidenceHTTPProtocol, status)
	}
	var response privateEvidenceServerContext
	if err := decodePrivateEvidenceJSON(body, &response); err != nil {
		return privateEvidenceServerContext{}, fmt.Errorf("%w: malformed context handshake", ErrPrivateEvidenceHTTPProtocol)
	}
	if response.APIVersion != "v0" || response.BdVersion == "" || response.ProjectID == "" || response.Database == "" {
		return privateEvidenceServerContext{}, fmt.Errorf("%w: context identity is incomplete", ErrPrivateEvidenceHTTPProtocol)
	}
	return response, nil
}

func (c *privateEvidenceHTTPClient) verifyContextIdentity(response privateEvidenceServerContext) error {
	if response.ProjectID != c.projectID || response.Database != c.database || response.Backend != "dolt" || response.DoltMode != "server" {
		return fmt.Errorf("%w: configured scope %q does not match the served Beads workspace", ErrPrivateEvidenceHTTPIdentity, c.scopeRef)
	}
	return nil
}

func (c *privateEvidenceHTTPClient) verifyRevisionTransitionContext(ctx context.Context) error {
	if !c.revisionTransitions {
		return ErrControllerMetadataTransitionUnavailable
	}
	response, err := c.readRevisionTransitionContext(ctx)
	if err != nil {
		return fmt.Errorf("%w: context handshake failed", ErrControllerMetadataTransitionUnavailable)
	}
	if err := c.verifyContextIdentity(response); err != nil {
		return fmt.Errorf("%w: configured workspace identity mismatch", ErrControllerMetadataTransitionProtocol)
	}
	for _, required := range []string{"issues.transitionMetadata", "issues.transitionReceipt.get", "project.enforce"} {
		if !containsString(response.Capabilities, required) {
			return fmt.Errorf("%w: Beads server lacks required capability %q", ErrControllerMetadataTransitionProtocol, required)
		}
	}
	return nil
}

func (c *privateEvidenceHTTPClient) verifyListRoute(ctx context.Context) error {
	query := url.Values{"limit": {"1"}, "sort": {"priority"}}
	body, status, err := c.request(ctx, http.MethodGet, "/v0/beads/issues?"+query.Encode(), nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("%w: issue list route returned HTTP %d", ErrPrivateEvidenceHTTPProtocol, status)
	}
	var page struct {
		Items      []json.RawMessage `json:"items"`
		HasMore    bool              `json:"has_more"`
		NextCursor string            `json:"next_cursor"`
	}
	if err := decodePrivateEvidenceJSON(body, &page); err != nil || len(page.Items) > 1 || (page.HasMore && strings.TrimSpace(page.NextCursor) == "") {
		return fmt.Errorf("%w: issue list route response is malformed", ErrPrivateEvidenceHTTPProtocol)
	}
	return nil
}

func (c *privateEvidenceHTTPClient) request(ctx context.Context, method, path string, body []byte) ([]byte, int, error) {
	return c.requestWithResponseCaps(ctx, method, path, body, privateEvidenceHTTPMaxResponseBody, privateEvidenceHTTPMaxResponseBody)
}

func (c *privateEvidenceHTTPClient) requestWithResponseCaps(ctx context.Context, method, path string, body []byte, successCap, problemCap int) ([]byte, int, error) {
	if len(body) > privateEvidenceHTTPMaxRequestBody {
		return nil, 0, fmt.Errorf("%w: request exceeds size limit", ErrPrivateEvidenceHTTPProtocol)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("%w: request could not be formed", ErrPrivateEvidenceHTTPProtocol)
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
				return nil, status, fmt.Errorf("%w: redirect refused", ErrPrivateEvidenceHTTPProtocol)
			}
		}
		// Do not retain the underlying URL/request error: transport diagnostics
		// can include caller-controlled data and this path handles private bytes.
		return nil, 0, fmt.Errorf("%w: %s request failed", ErrPrivateEvidenceHTTPUnavailable, method)
	}
	responseCap := successCap
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		responseCap = problemCap
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, int64(responseCap)+1))
	closeErr := response.Body.Close()
	if err != nil || closeErr != nil {
		return nil, response.StatusCode, fmt.Errorf("%w: %s response could not be read", ErrPrivateEvidenceHTTPUnavailable, method)
	}
	if len(data) > responseCap {
		return nil, response.StatusCode, fmt.Errorf("%w: response exceeds size limit", ErrPrivateEvidenceHTTPProtocol)
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return nil, response.StatusCode, fmt.Errorf("%w: redirect refused", ErrPrivateEvidenceHTTPProtocol)
	}
	return data, response.StatusCode, nil
}

func (c *privateEvidenceHTTPClient) createEvidenceArchive(ctx context.Context, bead Bead, ephemeral, noHistory bool) (Bead, error) {
	if err := validatePrivateEvidenceArchiveCreate(bead, ephemeral, noHistory); err != nil {
		return Bead{}, err
	}
	if err := c.verifyContext(ctx); err != nil {
		return Bead{}, err
	}
	request := struct {
		Actor     string            `json:"actor"`
		Title     string            `json:"title"`
		IssueType string            `json:"issue_type"`
		Status    string            `json:"status"`
		Labels    []string          `json:"labels"`
		Metadata  map[string]string `json:"metadata"`
	}{
		Actor: privateEvidenceActor, Title: bead.Title, IssueType: bead.Type,
		Status: "closed", Labels: bead.Labels, Metadata: map[string]string(bead.Metadata),
	}
	body, err := json.Marshal(request)
	if err != nil {
		return Bead{}, fmt.Errorf("%w: evidence archive create could not be encoded", ErrPrivateEvidenceHTTPProtocol)
	}
	responseBody, status, err := c.request(ctx, http.MethodPost, "/v0/beads/issues", body)
	if err != nil {
		return Bead{}, err
	}
	if status == http.StatusNotFound {
		return Bead{}, ErrNotFound
	}
	if status != http.StatusOK {
		return Bead{}, fmt.Errorf("%w: evidence archive create returned HTTP %d", ErrPrivateEvidenceHTTPProtocol, status)
	}
	var issue bdIssue
	if err := decodePrivateEvidenceJSON(responseBody, &issue); err != nil || strings.TrimSpace(issue.ID) == "" || issue.Status != "closed" {
		return Bead{}, fmt.Errorf("%w: evidence archive create response is incomplete", ErrPrivateEvidenceHTTPProtocol)
	}
	created := issue.toBead()
	for key, value := range bead.Metadata {
		if created.Metadata[key] != value {
			return Bead{}, fmt.Errorf("%w: evidence archive create readback did not preserve metadata", ErrPrivateEvidenceHTTPProtocol)
		}
	}
	if !IsAttemptEvidenceArchive(created) {
		return Bead{}, fmt.Errorf("%w: evidence archive create response is not an archive", ErrPrivateEvidenceHTTPProtocol)
	}
	return created, nil
}

func (c *privateEvidenceHTTPClient) listEvidenceArchives(ctx context.Context, ownerID, attemptID string) ([]Bead, error) {
	if strings.TrimSpace(ownerID) == "" {
		return nil, fmt.Errorf("%w: archive owner is required", ErrPrivateEvidenceHTTPProtocol)
	}
	if err := c.verifyContext(ctx); err != nil {
		return nil, err
	}
	values := url.Values{}
	values.Add("metadata_field", beadmeta.AttemptEvidenceArchiveOwnerIDMetadataKey+"="+ownerID)
	if attemptID != "" {
		values.Add("metadata_field", beadmeta.AttemptEvidenceArchiveAttemptIDMetadataKey+"="+attemptID)
	}
	values.Set("all", "true")
	values.Set("include_templates", "true")
	values.Set("include_gates", "true")
	values.Set("include_infra", "true")
	values.Set("limit", "16")
	var out []Bead
	seenCursors := make(map[string]struct{})
	cursor := ""
	for pageNumber := 0; pageNumber < 4096; pageNumber++ {
		query := cloneURLValues(values)
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		body, status, err := c.request(ctx, http.MethodGet, "/v0/beads/issues?"+query.Encode(), nil)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("%w: evidence archive list returned HTTP %d", ErrPrivateEvidenceHTTPProtocol, status)
		}
		var page struct {
			Items      []json.RawMessage `json:"items"`
			HasMore    bool              `json:"has_more"`
			NextCursor string            `json:"next_cursor"`
		}
		if err := decodePrivateEvidenceJSON(body, &page); err != nil {
			return nil, fmt.Errorf("%w: malformed evidence archive list response", ErrPrivateEvidenceHTTPProtocol)
		}
		for _, raw := range page.Items {
			var issue bdIssue
			if err := decodePrivateEvidenceJSON(raw, &issue); err != nil || strings.TrimSpace(issue.ID) == "" {
				return nil, fmt.Errorf("%w: malformed evidence archive row", ErrPrivateEvidenceHTTPProtocol)
			}
			bead := issue.toBead()
			if bead.Metadata[beadmeta.AttemptEvidenceArchiveOwnerIDMetadataKey] != ownerID ||
				(attemptID != "" && bead.Metadata[beadmeta.AttemptEvidenceArchiveAttemptIDMetadataKey] != attemptID) {
				return nil, fmt.Errorf("%w: archive list returned a row outside its filter", ErrPrivateEvidenceHTTPProtocol)
			}
			out = append(out, bead)
		}
		if !page.HasMore {
			return out, nil
		}
		if strings.TrimSpace(page.NextCursor) == "" {
			return nil, fmt.Errorf("%w: archive list omitted its next cursor", ErrPrivateEvidenceHTTPProtocol)
		}
		if _, duplicate := seenCursors[page.NextCursor]; duplicate {
			return nil, fmt.Errorf("%w: archive list repeated a cursor", ErrPrivateEvidenceHTTPProtocol)
		}
		seenCursors[page.NextCursor] = struct{}{}
		cursor = page.NextCursor
	}
	return nil, fmt.Errorf("%w: archive list exceeded page bound", ErrPrivateEvidenceHTTPProtocol)
}

func (c *privateEvidenceHTTPClient) listOwnerIndexes(ctx context.Context, ownerID string) (map[string]string, error) {
	if strings.TrimSpace(ownerID) == "" {
		return nil, fmt.Errorf("%w: owner is required", ErrPrivateEvidenceHTTPProtocol)
	}
	if err := c.verifyContext(ctx); err != nil {
		return nil, err
	}
	metadata, err := c.readMetadata(ctx, ownerID)
	if errors.Is(err, ErrNotFound) {
		// Owner deletion must not hide the independent durable archive.
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	indexes := make(map[string]string)
	for key, raw := range metadata {
		if !strings.HasPrefix(key, beadmeta.AttemptEvidenceIndexPrefix) || len(key) == len(beadmeta.AttemptEvidenceIndexPrefix) {
			continue
		}
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 || raw[0] != '"' {
			return nil, fmt.Errorf("%w: owner-index value is not a JSON string", ErrPrivateEvidenceHTTPProtocol)
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, fmt.Errorf("%w: owner-index value is malformed", ErrPrivateEvidenceHTTPProtocol)
		}
		indexes[key] = value
	}
	return indexes, nil
}

func (c *privateEvidenceHTTPClient) getBead(ctx context.Context, id string) (Bead, error) {
	if strings.TrimSpace(id) == "" {
		return Bead{}, ErrNotFound
	}
	if err := c.verifyContext(ctx); err != nil {
		return Bead{}, err
	}
	body, status, err := c.request(ctx, http.MethodGet, "/v0/beads/issues/"+url.PathEscape(id), nil)
	if err != nil {
		return Bead{}, err
	}
	if status == http.StatusNotFound {
		return Bead{}, ErrNotFound
	}
	if status != http.StatusOK {
		return Bead{}, fmt.Errorf("%w: issue read returned HTTP %d", ErrPrivateEvidenceHTTPProtocol, status)
	}
	var issue bdIssue
	if err := decodePrivateEvidenceJSON(body, &issue); err != nil || issue.ID != id {
		return Bead{}, fmt.Errorf("%w: malformed or mismatched issue read response", ErrPrivateEvidenceHTTPProtocol)
	}
	return issue.toBead(), nil
}

func cloneURLValues(values url.Values) url.Values {
	clone := make(url.Values, len(values))
	for key, items := range values {
		clone[key] = append([]string(nil), items...)
	}
	return clone
}

func decodePrivateEvidenceJSON(raw []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

func (c *privateEvidenceHTTPClient) compareAndSetMetadata(ctx context.Context, id, key, expected, next string) (bool, error) {
	if strings.TrimSpace(id) == "" || !strings.HasPrefix(key, beadmeta.AttemptEvidenceIndexPrefix) || len(key) == len(beadmeta.AttemptEvidenceIndexPrefix) {
		return false, fmt.Errorf("%w: invalid owner-index target", ErrPrivateEvidenceHTTPProtocol)
	}
	if !utf8.ValidString(next) || len(next) > privateEvidenceHTTPMaxRequestBody {
		return false, fmt.Errorf("%w: metadata value exceeds size limit", ErrPrivateEvidenceHTTPProtocol)
	}
	if err := c.verifyContext(ctx); err != nil {
		return false, err
	}
	metadata, err := c.readMetadata(ctx, id)
	if err != nil {
		return false, err
	}
	current, present := metadata[key]
	if present {
		current = bytes.TrimSpace(current)
		// encoding/json accepts JSON null when unmarshaling into a string and
		// leaves the destination as "". Raw metadata type is part of the CAS
		// contract, so a present null must never be treated as a present empty
		// string.
		if len(current) == 0 || current[0] != '"' {
			return false, nil
		}
		var currentString string
		if err := json.Unmarshal(current, &currentString); err != nil || currentString != expected {
			return false, nil
		}
	} else if expected != "" {
		return false, nil
	}
	request := struct {
		Actor    string          `json:"actor"`
		Key      string          `json:"key"`
		Expected json.RawMessage `json:"expected,omitempty"`
		Value    string          `json:"value"`
	}{Actor: privateEvidenceActor, Key: key, Value: next}
	if present {
		request.Expected = json.RawMessage(strconvQuote(expected))
	}
	body, err := json.Marshal(request)
	if err != nil {
		return false, fmt.Errorf("%w: CAS request could not be encoded", ErrPrivateEvidenceHTTPProtocol)
	}
	path := "/v0/beads/issues/" + url.PathEscape(id) + ":casMetadata"
	responseBody, status, err := c.request(ctx, http.MethodPost, path, body)
	if err != nil {
		return false, err
	}
	if status == http.StatusNotFound {
		return false, ErrNotFound
	}
	if status != http.StatusOK {
		return false, fmt.Errorf("%w: metadata CAS returned HTTP %d", ErrPrivateEvidenceHTTPProtocol, status)
	}
	var response struct {
		Swapped *bool `json:"swapped"`
	}
	if err := decodePrivateEvidenceJSON(responseBody, &response); err != nil || response.Swapped == nil {
		return false, fmt.Errorf("%w: malformed metadata CAS response", ErrPrivateEvidenceHTTPProtocol)
	}
	return *response.Swapped, nil
}

func strconvQuote(value string) []byte {
	encoded, _ := json.Marshal(value)
	return encoded
}

func (c *privateEvidenceHTTPClient) readMetadata(ctx context.Context, id string) (map[string]json.RawMessage, error) {
	path := "/v0/beads/issues/" + url.PathEscape(id)
	body, status, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: issue read returned HTTP %d", ErrPrivateEvidenceHTTPProtocol, status)
	}
	var issue struct {
		ID       string          `json:"id"`
		Metadata json.RawMessage `json:"metadata"`
	}
	if err := decodePrivateEvidenceJSON(body, &issue); err != nil || issue.ID != id {
		return nil, fmt.Errorf("%w: malformed issue read response", ErrPrivateEvidenceHTTPProtocol)
	}
	if len(issue.Metadata) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(issue.Metadata, &metadata); err != nil || metadata == nil {
		return nil, fmt.Errorf("%w: issue metadata is not an object", ErrPrivateEvidenceHTTPProtocol)
	}
	return metadata, nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// ApplyPrivateEvidenceMetadataCAS uses the dedicated body-only transport when
// present, and otherwise preserves the existing metadata-CAS path for stores
// already allow-listed for private payloads. Ambiguous HTTP failures remain
// errors; callers may safely retry and resolve the winner through raw readback.
func ApplyPrivateEvidenceMetadataCAS(store Store, id, key, expected, next string) (MetadataCASOutcome, error) {
	writer, ok := PrivateEvidenceMetadataCASWriterFor(store)
	if !ok {
		return ApplyMetadataCAS(store, id, key, expected, next)
	}
	swapped, err := writer.CompareAndSetPrivateEvidenceMetadataKey(id, key, expected, next)
	if err != nil {
		return "", fmt.Errorf("private evidence metadata CAS for %q key %q: %w", id, key, err)
	}
	if swapped {
		return MetadataCASSwapped, nil
	}
	value, present, err := writer.ReadPrivateEvidenceMetadataKey(id, key)
	if err != nil {
		return "", fmt.Errorf("read private evidence metadata CAS result for %q: %w", id, err)
	}
	if present && value == next {
		return MetadataCASAlreadyNext, nil
	}
	return MetadataCASConflict, nil
}

// ReadPrivateEvidenceMetadataKey avoids the bd stdout/telemetry path when the
// backing provides a dedicated reader. Existing private-safe stores continue
// to use typed bead reads; unsupported stores refuse before a generic read.
func ReadPrivateEvidenceMetadataKey(store Store, id, key string) (string, bool, error) {
	if store == nil {
		return "", false, errors.New("private evidence metadata read: bead store is unavailable")
	}
	if writer, ok := PrivateEvidenceMetadataCASWriterFor(store); ok {
		return writer.ReadPrivateEvidenceMetadataKey(id, key)
	}
	if reader, ok := PrivateEvidenceArchiveReaderFor(store); ok {
		indexes, err := reader.ListPrivateEvidenceOwnerIndexes(id)
		if err != nil {
			return "", false, err
		}
		value, present := indexes[key]
		return value, present, nil
	}
	if !SupportsPrivatePayloadValues(store) {
		return "", false, ErrPrivateEvidenceTransportUnsupported
	}
	bead, err := store.Get(id)
	if err != nil {
		return "", false, err
	}
	value, present := bead.Metadata[key]
	return value, present, nil
}
