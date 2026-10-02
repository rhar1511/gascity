package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/qualification"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionauthority"
)

type sessionAuthorityTestState struct {
	*fakeState
	configSHA string
}

func (s sessionAuthorityTestState) QualificationReport() qualification.ControllerReport {
	return qualification.ControllerReport{Qualification: qualification.Snapshot{EffectiveConfigIdentitySHA256: s.configSHA}}
}

func TestSessionAuthorityTransitionPersistsExactProofAndLaunchRevalidates(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	configSHA := strings.Repeat("a", 64)
	state := sessionAuthorityTestState{fakeState: fs, configSHA: configSHA}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	trust := sessionauthority.TrustConfig{
		Keys:        []sessionauthority.TrustedKey{{KeyID: "profile-key", PublicKey: base64.StdEncoding.EncodeToString(pub)}},
		Authorities: []sessionauthority.Authority{{KeyID: "profile-key", Issuer: "ricky", Subject: "operator", Profiles: []sessionauthority.Profile{sessionauthority.ProfileWorker, sessionauthority.ProfileOperator}}},
	}
	trustRaw, _ := json.Marshal(trust)
	trustPath := filepath.Join(t.TempDir(), "session-authority.json")
	if err := os.WriteFile(trustPath, trustRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(sessionauthority.HostTrustFileEnv, trustPath)

	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"myrig/worker"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create status = %d: %s", w.Code, w.Body.String())
	}
	accepted := decodeAsyncAccepted(t, w.Body)
	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("create failed: %+v", failure)
	}
	suspendSessionForPermissionModeTest(t, fs, success.Session.ID)

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	info, err := mgr.Get(success.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	generation, _ := strconv.ParseUint(info.Generation, 10, 64)
	now := time.Now().UTC()
	verifier, err := sessionauthority.NewVerifier(trust, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	claims := sessionauthority.Claims{
		SchemaVersion: sessionauthority.SchemaVersionV1, AuthorizationID: "profile-auth-1", TokenID: "profile-token-1",
		KeyID: "profile-key", Issuer: "ricky", Subject: "operator", City: fs.CityName(),
		SessionID: info.ID, Generation: generation, EffectiveConfigSHA256: configSHA,
		FromProfile: sessionauthority.ProfileDesign, ToProfile: sessionauthority.ProfileWorker,
		PermissionMode: "plan", IssuedAt: now.Add(-time.Minute).Unix(), ExpiresAt: now.Add(time.Minute).Unix(),
	}
	token := mintSessionAuthorityToken(t, priv, claims)
	body, _ := json.Marshal(SessionPermissionModeBody{
		PermissionMode: "plan", AuthorityProfile: string(sessionauthority.ProfileWorker),
		ExpectedGeneration: generation, EffectiveConfigSHA256: configSHA, Authorization: token,
	})
	req = newPostRequest(cityURL(fs, "/session/"+info.ID+"/permission-mode"), strings.NewReader(string(body)))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("transition status = %d: %s", w.Code, w.Body.String())
	}
	b, err := fs.cityBeadStore.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Metadata[sessionauthority.MetadataProfile] != string(sessionauthority.ProfileWorker) || b.Metadata[sessionauthority.MetadataAuthorization] == "" {
		t.Fatalf("authority metadata missing: %#v", b.Metadata)
	}
	var records []sessionauthority.TransitionRecord
	if err := json.Unmarshal([]byte(b.Metadata[sessionauthority.MetadataTransitions]), &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Outcome != "accepted" || records[0].AuthorizationID != "profile-auth-1" {
		t.Fatalf("transition records = %#v", records)
	}
	req = newPostRequest(cityURL(fs, "/session/"+info.ID+"/permission-mode"), strings.NewReader(string(body)))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("idempotent transition status = %d: %s", w.Code, w.Body.String())
	}
	b, _ = fs.cityBeadStore.Get(info.ID)
	if err := json.Unmarshal([]byte(b.Metadata[sessionauthority.MetadataTransitions]), &records); err != nil || len(records) != 1 {
		t.Fatalf("idempotent transition appended history: records=%#v err=%v", records, err)
	}
	replayedClaims := claims
	replayedClaims.TokenID = "profile-token-2"
	replayedClaims.FromProfile = sessionauthority.ProfileWorker
	replayedToken := mintSessionAuthorityToken(t, priv, replayedClaims)
	replayedBody := SessionPermissionModeBody{
		PermissionMode: "plan", AuthorityProfile: string(sessionauthority.ProfileWorker),
		ExpectedGeneration: generation, EffectiveConfigSHA256: configSHA, Authorization: replayedToken,
	}
	replayedRaw, _ := json.Marshal(replayedBody)
	req = newPostRequest(cityURL(fs, "/session/"+info.ID+"/permission-mode"), strings.NewReader(string(replayedRaw)))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("replayed authorization status = %d, want 409: %s", w.Code, w.Body.String())
	}
	b, _ = fs.cityBeadStore.Get(info.ID)
	if err := json.Unmarshal([]byte(b.Metadata[sessionauthority.MetadataTransitions]), &records); err != nil || len(records) != 2 || records[1].Outcome != "denied" || records[1].Reason != "authorization_replayed" {
		t.Fatalf("replay denial history: records=%#v err=%v", records, err)
	}
	if _, err := srv.resolveWorkerSessionRuntimeWithMetadata(info, "", b.Metadata); err != nil {
		t.Fatalf("authorized runtime resolution: %v", err)
	}
	retainedHistory := make(map[string]string, len(b.Metadata))
	for key, value := range b.Metadata {
		retainedHistory[key] = value
	}
	delete(retainedHistory, sessionauthority.MetadataProfile)
	delete(retainedHistory, sessionauthority.MetadataAuthorization)
	delete(retainedHistory, sessionauthority.MetadataTemplateOverrides)
	t.Setenv(sessionauthority.HostTrustFileEnv, "")
	if _, err := srv.resolveWorkerSessionRuntimeWithMetadata(info, "", retainedHistory); !errors.Is(err, sessionauthority.ErrUnavailable) {
		t.Fatalf("retained authority history without host trust error = %v, want authority unavailable", err)
	}
	bareReq := newPostRequest(cityURL(fs, "/session/"+info.ID+"/permission-mode"), strings.NewReader(`{"permission_mode":"plan"}`))
	bareResp := httptest.NewRecorder()
	h.ServeHTTP(bareResp, bareReq)
	if bareResp.Code != http.StatusConflict {
		t.Fatalf("unsigned transition on protected session status = %d, want %d: %s", bareResp.Code, http.StatusConflict, bareResp.Body.String())
	}
	t.Setenv(sessionauthority.HostTrustFileEnv, trustPath)
	b.Metadata["template_overrides"] = `{`
	b.Metadata["command"] = "provider --privileged"
	if _, err := srv.resolveWorkerSessionRuntimeWithMetadata(info, "", b.Metadata); err == nil || !strings.Contains(err.Error(), "invalid authority-controlled template overrides") {
		t.Fatalf("malformed protected runtime error = %v, want fail-closed rejection", err)
	}
	b.Metadata["template_overrides"] = `{"permission_mode":"plan"}`
	b.Metadata["generation"] = strconv.FormatUint(generation+1, 10)
	if _, err := srv.resolveWorkerSessionRuntimeWithMetadata(info, "", b.Metadata); err == nil {
		t.Fatal("runtime resolution accepted stale authorization after generation change")
	}

	staleClaims := claims
	staleClaims.AuthorizationID = "profile-auth-stale"
	staleClaims.TokenID = "profile-token-stale"
	staleClaims.ToProfile = sessionauthority.ProfileOperator
	staleClaims.PermissionMode = "default"
	staleToken := mintSessionAuthorityToken(t, priv, staleClaims)
	staleWant := sessionauthority.Expectation{
		City: fs.CityName(), SessionID: info.ID, Generation: generation, EffectiveConfigSHA256: configSHA,
		FromProfile: sessionauthority.ProfileDesign, ToProfile: sessionauthority.ProfileOperator, PermissionMode: "default",
	}
	staleAuth, err := verifier.Verify(staleToken, staleWant)
	if err != nil {
		t.Fatal(err)
	}
	staleRecord := sessionauthority.TransitionRecord{
		AttemptedAt: now.Format(time.RFC3339Nano), Outcome: "accepted", Reason: "authorized",
		SessionID: info.ID, Generation: generation, EffectiveConfigSHA256: configSHA,
		FromProfile: sessionauthority.ProfileDesign, ToProfile: sessionauthority.ProfileOperator,
		PermissionMode: "default", AuthorizationID: staleAuth.Claims.AuthorizationID, Principal: staleAuth.Principal,
	}
	if _, err := mgr.UpdateAuthorityProfile(info.ID, generation, "default", sessionauthority.ProfileOperator, staleAuth, staleRecord); !errors.Is(err, sessionauthority.ErrTargetMismatch) {
		t.Fatalf("stale from-profile update error = %v, want target mismatch", err)
	}
	afterStale, err := fs.cityBeadStore.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := afterStale.Metadata[sessionauthority.MetadataProfile]; got != string(sessionauthority.ProfileWorker) {
		t.Fatalf("profile after stale transition = %q, want worker", got)
	}
	if err := fs.cityBeadStore.SetMetadata(info.ID, sessionauthority.MetadataProfile, "malformed"); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.UpdateAuthorityProfile(info.ID, generation, "default", sessionauthority.ProfileOperator, staleAuth, staleRecord); !errors.Is(err, sessionauthority.ErrTargetMismatch) {
		t.Fatalf("malformed current profile error = %v, want target mismatch", err)
	}
	if err := fs.cityBeadStore.SetMetadata(info.ID, sessionauthority.MetadataProfile, string(sessionauthority.ProfileWorker)); err != nil {
		t.Fatal(err)
	}

	closedClaims := claims
	closedClaims.AuthorizationID = "profile-auth-closed"
	closedClaims.TokenID = "profile-token-closed"
	closedClaims.FromProfile = sessionauthority.ProfileWorker
	closedClaims.ToProfile = sessionauthority.ProfileOperator
	closedClaims.PermissionMode = "default"
	closedToken := mintSessionAuthorityToken(t, priv, closedClaims)
	closedWant := sessionauthority.Expectation{
		City: fs.CityName(), SessionID: info.ID, Generation: generation, EffectiveConfigSHA256: configSHA,
		FromProfile: sessionauthority.ProfileWorker, ToProfile: sessionauthority.ProfileOperator, PermissionMode: "default",
	}
	closedAuth, err := verifier.Verify(closedToken, closedWant)
	if err != nil {
		t.Fatal(err)
	}
	closedRecord := sessionauthority.TransitionRecord{
		AttemptedAt: now.Format(time.RFC3339Nano), Outcome: "accepted", Reason: "authorized",
		SessionID: info.ID, Generation: generation, EffectiveConfigSHA256: configSHA,
		FromProfile: sessionauthority.ProfileWorker, ToProfile: sessionauthority.ProfileOperator,
		PermissionMode: "default", AuthorizationID: closedAuth.Claims.AuthorizationID, Principal: closedAuth.Principal,
	}
	if err := fs.cityBeadStore.Close(info.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.UpdateAuthorityProfile(info.ID, generation, "default", sessionauthority.ProfileOperator, closedAuth, closedRecord); !errors.Is(err, session.ErrSessionClosed) {
		t.Fatalf("closed session update error = %v, want session closed", err)
	}
	afterClose, err := fs.cityBeadStore.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := afterClose.Metadata[sessionauthority.MetadataProfile]; got != string(sessionauthority.ProfileWorker) {
		t.Fatalf("profile after close race = %q, want worker", got)
	}
}

