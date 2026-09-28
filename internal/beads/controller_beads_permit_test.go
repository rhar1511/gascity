package beads

import (
	"crypto"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestControllerBeadsProtectedBatchDigestMatchesPinnedGolden(t *testing.T) {
	request := ControllerProtectedCreateAndLinkRequest{
		Actor: "controller-a",
		Record: ControllerProtectedRecord{
			ID: "bd-protected-1", Title: "Protected record", ProtectionClass: "source-decision",
		},
		Links: []ControllerDependencyLink{
			{SourceID: "bd-protected-1", TargetID: "bd-map", Type: "relates-to"},
			{SourceID: "bd-prerequisite", TargetID: "bd-protected-1", Type: "blocks"},
		},
		ReceiptID:       "excluded-receipt",
		ProtectedPermit: "excluded-permit",
	}

	digest, err := controllerBeadsProtectedBatchDigest(request)
	if err != nil {
		t.Fatalf("controllerBeadsProtectedBatchDigest: %v", err)
	}
	const want = "1a17901ea4ab71a9c88ad6981d904ad84860a3e9f8c0d0a00dbaee814fd88514"
	if digest != want {
		t.Fatalf("controller digest = %q, want pinned Beads digest %q", digest, want)
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
