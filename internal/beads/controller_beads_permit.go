package beads

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	controllerBeadsPermitSchemaV1       = "beads.protected-mutation-permit.v1"
	controllerBeadsPermitPurpose        = "protected_mutation_permit"
	controllerBeadsPermitOperation      = "issue.batch_apply"
	controllerBeadsPermitSigningDomain  = "beads.protected-mutation-permit.v1\n"
	controllerBeadsPermitMaxLifetime    = 5 * time.Minute
	controllerBeadsPermitMaxPayloadSize = 32 << 10
	controllerBeadsPermitMaxTokenSize   = 64 << 10
)

var (
	ErrControllerBeadsPermitIssuerConfig = errors.New("controller Beads permit issuer configuration is invalid")
	ErrControllerBeadsPermitRequest      = errors.New("controller Beads permit request is invalid")
	ErrControllerBeadsPermitSigning      = errors.New("controller Beads permit signing failed")
)

// ControllerBeadsPermitIssuerConfig is explicit in-memory signing input. The
// signer is injected by its caller; this issuer has no key-file, startup, or
// service integration.
type ControllerBeadsPermitIssuerConfig struct {
	Audience  string
	ProjectID string
	Database  string
	KeyID     string
	Issuer    string
	Lifetime  time.Duration
	Signer    crypto.Signer
	Now       func() time.Time
}

// ControllerBeadsPermitIssuer signs the Beads v1 permit accepted by the
// pinned Beads verifier. It is an inert helper and has no production call site.
type ControllerBeadsPermitIssuer struct {
	audience  string
	projectID string
	database  string
	keyID     string
	issuer    string
	lifetime  time.Duration
	signer    crypto.Signer
	publicKey ed25519.PublicKey
	now       func() time.Time
}

// NewControllerBeadsPermitIssuer validates exact workspace bindings and an
// injected Ed25519 signer. The Beads verifier caps grants at five minutes.
func NewControllerBeadsPermitIssuer(config ControllerBeadsPermitIssuerConfig) (*ControllerBeadsPermitIssuer, error) {
	if !validControllerBeadsPermitAtom(config.Audience, 255) ||
		!validControllerBeadsPermitAtom(config.ProjectID, 255) ||
		!validControllerBeadsPermitAtom(config.Database, 255) ||
		!validControllerBeadsPermitAtom(config.KeyID, 200) ||
		!validControllerBeadsPermitAtom(config.Issuer, 256) ||
		config.Lifetime <= 0 || config.Lifetime > controllerBeadsPermitMaxLifetime || config.Signer == nil {
		return nil, ErrControllerBeadsPermitIssuerConfig
	}
	publicKey, ok := config.Signer.Public().(ed25519.PublicKey)
	if !ok || len(publicKey) != ed25519.PublicKeySize {
		return nil, ErrControllerBeadsPermitIssuerConfig
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &ControllerBeadsPermitIssuer{
		audience: config.Audience, projectID: config.ProjectID, database: config.Database,
		keyID: config.KeyID, issuer: config.Issuer, lifetime: config.Lifetime,
		signer: config.Signer, publicKey: append(ed25519.PublicKey(nil), publicKey...), now: config.Now,
	}, nil
}

// IssueProtectedCreateAndLink signs one exact protected create-and-link
// request. Pass the returned token unchanged as request.ProtectedPermit.
// replayID is separate from the batch receipt ID and must be unique within the
// permit audience.
func (i *ControllerBeadsPermitIssuer) IssueProtectedCreateAndLink(request ControllerProtectedCreateAndLinkRequest, replayID string) (string, error) {
	if i == nil || i.signer == nil || len(i.publicKey) != ed25519.PublicKeySize || request.ProtectedPermit != "" || !validControllerBeadsPermitReplayID(replayID) {
		return "", ErrControllerBeadsPermitRequest
	}
	digest, err := controllerBeadsProtectedBatchDigest(request)
	if err != nil {
		return "", fmt.Errorf("%w: unsupported protected batch shape", ErrControllerBeadsPermitRequest)
	}
	resources, err := controllerBeadsProtectedBatchResources(request)
	if err != nil {
		return "", fmt.Errorf("%w: invalid resource set", ErrControllerBeadsPermitRequest)
	}

	issuedAt := i.now().UTC()
	expiresAt := issuedAt.Add(i.lifetime)
	claims := controllerBeadsPermitClaimsV1{
		SchemaVersion: controllerBeadsPermitSchemaV1,
		Purpose:       controllerBeadsPermitPurpose,
		KeyID:         i.keyID,
		Issuer:        i.issuer,
		Audience:      i.audience,
		ProjectID:     i.projectID,
		Database:      i.database,
		Operation:     controllerBeadsPermitOperation,
		ResourceIDs:   resources,
		RequestDigest: digest,
		ReplayID:      replayID,
		IssuedAt:      issuedAt.Format(time.RFC3339Nano),
		ExpiresAt:     expiresAt.Format(time.RFC3339Nano),
	}
	payload, err := canonicalControllerBeadsPermitClaims(claims)
	if err != nil {
		return "", fmt.Errorf("%w: permit claims are invalid", ErrControllerBeadsPermitRequest)
	}
	signingBytes := make([]byte, 0, len(controllerBeadsPermitSigningDomain)+len(payload))
	signingBytes = append(signingBytes, controllerBeadsPermitSigningDomain...)
	signingBytes = append(signingBytes, payload...)
	signature, err := i.signer.Sign(rand.Reader, signingBytes, crypto.Hash(0))
	if err != nil || len(signature) != ed25519.SignatureSize || !ed25519.Verify(i.publicKey, signingBytes, signature) {
		return "", ErrControllerBeadsPermitSigning
	}
	token := base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature)
	if len(token) > controllerBeadsPermitMaxTokenSize {
		return "", ErrControllerBeadsPermitSigning
	}
	return token, nil
}

