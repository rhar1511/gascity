package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/citywriteauth"
	"github.com/gastownhall/gascity/internal/qualification"
	"github.com/gastownhall/gascity/internal/retirementrelease"
)

type retirementSourceState struct {
	State
	adapter *retirementrelease.Adapter
}

func TestRetirementSourceHTTPCommonEnvelopeLimit(t *testing.T) {
	f := newHumanSourceHTTPFixture(t)
	rec := f.request(t, http.MethodPost, "/retirement-release/verify", "", retirementrelease.Request{Gate: "human-review", ManifestJSON: strings.Repeat("x", citywriteauth.MaxHTTPBodyBytes), RetainedBase64: "e30="})
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("signed over-budget envelope: %d %s", rec.Code, rec.Body.String())
	}
	if maxWriteBodyBytes != citywriteauth.MaxHTTPBodyBytes {
		t.Fatal("source and city-write envelope budgets diverged")
	}
}

func (s *retirementSourceState) RetirementSourceAdapter() *retirementrelease.Adapter {
	return s.adapter
}

func TestRetirementSourceHTTPDefaultOffAndRejectsCallerVerdicts(t *testing.T) {
	f := newHumanSourceHTTPFixture(t)
	path := "/retirement-release/verify"
	rec := f.request(t, http.MethodPost, path, "", retirementrelease.Request{Gate: "human-review", RequestSHA256: "untrusted", ManifestJSON: "{}", RetainedBase64: "e30="})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing trusted adapter status=%d body=%s", rec.Code, rec.Body.String())
	}
	wrapped := &retirementSourceState{State: f.state, adapter: retirementrelease.NewAdapter(retirementrelease.Options{})}
	pub := f.workerKey.Public().(ed25519.PublicKey)
	f.handler = writeAuthMiddleware(newTestWriteVerifier(t, pub, f.now), false, newTestCityHandler(t, wrapped))
	v := decodeHumanSource[retirementrelease.Verdict](t, f.request(t, http.MethodPost, path, "", retirementrelease.Request{Gate: "human-review"}))
	if v.Status != "unavailable" || v.Assurance != "unavailable" {
		t.Fatalf("disabled adapter elevated source: %+v", v)
	}
	rec = f.request(t, http.MethodPost, path, "", map[string]any{"gate": "human-review", "request_sha256": "forged", "manifest_json": "{}", "retained_base64": "e30=", "controller_authorized": true})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("caller verdict accepted: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRetirementSourceHTTPRequiresAuthenticationEvenOnUnhardenedListener(t *testing.T) {
	state := newFakeState(t)
	handler := newTestCityHandler(t, &retirementSourceState{State: state, adapter: retirementrelease.NewAdapter(retirementrelease.Options{})})
	rec := postDecisionFrontier(t, handler, cityURL(state, "/retirement-release/verify"), "", retirementrelease.Request{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unsigned retirement source route: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRetirementSourceHTTPMalformedManifestCannotReachComposition(t *testing.T) {
	f := newHumanSourceHTTPFixture(t)
	called := false
	wrapped := &retirementSourceState{State: f.state, adapter: retirementrelease.NewAdapter(retirementrelease.Options{Enabled: true, Now: func() time.Time { return f.now }, Compose: func(context.Context, retirementrelease.Binding) (retirementrelease.TrustedInputs, error) {
		called = true
		return retirementrelease.TrustedInputs{}, errors.New("malformed requests must not reach composition")
	}})}
	f.handler = writeAuthMiddleware(newTestWriteVerifier(t, f.workerKey.Public().(ed25519.PublicKey), f.now), false, newTestCityHandler(t, wrapped))
	rec := f.request(t, http.MethodPost, "/retirement-release/verify", "", retirementrelease.Request{Gate: "human-review", ManifestJSON: `{"verified":true}`, RetainedBase64: "e30=", RequestSHA256: string(bytes.Repeat([]byte{'a'}, 64))})
	if rec.Code != http.StatusBadRequest || called {
		t.Fatalf("malformed manifest reached composition=%v, status=%d body=%s", called, rec.Code, rec.Body.String())
	}
}

func TestRetirementSourceHTTPErrorCategories(t *testing.T) {
	for _, tc := range []struct {
		name, status, reason string
		cause                error
		want                 int
	}{
		{"invalid", "rejected", "manifest_invalid", errors.New("invalid manifest"), http.StatusBadRequest},
		{"permission", "unavailable", "scoped_permission_denied", qualification.ErrCompatibilityDenied, http.StatusForbidden},
		{"composition", "unavailable", "trusted_composition_failed", errors.New("loader unavailable"), http.StatusServiceUnavailable},
		{"context", "unavailable", "context_changed", errors.New("epoch changed"), http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := retirementSourceAPIError(retirementrelease.Verdict{Status: tc.status, Reason: tc.reason}, tc.cause)
			status, ok := err.(interface{ GetStatus() int })
			if !ok || status.GetStatus() != tc.want {
				t.Fatalf("error category %s: %v", tc.name, err)
			}
		})
	}
}
