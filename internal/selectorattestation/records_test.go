package selectorattestation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type mutableSource struct {
	bundle Bundle
	loads  int
	err    error
}

func (s *mutableSource) Load(context.Context) (Bundle, error) {
	s.loads++
	return s.bundle, s.err
}

type fixture struct {
	now             time.Time
	observationKey  ed25519.PrivateKey
	reviewKey       ed25519.PrivateKey
	revocationKey   ed25519.PrivateKey
	source          *mutableSource
	verifier        *Verifier
	observation     ObservationClaims
	observationWant ObservationExpectation
	review          ReviewClaims
	reviewWant      ReviewExpectation
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	observationPublic, observationPrivate := generateKey(t)
	reviewPublic, reviewPrivate := generateKey(t)
	revocationPublic, revocationPrivate := generateKey(t)
	now := time.Date(2026, 9, 28, 15, 0, 0, 0, time.UTC)
	observation := ObservationClaims{
		SchemaVersion:                 ObservationSchemaVersionV1,
		Purpose:                       ObservationKeyPurpose,
		RecordID:                      "selector-observation-0001",
		KeyID:                         "selector-observation-key",
		Issuer:                        "host-selector-collector",
		CollectorIdentity:             "collector:primary",
		Audience:                      "selector-retirement",
		Workspace:                     "workspace-a",
		CandidateManifestSHA256:       digest("1"),
		SelectorSnapshotSHA256:        digest("2"),
		ObservationLedgerSHA256:       digest("3"),
		SequenceStart:                 101,
		SequenceEnd:                   901,
		SequenceCompletenessResult:    "complete",
		CoverageResult:                "complete",
		RuntimeIdentitySHA256:         digest("4"),
		BuildIdentitySHA256:           digest("5"),
		ConfigIdentitySHA256:          digest("6"),
		OrderInventorySHA256:          digest("7"),
		ExternalWriterInventorySHA256: digest("8"),
		InFlightResolutionSHA256:      digest("9"),
		ObservationStartedAt:          now.Add(-49 * time.Hour).Format(time.RFC3339Nano),
		ObservationEndedAt:            now.Add(-time.Hour).Format(time.RFC3339Nano),
		IssuedAt:                      now.Add(-30 * time.Minute).Format(time.RFC3339Nano),
		ExpiresAt:                     now.Add(30 * time.Minute).Format(time.RFC3339Nano),
	}
	observationWant := ObservationExpectation{
		RecordID: observation.RecordID, CollectorIdentity: observation.CollectorIdentity,
		Audience: observation.Audience, Workspace: observation.Workspace,
		CandidateManifestSHA256: observation.CandidateManifestSHA256,
		SelectorSnapshotSHA256:  observation.SelectorSnapshotSHA256,
		ObservationLedgerSHA256: observation.ObservationLedgerSHA256,
		SequenceStart:           observation.SequenceStart, SequenceEnd: observation.SequenceEnd,
		SequenceCompletenessResult:    observation.SequenceCompletenessResult,
		CoverageResult:                observation.CoverageResult,
		RuntimeIdentitySHA256:         observation.RuntimeIdentitySHA256,
		BuildIdentitySHA256:           observation.BuildIdentitySHA256,
		ConfigIdentitySHA256:          observation.ConfigIdentitySHA256,
		OrderInventorySHA256:          observation.OrderInventorySHA256,
		ExternalWriterInventorySHA256: observation.ExternalWriterInventorySHA256,
		InFlightResolutionSHA256:      observation.InFlightResolutionSHA256,
		ObservationStartedAt:          mustTime(t, observation.ObservationStartedAt),
		ObservationEndedAt:            mustTime(t, observation.ObservationEndedAt),
	}
	observationRecordSHA256 := sha256Text(observationToken(t, observation, observationPrivate))
	review := ReviewClaims{
		SchemaVersion:           ReviewSchemaVersionV1,
		Purpose:                 ReviewKeyPurpose,
		RecordID:                "selector-review-0001",
		KeyID:                   "selector-review-key",
		Issuer:                  "host-human-review-authority",
		ReviewerIdentity:        "human:ricky",
		Audience:                observation.Audience,
		Workspace:               observation.Workspace,
		CandidateManifestSHA256: observation.CandidateManifestSHA256,
		ObservationRecordSHA256: observationRecordSHA256,
		Decision:                DecisionApproved,
		ReviewedDeltaSHA256:     digest("b"),
		ReviewedAt:              now.Add(-20 * time.Minute).Format(time.RFC3339Nano),
		IssuedAt:                now.Add(-10 * time.Minute).Format(time.RFC3339Nano),
		ExpiresAt:               now.Add(20 * time.Minute).Format(time.RFC3339Nano),
	}
	reviewWant := ReviewExpectation{
		RecordID: review.RecordID, ReviewerIdentity: review.ReviewerIdentity,
		Audience: review.Audience, Workspace: review.Workspace,
		CandidateManifestSHA256: review.CandidateManifestSHA256,
		ObservationRecordSHA256: review.ObservationRecordSHA256,
		Decision:                review.Decision, ReviewedDeltaSHA256: review.ReviewedDeltaSHA256,
		ReviewedAt: mustTime(t, review.ReviewedAt),
	}
	revocations := Revocations{
		SchemaVersion:    RevocationsSchemaVersionV1,
		Purpose:          RevocationKeyPurpose,
		KeyID:            "selector-attestation-revocation-key",
		Issuer:           "host-selector-revocation-authority",
		Audience:         observation.Audience,
		Workspace:        observation.Workspace,
		IssuedAt:         now.Add(-time.Minute).Format(time.RFC3339Nano),
		ExpiresAt:        now.Add(time.Hour).Format(time.RFC3339Nano),
		RevokedRecordIDs: []string{},
	}
	signedRevocations := signRevocations(t, revocations, revocationPrivate)
	source := &mutableSource{bundle: Bundle{
		Keys: []TrustedKey{
			{KeyID: observation.KeyID, Issuer: observation.Issuer, Subject: observation.CollectorIdentity, Purpose: ObservationKeyPurpose, PublicKey: base64.StdEncoding.EncodeToString(observationPublic)},
			{KeyID: review.KeyID, Issuer: review.Issuer, Subject: review.ReviewerIdentity, Purpose: ReviewKeyPurpose, PublicKey: base64.StdEncoding.EncodeToString(reviewPublic)},
			{KeyID: revocations.KeyID, Issuer: revocations.Issuer, Purpose: RevocationKeyPurpose, PublicKey: base64.StdEncoding.EncodeToString(revocationPublic)},
		},
		Revocations: signedRevocations,
	}}
	verifier, err := NewVerifier(source, Options{
		Audience: observation.Audience, RevocationPayloadSHA256: sha256Bytes(signedRevocations.Payload),
		MaxRecordAge: 2 * time.Hour, MaxRevocationAge: 2 * time.Hour,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return fixture{
		now: now, observationKey: observationPrivate, reviewKey: reviewPrivate,
		revocationKey: revocationPrivate, source: source, verifier: verifier,
		observation: observation, observationWant: observationWant,
		review: review, reviewWant: reviewWant,
	}
}

func TestVerifyObservationBindsEveryEvidenceIdentity(t *testing.T) {
	fixture := newFixture(t)
	verified, err := fixture.verifier.VerifyObservation(context.Background(), observationToken(t, fixture.observation, fixture.observationKey), fixture.observationWant)
	if err != nil {
		t.Fatal(err)
	}
	if !verified.Valid() || verified.RecordID != fixture.observation.RecordID ||
		verified.CandidateManifestSHA256 != fixture.observation.CandidateManifestSHA256 ||
		verified.ObservationLedgerSHA256 != fixture.observation.ObservationLedgerSHA256 ||
		verified.SequenceStart != fixture.observation.SequenceStart || verified.SequenceEnd != fixture.observation.SequenceEnd ||
		verified.SignedRecordSHA256 != fixture.review.ObservationRecordSHA256 {
		t.Fatalf("verified observation lost signed bindings: %#v", verified)
	}
	verified.CoverageResult = "tampered"
	if verified.Valid() {
		t.Fatal("mutated verified observation remained valid")
	}
}

func TestVerifyObservationRejectsEveryChangedExpectation(t *testing.T) {
	fixture := newFixture(t)
	token := observationToken(t, fixture.observation, fixture.observationKey)
	cases := map[string]func(*ObservationExpectation){
		"record":            func(w *ObservationExpectation) { w.RecordID = "selector-observation-other" },
		"collector":         func(w *ObservationExpectation) { w.CollectorIdentity = "collector:other" },
		"workspace":         func(w *ObservationExpectation) { w.Workspace = "workspace-b" },
		"candidate":         func(w *ObservationExpectation) { w.CandidateManifestSHA256 = digest("c") },
		"snapshot":          func(w *ObservationExpectation) { w.SelectorSnapshotSHA256 = digest("c") },
		"ledger":            func(w *ObservationExpectation) { w.ObservationLedgerSHA256 = digest("c") },
		"sequence start":    func(w *ObservationExpectation) { w.SequenceStart++ },
		"sequence end":      func(w *ObservationExpectation) { w.SequenceEnd++ },
		"completeness":      func(w *ObservationExpectation) { w.SequenceCompletenessResult = "incomplete" },
		"coverage":          func(w *ObservationExpectation) { w.CoverageResult = "incomplete" },
		"runtime":           func(w *ObservationExpectation) { w.RuntimeIdentitySHA256 = digest("c") },
		"build":             func(w *ObservationExpectation) { w.BuildIdentitySHA256 = digest("c") },
		"config":            func(w *ObservationExpectation) { w.ConfigIdentitySHA256 = digest("c") },
		"orders":            func(w *ObservationExpectation) { w.OrderInventorySHA256 = digest("c") },
		"external writers":  func(w *ObservationExpectation) { w.ExternalWriterInventorySHA256 = digest("c") },
		"in-flight":         func(w *ObservationExpectation) { w.InFlightResolutionSHA256 = digest("c") },
		"observation start": func(w *ObservationExpectation) { w.ObservationStartedAt = w.ObservationStartedAt.Add(time.Second) },
		"observation end":   func(w *ObservationExpectation) { w.ObservationEndedAt = w.ObservationEndedAt.Add(time.Second) },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			changed := fixture.observationWant
			change(&changed)
			if _, err := fixture.verifier.VerifyObservation(context.Background(), token, changed); !errors.Is(err, ErrBinding) {
				t.Fatalf("VerifyObservation changed %s = %v, want ErrBinding", name, err)
			}
		})
	}
	changedAudience := fixture.observationWant
	changedAudience.Audience = "other-audience"
	if _, err := fixture.verifier.VerifyObservation(context.Background(), token, changedAudience); !errors.Is(err, ErrAudience) {
		t.Fatalf("VerifyObservation changed audience = %v, want ErrAudience", err)
	}
}

