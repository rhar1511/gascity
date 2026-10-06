package selectorwriter

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/selectorattestation"
	"github.com/gastownhall/gascity/internal/selectorinventory"
)

type fixtureIdentityReader struct {
	identities []HostIdentity
	calls      int
}

func (r *fixtureIdentityReader) ReadIdentity(context.Context) (HostIdentity, error) {
	if len(r.identities) == 0 {
		return HostIdentity{}, errors.New("identity unavailable")
	}
	index := r.calls
	if index >= len(r.identities) {
		index = len(r.identities) - 1
	}
	r.calls++
	return r.identities[index], nil
}

type fixtureOrderLedgerReader struct {
	snapshot OrderLedgerSnapshot
	calls    int
	window   selectorinventory.CaptureWindow
	limits   selectorinventory.Limits
}

func (r *fixtureOrderLedgerReader) ReadOrderLedger(_ context.Context, window selectorinventory.CaptureWindow, limits selectorinventory.Limits) (OrderLedgerSnapshot, error) {
	r.calls++
	r.window = window
	r.limits = limits
	return r.snapshot, nil
}

type fixtureLinuxScopeReader struct {
	coverage map[string]LinuxScopeSnapshot
	calls    []string
	window   selectorinventory.CaptureWindow
	limits   selectorinventory.Limits
}

func (r *fixtureLinuxScopeReader) ReadLinuxScope(_ context.Context, scope string, window selectorinventory.CaptureWindow, limits selectorinventory.Limits) (LinuxScopeSnapshot, error) {
	r.calls = append(r.calls, scope)
	r.window = window
	r.limits = limits
	coverage, ok := r.coverage[scope]
	if !ok {
		return LinuxScopeSnapshot{}, errors.New("scope unavailable")
	}
	return coverage, nil
}

type fixtureKeyProvider struct{ key ed25519.PrivateKey }

func (p fixtureKeyProvider) PrivateKey(context.Context, string) (ed25519.PrivateKey, error) {
	return append(ed25519.PrivateKey(nil), p.key...), nil
}

func TestSigningKeyProviderFuncForwardsContextAndExactKeyID(t *testing.T) {
	type contextKey struct{}
	ctx := context.WithValue(context.Background(), contextKey{}, "collector startup")
	_, wantKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const keyID = "root-host-collector-key-2026-09"
	called := false
	provider := SigningKeyProviderFunc(func(gotContext context.Context, gotKeyID string) (ed25519.PrivateKey, error) {
		called = true
		if gotContext != ctx {
			t.Errorf("callback context = %v, want exact supplied context", gotContext)
		}
		if gotKeyID != keyID {
			t.Errorf("callback key ID = %q, want %q", gotKeyID, keyID)
		}
		return wantKey, nil
	})

	gotKey, err := provider.PrivateKey(ctx, keyID)
	if err != nil {
		t.Fatalf("provider returned error: %v", err)
	}
	if !called || !bytes.Equal(gotKey, wantKey) {
		t.Fatalf("provider result called=%t key matches=%t", called, bytes.Equal(gotKey, wantKey))
	}
}

func TestSigningKeyProviderFuncFailsClosedForNilCallbackErrorAndInvalidKey(t *testing.T) {
	ctx := context.Background()
	if key, err := SigningKeyProviderFunc(nil).PrivateKey(ctx, "collector-key"); err == nil || key != nil {
		t.Fatalf("nil callback returned key=%t err=%v, want no key and an error", key != nil, err)
	}

	callbackErr := errors.New("key source unavailable")
	provider := SigningKeyProviderFunc(func(context.Context, string) (ed25519.PrivateKey, error) {
		return make(ed25519.PrivateKey, ed25519.PrivateKeySize), callbackErr
	})
	if key, err := provider.PrivateKey(ctx, "collector-key"); !errors.Is(err, callbackErr) || key != nil {
		t.Fatalf("callback failure returned key=%t err=%v, want original error and no key", key != nil, err)
	}

	for _, size := range []int{0, ed25519.PrivateKeySize - 1, ed25519.PrivateKeySize + 1} {
		t.Run(fmt.Sprintf("key length %d", size), func(t *testing.T) {
			provider := SigningKeyProviderFunc(func(context.Context, string) (ed25519.PrivateKey, error) {
				return make(ed25519.PrivateKey, size), nil
			})
			if key, err := provider.PrivateKey(ctx, "collector-key"); !errors.Is(err, ErrUnavailable) || key != nil {
				t.Fatalf("invalid key returned key=%t err=%v, want unavailable and no key", key != nil, err)
			}
		})
	}
	t.Run("inconsistent canonical public half", func(t *testing.T) {
		key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x5a}, ed25519.SeedSize))
		key[ed25519.SeedSize] ^= 0xff
		provider := SigningKeyProviderFunc(func(context.Context, string) (ed25519.PrivateKey, error) {
			return key, nil
		})
		if got, err := provider.PrivateKey(ctx, "collector-key"); !errors.Is(err, ErrUnavailable) || got != nil {
			t.Fatalf("inconsistent key returned key=%t err=%v, want unavailable and no key", got != nil, err)
		}
	})
}

