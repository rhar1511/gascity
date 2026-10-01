//go:build cross_language_harness

package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/retirementrelease"
	"github.com/gastownhall/gascity/internal/selectorattestation"
	"github.com/gastownhall/gascity/internal/selectorinventory"
	"github.com/gastownhall/gascity/internal/selectorwriter"
)

type retirementExportExpectation struct {
	HTTPStatus int    `json:"http_status,omitempty"`
	Status     string `json:"status,omitempty"`
	Assurance  string `json:"assurance,omitempty"`
	Reason     string `json:"reason,omitempty"`
}
type retirementExportCase struct {
	Name            string                      `json:"name"`
	BaseURL         string                      `json:"base_url"`
	City            string                      `json:"city"`
	CID             string                      `json:"cid"`
	Epoch           int                         `json:"epoch"`
	Now             string                      `json:"now"`
	WriteKID        string                      `json:"write_kid"`
	WriteSeedBase64 string                      `json:"write_seed_base64"`
	Gate            string                      `json:"gate"`
	ManifestJSON    string                      `json:"manifest_json"`
	RetainedBase64  string                      `json:"retained_base64"`
	Expect          retirementExportExpectation `json:"expect"`
}
type retirementExportPlan struct {
	SchemaVersion int                    `json:"schema_version"`
	FixtureOnly   bool                   `json:"fixture_only"`
	Cases         []retirementExportCase `json:"cases"`
}
type crossLanguageRetirementExport struct {
	plan retirementExportPlan
	stop chan struct{}
	once sync.Once
}

func (f *crossLanguageRetirementSource) retarget(t *testing.T, gate string, raw []byte) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(f.request.ManifestJSON), &m); err != nil {
		t.Fatal(err)
	}
	m["gate"] = gate
	f.request.Gate = gate
	f.request.ManifestJSON = crossLanguageCanonical(t, m)
	f.request.RequestSHA256 = crossLanguageJSONDigest(t, m)
	f.request.RetainedBase64 = base64.StdEncoding.EncodeToString(raw)
	f.inputs.ExpectedManifestJSON = f.request.ManifestJSON
	f.inputs.RetainedSHA256 = crossLanguageDigest(raw)
	f.inputs.PermissionScope.Formula.Name = "retirement-source-read/" + gate
	f.inputs.PermissionScope.Formula.ContentSHA256 = f.request.RequestSHA256
	f.inputs.PermissionAuthority = crossLanguageAuthority{f.inputs.PermissionScope}
}