func TestVerifyReviewBindsDecisionReviewerAndExactDigests(t *testing.T) {
	fixture := newFixture(t)
	verified, err := fixture.verifier.VerifyReview(context.Background(), reviewToken(t, fixture.review, fixture.reviewKey), fixture.reviewWant)
	if err != nil {
		t.Fatal(err)
	}
	if !verified.Valid() || verified.Decision != DecisionApproved || verified.ReviewerIdentity != "human:ricky" ||
		verified.CandidateManifestSHA256 != fixture.review.CandidateManifestSHA256 ||
		verified.ObservationRecordSHA256 != fixture.review.ObservationRecordSHA256 ||
		verified.ReviewedDeltaSHA256 != fixture.review.ReviewedDeltaSHA256 {
		t.Fatalf("verified review lost signed bindings: %#v", verified)
	}
	verified.Decision = DecisionRejected
	if verified.Valid() {
		t.Fatal("mutated verified review remained valid")
	}
	rejected := fixture.review
	rejected.Decision = DecisionRejected
	rejectedWant := fixture.reviewWant
	rejectedWant.Decision = DecisionRejected
	if _, err := fixture.verifier.VerifyReview(context.Background(), reviewToken(t, rejected, fixture.reviewKey), rejectedWant); err != nil {
		t.Fatalf("VerifyReview rejected decision: %v", err)
	}

	token := reviewToken(t, fixture.review, fixture.reviewKey)
	cases := map[string]func(*ReviewExpectation){
		"record":      func(w *ReviewExpectation) { w.RecordID = "selector-review-other" },
		"reviewer":    func(w *ReviewExpectation) { w.ReviewerIdentity = "human:other" },
		"workspace":   func(w *ReviewExpectation) { w.Workspace = "workspace-b" },
		"candidate":   func(w *ReviewExpectation) { w.CandidateManifestSHA256 = digest("c") },
		"observation": func(w *ReviewExpectation) { w.ObservationRecordSHA256 = digest("c") },
		"decision":    func(w *ReviewExpectation) { w.Decision = DecisionRejected },
		"deltas":      func(w *ReviewExpectation) { w.ReviewedDeltaSHA256 = digest("c") },
		"review time": func(w *ReviewExpectation) { w.ReviewedAt = w.ReviewedAt.Add(time.Second) },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			changed := fixture.reviewWant
			change(&changed)
			if _, err := fixture.verifier.VerifyReview(context.Background(), token, changed); !errors.Is(err, ErrBinding) {
				t.Fatalf("VerifyReview changed %s = %v, want ErrBinding", name, err)
			}
		})
	}
	changedAudience := fixture.reviewWant
	changedAudience.Audience = "other-audience"
	if _, err := fixture.verifier.VerifyReview(context.Background(), token, changedAudience); !errors.Is(err, ErrAudience) {
		t.Fatalf("VerifyReview changed audience = %v, want ErrAudience", err)
	}
}