func TestSigningKeyProviderFuncReturnsIsolatedKeyCopy(t *testing.T) {
	_, sourceKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	original := append(ed25519.PrivateKey(nil), sourceKey...)
	provider := SigningKeyProviderFunc(func(context.Context, string) (ed25519.PrivateKey, error) {
		return sourceKey, nil
	})
	gotKey, err := provider.PrivateKey(context.Background(), "collector-key")
	if err != nil {
		t.Fatalf("provider returned error: %v", err)
	}

	sourceKey[0] ^= 0xff
	if !bytes.Equal(gotKey, original) {
		t.Fatal("provider result changed when callback-owned key bytes changed")
	}
	gotKey[1] ^= 0xff
	if !bytes.Equal(sourceKey[1:], original[1:]) {
		t.Fatal("callback-owned key bytes changed when provider result changed")
	}
}

type fixtureClock struct{ now time.Time }

func (c fixtureClock) Now() time.Time { return c.now }

type fixtureAttestationAuthority struct{ bundle selectorattestation.Bundle }

func (s fixtureAttestationAuthority) Load(context.Context) (selectorattestation.Bundle, error) {
	return s.bundle, nil
}

type retainedFixture struct {
	raw         []byte
	lastAttempt []byte
	recordID    string
	retainTil   time.Time
	calls       int
	failure     error
}

func (s *retainedFixture) Retain(_ context.Context, recordID string, raw []byte, until time.Time) error {
	s.calls++
	s.lastAttempt = append([]byte(nil), raw...)
	if s.recordID != "" {
		return errors.New("observation ID already retained")
	}
	s.recordID = recordID
	s.raw = append([]byte(nil), raw...)
	s.retainTil = until
	return s.failure
}

type collectorFixture struct {
	now        time.Time
	window     selectorinventory.CaptureWindow
	controller selectorinventory.ControllerBinding
	execution  string
	hostID     string
	bootID     string
	resolution string
	key        ed25519.PrivateKey
	public     ed25519.PublicKey
	collector  Collector
	orders     *fixtureOrderLedgerReader
	linux      *fixtureLinuxScopeReader
	identity   *fixtureIdentityReader
	retained   *retainedFixture
}

