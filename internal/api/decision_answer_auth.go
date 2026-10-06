package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/decisionfrontier"
)

const decisionAnswerProtocol = "gascity.decision-answer.v1"

var decisionAnswerSigningDomain = []byte(decisionAnswerProtocol + "\x00")

// ErrDecisionAnswerGrantMalformed and the other grant errors distinguish the
// rejected human answer protocol, authority scope, and exact challenge binding.
var (
	ErrDecisionAnswerGrantMalformed = errors.New("decision answer grant is malformed")
	ErrDecisionAnswerGrantDomain    = errors.New("decision answer grant protocol is unsupported")
	ErrDecisionAnswerGrantScope     = errors.New("decision answer grant lacks decision.answer authority")
	ErrDecisionAnswerGrantTarget    = errors.New("decision answer grant does not match the current challenge")
)

// DecisionAnswerGrantClaims is the wire payload signed by the separate human
// answer protocol. Challenge is the complete current server-built challenge.
type DecisionAnswerGrantClaims struct {
	Protocol  string                           `json:"protocol"`
	KeyID     string                           `json:"kid"`
	Issuer    string                           `json:"iss"`
	Subject   string                           `json:"sub"`
	Scope     string                           `json:"scope"`
	Challenge decisionfrontier.AnswerChallenge `json:"challenge"`
	IssuedAt  int64                            `json:"iat"`
	ExpiresAt int64                            `json:"exp"`
	TokenID   string                           `json:"jti"`
}

// DecisionAnswerGrantVerifier checks signed answers with keys from the
// supervisor-managed human trust set. JTI identifies the short-lived grant;
// exact same-grant retries remain idempotent and do not consume a replay slot.
type DecisionAnswerGrantVerifier struct {
	human *PRHumanGrantVerifier
}

// NewDecisionAnswerGrantVerifier builds the answer protocol over the existing
// human keyring. Nil trust remains unavailable so callers fail closed.
func NewDecisionAnswerGrantVerifier(human *PRHumanGrantVerifier) *DecisionAnswerGrantVerifier {
	if human == nil {
		return nil
	}
	return &DecisionAnswerGrantVerifier{human: human}
}

// DecisionAnswerGrantSigningInput returns the domain-prefixed canonical bytes
// an answer authority signs with Ed25519.
func DecisionAnswerGrantSigningInput(claims DecisionAnswerGrantClaims) ([]byte, error) {
	if claims.Protocol != decisionAnswerProtocol {
		return nil, ErrDecisionAnswerGrantDomain
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return nil, fmt.Errorf("%w: encode claims", ErrDecisionAnswerGrantMalformed)
	}
	input := append([]byte(nil), decisionAnswerSigningDomain...)
	input = append(input, payload...)
	return input, nil
}

// VerifyDecisionAnswer authenticates one exact answer challenge and returns
// only the principal and challenge fields accepted by this verifier.
func (v *DecisionAnswerGrantVerifier) VerifyDecisionAnswer(
	ctx context.Context,
	challenge decisionfrontier.AnswerChallenge,
	submission decisionfrontier.AnswerSubmission,
) (decisionfrontier.VerifiedAnswer, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return decisionfrontier.VerifiedAnswer{}, err
		}
	}
	if !decisionAnswerSubmissionMatches(challenge, submission) {
		return decisionfrontier.VerifiedAnswer{}, ErrDecisionAnswerGrantTarget
	}
	claims, err := v.verify(submission.Proof, challenge)
	if err != nil {
		return decisionfrontier.VerifiedAnswer{}, err
	}
	return decisionfrontier.VerifiedAnswer{
		CityRef: challenge.CityRef, StoreRef: challenge.StoreRef,
		KeyID: claims.KeyID, Issuer: claims.Issuer, Subject: claims.Subject,
		WorkID: challenge.WorkID, WorkRevision: challenge.WorkRevision, WorkDigest: challenge.WorkDigest,
		MapID: challenge.MapID, TicketID: challenge.TicketID, QuestionID: challenge.QuestionID,
		QuestionVersion: challenge.QuestionVersion, AnswerDigest: challenge.AnswerDigest,
		Resolution: challenge.Resolution,
	}, nil
}