// controllerBeadsProtectedBatchDigest maps the narrow controller request to
// the JSON shape hashed by Beads issueops.ApplyBatchRequestDigest. The mirror is
// pinned to Beads commit 37a12058fd2a50d5b1cd1b484690003b3196f324; the
// cross-repository golden is also recorded in Beads commit 519da36c1. Update
// this mapper only with a matching Beads pin and fixture change.
func controllerBeadsProtectedBatchDigest(request ControllerProtectedCreateAndLinkRequest) (string, error) {
	if request.Record.ProtectionClass == "" {
		return "", ErrControllerBeadsPermitRequest
	}
	validated := request
	if validated.ProtectedPermit == "" {
		// Reuse the existing client planner's narrow-shape validation while the
		// issuer prepares the permit that will complete the protected pair.
		validated.ProtectedPermit = "permit-pending-issuance"
	}
	if _, err := planControllerProtectedCreateAndLink(validated); err != nil {
		return "", err
	}

	items := make([]controllerBeadsDigestApplyItem, 0, len(request.Links)+1)
	items = append(items, controllerBeadsDigestApplyItem{
		Kind: "create",
		Create: &controllerBeadsDigestCreateItem{
			Issue: &controllerBeadsDigestIssue{
				ID: request.Record.ID, Title: request.Record.Title,
			},
			ProtectionClass: request.Record.ProtectionClass,
		},
	})
	for _, link := range request.Links {
		items = append(items, controllerBeadsDigestApplyItem{
			Kind: "dep_add",
			DepAdd: &controllerBeadsDigestDepAddItem{
				Source: controllerBeadsDigestRef{ID: link.SourceID},
				Target: controllerBeadsDigestRef{ID: link.TargetID},
				Type:   link.Type,
			},
		})
	}
	createSourceRepos := make([]string, len(items))
	encoded, err := json.Marshal(struct {
		Request           controllerBeadsDigestApplyBatchRequest
		CreateSourceRepos []string
	}{
		Request: controllerBeadsDigestApplyBatchRequest{
			Actor: request.Actor, ProtectedPermit: "", Items: items,
		},
		CreateSourceRepos: createSourceRepos,
	})
	if err != nil {
		return "", fmt.Errorf("encode Beads protected batch digest input: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func controllerBeadsProtectedBatchResources(request ControllerProtectedCreateAndLinkRequest) ([]string, error) {
	set := make(map[string]struct{}, 1+len(request.Links)*2)
	set[request.Record.ID] = struct{}{}
	for _, link := range request.Links {
		set[link.SourceID] = struct{}{}
		set[link.TargetID] = struct{}{}
	}
	if len(set) == 0 || len(set) > 256 {
		return nil, ErrControllerBeadsPermitRequest
	}
	resources := make([]string, 0, len(set))
	for resource := range set {
		if !validControllerBeadsPermitAtom(resource, 512) {
			return nil, ErrControllerBeadsPermitRequest
		}
		resources = append(resources, resource)
	}
	// Go string ordering compares the UTF-8 bytes lexicographically, matching
	// Beads' canonical resource ordering.
	sort.Strings(resources)
	return resources, nil
}

type controllerBeadsPermitClaimsV1 struct {
	SchemaVersion string   `json:"schema_version"`
	Purpose       string   `json:"purpose"`
	KeyID         string   `json:"key_id"`
	Issuer        string   `json:"issuer"`
	Audience      string   `json:"audience"`
	ProjectID     string   `json:"project_id"`
	Database      string   `json:"database"`
	Operation     string   `json:"operation"`
	ResourceIDs   []string `json:"resource_ids"`
	RequestDigest string   `json:"request_digest"`
	ReplayID      string   `json:"replay_id"`
	IssuedAt      string   `json:"issued_at"`
	ExpiresAt     string   `json:"expires_at"`
}

func canonicalControllerBeadsPermitClaims(claims controllerBeadsPermitClaimsV1) ([]byte, error) {
	if claims.SchemaVersion != controllerBeadsPermitSchemaV1 || claims.Purpose != controllerBeadsPermitPurpose ||
		!validControllerBeadsPermitAtom(claims.KeyID, 200) || !validControllerBeadsPermitAtom(claims.Issuer, 256) ||
		!validControllerBeadsPermitAtom(claims.Audience, 255) || !validControllerBeadsPermitAtom(claims.ProjectID, 255) ||
		!validControllerBeadsPermitAtom(claims.Database, 255) || claims.Operation != controllerBeadsPermitOperation ||
		!validControllerBeadsPermitSHA256(claims.RequestDigest) || !validControllerBeadsPermitReplayID(claims.ReplayID) {
		return nil, ErrControllerBeadsPermitRequest
	}
	resources := append([]string(nil), claims.ResourceIDs...)
	if len(resources) == 0 || len(resources) > 256 {
		return nil, ErrControllerBeadsPermitRequest
	}
	for _, resource := range resources {
		if !validControllerBeadsPermitAtom(resource, 512) {
			return nil, ErrControllerBeadsPermitRequest
		}
	}
	sort.Strings(resources)
	for index := 1; index < len(resources); index++ {
		if resources[index] == resources[index-1] {
			return nil, ErrControllerBeadsPermitRequest
		}
	}
	issuedAt, err := time.Parse(time.RFC3339Nano, claims.IssuedAt)
	if err != nil || issuedAt.Format(time.RFC3339Nano) != claims.IssuedAt {
		return nil, ErrControllerBeadsPermitRequest
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, claims.ExpiresAt)
	if err != nil || expiresAt.Format(time.RFC3339Nano) != claims.ExpiresAt ||
		!expiresAt.After(issuedAt) || expiresAt.Sub(issuedAt) > controllerBeadsPermitMaxLifetime {
		return nil, ErrControllerBeadsPermitRequest
	}
	claims.ResourceIDs = resources
	encoded, err := json.Marshal(claims)
	if err != nil || len(encoded) > controllerBeadsPermitMaxPayloadSize {
		return nil, ErrControllerBeadsPermitRequest
	}
	return encoded, nil
}

func validControllerBeadsPermitAtom(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validControllerBeadsPermitReplayID(value string) bool {
	if len(value) < 16 || len(value) > 200 {
		return false
	}
	for index, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case index > 0 && (r == '.' || r == '_' || r == ':' || r == '-'):
		default:
			return false
		}
	}
	return true
}

func validControllerBeadsPermitSHA256(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// These private shapes mirror the exported Beads values' encoding/json output
// at the pinned revision. Keep their field order and zero-value emission fixed
// with TestControllerBeadsProtectedBatchDigestMatchesPinnedGolden.
type controllerBeadsDigestApplyBatchRequest struct {
	Actor                 string
	ProtectedPermit       string
	Items                 []controllerBeadsDigestApplyItem
	Provenance            string
	ForceIDPrefix         bool
	SkipPerEdgeCycleCheck bool
}

type controllerBeadsDigestApplyItem struct {
	Kind   string
	Create *controllerBeadsDigestCreateItem
	Update *controllerBeadsDigestUpdateItem
	Close  *controllerBeadsDigestCloseItem
	DepAdd *controllerBeadsDigestDepAddItem
}

type controllerBeadsDigestCreateItem struct {
	Key             string
	Issue           *controllerBeadsDigestIssue
	ProtectionClass string
	MetadataRefs    map[string]controllerBeadsDigestRef
}

type controllerBeadsDigestIssue struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Priority  int       `json:"priority"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type controllerBeadsDigestUpdateItem struct{}

type controllerBeadsDigestCloseItem struct{}

type controllerBeadsDigestDepAddItem struct {
	Source   controllerBeadsDigestRef
	Target   controllerBeadsDigestRef
	Type     string
	Metadata string
}

type controllerBeadsDigestRef struct {
	Key string
	ID  string
}