func newCollectorFixture(t *testing.T) *collectorFixture {
	t.Helper()
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	window := selectorinventory.CaptureWindow{Start: now.Add(-2 * time.Minute), End: now.Add(-time.Minute)}
	controller := selectorinventory.ControllerBinding{
		SnapshotSHA256: digest("controller-snapshot"), Generation: 24, Build: "gc-build-2026-09-29",
	}
	resolution := digest("in-flight-resolution")
	orders := &fixtureOrderLedgerReader{snapshot: OrderLedgerSnapshot{
		Coverage: selectorinventory.ExternalSourceCoverage{
			ScopeID: ScopeOrders, Status: selectorinventory.StatusAvailable, Complete: true,
			Capture: window,
		},
		EvidenceSHA256: digest("orders-evidence"),
		Retention:      selectorinventory.CaptureWindow{Start: now.Add(-24 * time.Hour), End: window.End},
		SequenceStart:  41, SequenceEnd: 41,
		Sequences: []selectorinventory.LedgerSequence{{
			Sequence: 41, SourceScope: ScopeOrders, EntrySHA256: digest("order-entry-41"),
		}},
	}}
	coverage := make(map[string]LinuxScopeSnapshot)
	for _, scope := range []string{ScopeStartup, ScopeServices, ScopeCron, ScopeManual} {
		coverage[scope] = LinuxScopeSnapshot{
			Coverage: selectorinventory.ExternalSourceCoverage{
				ScopeID: scope, Status: selectorinventory.StatusAvailable, Complete: true,
				Capture: window,
			},
			EvidenceSHA256: digest(scope + "-evidence"),
		}
	}
	linux := &fixtureLinuxScopeReader{coverage: coverage}
	identity := &fixtureIdentityReader{identities: []HostIdentity{
		{HostID: "fixture-host-machine-id", BootID: "fixture-boot-id", BootStartedAt: now.Add(-24 * time.Hour)},
		{HostID: "fixture-host-machine-id", BootID: "fixture-boot-id", BootStartedAt: now.Add(-24 * time.Hour)},
	}}
	retained := &retainedFixture{}
	collector := Collector{
		KeyID: "fixture-root-host-writer-key", Keys: fixtureKeyProvider{key: private},
		Identity: identity, Orders: orders, Linux: linux, Clock: fixtureClock{now: now}, Retention: retained,
	}
	return &collectorFixture{
		now: now, window: window, controller: controller, execution: "execution-generation-24",
		hostID: "fixture-host-machine-id", bootID: "fixture-boot-id", resolution: resolution,
		key: private, public: public, collector: collector, orders: orders, linux: linux,
		identity: identity, retained: retained,
	}
}

func (f *collectorFixture) request() Request {
	return Request{
		ObservationID: "observation-fixture-24", Controller: f.controller,
		ExecutionGeneration: f.execution, Capture: f.window,
		InFlightResolutionSHA256: f.resolution,
	}
}

func (f *collectorFixture) collect(t *testing.T) []byte {
	t.Helper()
	raw, err := f.collector.Collect(context.Background(), f.request())
	if err != nil {
		t.Fatalf("collect signed external ledger: %v", err)
	}
	return raw
}

func TestCollectorSignsCanonicalExternalLedgerWithMandatoryScopes(t *testing.T) {
	f := newCollectorFixture(t)
	raw := f.collect(t)
	if f.orders.calls != 1 || !f.orders.window.Start.Equal(f.window.Start) || !f.orders.window.End.Equal(f.window.End) {
		t.Fatalf("order reader calls/window = %d/%#v, want one exact fixed window", f.orders.calls, f.orders.window)
	}
	wantLinuxScopes := make([]string, 0, len(MandatoryScopes())-1)
	for _, scope := range MandatoryScopes() {
		if scope != ScopeOrders {
			wantLinuxScopes = append(wantLinuxScopes, scope)
		}
	}
	if !equalStrings(f.linux.calls, wantLinuxScopes) {
		t.Fatalf("Linux source scopes = %#v, want %#v", f.linux.calls, wantLinuxScopes)
	}
	if f.orders.limits.MaxEntries == 0 || f.orders.limits.MaxBytes == 0 ||
		f.linux.limits.MaxEntries == 0 || f.linux.limits.MaxBytes == 0 || f.linux.limits.MaxCommands == 0 {
		t.Fatalf("source readers received incomplete bounds: order=%#v linux=%#v", f.orders.limits, f.linux.limits)
	}
	if f.identity.calls != 2 {
		t.Fatalf("identity reads = %d, want before/after reboot fence", f.identity.calls)
	}
	if f.retained.calls != 1 || f.retained.recordID != f.request().ObservationID || !f.retained.retainTil.Equal(f.now.Add(RetentionPeriod)) {
		t.Fatalf("retention sink = %#v, want one record retained for 30 days", f.retained)
	}
	if !bytes.Equal(raw, f.retained.raw) {
		t.Fatal("retention sink did not receive exact signed bytes")
	}
	if bytes.Contains(raw, []byte(f.hostID)) || bytes.Contains(raw, []byte(f.bootID)) {
		t.Fatal("signed host record contains raw host or boot identity")
	}
	verifier, err := NewVerifier(f.collector.KeyID, f.public)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := verifier.Verify(context.Background(), raw, f.now, f.hostID, f.bootID)
	if err != nil {
		t.Fatalf("verify signed host record: %v", err)
	}
	if !verified.Valid() || verified.Claims.ControllerSnapshotSHA256 != f.controller.SnapshotSHA256 ||
		verified.Claims.ControllerGeneration != f.controller.Generation || verified.Claims.ControllerBuild != f.controller.Build ||
		verified.Claims.ExecutionGeneration != f.execution || !verified.Claims.Capture.Start.Equal(f.window.Start) ||
		!verified.Claims.Capture.End.Equal(f.window.End) {
		t.Fatalf("verified record lost exact binding: %#v", verified)
	}
	ledger := verified.ExternalLedgerJSON()
	if len(ledger) == 0 || !bytes.Contains(ledger, []byte(`"source_scope":"orders"`)) {
		t.Fatal("signed host record did not carry the canonical external ledger")
	}
	var ledgerEvidence selectorinventory.ExternalLedgerEvidence
	if err := json.Unmarshal(ledger, &ledgerEvidence); err != nil {
		t.Fatalf("decode signed canonical ledger: %v", err)
	}
	if ledgerEvidence.SchemaVersion != selectorinventory.ExternalLedgerSchemaVersionV1 ||
		strings.Contains(string(ledger), `"entry_kind"`) || strings.Contains(string(ledger), `"coverage_window"`) {
		t.Fatalf("ordinary event-only ledger did not preserve v1 bytes: version=%d ledger=%s", ledgerEvidence.SchemaVersion, ledger)
	}
	for _, scope := range MandatoryScopes() {
		if !bytes.Contains(ledger, []byte(`"scope_id":"`+scope+`"`)) {
			t.Errorf("canonical ledger does not carry mandatory scope %q", scope)
		}
	}
	for index, scope := range MandatoryScopes() {
		if ledgerEvidence.SourceCoverage[index].ScopeID != scope {
			t.Fatalf("source coverage order = %#v, want canonical order %#v", ledgerEvidence.SourceCoverage, MandatoryScopes())
		}
	}
}

