package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/decisionfrontier"
)

func TestDecisionAnswerGrantVerifierBindsCompleteChallengeAndPrincipal(t *testing.T) {
	verifier, privateKey, now := newDecisionAnswerVerifier(t, []string{DecisionAnswerScope})
	challenge, submission := decisionAnswerTestChallenge()
	claims := decisionAnswerTestClaims(challenge, now)
	submission.Proof = signDecisionAnswerClaims(t, privateKey, claims)

	verified, err := verifier.VerifyDecisionAnswer(context.Background(), challenge, submission)
	if err != nil {
		t.Fatalf("verify exact answer grant: %v", err)
	}
	want := decisionfrontier.VerifiedAnswer{
		CityRef: challenge.CityRef, StoreRef: challenge.StoreRef,
		KeyID: claims.KeyID, Issuer: claims.Issuer, Subject: claims.Subject,
		WorkID: challenge.WorkID, WorkRevision: challenge.WorkRevision, WorkDigest: challenge.WorkDigest,
		MapID: challenge.MapID, TicketID: challenge.TicketID, QuestionID: challenge.QuestionID,
		QuestionVersion: challenge.QuestionVersion, AnswerDigest: challenge.AnswerDigest,
		Resolution: challenge.Resolution,
	}
	if verified != want {
		t.Fatalf("verified answer = %+v, want %+v", verified, want)
	}
	if _, err := verifier.VerifyDecisionAnswer(context.Background(), challenge, submission); err != nil {
		t.Fatalf("same signed challenge should remain retryable: %v", err)
	}
}

func TestDecisionAnswerGrantVerifierRejectsEveryChangedChallengeField(t *testing.T) {
	verifier, privateKey, now := newDecisionAnswerVerifier(t, []string{DecisionAnswerScope})
	want, submission := decisionAnswerTestChallenge()
	mutations := []struct {
		name   string
		change func(*decisionfrontier.AnswerChallenge)
	}{
		{"city", func(c *decisionfrontier.AnswerChallenge) { c.CityRef += ":other" }},
		{"store", func(c *decisionfrontier.AnswerChallenge) { c.StoreRef += ":other" }},
		{"work ID", func(c *decisionfrontier.AnswerChallenge) { c.WorkID += "-other" }},
		{"work revision", func(c *decisionfrontier.AnswerChallenge) { c.WorkRevision += "-other" }},
		{"work digest", func(c *decisionfrontier.AnswerChallenge) { c.WorkDigest = strings.Repeat("b", 64) }},
		{"map ID", func(c *decisionfrontier.AnswerChallenge) { c.MapID += "-other" }},
		{"ticket ID", func(c *decisionfrontier.AnswerChallenge) { c.TicketID += "-other" }},
		{"question ID", func(c *decisionfrontier.AnswerChallenge) { c.QuestionID += "-other" }},
		{"question version", func(c *decisionfrontier.AnswerChallenge) { c.QuestionVersion += "-other" }},
		{"answer digest", func(c *decisionfrontier.AnswerChallenge) { c.AnswerDigest = strings.Repeat("c", 64) }},
		{"resolution", func(c *decisionfrontier.AnswerChallenge) { c.Resolution = decisionfrontier.ResolutionDeclined }},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			changed := want
			tc.change(&changed)
			claims := decisionAnswerTestClaims(changed, now)
			attempt := submission
			attempt.Proof = signDecisionAnswerClaims(t, privateKey, claims)
			if _, err := verifier.VerifyDecisionAnswer(context.Background(), want, attempt); !errors.Is(err, ErrDecisionAnswerGrantTarget) {
				t.Fatalf("changed %s error = %v, want challenge mismatch", tc.name, err)
			}
		})
	}
}