func TestVerifyRecordsRejectWrongPurposeDomainAndAuthority(t *testing.T) {
	fixture := newFixture(t)
	t.Run("wrong observation purpose", func(t *testing.T) {
		changed := fixture.observation
		changed.Purpose = ReviewKeyPurpose
		token := rawToken(t, changed, fixture.observationKey, ObservationSigningDomain)
		if _, err := fixture.verifier.VerifyObservation(context.Background(), token, fixture.observationWant); !errors.Is(err, ErrPurpose) {
			t.Fatalf("VerifyObservation wrong purpose = %v, want ErrPurpose", err)
		}
	})
	t.Run("foreign signature domain", func(t *testing.T) {
		payload := canonicalObservation(t, fixture.observation)
		token := tokenForPayload(payload, ed25519.Sign(fixture.observationKey, append([]byte("gascity.compatibility-release.v1\n"), payload...)))
		if _, err := fixture.verifier.VerifyObservation(context.Background(), token, fixture.observationWant); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("VerifyObservation foreign domain = %v, want ErrBadSignature", err)
		}
	})
	t.Run("reviewer not bound to key", func(t *testing.T) {
		changed := fixture.review
		changed.ReviewerIdentity = "human:other"
		token := reviewToken(t, changed, fixture.reviewKey)
		want := fixture.reviewWant
		want.ReviewerIdentity = changed.ReviewerIdentity
		if _, err := fixture.verifier.VerifyReview(context.Background(), token, want); !errors.Is(err, ErrAuthority) {
			t.Fatalf("VerifyReview foreign reviewer = %v, want ErrAuthority", err)
		}
	})
}