func TestSessionAuthorityActiveSessionDenialIsRecorded(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	configSHA := strings.Repeat("c", 64)
	state := sessionAuthorityTestState{fakeState: fs, configSHA: configSHA}
	t.Setenv(sessionauthority.HostTrustFileEnv, filepath.Join(t.TempDir(), "missing.json"))
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"myrig/worker"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	accepted := decodeAsyncAccepted(t, w.Body)
	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("create failed: %+v", failure)
	}
	info, err := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp).Get(success.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	generation, _ := strconv.ParseUint(info.Generation, 10, 64)
	body, _ := json.Marshal(SessionPermissionModeBody{
		PermissionMode: "plan", AuthorityProfile: string(sessionauthority.ProfileWorker), ExpectedGeneration: generation,
		EffectiveConfigSHA256: configSHA, Authorization: "invalid",
	})
	req = newPostRequest(cityURL(fs, "/session/"+info.ID+"/permission-mode"), strings.NewReader(string(body)))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("active denial status = %d, want 409: %s", w.Code, w.Body.String())
	}
	b, err := fs.cityBeadStore.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	var records []sessionauthority.TransitionRecord
	if err := json.Unmarshal([]byte(b.Metadata[sessionauthority.MetadataTransitions]), &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Outcome != "denied" || records[0].Reason != "session_active" {
		t.Fatalf("active denial history = %#v", records)
	}
}

