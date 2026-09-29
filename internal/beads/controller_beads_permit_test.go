package beads

import (
	"crypto"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestControllerBeadsProtectedBatchDigestMatchesPinnedGolden(t *testing.T) {
	request := controllerBeadsFrontierDigestRequest(true)
	request.ReceiptID = "excluded-receipt"
	request.ProtectedPermit = "excluded-permit"

	digest, err := controllerBeadsProtectedBatchDigest(request)
	if err != nil {
		t.Fatalf("controllerBeadsProtectedBatchDigest: %v", err)
	}
	const want = "f05bb472b3b4f5a797586c3d6df1a2afec9e6a8f065f03e48f42b3d966bbf568"
	if digest != want {
		t.Fatalf("controller digest = %q, want pinned Beads digest %q", digest, want)
	}

	reorderedMetadata := request
	reorderedMetadata.Record.Metadata = map[string]string{
		"gc.decision_frontier.state":  "pending",
		"gc.decision_frontier.record": "decision-frontier/map/v1",
	}
	metadataDigest, err := controllerBeadsProtectedBatchDigest(reorderedMetadata)
	if err != nil {
		t.Fatalf("controllerBeadsProtectedBatchDigest with reordered metadata: %v", err)
	}
	if metadataDigest != digest {
		t.Fatalf("metadata map insertion order changed linked digest: %q != %q", metadataDigest, digest)
	}
	reorderedLinks := request
	reorderedLinks.Links = []ControllerDependencyLink{request.Links[1], request.Links[0]}
	linkDigest, err := controllerBeadsProtectedBatchDigest(reorderedLinks)
	if err != nil {
		t.Fatalf("controllerBeadsProtectedBatchDigest with reordered links: %v", err)
	}
	if linkDigest == digest {
		t.Fatalf("dependency link order did not change digest: %q", linkDigest)
	}
}

func TestControllerBeadsProtectedCreateOnlyDigestMatchesPinnedGolden(t *testing.T) {
	request := controllerBeadsFrontierDigestRequest(false)

	digest, err := controllerBeadsProtectedBatchDigest(request)
	if err != nil {
		t.Fatalf("controllerBeadsProtectedBatchDigest: %v", err)
	}
	const want = "f9bdd7a3fd99c83991c5288e92bb09da9b71ecc222809ac9faaded09ad796c85"
	if digest != want {
		t.Fatalf("controller digest = %q, want pinned Beads create-only digest %q", digest, want)
	}

	request.Record.Metadata = map[string]string{
		"gc.decision_frontier.state":  "pending",
		"gc.decision_frontier.record": "decision-frontier/map/v1",
	}
	reordered, err := controllerBeadsProtectedBatchDigest(request)
	if err != nil {
		t.Fatalf("controllerBeadsProtectedBatchDigest with reordered metadata: %v", err)
	}
	if reordered != digest {
		t.Fatalf("metadata map insertion order changed digest: %q != %q", reordered, digest)
	}
}

func TestControllerBeadsProtectedLinkDigestMatchesPinnedGolden(t *testing.T) {
	request := controllerBeadsProtectedLinkRequest()
	request.ReceiptID = "excluded-link-receipt"

	digest, err := controllerBeadsProtectedLinkDigest(request)
	if err != nil {
		t.Fatalf("controllerBeadsProtectedLinkDigest: %v", err)
	}
	const want = "a1613932a1e5cf482be9f1d79e0cbd274cc21923987e016811324a406678306d"
	if digest != want {
		t.Fatalf("controller digest = %q, want pinned Beads link-only digest %q", digest, want)
	}

	request.ReceiptID = "a-different-receipt"
	receiptDigest, err := controllerBeadsProtectedLinkDigest(request)
	if err != nil {
		t.Fatalf("controllerBeadsProtectedLinkDigest with changed receipt: %v", err)
	}
	if receiptDigest != digest {
		t.Fatalf("receipt ID changed the protected link digest: %q != %q", receiptDigest, digest)
	}
	reversed := request
	reversed.Link.SourceID, reversed.Link.TargetID = reversed.Link.TargetID, reversed.Link.SourceID
	reversedDigest, err := controllerBeadsProtectedLinkDigest(reversed)
	if err != nil {
		t.Fatalf("controllerBeadsProtectedLinkDigest with reversed edge: %v", err)
	}
	if reversedDigest == digest {
		t.Fatalf("reversing the dependency edge did not change the digest: %q", digest)
	}
}

func TestControllerBeadsPermitIssuerBindsOnlyLinkEndpoints(t *testing.T) {
	privateKey := ed25519.NewKeyFromSeed([]byte("0123456789abcdef0123456789abcdef"))
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	issuer, err := NewControllerBeadsPermitIssuer(ControllerBeadsPermitIssuerConfig{
		Audience: "audience-a", ProjectID: "project-a", Database: "beads_fixture",
		KeyID: "key-a", Issuer: "issuer-a", Lifetime: time.Minute,
		Signer: privateKey, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewControllerBeadsPermitIssuer: %v", err)
	}
	request := controllerBeadsProtectedLinkRequest()
	token, err := issuer.IssueProtectedLink(request, "replay-link-identifier-0001")
	if err != nil {
		t.Fatalf("IssueProtectedLink: %v", err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		t.Fatalf("permit token has %d segments, want 2", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || base64.RawURLEncoding.EncodeToString(payload) != parts[0] {
		t.Fatalf("permit payload is not canonical raw URL base64: %v", err)
	}
	var claims controllerBeadsPermitClaimsV1
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("decode permit claims: %v", err)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || base64.RawURLEncoding.EncodeToString(signature) != parts[1] ||
		!ed25519.Verify(privateKey.Public().(ed25519.PublicKey), append([]byte(controllerBeadsPermitSigningDomain), payload...), signature) {
		t.Fatal("link permit signature did not verify against the pinned signing domain")
	}
	digest, err := controllerBeadsProtectedLinkDigest(request)
	if err != nil {
		t.Fatalf("controllerBeadsProtectedLinkDigest: %v", err)
	}
	wantResources := []string{"bd-question-a", "bd-question-b"}
	if claims.RequestDigest != digest || !reflect.DeepEqual(claims.ResourceIDs, wantResources) {
		t.Fatalf("link-only claims digest/resources = (%q, %v), want (%q, %v)", claims.RequestDigest, claims.ResourceIDs, digest, wantResources)
	}
}

func controllerBeadsFrontierDigestRequest(withLinks bool) ControllerProtectedCreateAndLinkRequest {
	request := ControllerProtectedCreateAndLinkRequest{
		Actor: "controller-a",
		Record: ControllerProtectedRecord{
			ID: "bd-frontier-map-1", Type: "gate", Title: "Decision map for wrk-1",
			Description: `{"schema_version":1,"kind":"decision-frontier/map/v1"}`,
			Labels:      []string{"decision-frontier"},
			Metadata: map[string]string{
				"gc.decision_frontier.record": "decision-frontier/map/v1",
				"gc.decision_frontier.state":  "pending",
			},
			ProtectionClass: "source-decision",
		},
	}
	if withLinks {
		request.Links = []ControllerDependencyLink{
			{SourceID: "bd-frontier-map-1", TargetID: "bd-question-1", Type: "relates-to"},
			{SourceID: "bd-prerequisite", TargetID: "bd-frontier-map-1", Type: "blocks"},
		}
	}
	return request
}

func controllerBeadsProtectedLinkRequest() ControllerProtectedLinkRequest {
	return ControllerProtectedLinkRequest{
		Actor: "controller-a",
		Link: ControllerDependencyLink{
			SourceID: "bd-question-b", TargetID: "bd-question-a", Type: "blocks",
		},
	}
}

func TestControllerBeadsPermitIssuerEmitsPinnedV1Claims(t *testing.T) {
	privateKey := ed25519.NewKeyFromSeed([]byte("0123456789abcdef0123456789abcdef"))
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	issuer, err := NewControllerBeadsPermitIssuer(ControllerBeadsPermitIssuerConfig{
		Audience: "audience-a", ProjectID: "project-a", Database: "beads_fixture",
		KeyID: "key-a", Issuer: "issuer-a", Lifetime: time.Minute,
		Signer: privateKey, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewControllerBeadsPermitIssuer: %v", err)
	}

	request := ControllerProtectedCreateAndLinkRequest{
		Actor: "controller-a",
		Record: ControllerProtectedRecord{
			ID: "bd-protected-1", Title: "Protected record", ProtectionClass: "source-decision",
		},
		Links: []ControllerDependencyLink{
			{SourceID: "bd-protected-1", TargetID: "bd-map", Type: "relates-to"},
			{SourceID: "bd-prerequisite", TargetID: "bd-protected-1", Type: "blocks"},
		},
		ReceiptID: "excluded-receipt",
	}
	token, err := issuer.IssueProtectedCreateAndLink(request, "replay-identifier-0001")
	if err != nil {
		t.Fatalf("IssueProtectedCreateAndLink: %v", err)
	}

	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		t.Fatalf("permit token has %d segments, want 2", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || base64.RawURLEncoding.EncodeToString(payload) != parts[0] {
		t.Fatalf("permit payload is not canonical raw URL base64: %v", err)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || base64.RawURLEncoding.EncodeToString(signature) != parts[1] {
		t.Fatalf("permit signature is not canonical raw URL base64: %v", err)
	}
	const wantPayload = `{"schema_version":"beads.protected-mutation-permit.v1","purpose":"protected_mutation_permit","key_id":"key-a","issuer":"issuer-a","audience":"audience-a","project_id":"project-a","database":"beads_fixture","operation":"issue.batch_apply","resource_ids":["bd-map","bd-prerequisite","bd-protected-1"],"request_digest":"1a17901ea4ab71a9c88ad6981d904ad84860a3e9f8c0d0a00dbaee814fd88514","replay_id":"replay-identifier-0001","issued_at":"2026-01-02T03:04:05Z","expires_at":"2026-01-02T03:05:05Z"}`
	if string(payload) != wantPayload {
		t.Fatalf("permit payload differs from pinned Beads v1 fixture:\n got: %s\nwant: %s", payload, wantPayload)
	}
	if len(signature) != ed25519.SignatureSize || !ed25519.Verify(privateKey.Public().(ed25519.PublicKey), append([]byte(controllerBeadsPermitSigningDomain), payload...), signature) {
		t.Fatal("permit signature did not verify against the pinned signing domain")
	}
	const wantToken = "eyJzY2hlbWFfdmVyc2lvbiI6ImJlYWRzLnByb3RlY3RlZC1tdXRhdGlvbi1wZXJtaXQudjEiLCJwdXJwb3NlIjoicHJvdGVjdGVkX211dGF0aW9uX3Blcm1pdCIsImtleV9pZCI6ImtleS1hIiwiaXNzdWVyIjoiaXNzdWVyLWEiLCJhdWRpZW5jZSI6ImF1ZGllbmNlLWEiLCJwcm9qZWN0X2lkIjoicHJvamVjdC1hIiwiZGF0YWJhc2UiOiJiZWFkc19maXh0dXJlIiwib3BlcmF0aW9uIjoiaXNzdWUuYmF0Y2hfYXBwbHkiLCJyZXNvdXJjZV9pZHMiOlsiYmQtbWFwIiwiYmQtcHJlcmVxdWlzaXRlIiwiYmQtcHJvdGVjdGVkLTEiXSwicmVxdWVzdF9kaWdlc3QiOiIxYTE3OTAxZWE0YWI3MWE5Yzg4YWQ2OTgxZDkwNGFkODQ4NjBhM2U5ZjhjMGQwYTAwZGJhZWU4MTRmZDg4NTE0IiwicmVwbGF5X2lkIjoicmVwbGF5LWlkZW50aWZpZXItMDAwMSIsImlzc3VlZF9hdCI6IjIwMjYtMDEtMDJUMDM6MDQ6MDVaIiwiZXhwaXJlc19hdCI6IjIwMjYtMDEtMDJUMDM6MDU6MDVaIn0.aw2BYvQ8hUnbIrYjMOC4I2Si7hLNybejNdTbmDtBuU07qK8n80ZbwLZcCUPEwMDZtmlUyzhgfk-W8K1vpcgNBQ"
	if token != wantToken {
		t.Fatalf("permit token differs from the pinned Beads v1 fixture:\n got: %s\nwant: %s", token, wantToken)
	}
}

func TestControllerBeadsPermitIssuerSignsFrontierCreateOnly(t *testing.T) {
	privateKey := ed25519.NewKeyFromSeed([]byte("0123456789abcdef0123456789abcdef"))
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	issuer, err := NewControllerBeadsPermitIssuer(ControllerBeadsPermitIssuerConfig{
		Audience: "audience-a", ProjectID: "project-a", Database: "beads_fixture",
		KeyID: "key-a", Issuer: "issuer-a", Lifetime: time.Minute,
		Signer: privateKey, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewControllerBeadsPermitIssuer: %v", err)
	}
	request := controllerBeadsFrontierDigestRequest(false)
	token, err := issuer.IssueProtectedCreateAndLink(request, "replay-identifier-0002")
	if err != nil {
		t.Fatalf("IssueProtectedCreateAndLink: %v", err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		t.Fatalf("permit token has %d segments, want 2", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || base64.RawURLEncoding.EncodeToString(payload) != parts[0] {
		t.Fatalf("permit payload is not canonical raw URL base64: %v", err)
	}
	var claims controllerBeadsPermitClaimsV1
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("decode permit claims: %v", err)
	}
	digest, err := controllerBeadsProtectedBatchDigest(request)
	if err != nil {
		t.Fatalf("controllerBeadsProtectedBatchDigest: %v", err)
	}
	if claims.RequestDigest != digest || !reflect.DeepEqual(claims.ResourceIDs, []string{request.Record.ID}) {
		t.Fatalf("create-only claims digest/resources = (%q, %v), want (%q, [%q])", claims.RequestDigest, claims.ResourceIDs, digest, request.Record.ID)
	}
}

func TestControllerBeadsPermitIssuerRejectsInvalidAuthorityAndInput(t *testing.T) {
	privateKey := ed25519.NewKeyFromSeed([]byte("0123456789abcdef0123456789abcdef"))
	base := ControllerBeadsPermitIssuerConfig{
		Audience: "audience-a", ProjectID: "project-a", Database: "beads_fixture",
		KeyID: "key-a", Issuer: "issuer-a", Lifetime: time.Minute, Signer: privateKey,
	}
	for name, mutate := range map[string]func(*ControllerBeadsPermitIssuerConfig){
		"missing audience":  func(config *ControllerBeadsPermitIssuerConfig) { config.Audience = "" },
		"overlong audience": func(config *ControllerBeadsPermitIssuerConfig) { config.Audience = strings.Repeat("a", 256) },
		"missing project":   func(config *ControllerBeadsPermitIssuerConfig) { config.ProjectID = "" },
		"missing database":  func(config *ControllerBeadsPermitIssuerConfig) { config.Database = "" },
		"missing key ID":    func(config *ControllerBeadsPermitIssuerConfig) { config.KeyID = "" },
		"missing issuer":    func(config *ControllerBeadsPermitIssuerConfig) { config.Issuer = "" },
		"zero lifetime":     func(config *ControllerBeadsPermitIssuerConfig) { config.Lifetime = 0 },
		"excess lifetime": func(config *ControllerBeadsPermitIssuerConfig) {
			config.Lifetime = 5*time.Minute + time.Nanosecond
		},
		"no signer": func(config *ControllerBeadsPermitIssuerConfig) { config.Signer = nil },
	} {
		t.Run(name, func(t *testing.T) {
			config := base
			mutate(&config)
			if _, err := NewControllerBeadsPermitIssuer(config); err == nil {
				t.Fatal("constructor accepted invalid authority configuration")
			}
		})
	}

	issuer, err := NewControllerBeadsPermitIssuer(base)
	if err != nil {
		t.Fatalf("NewControllerBeadsPermitIssuer: %v", err)
	}
	request := controllerBeadsPermitGoldenRequest()
	for _, replayID := range []string{"short", "-replay-identifier-0001", "replay identifier 0001", "réplay-identifier-0001"} {
		if _, err := issuer.IssueProtectedCreateAndLink(request, replayID); err == nil {
			t.Errorf("issuer accepted invalid replay ID %q", replayID)
		}
	}
	ordinary := request
	ordinary.Record.ProtectionClass = ""
	if _, err := issuer.IssueProtectedCreateAndLink(ordinary, "replay-identifier-0001"); err == nil {
		t.Fatal("issuer accepted an ordinary create")
	}
	withPermit := request
	withPermit.ProtectedPermit = "already-issued"
	if _, err := issuer.IssueProtectedCreateAndLink(withPermit, "replay-identifier-0001"); err == nil {
		t.Fatal("issuer replaced an existing permit")
	}
	linkRequest := controllerBeadsProtectedLinkRequest()
	if _, err := issuer.IssueProtectedLink(linkRequest, "short"); err == nil {
		t.Fatal("issuer accepted an invalid link replay ID")
	}
	linkRequest.ProtectedPermit = "already-issued"
	if _, err := issuer.IssueProtectedLink(linkRequest, "replay-link-identifier-0001"); err == nil {
		t.Fatal("issuer replaced an existing link permit")
	}
	linkRequest = controllerBeadsProtectedLinkRequest()
	linkRequest.Link.TargetID = linkRequest.Link.SourceID
	if _, err := issuer.IssueProtectedLink(linkRequest, "replay-link-identifier-0001"); err == nil {
		t.Fatal("issuer accepted a self dependency link")
	}
}

func TestControllerBeadsPermitIssuerRejectsBadEd25519Signature(t *testing.T) {
	privateKey := ed25519.NewKeyFromSeed([]byte("0123456789abcdef0123456789abcdef"))
	issuer, err := NewControllerBeadsPermitIssuer(ControllerBeadsPermitIssuerConfig{
		Audience: "audience-a", ProjectID: "project-a", Database: "beads_fixture",
		KeyID: "key-a", Issuer: "issuer-a", Lifetime: time.Minute,
		Signer: controllerBadEd25519Signer{public: privateKey.Public().(ed25519.PublicKey)},
	})
	if err != nil {
		t.Fatalf("NewControllerBeadsPermitIssuer: %v", err)
	}
	if _, err := issuer.IssueProtectedCreateAndLink(controllerBeadsPermitGoldenRequest(), "replay-identifier-0001"); err == nil {
		t.Fatal("issuer accepted a signature that did not verify")
	}
}

func TestControllerBeadsPermitIssuerRequiresEd25519Signer(t *testing.T) {
	_, err := NewControllerBeadsPermitIssuer(ControllerBeadsPermitIssuerConfig{
		Audience: "audience-a", ProjectID: "project-a", Database: "beads_fixture",
		KeyID: "key-a", Issuer: "issuer-a", Lifetime: time.Minute,
		Signer: controllerNonEd25519Signer{},
	})
	if err == nil {
		t.Fatal("constructor accepted a signer with a non-Ed25519 public key")
	}
}

type controllerBadEd25519Signer struct {
	public ed25519.PublicKey
}

func (signer controllerBadEd25519Signer) Public() crypto.PublicKey { return signer.public }

func (signer controllerBadEd25519Signer) Sign(_ io.Reader, _ []byte, _ crypto.SignerOpts) ([]byte, error) {
	return make([]byte, ed25519.SignatureSize), nil
}

type controllerNonEd25519Signer struct{}

func (controllerNonEd25519Signer) Public() crypto.PublicKey { return struct{}{} }

func (controllerNonEd25519Signer) Sign(_ io.Reader, _ []byte, _ crypto.SignerOpts) ([]byte, error) {
	return nil, errors.New("unused")
}

func controllerBeadsPermitGoldenRequest() ControllerProtectedCreateAndLinkRequest {
	return ControllerProtectedCreateAndLinkRequest{
		Actor: "controller-a",
		Record: ControllerProtectedRecord{
			ID: "bd-protected-1", Title: "Protected record", ProtectionClass: "source-decision",
		},
		Links: []ControllerDependencyLink{
			{SourceID: "bd-protected-1", TargetID: "bd-map", Type: "relates-to"},
			{SourceID: "bd-prerequisite", TargetID: "bd-protected-1", Type: "blocks"},
		},
	}
}