func TestCollectorSignsExactQuietOrderWindowCheckpoint(t *testing.T) {
	f := newCollectorFixture(t)
	setQuietOrderWindowCheckpoint(f)
	raw := f.collect(t)
	verifier, err := NewVerifier(f.collector.KeyID, f.public)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := verifier.Verify(context.Background(), raw, f.now, f.hostID, f.bootID)
	if err != nil {
		t.Fatalf("verify quiet-window record: %v", err)
	}
	var ledger selectorinventory.ExternalLedgerEvidence
	if err := json.Unmarshal(verified.ExternalLedgerJSON(), &ledger); err != nil {
		t.Fatalf("decode quiet-window ledger: %v", err)
	}
	if ledger.SchemaVersion != selectorinventory.ExternalLedgerSchemaVersionV2 {
		t.Fatalf("quiet-window schema = %d, want v2", ledger.SchemaVersion)
	}
	if ledger.SequenceStart != 42 || ledger.SequenceEnd != 43 || len(ledger.Sequences) != 2 {
		t.Fatalf("quiet-window sequence bounds/rows = %d..%d/%d, want checkpoint sequences 42..43", ledger.SequenceStart, ledger.SequenceEnd, len(ledger.Sequences))
	}
	coveredThrough := f.window.Start
	for _, checkpoint := range ledger.Sequences {
		if checkpoint.EntryKind != selectorinventory.LedgerSequenceKindCoverageCheckpoint ||
			checkpoint.SourceScope != ScopeOrders || checkpoint.CoverageWindow == nil ||
			!checkpoint.CoverageWindow.Start.Equal(coveredThrough) {
			t.Fatalf("quiet-window checkpoint = %#v, want contiguous order-scope coverage", checkpoint)
		}
		coveredThrough = checkpoint.CoverageWindow.End
	}
	if !coveredThrough.Equal(f.window.End) {
		t.Fatalf("quiet-window checkpoints cover through %s, want %s", coveredThrough, f.window.End)
	}
}