func TestVerifyRecordsRejectFreshnessChronologyAndInvalidDecision(t *testing.T) {
	fixture := newFixture(t)
	t.Run("expired observation", func(t *testing.T) {
		changed := fixture.observation
		changed.ExpiresAt = fixture.now.Format(time.RFC3339Nano)
		if _, err := fixture.verifier.VerifyObservation(context.Background(), observationToken(t, changed, fixture.observationKey), fixture.observationWant); !errors.Is(err, ErrExpired) {
			t.Fatalf("VerifyObservation expired = %v, want ErrExpired", err)
		}
	})
	t.Run("future review", func(t *testing.T) {
		changed := fixture.review
		changed.IssuedAt = fixture.now.Add(time.Second).Format(time.RFC3339Nano)
		changed.ExpiresAt = fixture.now.Add(time.Hour).Format(time.RFC3339Nano)
		if _, err := fixture.verifier.VerifyReview(context.Background(), reviewToken(t, changed, fixture.reviewKey), fixture.reviewWant); !errors.Is(err, ErrNotYetValid) {
			t.Fatalf("VerifyReview future = %v, want ErrNotYetValid", err)
		}
	})
	t.Run("observation issued before window closes", func(t *testing.T) {
		changed := fixture.observation
		changed.IssuedAt = fixture.now.Add(-2 * time.Hour).Format(time.RFC3339Nano)
		if _, err := ObservationSigningBytes(changed); !errors.Is(err, ErrMalformed) {
			t.Fatalf("ObservationSigningBytes invalid chronology = %v, want ErrMalformed", err)
		}
	})
	t.Run("review issued before review time", func(t *testing.T) {
		changed := fixture.review
		changed.IssuedAt = fixture.now.Add(-30 * time.Minute).Format(time.RFC3339Nano)
		if _, err := ReviewSigningBytes(changed); !errors.Is(err, ErrMalformed) {
			t.Fatalf("ReviewSigningBytes invalid chronology = %v, want ErrMalformed", err)
		}
	})
	t.Run("invalid decision", func(t *testing.T) {
		changed := fixture.review
		changed.Decision = "accept"
		if _, err := ReviewSigningBytes(changed); !errors.Is(err, ErrMalformed) {
			t.Fatalf("ReviewSigningBytes invalid decision = %v, want ErrMalformed", err)
		}
	})
}