// external follows the canonical signed host/ledger/fenced registry join in
// retirementrelease/external_test.go, including all mandatory source scopes.
func (f *crossLanguageRetirementSource) external(t *testing.T, gate string) {
	t.Helper()
	sha := strings.Repeat("a", 64)
	window := selectorinventory.CaptureWindow{Start: f.now.Add(-2 * time.Minute), End: f.now.Add(-time.Minute)}
	registry := selectorinventory.RegistrySnapshotInput{Available: true, ExecutionGeneration: "execution-1", StartFence: 1, EndFence: 1, Identities: []selectorinventory.RegistryDispatchIdentity{}}
	resolutionKey := []byte("fixture-only-resolution-key-32-bytes")
	resolution := selectorinventory.DigestInFlightResolution(registry, "execution-1", resolutionKey)
	if resolution.Status != selectorinventory.StatusAvailable {
		t.Fatal(resolution)
	}
	coverage := []selectorinventory.ExternalSourceCoverage{}
	scopes := []selectorwriter.ScopeEvidenceDigest{}
	for _, scope := range selectorwriter.MandatoryScopes() {
		coverage = append(coverage, selectorinventory.ExternalSourceCoverage{ScopeID: scope, Status: selectorinventory.StatusAvailable, Complete: true, Capture: window})
		scopes = append(scopes, selectorwriter.ScopeEvidenceDigest{ScopeID: scope, EvidenceSHA256: sha})
	}
	controller := f.inputs.CurrentController
	ledger := selectorinventory.ExternalLedgerEvidence{SchemaVersion: selectorinventory.ExternalLedgerSchemaVersionV2, ObservationID: f.inputs.Observation.RecordID, ControllerSnapshotSHA256: sha, GraphConfigGeneration: 1, ControllerBuild: controller.Build, ExecutionGeneration: "execution-1", Capture: window, Retention: selectorinventory.CaptureWindow{Start: f.now.Add(-24 * time.Hour), End: window.End}, SequenceStart: 1, SequenceEnd: 1, Sequences: []selectorinventory.LedgerSequence{{Sequence: 1, SourceScope: "orders", EntryKind: selectorinventory.LedgerSequenceKindCoverageCheckpoint, CoverageWindow: &window, EntrySHA256: selectorinventory.LedgerSequenceCoverageCheckpointDigest(1, window)}}, ExternalScope: selectorwriter.MandatoryScopes(), SourceCoverage: coverage, CompleteResultAtoms: []selectorinventory.CompleteResultAtom{{Atom: "external-writer-mandatory-scopes.v1", Complete: true}}, InFlightResolutionSHA256: resolution.SHA256}
	ledger, ledgerBytes, err := selectorinventory.CanonicalizeExternalLedger(ledger)
	if err != nil {
		t.Fatal(err)
	}
	w := f.inputs.Observation
	w.SequenceEnd = 1
	w.ObservationLedgerSHA256 = ledger.DigestSHA256
	w.ExternalWriterInventorySHA256 = ledger.ExternalWriterInventorySHA256
	w.InFlightResolutionSHA256 = resolution.SHA256
	w.ObservationStartedAt, w.ObservationEndedAt = window.Start, window.End
	claims := selectorattestation.ObservationClaims{SchemaVersion: selectorattestation.ObservationSchemaVersionV1, Purpose: selectorattestation.ObservationKeyPurpose, RecordID: w.RecordID, KeyID: "observation-key", Issuer: "collector", CollectorIdentity: w.CollectorIdentity, Audience: w.Audience, Workspace: w.Workspace, CandidateManifestSHA256: w.CandidateManifestSHA256, SelectorSnapshotSHA256: w.SelectorSnapshotSHA256, ObservationLedgerSHA256: w.ObservationLedgerSHA256, SequenceStart: w.SequenceStart, SequenceEnd: w.SequenceEnd, SequenceCompletenessResult: w.SequenceCompletenessResult, CoverageResult: w.CoverageResult, RuntimeIdentitySHA256: w.RuntimeIdentitySHA256, BuildIdentitySHA256: w.BuildIdentitySHA256, ConfigIdentitySHA256: w.ConfigIdentitySHA256, OrderInventorySHA256: w.OrderInventorySHA256, ExternalWriterInventorySHA256: w.ExternalWriterInventorySHA256, InFlightResolutionSHA256: w.InFlightResolutionSHA256, ObservationStartedAt: window.Start.Format(time.RFC3339), ObservationEndedAt: window.End.Format(time.RFC3339), IssuedAt: window.End.Format(time.RFC3339), ExpiresAt: f.now.Add(time.Minute).Format(time.RFC3339)}
	b, err := selectorattestation.ObservationSigningBytes(claims)
	f.inputs.ObservationToken = crossLanguageToken(t, b, err, 1)
	f.inputs.Observation = w
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{4}, 32))
	host := selectorwriter.HostRecordClaims{SchemaVersion: selectorwriter.SchemaVersion, KeyPurpose: selectorwriter.KeyPurpose, KeyID: "external-key", ObservationID: w.RecordID, HostFingerprintSHA256: crossLanguageDigest([]byte("gascity.selectorwriter.host.v1\x00" + f.inputs.HostID)), BootFingerprintSHA256: crossLanguageDigest([]byte("gascity.selectorwriter.boot.v1\x00" + f.inputs.BootID)), BootStartedAt: f.now.Add(-24 * time.Hour), ControllerSnapshotSHA256: sha, ControllerGeneration: 1, ControllerBuild: controller.Build, ExecutionGeneration: "execution-1", Capture: window, LedgerSHA256: ledger.DigestSHA256, ScopeEvidence: scopes, IssuedAt: window.End, RetainUntil: window.End.Add(selectorwriter.RetentionPeriod)}
	hostBytes, err := json.Marshal(host)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(selectorwriter.SignedRecord{Claims: host, Ledger: base64.StdEncoding.EncodeToString(ledgerBytes), Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, append([]byte(selectorwriter.SigningDomain), hostBytes...)))})
	if err != nil {
		t.Fatal(err)
	}
	f.inputs.Writers, err = selectorwriter.NewVerifier(host.KeyID, key.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	f.inputs.Registry = registry
	f.inputs.External = selectorinventory.ExternalLedgerExpectation{Controller: controller, ObservationID: w.RecordID, Audience: w.Audience, Workspace: w.Workspace, CandidateManifestSHA256: w.CandidateManifestSHA256, ExecutionGeneration: "execution-1", BuildIdentitySHA256: w.BuildIdentitySHA256, ConfigIdentitySHA256: w.ConfigIdentitySHA256, MaxObservationAge: 5 * time.Minute, ExternalScope: selectorwriter.MandatoryScopes(), RequiredResultAtoms: []string{"external-writer-mandatory-scopes.v1"}, ResolutionKey: resolutionKey}
	f.retarget(t, gate, raw)
}