func TestCollectorRejectsMalformedQuietOrderWindowCheckpoint(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*collectorFixture)
	}{
		{name: "missing checkpoint", setup: func(f *collectorFixture) { f.orders.snapshot.Sequences = nil }},
		{name: "coverage starts late", setup: func(f *collectorFixture) {
			sequence := f.orders.snapshot.Sequences[0]
			coverage := *sequence.CoverageWindow
			coverage.Start = coverage.Start.Add(time.Second)
			sequence.CoverageWindow = &coverage
			f.orders.snapshot.Sequences[0] = sequence
		}},
		{name: "checkpoint digest does not bind interval", setup: func(f *collectorFixture) {
			first := f.orders.snapshot.Sequences[0]
			second := f.orders.snapshot.Sequences[1]
			firstCoverage := *first.CoverageWindow
			secondCoverage := *second.CoverageWindow
			boundary := firstCoverage.End.Add(time.Second)
			firstCoverage.End = boundary
			secondCoverage.Start = boundary
			first.CoverageWindow = &firstCoverage
			second.CoverageWindow = &secondCoverage
			f.orders.snapshot.Sequences[0] = first
			f.orders.snapshot.Sequences[1] = second
		}},
		{name: "mixed order entry", setup: func(f *collectorFixture) {
			f.orders.snapshot.Sequences = append(f.orders.snapshot.Sequences, selectorinventory.LedgerSequence{
				Sequence: f.orders.snapshot.SequenceEnd + 1, SourceScope: ScopeOrders, EntrySHA256: digest("order-entry-after-checkpoint"),
			})
			f.orders.snapshot.SequenceEnd++
		}},
		{name: "checkpoint digest missing", setup: func(f *collectorFixture) {
			sequence := f.orders.snapshot.Sequences[0]
			sequence.EntrySHA256 = ""
			f.orders.snapshot.Sequences[0] = sequence
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newCollectorFixture(t)
			setQuietOrderWindowCheckpoint(f)
			test.setup(f)
			if _, err := f.collector.Collect(context.Background(), f.request()); err == nil {
				t.Fatal("collector signed malformed quiet-window order evidence")
			}
			if f.retained.calls != 0 {
				t.Fatal("malformed quiet-window evidence reached retention")
			}
		})
	}
}

func setQuietOrderWindowCheckpoint(f *collectorFixture) {
	midpoint := f.window.Start.Add(f.window.End.Sub(f.window.Start) / 2)
	firstCoverage := selectorinventory.CaptureWindow{Start: f.window.Start, End: midpoint}
	secondCoverage := selectorinventory.CaptureWindow{Start: midpoint, End: f.window.End}
	f.orders.snapshot.SequenceStart = 42
	f.orders.snapshot.SequenceEnd = 43
	f.orders.snapshot.Sequences = []selectorinventory.LedgerSequence{
		{
			Sequence: 42, SourceScope: ScopeOrders,
			EntrySHA256: selectorinventory.LedgerSequenceCoverageCheckpointDigest(42, firstCoverage),
			EntryKind:   selectorinventory.LedgerSequenceKindCoverageCheckpoint, CoverageWindow: &firstCoverage,
		},
		{
			Sequence: 43, SourceScope: ScopeOrders,
			EntrySHA256: selectorinventory.LedgerSequenceCoverageCheckpointDigest(43, secondCoverage),
			EntryKind:   selectorinventory.LedgerSequenceKindCoverageCheckpoint, CoverageWindow: &secondCoverage,
		},
	}
}

func TestCollectorFailsWhenCreateOnlyRetentionRejectsObservationID(t *testing.T) {
	f := newCollectorFixture(t)
	first, err := f.collector.Collect(context.Background(), f.request())
	if err != nil {
		t.Fatalf("initial create-only retention failed: %v", err)
	}
	stored := append([]byte(nil), f.retained.raw...)
	if _, err := f.collector.Collect(context.Background(), f.request()); err == nil {
		t.Fatal("collector returned a record after immutable retention rejected a repeated observation ID")
	}
	if f.retained.calls != 2 || f.retained.recordID != f.request().ObservationID {
		t.Fatalf("retention sink calls = %#v, want initial write plus rejected create-only retry", f.retained)
	}
	if !bytes.Equal(f.retained.raw, stored) || !bytes.Equal(f.retained.lastAttempt, first) {
		t.Fatal("repeated observation ID replaced retained bytes or changed the signed record")
	}
}