func TestDecisionAnswerGrantVerifierSeparatesProtocolAndScopeFromPRGrants(t *testing.T) {
	verifier, privateKey, now := newDecisionAnswerVerifier(t, []string{DecisionAnswerScope, PRActionScopeMerge})
	challenge, submission := decisionAnswerTestChallenge()

	t.Run("wrong protocol", func(t *testing.T) {
		claims := decisionAnswerTestClaims(challenge, now)
		claims.Protocol = "gascity.pr-human-grant.v1"
		submission.Proof = signDecisionAnswerClaimsUnchecked(t, privateKey, claims)
		if _, err := verifier.VerifyDecisionAnswer(context.Background(), challenge, submission); !errors.Is(err, ErrDecisionAnswerGrantDomain) {
			t.Fatalf("wrong protocol error = %v, want domain rejection", err)
		}
	})

	t.Run("wrong scope", func(t *testing.T) {
		claims := decisionAnswerTestClaims(challenge, now)
		claims.Scope = PRActionScopeMerge
		submission.Proof = signDecisionAnswerClaims(t, privateKey, claims)
		if _, err := verifier.VerifyDecisionAnswer(context.Background(), challenge, submission); !errors.Is(err, ErrDecisionAnswerGrantScope) {
			t.Fatalf("wrong scope error = %v, want scope rejection", err)
		}
	})

	t.Run("PR token cannot cross protocols", func(t *testing.T) {
		prClaims := PRHumanGrantClaims{
			KeyID: "answer-key", Issuer: "city-governance", Subject: "reviewer@example.test",
			Scope: PRActionScopeMerge, City: "pilot", Owner: "acme", Repo: "widget", PullRequest: 12,
			HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40),
			PolicyVersion: "policy-1", WorkID: "wrk-42", IdempotencyKey: "merge-12",
			IssuedAt: now.Add(-time.Second).Unix(), ExpiresAt: now.Add(time.Minute).Unix(), TokenID: "pr-grant-1",
		}
		submission.Proof = signPRHumanGrant(t, privateKey, prClaims)
		if _, err := verifier.VerifyDecisionAnswer(context.Background(), challenge, submission); !errors.Is(err, ErrPRHumanGrantBadSignature) {
			t.Fatalf("PR token error = %v, want answer-domain signature rejection", err)
		}
	})

	t.Run("PR-only authority cannot answer", func(t *testing.T) {
		prOnly, privateKey, now := newDecisionAnswerVerifier(t, []string{PRActionScopeMerge})
		claims := decisionAnswerTestClaims(challenge, now)
		submission.Proof = signDecisionAnswerClaims(t, privateKey, claims)
		if _, err := prOnly.VerifyDecisionAnswer(context.Background(), challenge, submission); !errors.Is(err, ErrDecisionAnswerGrantScope) {
			t.Fatalf("PR-only authority error = %v, want scope rejection", err)
		}
	})
}

func TestDecisionAnswerGrantVerifierRejectsSharedWorkerKey(t *testing.T) {
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewPRHumanGrantVerifier(PRHumanTrustConfig{
		Keys: []PRHumanGrantKey{{KeyID: "answer-key", PublicKey: base64.StdEncoding.EncodeToString(publicKey)}},
		Authorities: []PRHumanAuthority{{
			KeyID: "answer-key", Issuer: "city-governance", Subject: "reviewer@example.test",
			Scopes: []string{DecisionAnswerScope},
		}},
	}, map[string]ed25519.PublicKey{"worker": publicKey}, nil)
	if !errors.Is(err, ErrPRHumanGrantSharedWorkerKey) {
		t.Fatalf("shared worker key error = %v, want rejection", err)
	}
}