func TestVerifyRecordsReloadRevocations(t *testing.T) {
	fixture := newFixture(t)
	observationToken := observationToken(t, fixture.observation, fixture.observationKey)
	if _, err := fixture.verifier.VerifyObservation(context.Background(), observationToken, fixture.observationWant); err != nil {
		t.Fatal(err)
	}
	original := fixture.source.bundle.Revocations
	replacement := signRevocations(t, Revocations{
		SchemaVersion: RevocationsSchemaVersionV1, Purpose: RevocationKeyPurpose,
		KeyID: "selector-attestation-revocation-key", Issuer: "host-selector-revocation-authority",
		Audience: fixture.observation.Audience, Workspace: fixture.observation.Workspace,
		IssuedAt: fixture.now.Format(time.RFC3339Nano), ExpiresAt: fixture.now.Add(time.Hour).Format(time.RFC3339Nano),
		RevokedRecordIDs: []string{fixture.observation.RecordID, fixture.review.RecordID},
	}, fixture.revocationKey)
	fixture.source.bundle.Revocations = replacement
	if _, err := fixture.verifier.VerifyObservation(context.Background(), observationToken, fixture.observationWant); !errors.Is(err, ErrRevocationSnapshot) {
		t.Fatalf("VerifyObservation unpinned revocation rotation = %v, want ErrRevocationSnapshot", err)
	}
	options := fixture.verifier.options
	options.RevocationPayloadSHA256 = sha256Bytes(replacement.Payload)
	rotatedVerifier, err := NewVerifier(fixture.source, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rotatedVerifier.VerifyObservation(context.Background(), observationToken, fixture.observationWant); !errors.Is(err, ErrRevoked) {
		t.Fatalf("VerifyObservation revoked = %v, want ErrRevoked", err)
	}
	if _, err := rotatedVerifier.VerifyReview(context.Background(), reviewToken(t, fixture.review, fixture.reviewKey), fixture.reviewWant); !errors.Is(err, ErrRevoked) {
		t.Fatalf("VerifyReview revoked = %v, want ErrRevoked", err)
	}
	fixture.source.bundle.Revocations = original
	if _, err := rotatedVerifier.VerifyObservation(context.Background(), observationToken, fixture.observationWant); !errors.Is(err, ErrRevocationSnapshot) {
		t.Fatalf("VerifyObservation rolled-back revocations = %v, want ErrRevocationSnapshot", err)
	}
	if fixture.source.loads != 5 {
		t.Fatalf("source loads = %d, want one per verification", fixture.source.loads)
	}
}

func TestNewVerifierRequiresPinnedRevocationPayload(t *testing.T) {
	fixture := newFixture(t)
	options := fixture.verifier.options
	options.RevocationPayloadSHA256 = ""
	if _, err := NewVerifier(fixture.source, options); !errors.Is(err, ErrMalformed) {
		t.Fatalf("NewVerifier without revocation pin = %v, want ErrMalformed", err)
	}
}

func TestVerifyRecordsRejectMissingUnknownAmbiguousAndNoncanonicalMaterial(t *testing.T) {
	fixture := newFixture(t)
	token := observationToken(t, fixture.observation, fixture.observationKey)
	t.Run("unavailable source", func(t *testing.T) {
		changed := *fixture.source
		changed.err = errors.New("unavailable")
		verifier, err := NewVerifier(&changed, fixture.verifier.options)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := verifier.VerifyObservation(context.Background(), token, fixture.observationWant); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("VerifyObservation unavailable = %v, want ErrUnavailable", err)
		}
	})
	t.Run("missing revocation snapshot", func(t *testing.T) {
		changed := *fixture.source
		changed.bundle = fixture.source.bundle
		changed.bundle.Revocations = SignedRevocations{}
		verifier, err := NewVerifier(&changed, fixture.verifier.options)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := verifier.VerifyObservation(context.Background(), token, fixture.observationWant); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("VerifyObservation missing revocations = %v, want ErrUnavailable", err)
		}
	})
	t.Run("unknown key", func(t *testing.T) {
		changed := fixture.observation
		changed.KeyID = "unknown-selector-key"
		unknown := observationToken(t, changed, fixture.observationKey)
		if _, err := fixture.verifier.VerifyObservation(context.Background(), unknown, fixture.observationWant); !errors.Is(err, ErrUnknownKey) {
			t.Fatalf("VerifyObservation unknown key = %v, want ErrUnknownKey", err)
		}
	})
	t.Run("ambiguous key id", func(t *testing.T) {
		changed := *fixture.source
		changed.bundle = fixture.source.bundle
		changed.bundle.Keys = append([]TrustedKey(nil), changed.bundle.Keys...)
		changed.bundle.Keys = append(changed.bundle.Keys, changed.bundle.Keys[0])
		verifier, err := NewVerifier(&changed, fixture.verifier.options)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := verifier.VerifyObservation(context.Background(), token, fixture.observationWant); !errors.Is(err, ErrAmbiguous) {
			t.Fatalf("VerifyObservation duplicate key = %v, want ErrAmbiguous", err)
		}
	})
	t.Run("ambiguous reused key material", func(t *testing.T) {
		changed := *fixture.source
		changed.bundle = fixture.source.bundle
		changed.bundle.Keys = append([]TrustedKey(nil), changed.bundle.Keys...)
		changed.bundle.Keys[1].PublicKey = changed.bundle.Keys[0].PublicKey
		verifier, err := NewVerifier(&changed, fixture.verifier.options)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := verifier.VerifyObservation(context.Background(), token, fixture.observationWant); !errors.Is(err, ErrAmbiguous) {
			t.Fatalf("VerifyObservation reused key material = %v, want ErrAmbiguous", err)
		}
	})
	t.Run("noncanonical signed JSON", func(t *testing.T) {
		payload := append([]byte(" "), canonicalObservation(t, fixture.observation)...)
		noncanonical := tokenForPayload(payload, ed25519.Sign(fixture.observationKey, append([]byte(ObservationSigningDomain), payload...)))
		if _, err := fixture.verifier.VerifyObservation(context.Background(), noncanonical, fixture.observationWant); !errors.Is(err, ErrMalformed) {
			t.Fatalf("VerifyObservation noncanonical = %v, want ErrMalformed", err)
		}
	})
	t.Run("noncanonical revocation JSON", func(t *testing.T) {
		changed := *fixture.source
		changed.bundle = fixture.source.bundle
		payload := append([]byte(" "), changed.bundle.Revocations.Payload...)
		changed.bundle.Revocations = SignedRevocations{
			Payload: payload,
			Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(
				fixture.revocationKey,
				append([]byte(RevocationSigningDomain), payload...),
			)),
		}
		verifier, err := NewVerifier(&changed, fixture.verifier.options)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := verifier.VerifyObservation(context.Background(), token, fixture.observationWant); !errors.Is(err, ErrMalformed) {
			t.Fatalf("VerifyObservation noncanonical revocations = %v, want ErrMalformed", err)
		}
	})
	t.Run("duplicate revoked id", func(t *testing.T) {
		list := Revocations{
			SchemaVersion: RevocationsSchemaVersionV1, Purpose: RevocationKeyPurpose,
			KeyID: "selector-attestation-revocation-key", Issuer: "host-selector-revocation-authority",
			Audience: fixture.observation.Audience, Workspace: fixture.observation.Workspace,
			IssuedAt: fixture.now.Format(time.RFC3339Nano), ExpiresAt: fixture.now.Add(time.Hour).Format(time.RFC3339Nano),
			RevokedRecordIDs: []string{fixture.review.RecordID, fixture.review.RecordID},
		}
		if _, err := RevocationSigningBytes(list); !errors.Is(err, ErrAmbiguous) {
			t.Fatalf("RevocationSigningBytes duplicate record = %v, want ErrAmbiguous", err)
		}
	})
}

func generateKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return public, private
}

func digest(character string) string {
	return strings.Repeat(character, 64)
}

func sha256Text(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func observationToken(t *testing.T, claims ObservationClaims, private ed25519.PrivateKey) string {
	t.Helper()
	signed, err := ObservationSigningBytes(claims)
	if err != nil {
		t.Fatal(err)
	}
	payload := signed[len(ObservationSigningDomain):]
	return tokenForPayload(payload, ed25519.Sign(private, signed))
}

func reviewToken(t *testing.T, claims ReviewClaims, private ed25519.PrivateKey) string {
	t.Helper()
	signed, err := ReviewSigningBytes(claims)
	if err != nil {
		t.Fatal(err)
	}
	payload := signed[len(ReviewSigningDomain):]
	return tokenForPayload(payload, ed25519.Sign(private, signed))
}

func rawToken(t *testing.T, claims any, private ed25519.PrivateKey, domain string) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return tokenForPayload(payload, ed25519.Sign(private, append([]byte(domain), payload...)))
}

func canonicalObservation(t *testing.T, claims ObservationClaims) []byte {
	t.Helper()
	signed, err := ObservationSigningBytes(claims)
	if err != nil {
		t.Fatal(err)
	}
	return append([]byte(nil), signed[len(ObservationSigningDomain):]...)
}

func tokenForPayload(payload, signature []byte) string {
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func signRevocations(t *testing.T, claims Revocations, private ed25519.PrivateKey) SignedRevocations {
	t.Helper()
	signed, err := RevocationSigningBytes(claims)
	if err != nil {
		t.Fatal(err)
	}
	payload := append([]byte(nil), signed[len(RevocationSigningDomain):]...)
	return SignedRevocations{Payload: payload, Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, signed))}
}