func newCrossLanguageRetirementExport(t *testing.T) *crossLanguageRetirementExport {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	x := &crossLanguageRetirementExport{plan: retirementExportPlan{SchemaVersion: 1, FixtureOnly: true}, stop: make(chan struct{})}
	names := []string{"positive-compatibility", "positive-human-review", "positive-external-writers", "positive-in-flight-resolution", "invalid-proof", "revocation", "context-mismatch", "unsupported-trial-ledger"}
	for _, name := range names {
		h := newHumanSourceHTTPFixture(t)
		f := newCrossLanguageRetirementFixture(t, h.now)
		expect := retirementExportExpectation{Status: "verified", Assurance: "fixture", Reason: "exact_signed_review_verified"}
		switch name {
		case "positive-compatibility":
			f.inputs.CompatibilityScope = f.inputs.PermissionScope
			f.inputs.CompatibilityScope.Formula.Name = "fixture-release-formula"
			f.inputs.CompatibilityAuthority = crossLanguageAuthority{f.inputs.CompatibilityScope}
			f.inputs.CapabilityProver = crossLanguageProver{}
			f.retarget(t, "compatibility", []byte("fixture-retained-compatibility-reference"))
			expect.Reason = "canonical_compatibility_verified"
		case "positive-external-writers", "positive-in-flight-resolution":
			f.external(t, strings.TrimPrefix(name, "positive-"))
			expect.Reason = "signed_fenced_external_ledger_join_verified"
			if f.request.Gate == "in-flight-resolution" {
				expect.Reason = "signed_fenced_in_flight_resolution_join_verified"
			}
		case "invalid-proof":
			raw, err := base64.StdEncoding.DecodeString(f.request.RetainedBase64)
			if err != nil {
				t.Fatal(err)
			}
			parts := strings.Split(string(raw), ".")
			sig, err := base64.RawURLEncoding.DecodeString(parts[1])
			if err != nil {
				t.Fatal(err)
			}
			sig[0] ^= 1
			f.retarget(t, "human-review", []byte(parts[0]+"."+base64.RawURLEncoding.EncodeToString(sig)))
			expect = retirementExportExpectation{HTTPStatus: 503}
		case "revocation":
			var rev selectorattestation.Revocations
			if err := json.Unmarshal(f.keys.bundle.Revocations.Payload, &rev); err != nil {
				t.Fatal(err)
			}
			rev.RevokedRecordIDs = []string{f.inputs.Review.RecordID}
			b, err := selectorattestation.RevocationSigningBytes(rev)
			if err != nil {
				t.Fatal(err)
			}
			payload := b[len(selectorattestation.RevocationSigningDomain):]
			f.keys.bundle.Revocations = selectorattestation.SignedRevocations{Payload: payload, Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{3}, 32)), b))}
			f.inputs.Attestations, err = selectorattestation.NewVerifier(f.keys, selectorattestation.Options{Audience: f.inputs.CurrentContext.Audience, RevocationPayloadSHA256: crossLanguageDigest(payload), MaxRecordAge: time.Hour, MaxRevocationAge: time.Hour, Now: func() time.Time { return f.now }})
			if err != nil {
				t.Fatal(err)
			}
			expect = retirementExportExpectation{HTTPStatus: 503}
		case "context-mismatch":
			f.inputs.CurrentContext.ControllerGeneration++
			expect = retirementExportExpectation{HTTPStatus: 409}
		case "unsupported-trial-ledger":
			f.retarget(t, "trial-ledger", []byte(f.inputs.ObservationToken))
			expect = retirementExportExpectation{Status: "unavailable", Assurance: "unavailable", Reason: "signed_observation_verified_useful_acceptance_trial_contract_unavailable"}
		}
		state := &crossLanguageState{decisionFrontierState: h.wrapped, retirement: f.adapter(t)}
		handler := writeAuthMiddleware(newTestWriteVerifier(t, h.workerKey.Public().(ed25519.PublicKey), h.now), false, newTestCityHandler(t, state))
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		server := &http.Server{ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/__fixture/stop" {
				x.once.Do(func() { close(x.stop) })
				w.WriteHeader(http.StatusNoContent)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
			handler.ServeHTTP(w, r)
		})}
		done := make(chan error, 1)
		go func() { done <- server.Serve(ln) }()
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := server.Shutdown(ctx); err != nil {
				t.Error(err)
				_ = server.Close()
			}
			if err := <-done; !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("retirement fixture serve: %v", err)
			}
		})
		x.plan.Cases = append(x.plan.Cases, retirementExportCase{Name: name, BaseURL: "http://" + ln.Addr().String(), City: h.state.CityName(), CID: "fixture-tenant", Epoch: 0, Now: h.now.Format(time.RFC3339), WriteKID: "k1", WriteSeedBase64: base64.StdEncoding.EncodeToString(h.workerKey.Seed()), Gate: f.request.Gate, ManifestJSON: f.request.ManifestJSON, RetainedBase64: f.request.RetainedBase64, Expect: expect})
	}
	return x
}