func TestDecisionAnswerGrantVerifierRejectsExpiredBadSignatureAndMissingJTI(t *testing.T) {
	verifier, privateKey, now := newDecisionAnswerVerifier(t, []string{DecisionAnswerScope})
	challenge, submission := decisionAnswerTestChallenge()

	t.Run("expired", func(t *testing.T) {
		claims := decisionAnswerTestClaims(challenge, now.Add(-3*time.Minute))
		claims.ExpiresAt = now.Add(-2 * time.Minute).Unix()
		submission.Proof = signDecisionAnswerClaims(t, privateKey, claims)
		if _, err := verifier.VerifyDecisionAnswer(context.Background(), challenge, submission); !errors.Is(err, ErrPRHumanGrantExpired) {
			t.Fatalf("expired grant error = %v, want expired", err)
		}
	})

	t.Run("bad signature", func(t *testing.T) {
		claims := decisionAnswerTestClaims(challenge, now)
		token := signDecisionAnswerClaims(t, privateKey, claims)
		payload, encodedSignature, _ := strings.Cut(token, ".")
		signature, err := base64.RawURLEncoding.DecodeString(encodedSignature)
		if err != nil {
			t.Fatal(err)
		}
		signature[0] ^= 0xff
		submission.Proof = payload + "." + base64.RawURLEncoding.EncodeToString(signature)
		if _, err := verifier.VerifyDecisionAnswer(context.Background(), challenge, submission); !errors.Is(err, ErrPRHumanGrantBadSignature) {
			t.Fatalf("bad signature error = %v, want signature rejection", err)
		}
	})

	t.Run("missing JTI", func(t *testing.T) {
		claims := decisionAnswerTestClaims(challenge, now)
		claims.TokenID = ""
		submission.Proof = signDecisionAnswerClaims(t, privateKey, claims)
		if _, err := verifier.VerifyDecisionAnswer(context.Background(), challenge, submission); !errors.Is(err, ErrDecisionAnswerGrantMalformed) {
			t.Fatalf("missing JTI error = %v, want malformed grant", err)
		}
	})
}

func TestDecisionAnswerGrantVerifierRejectsNoncanonicalProofEncoding(t *testing.T) {
	verifier, privateKey, now := newDecisionAnswerVerifier(t, []string{DecisionAnswerScope})
	challenge, submission := decisionAnswerTestChallenge()
	claims := decisionAnswerTestClaims(challenge, now)
	token := signDecisionAnswerClaims(t, privateKey, claims)

	for _, test := range []struct {
		name  string
		proof string
	}{
		{name: "leading whitespace", proof: " " + token},
		{name: "trailing whitespace", proof: token + "\n"},
		{name: "unused base64 bits", proof: noncanonicalSignatureEncoding(t, token)},
	} {
		t.Run(test.name, func(t *testing.T) {
			attempt := submission
			attempt.Proof = test.proof
			if _, err := verifier.VerifyDecisionAnswer(context.Background(), challenge, attempt); !errors.Is(err, ErrDecisionAnswerGrantMalformed) {
				t.Fatalf("noncanonical proof error = %v, want malformed grant", err)
			}
		})
	}
}

func TestDecisionAnswerGrantVerifierRejectsUntrustedIdentityAndInvalidTimes(t *testing.T) {
	verifier, privateKey, now := newDecisionAnswerVerifier(t, []string{DecisionAnswerScope})
	challenge, submission := decisionAnswerTestChallenge()

	for _, test := range []struct {
		name   string
		change func(*DecisionAnswerGrantClaims)
		want   error
	}{
		{
			name: "unknown key",
			change: func(claims *DecisionAnswerGrantClaims) {
				claims.KeyID = "other-key"
			},
			want: ErrPRHumanGrantUnknownKey,
		},
		{
			name: "unknown issuer",
			change: func(claims *DecisionAnswerGrantClaims) {
				claims.Issuer = "other-governance"
			},
			want: ErrPRHumanGrantUnknownSubject,
		},
		{
			name: "unknown subject",
			change: func(claims *DecisionAnswerGrantClaims) {
				claims.Subject = "other-reviewer@example.test"
			},
			want: ErrPRHumanGrantUnknownSubject,
		},
		{
			name: "not yet valid",
			change: func(claims *DecisionAnswerGrantClaims) {
				claims.IssuedAt = now.Add(time.Minute).Unix()
				claims.ExpiresAt = now.Add(2 * time.Minute).Unix()
			},
			want: ErrPRHumanGrantExpired,
		},
		{
			name: "TTL too long",
			change: func(claims *DecisionAnswerGrantClaims) {
				claims.ExpiresAt = time.Unix(claims.IssuedAt, 0).Add(maxPRHumanGrantTTL + time.Second).Unix()
			},
			want: ErrPRHumanGrantExpired,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			claims := decisionAnswerTestClaims(challenge, now)
			test.change(&claims)
			submission.Proof = signDecisionAnswerClaims(t, privateKey, claims)
			if _, err := verifier.VerifyDecisionAnswer(context.Background(), challenge, submission); !errors.Is(err, test.want) {
				t.Fatalf("verification error = %v, want %v", err, test.want)
			}
		})
	}
}