func TestCollectorFailsClosedForMissingScopeOrReaderFailure(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*collectorFixture)
	}{
		{name: "inaccessible cron", setup: func(f *collectorFixture) { delete(f.linux.coverage, ScopeCron) }},
		{name: "incomplete manual", setup: func(f *collectorFixture) {
			result := f.linux.coverage[ScopeManual]
			result.Coverage.Complete = false
			f.linux.coverage[ScopeManual] = result
		}},
		{name: "bad source digest", setup: func(f *collectorFixture) {
			result := f.linux.coverage[ScopeServices]
			result.EvidenceSHA256 = ""
			f.linux.coverage[ScopeServices] = result
		}},
		{name: "order gap", setup: func(f *collectorFixture) { f.orders.snapshot.Sequences = nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newCollectorFixture(t)
			test.setup(f)
			if _, err := f.collector.Collect(context.Background(), f.request()); err == nil {
				t.Fatal("incomplete external writer evidence was signed")
			}
			if f.retained.calls != 0 {
				t.Fatal("unavailable evidence reached the retention sink")
			}
		})
	}
}

func TestCollectorRejectsBootChangeAndStaleWindow(t *testing.T) {
	t.Run("boot changed", func(t *testing.T) {
		f := newCollectorFixture(t)
		f.identity.identities[1].BootID = "next-boot"
		if _, err := f.collector.Collect(context.Background(), f.request()); err == nil {
			t.Fatal("collector signed evidence spanning a reboot")
		}
	})
	t.Run("stale", func(t *testing.T) {
		f := newCollectorFixture(t)
		f.collector.Clock = fixtureClock{now: f.now.Add(time.Second)}
		f.window.End = f.now.Add(-MaxObservationAge - time.Second)
		f.window.Start = f.window.End.Add(-time.Minute)
		f.orders.snapshot.Coverage.Capture = f.window
		f.orders.snapshot.Retention.End = f.window.End
		for scope, result := range f.linux.coverage {
			result.Coverage.Capture = f.window
			f.linux.coverage[scope] = result
		}
		if _, err := f.collector.Collect(context.Background(), f.request()); err == nil {
			t.Fatal("collector signed an observation older than five minutes")
		}
	})
	t.Run("window crosses current boot boundary", func(t *testing.T) {
		f := newCollectorFixture(t)
		f.identity.identities[0].BootStartedAt = f.window.Start.Add(time.Second)
		if _, err := f.collector.Collect(context.Background(), f.request()); err == nil {
			t.Fatal("collector signed a window beginning before the current boot")
		}
	})
}

func TestVerifierRejectsTamperedLedgerAndBindingMismatch(t *testing.T) {
	f := newCollectorFixture(t)
	raw := f.collect(t)
	verifier, err := NewVerifier(f.collector.KeyID, f.public)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(context.Background(), raw, f.now, "another-host", f.bootID); err == nil {
		t.Fatal("host fingerprint mismatch was accepted")
	}
	if _, err := verifier.Verify(context.Background(), raw, f.now, f.hostID, "another-boot"); err == nil {
		t.Fatal("boot fingerprint mismatch was accepted")
	}
	mutated := append([]byte(nil), raw...)
	mutated[len(mutated)/2] ^= 1
	if _, err := verifier.Verify(context.Background(), mutated, f.now, f.hostID, f.bootID); err == nil {
		t.Fatal("tampered signed record was accepted")
	}
	if _, err := verifier.Verify(context.Background(), raw, f.now.Add(MaxObservationAge+time.Second), f.hostID, f.bootID); err == nil {
		t.Fatal("stale signed record was accepted")
	}
}