// TestCrossLanguageRetirementHarnessServe exports transient test-only authorities.
func TestCrossLanguageRetirementHarnessServe(t *testing.T) {
	if os.Getenv("GC_RETIREMENT_WIRE_HARNESS") != "1" {
		t.Skip("set GC_RETIREMENT_WIRE_HARNESS=1 to serve the disposable plan")
	}
	x := newCrossLanguageRetirementExport(t)
	b, err := json.Marshal(x.plan)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("GC_RETIREMENT_PLAN=%s\n", b)
	timer := time.NewTimer(120 * time.Second)
	defer timer.Stop()
	select {
	case <-x.stop:
	case <-timer.C:
		t.Fatal("retirement fixture timeout; POST /__fixture/stop to any case listener")
	}
}

// TestCrossLanguageRetirementExportRealWire owns plan-to-real-Huma wiring;
// canonical verifier branch matrices remain in retirementrelease tests.
func TestCrossLanguageRetirementExportRealWire(t *testing.T) {
	x := newCrossLanguageRetirementExport(t)
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	for i, c := range x.plan.Cases {
		t.Run(c.Name, func(t *testing.T) {
			body, err := json.Marshal(retirementrelease.Request{Gate: c.Gate, RequestSHA256: crossLanguageDigest([]byte(c.ManifestJSON)), ManifestJSON: c.ManifestJSON, RetainedBase64: c.RetainedBase64})
			if err != nil {
				t.Fatal(err)
			}
			path := "/v0/city/" + c.City + "/retirement-release/verify"
			req, err := http.NewRequest(http.MethodPost, c.BaseURL+path, bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			now, err := time.Parse(time.RFC3339, c.Now)
			if err != nil {
				t.Fatal(err)
			}
			seed, err := base64.StdEncoding.DecodeString(c.WriteSeedBase64)
			if err != nil {
				t.Fatal(err)
			}
			grant := grantFor(now, c.City, "POST", path, body, fmt.Sprintf("export-%d", i))
			grant.CID = c.CID
			req.Header.Set(writeAuthHeader, mintToken(t, ed25519.NewKeyFromSeed(seed), grant))
			req.Header.Set(csrfHeaderName, "true")
			req.Header.Set("Content-Type", "application/json")
			res, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			raw, err := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
			if err != nil {
				t.Fatal(err)
			}
			status := c.Expect.HTTPStatus
			if status == 0 {
				status = 200
			}
			if res.StatusCode != status {
				t.Fatalf("HTTP %d want %d: %s", res.StatusCode, status, raw)
			}
			if status != 200 {
				return
			}
			var v retirementrelease.Verdict
			if err := json.Unmarshal(raw, &v); err != nil {
				t.Fatal(err)
			}
			if v.Status != c.Expect.Status || v.Assurance != c.Expect.Assurance || v.Reason != c.Expect.Reason || v.RequestSHA256 != crossLanguageDigest([]byte(c.ManifestJSON)) {
				t.Fatalf("unexpected verdict: %+v", v)
			}
		})
	}
	t.Run("missing-write-grant", func(t *testing.T) {
		c := x.plan.Cases[1]
		body, err := json.Marshal(retirementrelease.Request{Gate: c.Gate, RequestSHA256: crossLanguageDigest([]byte(c.ManifestJSON)), ManifestJSON: c.ManifestJSON, RetainedBase64: c.RetainedBase64})
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(http.MethodPost, c.BaseURL+"/v0/city/"+c.City+"/retirement-release/verify", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(csrfHeaderName, "true")
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != 401 {
			t.Fatalf("unauthenticated HTTP %d", res.StatusCode)
		}
	})
	t.Run("explicit-http-body-limit", func(t *testing.T) {
		c := x.plan.Cases[0]
		body := bytes.Repeat([]byte{' '}, (1<<20)+1)
		path := "/v0/city/" + c.City + "/retirement-release/verify"
		req, err := http.NewRequest(http.MethodPost, c.BaseURL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		now, err := time.Parse(time.RFC3339, c.Now)
		if err != nil {
			t.Fatal(err)
		}
		seed, err := base64.StdEncoding.DecodeString(c.WriteSeedBase64)
		if err != nil {
			t.Fatal(err)
		}
		grant := grantFor(now, c.City, "POST", path, body, "export-body-limit")
		grant.CID = c.CID
		req.Header.Set(writeAuthHeader, mintToken(t, ed25519.NewKeyFromSeed(seed), grant))
		req.Header.Set(csrfHeaderName, "true")
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != 413 {
			t.Fatalf("oversized HTTP %d", res.StatusCode)
		}
	})
	t.Run("explicit-stop", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost, x.plan.Cases[0].BaseURL+"/__fixture/stop", nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != 204 {
			t.Fatalf("stop HTTP %d", res.StatusCode)
		}
		select {
		case <-x.stop:
		default:
			t.Fatal("stop response without fixture stop signal")
		}
	})
}