func newDecisionAnswerVerifier(t *testing.T, scopes []string) (*DecisionAnswerGrantVerifier, ed25519.PrivateKey, time.Time) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	human, err := NewPRHumanGrantVerifier(PRHumanTrustConfig{
		Keys: []PRHumanGrantKey{{KeyID: "answer-key", PublicKey: base64.StdEncoding.EncodeToString(publicKey)}},
		Authorities: []PRHumanAuthority{{
			KeyID: "answer-key", Issuer: "city-governance", Subject: "reviewer@example.test", Scopes: scopes,
		}},
	}, nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	return NewDecisionAnswerGrantVerifier(human), privateKey, now
}

func decisionAnswerTestChallenge() (decisionfrontier.AnswerChallenge, decisionfrontier.AnswerSubmission) {
	challenge := decisionfrontier.AnswerChallenge{
		CityRef: "city:pilot", StoreRef: "rig:widget", WorkID: "wrk-42", WorkRevision: "revision-7",
		WorkDigest: strings.Repeat("a", 64), MapID: "decision-map-42", TicketID: "decision-ticket-1",
		QuestionID: "choose", QuestionVersion: "question-version-3",
		Resolution: decisionfrontier.ResolutionAnswered,
	}
	submission := decisionfrontier.AnswerSubmission{
		TicketID: challenge.TicketID, WorkRevision: challenge.WorkRevision,
		QuestionVersion: challenge.QuestionVersion, Resolution: challenge.Resolution,
		Text: "Use the bounded pilot.",
	}
	challenge.AnswerDigest = decisionfrontier.AnswerDigest(submission.Resolution, submission.Text)
	return challenge, submission
}

func decisionAnswerTestClaims(challenge decisionfrontier.AnswerChallenge, now time.Time) DecisionAnswerGrantClaims {
	return DecisionAnswerGrantClaims{
		Protocol: decisionAnswerProtocol, KeyID: "answer-key", Issuer: "city-governance",
		Subject: "reviewer@example.test", Scope: DecisionAnswerScope, Challenge: challenge,
		IssuedAt: now.Add(-time.Second).Unix(), ExpiresAt: now.Add(time.Minute).Unix(), TokenID: "answer-grant-1",
	}
}

func signDecisionAnswerClaims(t *testing.T, privateKey ed25519.PrivateKey, claims DecisionAnswerGrantClaims) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	input, err := DecisionAnswerGrantSigningInput(claims)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, input))
}

func signDecisionAnswerClaimsUnchecked(t *testing.T, privateKey ed25519.PrivateKey, claims DecisionAnswerGrantClaims) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	input := append(append([]byte(nil), decisionAnswerSigningDomain...), payload...)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, input))
}

func noncanonicalSignatureEncoding(t *testing.T, token string) string {
	t.Helper()
	payload, signature, ok := strings.Cut(token, ".")
	if !ok || signature == "" {
		t.Fatalf("invalid canonical token fixture %q", token)
	}
	want, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		t.Fatal(err)
	}
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	for _, replacement := range alphabet {
		candidate := signature[:len(signature)-1] + string(replacement)
		if candidate == signature {
			continue
		}
		decoded, decodeErr := base64.RawURLEncoding.DecodeString(candidate)
		if decodeErr == nil && bytes.Equal(decoded, want) {
			return payload + "." + candidate
		}
	}
	t.Fatal("signature fixture has no alternate noncanonical raw-base64 encoding")
	return ""
}