func TestSessionAuthorityDenialIsRecorded(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	configSHA := strings.Repeat("b", 64)
	state := sessionAuthorityTestState{fakeState: fs, configSHA: configSHA}
	t.Setenv(sessionauthority.HostTrustFileEnv, filepath.Join(t.TempDir(), "missing.json"))
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"myrig/worker"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	accepted := decodeAsyncAccepted(t, w.Body)
	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("create failed: %+v", failure)
	}
	suspendSessionForPermissionModeTest(t, fs, success.Session.ID)
	info, err := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp).Get(success.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	generation, _ := strconv.ParseUint(info.Generation, 10, 64)
	body, _ := json.Marshal(SessionPermissionModeBody{
		PermissionMode: "plan", AuthorityProfile: string(sessionauthority.ProfileWorker), ExpectedGeneration: generation,
		EffectiveConfigSHA256: configSHA, Authorization: "invalid",
	})
	req = newPostRequest(cityURL(fs, "/session/"+info.ID+"/permission-mode"), strings.NewReader(string(body)))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("denial status = %d, want 503: %s", w.Code, w.Body.String())
	}
	b, _ := fs.cityBeadStore.Get(info.ID)
	var records []sessionauthority.TransitionRecord
	if err := json.Unmarshal([]byte(b.Metadata[sessionauthority.MetadataTransitions]), &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Outcome != "denied" || records[0].Reason != "authority_unavailable" {
		t.Fatalf("denial records = %#v", records)
	}
}

func mintSessionAuthorityToken(t *testing.T, key ed25519.PrivateKey, claims sessionauthority.Claims) string {
	t.Helper()
	signing, err := sessionauthority.SigningBytes(claims)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(claims)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, signing))
}