func (v *DecisionAnswerGrantVerifier) verify(token string, want decisionfrontier.AnswerChallenge) (DecisionAnswerGrantClaims, error) {
	if v == nil || v.human == nil {
		return DecisionAnswerGrantClaims{}, ErrPRHumanGrantUnavailable
	}
	if token == "" || token != strings.TrimSpace(token) || len(token) > maxPRHumanGrantBytes {
		return DecisionAnswerGrantClaims{}, ErrDecisionAnswerGrantMalformed
	}
	payload, encodedSignature, ok := strings.Cut(token, ".")
	if !ok || payload == "" || encodedSignature == "" || strings.Contains(encodedSignature, ".") {
		return DecisionAnswerGrantClaims{}, ErrDecisionAnswerGrantMalformed
	}
	encoding := base64.RawURLEncoding.Strict()
	decoded, err := encoding.DecodeString(payload)
	if err != nil || encoding.EncodeToString(decoded) != payload {
		return DecisionAnswerGrantClaims{}, fmt.Errorf("%w: payload encoding", ErrDecisionAnswerGrantMalformed)
	}
	signature, err := encoding.DecodeString(encodedSignature)
	if err != nil || len(signature) != ed25519.SignatureSize || encoding.EncodeToString(signature) != encodedSignature {
		return DecisionAnswerGrantClaims{}, fmt.Errorf("%w: signature encoding", ErrDecisionAnswerGrantMalformed)
	}
	var keyClaim struct {
		KeyID string `json:"kid"`
	}
	if err := json.Unmarshal(decoded, &keyClaim); err != nil || keyClaim.KeyID == "" {
		return DecisionAnswerGrantClaims{}, ErrDecisionAnswerGrantMalformed
	}
	publicKey, ok := v.human.keys[keyClaim.KeyID]
	if !ok {
		return DecisionAnswerGrantClaims{}, ErrPRHumanGrantUnknownKey
	}
	signingInput := make([]byte, 0, len(decisionAnswerSigningDomain)+len(decoded))
	signingInput = append(signingInput, decisionAnswerSigningDomain...)
	signingInput = append(signingInput, decoded...)
	if !ed25519.Verify(publicKey, signingInput, signature) {
		return DecisionAnswerGrantClaims{}, ErrPRHumanGrantBadSignature
	}

	var claims DecisionAnswerGrantClaims
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&claims); err != nil {
		return DecisionAnswerGrantClaims{}, fmt.Errorf("%w: claims", ErrDecisionAnswerGrantMalformed)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return DecisionAnswerGrantClaims{}, fmt.Errorf("%w: trailing claims", ErrDecisionAnswerGrantMalformed)
	}
	canonical, err := json.Marshal(claims)
	if err != nil || !bytes.Equal(canonical, decoded) {
		return DecisionAnswerGrantClaims{}, fmt.Errorf("%w: claims are not canonical JSON", ErrDecisionAnswerGrantMalformed)
	}
	if claims.Protocol != decisionAnswerProtocol {
		return DecisionAnswerGrantClaims{}, ErrDecisionAnswerGrantDomain
	}
	if claims.KeyID != keyClaim.KeyID || claims.Issuer == "" || claims.Subject == "" || claims.TokenID == "" {
		return DecisionAnswerGrantClaims{}, ErrDecisionAnswerGrantMalformed
	}
	binding := prHumanAuthorityKey{keyID: claims.KeyID, issuer: claims.Issuer, subject: claims.Subject}
	scopes, authorized := v.human.authorities[binding]
	if !authorized {
		return DecisionAnswerGrantClaims{}, ErrPRHumanGrantUnknownSubject
	}
	if claims.Scope != DecisionAnswerScope {
		return DecisionAnswerGrantClaims{}, ErrDecisionAnswerGrantScope
	}
	if _, ok := scopes[DecisionAnswerScope]; !ok {
		return DecisionAnswerGrantClaims{}, ErrDecisionAnswerGrantScope
	}
	if err := validateDecisionAnswerGrantTimes(claims, v.human.now()); err != nil {
		return DecisionAnswerGrantClaims{}, err
	}
	if !completeDecisionAnswerChallenge(want) || claims.Challenge != want {
		return DecisionAnswerGrantClaims{}, ErrDecisionAnswerGrantTarget
	}
	return claims, nil
}

func decisionAnswerSubmissionMatches(challenge decisionfrontier.AnswerChallenge, submission decisionfrontier.AnswerSubmission) bool {
	return challenge.TicketID == submission.TicketID &&
		challenge.WorkRevision == submission.WorkRevision &&
		challenge.QuestionVersion == submission.QuestionVersion &&
		challenge.Resolution == submission.Resolution &&
		challenge.AnswerDigest == decisionfrontier.AnswerDigest(submission.Resolution, submission.Text)
}

func completeDecisionAnswerChallenge(challenge decisionfrontier.AnswerChallenge) bool {
	return challenge.CityRef != "" && challenge.StoreRef != "" && challenge.WorkID != "" &&
		challenge.WorkRevision != "" && challenge.WorkDigest != "" && challenge.MapID != "" &&
		challenge.TicketID != "" && challenge.QuestionID != "" && challenge.QuestionVersion != "" &&
		challenge.AnswerDigest != "" && challenge.Resolution != ""
}

func validateDecisionAnswerGrantTimes(claims DecisionAnswerGrantClaims, now time.Time) error {
	if claims.IssuedAt <= 0 || claims.ExpiresAt <= claims.IssuedAt || claims.TokenID == "" {
		return ErrDecisionAnswerGrantMalformed
	}
	issuedAt := time.Unix(claims.IssuedAt, 0)
	expiresAt := time.Unix(claims.ExpiresAt, 0)
	if expiresAt.Sub(issuedAt) > maxPRHumanGrantTTL || now.After(expiresAt.Add(prHumanGrantSkew)) ||
		now.Add(prHumanGrantSkew).Before(issuedAt) {
		return ErrPRHumanGrantExpired
	}
	return nil
}