func TestVerifierPinsAndCopiesExplicitPublicKey(t *testing.T) {
	f := newCollectorFixture(t)
	raw := f.collect(t)
	originalPublicKey := append(ed25519.PublicKey(nil), f.public...)
	verifier, err := NewVerifier(f.collector.KeyID, f.public)
	if err != nil {
		t.Fatal(err)
	}
	f.public[0] ^= 0xff
	if !bytes.Equal(verifier.key, originalPublicKey) {
		t.Fatal("verifier key changed when caller-owned public-key bytes changed")
	}
	if _, err := verifier.Verify(context.Background(), raw, f.now, f.hostID, f.bootID); err != nil {
		t.Fatalf("verifier failed to use its pinned public key: %v", err)
	}

	otherPublicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherVerifier, err := NewVerifier(f.collector.KeyID, otherPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := otherVerifier.Verify(context.Background(), raw, f.now, f.hostID, f.bootID); !errors.Is(err, ErrSignature) {
		t.Fatalf("record signed by a different key returned %v, want ErrSignature", err)
	}
}

func TestVerifyAndJoinUsesExistingCanonicalExternalLedgerJoin(t *testing.T) {
	f := newCollectorFixture(t)
	resolutionKey := []byte("external-writer-join-fixture-resolution-key")
	registry := selectorinventory.RegistrySnapshotInput{
		Available: true, ExecutionGeneration: f.execution, StartFence: 11, EndFence: 11,
		Identities: []selectorinventory.RegistryDispatchIdentity{},
	}
	resolution := selectorinventory.DigestInFlightResolution(registry, f.execution, resolutionKey)
	if resolution.Status != selectorinventory.StatusAvailable {
		t.Fatalf("in-flight resolution unavailable: %#v", resolution)
	}
	f.resolution = resolution.SHA256
	raw := f.collect(t)
	verifiedHost, err := NewVerifier(f.collector.KeyID, f.public)
	if err != nil {
		t.Fatal(err)
	}
	var ledger selectorinventory.ExternalLedgerEvidence
	if err := json.Unmarshal(verifiedHostRecordLedger(t, verifiedHost, raw, f), &ledger); err != nil {
		t.Fatalf("decode canonical external ledger: %v", err)
	}

	audience, workspace := "selector-fixture", "city-fixture"
	candidateDigest := digest("candidate-manifest")
	buildDigest := digest("controller-build")
	configDigest := digest("controller-config")
	observationPublic, observationPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	revocationPublic, revocationPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	revocations := selectorattestation.Revocations{
		SchemaVersion: selectorattestation.RevocationsSchemaVersionV1,
		Purpose:       selectorattestation.RevocationKeyPurpose, KeyID: "revocation-fixture-key",
		Issuer: "fixture-revocation-authority", Audience: audience, Workspace: workspace,
		IssuedAt:  f.now.Add(-time.Hour).Format(time.RFC3339Nano),
		ExpiresAt: f.now.Add(time.Hour).Format(time.RFC3339Nano), RevokedRecordIDs: []string{},
	}
	revocationBytes, err := selectorattestation.RevocationSigningBytes(revocations)
	if err != nil {
		t.Fatal(err)
	}
	revocationPayload := bytes.TrimPrefix(revocationBytes, []byte(selectorattestation.RevocationSigningDomain))
	revocationDigest := sha256.Sum256(revocationPayload)
	bundle := selectorattestation.Bundle{
		Keys: []selectorattestation.TrustedKey{
			{
				KeyID: "observation-fixture-key", Issuer: "fixture-observation-authority", Subject: "fixture-collector",
				Purpose: selectorattestation.ObservationKeyPurpose, PublicKey: base64.StdEncoding.EncodeToString(observationPublic),
			},
			{
				KeyID: "revocation-fixture-key", Issuer: "fixture-revocation-authority",
				Purpose: selectorattestation.RevocationKeyPurpose, PublicKey: base64.StdEncoding.EncodeToString(revocationPublic),
			},
		},
		Revocations: selectorattestation.SignedRevocations{
			Payload:   append(json.RawMessage(nil), revocationPayload...),
			Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(revocationPrivate, revocationBytes)),
		},
	}
	attestationVerifier, err := selectorattestation.NewVerifier(fixtureAttestationAuthority{bundle: bundle}, selectorattestation.Options{
		Audience: audience, RevocationPayloadSHA256: hex.EncodeToString(revocationDigest[:]),
		MaxRecordAge: MaxObservationAge, MaxRevocationAge: 2 * time.Hour,
		Now: func() time.Time { return f.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	claims := selectorattestation.ObservationClaims{
		SchemaVersion: selectorattestation.ObservationSchemaVersionV1,
		Purpose:       selectorattestation.ObservationKeyPurpose,
		RecordID:      ledger.ObservationID, KeyID: "observation-fixture-key",
		Issuer: "fixture-observation-authority", CollectorIdentity: "fixture-collector",
		Audience: audience, Workspace: workspace, CandidateManifestSHA256: candidateDigest,
		SelectorSnapshotSHA256: ledger.ControllerSnapshotSHA256,
		SequenceStart:          ledger.SequenceStart, SequenceEnd: ledger.SequenceEnd,
		SequenceCompletenessResult: "complete", CoverageResult: "complete",
		RuntimeIdentitySHA256: digest("runtime"), BuildIdentitySHA256: buildDigest,
		ConfigIdentitySHA256: configDigest, OrderInventorySHA256: digest("order-inventory"),
		ObservationLedgerSHA256:       ledger.DigestSHA256,
		ExternalWriterInventorySHA256: ledger.ExternalWriterInventorySHA256,
		InFlightResolutionSHA256:      ledger.InFlightResolutionSHA256,
		ObservationStartedAt:          ledger.Capture.Start.Format(time.RFC3339Nano),
		ObservationEndedAt:            ledger.Capture.End.Format(time.RFC3339Nano),
		IssuedAt:                      f.now.Add(-time.Minute).Format(time.RFC3339Nano),
		ExpiresAt:                     f.now.Add(2 * time.Minute).Format(time.RFC3339Nano),
	}
	token := observationToken(t, claims, observationPrivate)
	wantObservation := selectorattestation.ObservationExpectation{
		RecordID: claims.RecordID, CollectorIdentity: claims.CollectorIdentity,
		Audience: audience, Workspace: workspace, CandidateManifestSHA256: candidateDigest,
		SelectorSnapshotSHA256:  claims.SelectorSnapshotSHA256,
		ObservationLedgerSHA256: claims.ObservationLedgerSHA256,
		SequenceStart:           claims.SequenceStart, SequenceEnd: claims.SequenceEnd,
		SequenceCompletenessResult: claims.SequenceCompletenessResult, CoverageResult: claims.CoverageResult,
		RuntimeIdentitySHA256: claims.RuntimeIdentitySHA256, BuildIdentitySHA256: buildDigest,
		ConfigIdentitySHA256: configDigest, OrderInventorySHA256: claims.OrderInventorySHA256,
		ExternalWriterInventorySHA256: claims.ExternalWriterInventorySHA256,
		InFlightResolutionSHA256:      claims.InFlightResolutionSHA256,
		ObservationStartedAt:          ledger.Capture.Start, ObservationEndedAt: ledger.Capture.End,
	}
	attestation, err := attestationVerifier.VerifyObservation(context.Background(), token, wantObservation)
	if err != nil {
		t.Fatalf("verify existing selector attestation: %v", err)
	}
	expected := selectorinventory.ExternalLedgerExpectation{
		Controller: f.controller, ObservationID: ledger.ObservationID, Audience: audience, Workspace: workspace,
		CandidateManifestSHA256: candidateDigest, ExecutionGeneration: f.execution,
		BuildIdentitySHA256: buildDigest, ConfigIdentitySHA256: configDigest,
		TrustedNow: f.now, MaxObservationAge: time.Hour,
		ExternalScope: MandatoryScopes(), RequiredResultAtoms: []string{completeAtom},
		ResolutionKey: resolutionKey, Attestation: attestation,
	}
	joined := verifiedHost.VerifyAndJoin(context.Background(), raw, expected, registry, f.hostID, f.bootID)
	if joined.Status != selectorinventory.StatusAvailable || joined.IssueCode != "" || joined.Evidence == nil || !joined.Evidence.Valid() {
		t.Fatalf("host verification did not feed canonical external ledger join: %#v", joined)
	}
	if joined.Evidence.LedgerDigestSHA256() != ledger.DigestSHA256 {
		t.Fatalf("joined digest = %s, want canonical ledger digest %s", joined.Evidence.LedgerDigestSHA256(), ledger.DigestSHA256)
	}
}

func verifiedHostRecordLedger(t *testing.T, verifier *Verifier, raw []byte, f *collectorFixture) []byte {
	t.Helper()
	verified, err := verifier.Verify(context.Background(), raw, f.now, f.hostID, f.bootID)
	if err != nil {
		t.Fatalf("verify host record: %v", err)
	}
	return verified.ExternalLedgerJSON()
}

func observationToken(t *testing.T, claims selectorattestation.ObservationClaims, privateKey ed25519.PrivateKey) string {
	t.Helper()
	signingBytes, err := selectorattestation.ObservationSigningBytes(claims)
	if err != nil {
		t.Fatalf("canonicalize observation claims: %v", err)
	}
	payload := bytes.TrimPrefix(signingBytes, []byte(selectorattestation.ObservationSigningDomain))
	signature := ed25519.Sign(privateKey, signingBytes)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func TestMandatoryScopesAreStableAndBounded(t *testing.T) {
	if len(MandatoryScopes()) != 5 || strings.Join(MandatoryScopes(), ",") != "cron,manual,orders,services,startup" {
		t.Fatalf("mandatory scope contract = %#v", MandatoryScopes())
	}
}
