package controlgrant

import (
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
)

const (
	testAuthorityStoreRef = "authority:workflow-control"
	testLedgerBeadPrefix  = "gc"
)

type ledgerGrantIssuer struct {
	verifier *Verifier
	private  ed25519.PrivateKey
	claims   Claims
	now      time.Time
}

func newLedgerGrantIssuer(t *testing.T) *ledgerGrantIssuer {
	t.Helper()
	verifier, private, claims, _, now := controlGrantFixture(t)
	return &ledgerGrantIssuer{verifier: verifier, private: private, claims: claims, now: now}
}

func (issuer *ledgerGrantIssuer) verify(t *testing.T, claims Claims) VerifiedGrant {
	t.Helper()
	want := Expectation{
		Kind: claims.Kind, City: claims.City, StoreRef: claims.StoreRef, ControlID: claims.ControlID,
		ControlRevision: claims.ControlRevision, WorkID: claims.WorkID, WorkRevision: claims.WorkRevision,
		Owner: claims.Owner, ExecutionGeneration: claims.ExecutionGeneration, IdempotencyKey: claims.IdempotencyKey,
	}
	grant, err := issuer.verifier.Verify(tokenForControlGrant(t, claims, issuer.private), want)
	if err != nil {
		t.Fatalf("verify test grant: %v", err)
	}
	return grant
}

func testLedger(t *testing.T, store beads.Store, now time.Time) *Ledger {
	t.Helper()
	ledger, err := NewLedger(store, testAuthorityStoreRef, testLedgerBeadPrefix, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewLedger: %v", err)
	}
	return ledger
}

func testExplicitMemStore() *beads.MemStore {
	store := beads.NewMemStore()
	store.HonorExplicitIDs = true
	return store
}

type stableIDStoreWithoutMetadataCAS struct {
	beads.Store
}

func (s stableIDStoreWithoutMetadataCAS) StableCreateIDResolveTarget() beads.Store {
	return s.Store
}

type prefixFencedLedgerStore struct {
	beads.Store
	prefix string
}

func (s prefixFencedLedgerStore) Create(bead beads.Bead) (beads.Bead, error) {
	if !strings.HasPrefix(bead.ID, s.prefix+"-") {
		return beads.Bead{}, beads.ErrPinnedIDOutsideNamespace
	}
	return s.Store.Create(bead)
}

func (s prefixFencedLedgerStore) StableCreateIDResolveTarget() beads.Store {
	return s.Store
}

func (s prefixFencedLedgerStore) ConditionalWritesResolveTarget() beads.Store {
	return s.Store
}

func TestLedgerReservesOnceAndSettlesKnownOutcome(t *testing.T) {
	issuer := newLedgerGrantIssuer(t)
	grant := issuer.verify(t, issuer.claims)
	store := testExplicitMemStore()
	ledger := testLedger(t, store, issuer.now)

	first, err := ledger.Reserve(grant, 3)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if !first.Execute || first.Existing || first.Receipt.State != ReceiptReserved || !first.Receipt.Uncertain {
		t.Fatalf("first reservation = %#v, want sole permit with uncertain reserved receipt", first)
	}
	duplicate, err := ledger.Reserve(grant, 3)
	if err != nil {
		t.Fatalf("duplicate Reserve: %v", err)
	}
	if duplicate.Execute || !duplicate.Existing || !duplicate.Receipt.Uncertain || duplicate.Receipt.State != ReceiptReserved {
		t.Fatalf("duplicate reservation = %#v, want existing uncertain receipt without execution", duplicate)
	}

	receipt, err := ledger.RecordKnownOutcome(grant, 2, "completed")
	if err != nil {
		t.Fatalf("RecordKnownOutcome: %v", err)
	}
	if receipt.State != ReceiptKnown || receipt.EffectsReserved != 3 || receipt.EffectsCharged != 2 || receipt.OutcomeCode != "completed" || receipt.Uncertain {
		t.Fatalf("known receipt = %#v, want known outcome with two charged effects", receipt)
	}
	replayed, err := ledger.RecordKnownOutcome(grant, 2, "completed")
	if err != nil || replayed != receipt {
		t.Fatalf("replayed outcome = %#v, %v; want identical receipt", replayed, err)
	}
	if _, err := ledger.RecordKnownOutcome(grant, 1, "different"); !errors.Is(err, ErrLedgerConflict) {
		t.Fatalf("conflicting terminal outcome = %v, want ErrLedgerConflict", err)
	}
}

func TestLedgerChargesUnknownAndReservedEffectsAcrossGrantTokens(t *testing.T) {
	issuer := newLedgerGrantIssuer(t)
	store := testExplicitMemStore()
	ledger := testLedger(t, store, issuer.now)
	firstGrant := issuer.verify(t, issuer.claims)
	first, err := ledger.Reserve(firstGrant, 4)
	if err != nil || !first.Execute {
		t.Fatalf("first Reserve = %#v, %v", first, err)
	}
	unknown, err := ledger.RecordUnknownOutcome(firstGrant, "transport_unknown")
	if err != nil {
		t.Fatalf("RecordUnknownOutcome: %v", err)
	}
	if unknown.State != ReceiptUnknown || unknown.EffectsReserved != 4 || unknown.EffectsCharged != 4 || !unknown.Uncertain {
		t.Fatalf("unknown receipt = %#v, want full four-effect charge", unknown)
	}
	prior, err := ledger.Reserve(firstGrant, 4)
	if err != nil || prior.Execute || !prior.Existing || prior.Receipt.State != ReceiptUnknown {
		t.Fatalf("Reserve after unknown outcome = %#v, %v; want prior receipt without execution", prior, err)
	}

	secondClaims := issuer.claims
	secondClaims.TokenID = "token-002"
	secondClaims.IdempotencyKey = "request-002"
	secondGrant := issuer.verify(t, secondClaims)
	second, err := ledger.Reserve(secondGrant, 2)
	if !errors.Is(err, ErrBudgetExhausted) || second.Execute {
		t.Fatalf("Reserve beyond unknown charge = %#v, %v; want exhausted budget", second, err)
	}
	second, err = ledger.Reserve(secondGrant, 1)
	if err != nil || !second.Execute {
		t.Fatalf("Reserve remaining one effect = %#v, %v", second, err)
	}
	if _, err := ledger.RecordKnownOutcome(secondGrant, 0, "known_no_effect"); err != nil {
		t.Fatalf("RecordKnownOutcome with zero actual effects: %v", err)
	}

	thirdClaims := issuer.claims
	thirdClaims.TokenID = "token-003"
	thirdClaims.IdempotencyKey = "request-003"
	thirdGrant := issuer.verify(t, thirdClaims)
	third, err := ledger.Reserve(thirdGrant, 1)
	if !errors.Is(err, ErrBudgetExhausted) || third.Execute {
		t.Fatalf("Reserve beyond invocation limit = %#v, %v; want exhausted invocation budget", third, err)
	}
}

func TestLedgerRejectsChangedPolicyAndChangedIdempotentRequest(t *testing.T) {
	issuer := newLedgerGrantIssuer(t)
	store := testExplicitMemStore()
	ledger := testLedger(t, store, issuer.now)
	grant := issuer.verify(t, issuer.claims)
	if reservation, err := ledger.Reserve(grant, 1); err != nil || !reservation.Execute {
		t.Fatalf("initial Reserve = %#v, %v", reservation, err)
	}

	changedPolicy := issuer.claims
	changedPolicy.TokenID = "token-002"
	changedPolicy.IdempotencyKey = "request-002"
	changedPolicy.EffectLimit++
	changedPolicyGrant := issuer.verify(t, changedPolicy)
	if _, err := ledger.Reserve(changedPolicyGrant, 1); !errors.Is(err, ErrLedgerConflict) {
		t.Fatalf("Reserve changed signed grant policy = %v, want ErrLedgerConflict", err)
	}

	changedRequest := issuer.claims
	changedRequest.WorkRevision++
	changedRequestGrant := issuer.verify(t, changedRequest)
	if _, err := ledger.Reserve(changedRequestGrant, 1); !errors.Is(err, ErrLedgerConflict) {
		t.Fatalf("Reserve reused idempotency key with changed work revision = %v, want ErrLedgerConflict", err)
	}
	if _, err := ledger.Reserve(grant, 2); !errors.Is(err, ErrLedgerConflict) {
		t.Fatalf("Reserve reused idempotency key with changed effect request = %v, want ErrLedgerConflict", err)
	}
}

func TestLedgerRefusesForgedOrMutatedVerifiedGrant(t *testing.T) {
	issuer := newLedgerGrantIssuer(t)
	store := testExplicitMemStore()
	ledger := testLedger(t, store, issuer.now)
	grant := issuer.verify(t, issuer.claims)

	if _, err := ledger.Reserve(VerifiedGrant{Claims: grant.Claims, Principal: grant.Principal, Budget: grant.Budget}, 1); !errors.Is(err, ErrGrantNotVerified) {
		t.Fatalf("caller-constructed grant = %v, want ErrGrantNotVerified", err)
	}
	grant.Claims.WorkRevision++
	if _, err := ledger.Reserve(grant, 1); !errors.Is(err, ErrGrantNotVerified) {
		t.Fatalf("mutated verified grant = %v, want ErrGrantNotVerified", err)
	}
}

func TestLedgerReturnsPendingReceiptAfterFileStoreReopen(t *testing.T) {
	issuer := newLedgerGrantIssuer(t)
	path := filepath.Join(t.TempDir(), "beads.json")
	firstStore, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	firstStore.HonorExplicitIDs = true
	firstLedger := testLedger(t, firstStore, issuer.now)
	grant := issuer.verify(t, issuer.claims)
	first, err := firstLedger.Reserve(grant, 3)
	if err != nil || !first.Execute {
		t.Fatalf("first Reserve = %#v, %v", first, err)
	}

	reopened, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	reopened.HonorExplicitIDs = true
	second, err := testLedger(t, reopened, issuer.now).Reserve(grant, 3)
	if err != nil {
		t.Fatalf("Reserve after reopen: %v", err)
	}
	if second.Execute || !second.Existing || !second.Receipt.Uncertain || second.Receipt.State != ReceiptReserved {
		t.Fatalf("reopened reservation = %#v, want uncertain receipt without re-execution", second)
	}
}

func TestLedgerConcurrentDuplicateHasOneExecutionPermit(t *testing.T) {
	issuer := newLedgerGrantIssuer(t)
	ledger := testLedger(t, testExplicitMemStore(), issuer.now)
	grant := issuer.verify(t, issuer.claims)
	const callers = 16
	var executeCount atomic.Int32
	var existingCount atomic.Int32
	var wait sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, callers)
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			decision, err := ledger.Reserve(grant, 2)
			if err != nil {
				errs <- err
				return
			}
			if decision.Execute {
				executeCount.Add(1)
			}
			if decision.Existing {
				existingCount.Add(1)
			}
		}()
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent Reserve: %v", err)
	}
	if got := executeCount.Load(); got != 1 {
		t.Errorf("execution permits = %d, want exactly one", got)
	}
	if got := existingCount.Load(); got != callers-1 {
		t.Errorf("existing receipts = %d, want %d", got, callers-1)
	}
}

func TestLedgerConcurrentDistinctReservationsRespectExactBudget(t *testing.T) {
	issuer := newLedgerGrantIssuer(t)
	store := testExplicitMemStore()
	ledger := testLedger(t, store, issuer.now)
	const callers = 24
	const budget = uint64(5)
	grants := make([]VerifiedGrant, callers)
	for index := range callers {
		claims := issuer.claims
		claims.InvocationLimit = budget
		claims.EffectLimit = budget
		claims.TokenID = fmt.Sprintf("budget-token-%02d", index)
		claims.IdempotencyKey = fmt.Sprintf("budget-request-%02d", index)
		grants[index] = issuer.verify(t, claims)
	}

	start := make(chan struct{})
	var wait sync.WaitGroup
	var executeCount atomic.Int32
	var exhaustedCount atomic.Int32
	errs := make(chan error, callers)
	for _, grant := range grants {
		grant := grant
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			decision, err := ledger.Reserve(grant, 1)
			if errors.Is(err, ErrBudgetExhausted) {
				exhaustedCount.Add(1)
				return
			}
			if err != nil {
				errs <- err
				return
			}
			if decision.Execute {
				executeCount.Add(1)
			} else {
				errs <- errors.New("distinct idempotency request got no execution permit without an error")
			}
		}()
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent Reserve: %v", err)
	}
	if got := executeCount.Load(); got != int32(budget) {
		t.Errorf("execution permits = %d, want exactly budget %d", got, budget)
	}
	if got := exhaustedCount.Load(); got != int32(callers-int(budget)) {
		t.Errorf("budget rejections = %d, want %d", got, callers-int(budget))
	}
	identity := budgetIdentityDigest(ledger.authorityStoreRef, ledger.beadPrefix, grants[0])
	_, _, state, exists, err := ledger.loadLedger(identity)
	if err != nil || !exists {
		t.Fatalf("read budget ledger: %v", err)
	}
	invocations, effects, err := grantUsage(state, identity)
	if err != nil || invocations != budget || effects != budget {
		t.Fatalf("recorded usage = invocations %d, effects %d, err %v; want exact budget %d", invocations, effects, err, budget)
	}
}

func TestLedgerBeadIsExcludedFromReadyProjection(t *testing.T) {
	issuer := newLedgerGrantIssuer(t)
	store := prefixFencedLedgerStore{Store: testExplicitMemStore(), prefix: testLedgerBeadPrefix}
	ledger := testLedger(t, store, issuer.now)
	grant := issuer.verify(t, issuer.claims)
	reservation, err := ledger.Reserve(grant, 1)
	if err != nil || !reservation.Execute {
		t.Fatalf("Reserve = %#v, %v", reservation, err)
	}
	beadID := ledgerBeadID(ledger.authorityStoreRef, ledger.beadPrefix, budgetIdentityDigest(ledger.authorityStoreRef, ledger.beadPrefix, grant))
	bead, err := store.Get(beadID)
	if err != nil {
		t.Fatalf("get ledger bead: %v", err)
	}
	if bead.Status != "open" || bead.Type != "gate" || !hasLedgerLabel(bead) {
		t.Fatalf("ledger bead = %#v, want open bookkeeping gate with reserved label", bead)
	}
	open, err := store.ListOpen()
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	if !containsLedgerBead(open, beadID) {
		t.Fatal("ledger bead should remain visible to explicit open bookkeeping queries")
	}
	assertLedgerNotReady(t, store, beadID)

	if _, err := ledger.RecordKnownOutcome(grant, 1, "completed"); err != nil {
		t.Fatalf("metadata CAS outcome update: %v", err)
	}
	updated, err := store.Get(beadID)
	if err != nil {
		t.Fatalf("get updated ledger bead: %v", err)
	}
	if updated.Type != "gate" || !hasLedgerLabel(updated) {
		t.Fatalf("updated ledger bead = %#v, want reserved gate identity retained", updated)
	}
	assertLedgerNotReady(t, store, beadID)
}

func TestLedgerRefusesMismatchedStoreNamespace(t *testing.T) {
	issuer := newLedgerGrantIssuer(t)
	backing := testExplicitMemStore()
	store := prefixFencedLedgerStore{Store: backing, prefix: "gcn"}
	ledger := testLedger(t, store, issuer.now)
	grant := issuer.verify(t, issuer.claims)
	if decision, err := ledger.Reserve(grant, 1); !errors.Is(err, ErrLedgerUnavailable) || decision.Execute {
		t.Fatalf("Reserve with mismatched authority namespace = %#v, %v; want unavailable without execution", decision, err)
	}
	if rows, err := backing.ListOpen(); err != nil || len(rows) != 0 {
		t.Fatalf("mismatched namespace created rows: %#v, %v", rows, err)
	}
}

func TestLedgerBeadIDUsesConfiguredPrefixAndPinnedBackendLength(t *testing.T) {
	identity := digestString("test budget identity")
	id := ledgerBeadID(testAuthorityStoreRef, "gcn", identity)
	if id == "" || !strings.HasPrefix(id, "gcn-") || len(id) > maxLedgerBeadIDBytes {
		t.Fatalf("ledger bead ID = %q (len %d), want configured gcn prefix and <= %d bytes", id, len(id), maxLedgerBeadIDBytes)
	}
	maxPrefixLen := maxLedgerBeadIDBytes - len(ledgerBeadIDSuffix) - 2*sha256.Size
	boundaryPrefix := strings.Repeat("g", maxPrefixLen)
	boundaryID := ledgerBeadID(testAuthorityStoreRef, boundaryPrefix, identity)
	if len(boundaryID) != maxLedgerBeadIDBytes {
		t.Fatalf("boundary ledger bead ID length = %d, want exactly %d", len(boundaryID), maxLedgerBeadIDBytes)
	}
	if ledgerBeadID(testAuthorityStoreRef, boundaryPrefix+"g", identity) != "" {
		t.Fatal("ledger bead ID exceeding the pinned backend's VARCHAR(255) bound was accepted")
	}
}

func TestLedgerRejectsGrantIdentityReuseAcrossTargetScopes(t *testing.T) {
	issuer := newLedgerGrantIssuer(t)
	store := testExplicitMemStore()
	firstLedger := testLedger(t, store, issuer.now)
	firstGrant := issuer.verify(t, issuer.claims)
	if decision, err := firstLedger.Reserve(firstGrant, 1); err != nil || !decision.Execute {
		t.Fatalf("first scope Reserve = %#v, %v", decision, err)
	}

	changedScope := issuer.claims
	changedScope.TokenID = "token-other-scope"
	changedScope.IdempotencyKey = "request-other-scope"
	changedScope.City = "city-b"
	changedScope.StoreRef = "city:city-b"
	changedScope.ControlID = "ga-control-b"
	secondGrant := issuer.verify(t, changedScope)
	secondLedger := testLedger(t, store, issuer.now)
	decision, err := secondLedger.Reserve(secondGrant, 1)
	if !errors.Is(err, ErrLedgerConflict) || decision.Execute {
		t.Fatalf("same GrantID+Principal at a different target = %#v, %v; want global policy conflict", decision, err)
	}
}

func TestLedgerRejectsInvalidTimeWithoutCreatingReservation(t *testing.T) {
	t.Run("expired new request", func(t *testing.T) {
		issuer := newLedgerGrantIssuer(t)
		store := testExplicitMemStore()
		clock := issuer.now
		ledger, err := NewLedger(store, testAuthorityStoreRef, testLedgerBeadPrefix, func() time.Time { return clock })
		if err != nil {
			t.Fatal(err)
		}
		grant := issuer.verify(t, issuer.claims)

		clock = time.Unix(grant.Claims.ExpiresAt, 0)
		if decision, err := ledger.Reserve(grant, 1); !errors.Is(err, ErrExpired) || decision.Execute {
			t.Fatalf("expired Reserve = %#v, %v; want ErrExpired without execution", decision, err)
		}
		assertNoLedgerRows(t, store)
	})

	t.Run("not yet valid new request", func(t *testing.T) {
		issuer := newLedgerGrantIssuer(t)
		store := testExplicitMemStore()
		clock := issuer.now
		ledger, err := NewLedger(store, testAuthorityStoreRef, testLedgerBeadPrefix, func() time.Time { return clock })
		if err != nil {
			t.Fatal(err)
		}
		grant := issuer.verify(t, issuer.claims)

		clock = time.Unix(grant.Claims.IssuedAt-1, 0)
		if decision, err := ledger.Reserve(grant, 1); !errors.Is(err, ErrExpired) || decision.Execute {
			t.Fatalf("not-yet-valid Reserve = %#v, %v; want ErrExpired without execution", decision, err)
		}
		assertNoLedgerRows(t, store)
	})

	t.Run("exact duplicate remains available after expiry", func(t *testing.T) {
		issuer := newLedgerGrantIssuer(t)
		store := testExplicitMemStore()
		clock := issuer.now
		ledger, err := NewLedger(store, testAuthorityStoreRef, testLedgerBeadPrefix, func() time.Time { return clock })
		if err != nil {
			t.Fatal(err)
		}
		grant := issuer.verify(t, issuer.claims)
		first, err := ledger.Reserve(grant, 1)
		if err != nil || !first.Execute {
			t.Fatalf("initial Reserve = %#v, %v", first, err)
		}

		clock = time.Unix(grant.Claims.ExpiresAt, 0)
		duplicate, err := ledger.Reserve(grant, 1)
		if err != nil || duplicate.Execute || !duplicate.Existing || !duplicate.Receipt.Uncertain {
			t.Fatalf("expired duplicate Reserve = %#v, %v; want existing uncertain receipt", duplicate, err)
		}
	})
}

func TestLedgerOutcomeWithoutReservationDoesNotCreateLedger(t *testing.T) {
	for _, status := range []string{"known", "unknown"} {
		t.Run(status, func(t *testing.T) {
			issuer := newLedgerGrantIssuer(t)
			store := testExplicitMemStore()
			ledger := testLedger(t, store, issuer.now)
			grant := issuer.verify(t, issuer.claims)
			var err error
			if status == "known" {
				_, err = ledger.RecordKnownOutcome(grant, 0, "completed")
			} else {
				_, err = ledger.RecordUnknownOutcome(grant, "transport_unknown")
			}
			if !errors.Is(err, ErrLedgerConflict) {
				t.Fatalf("outcome without reservation = %v, want ErrLedgerConflict", err)
			}
			assertNoLedgerRows(t, store)
		})
	}
}

func TestLedgerRequiresBothDurableCapabilitiesAndRejectsCorruption(t *testing.T) {
	issuer := newLedgerGrantIssuer(t)
	unsupported := beads.NewMemStore()
	if _, err := NewLedger(unsupported, testAuthorityStoreRef, testLedgerBeadPrefix, nil); !errors.Is(err, ErrLedgerUnavailable) {
		t.Fatalf("NewLedger without stable create IDs = %v, want unavailable", err)
	}
	if got, err := unsupported.ListOpen(); err != nil || len(got) != 0 {
		t.Fatalf("unsupported store created rows: %#v", got)
	}
	stableOnly := stableIDStoreWithoutMetadataCAS{Store: testExplicitMemStore()}
	if _, err := NewLedger(stableOnly, testAuthorityStoreRef, testLedgerBeadPrefix, nil); !errors.Is(err, ErrLedgerUnavailable) {
		t.Fatalf("NewLedger without metadata CAS = %v, want unavailable", err)
	}
	for _, authorityRef := range []string{
		"", "authority:", "authority:control:ambiguous", "authority:has space", "Authority:workflow-control",
		"authority:workflow--control", "authority:é", strings.Repeat("a", maxLedgerBeadIDBytes) + ":x",
	} {
		if _, err := NewLedger(testExplicitMemStore(), authorityRef, testLedgerBeadPrefix, nil); !errors.Is(err, ErrLedgerUnavailable) {
			t.Errorf("NewLedger with authority-store identity %q = %v, want unavailable", authorityRef, err)
		}
	}
	for _, prefix := range []string{"", "bad prefix", "gc-", strings.Repeat("x", maxLedgerBeadIDBytes)} {
		if _, err := NewLedger(testExplicitMemStore(), testAuthorityStoreRef, prefix, nil); !errors.Is(err, ErrLedgerUnavailable) {
			t.Errorf("NewLedger with bead prefix %q = %v, want unavailable", prefix, err)
		}
	}

	store := testExplicitMemStore()
	ledger := testLedger(t, store, issuer.now)
	grant := issuer.verify(t, issuer.claims)
	if reservation, err := ledger.Reserve(grant, 1); err != nil || !reservation.Execute {
		t.Fatalf("initial Reserve = %#v, %v", reservation, err)
	}
	beadID := ledgerBeadID(ledger.authorityStoreRef, ledger.beadPrefix, budgetIdentityDigest(ledger.authorityStoreRef, ledger.beadPrefix, grant))
	if err := store.SetMetadata(beadID, ledgerMetadataKey, `{"schema_version":`); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Reserve(grant, 1); !errors.Is(err, ErrLedgerUnavailable) {
		t.Fatalf("Reserve with malformed state = %v, want unavailable", err)
	}
	if err := store.SetMetadata(beadID, ledgerMetadataKey, strings.Repeat("x", maxLedgerBytes+1)); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Reserve(grant, 1); !errors.Is(err, ErrLedgerUnavailable) {
		t.Fatalf("Reserve with oversized state = %v, want unavailable", err)
	}
}

func TestLedgerValidatesDeterministicCreateWinnerIdentity(t *testing.T) {
	issuer := newLedgerGrantIssuer(t)
	store := testExplicitMemStore()
	ledger := testLedger(t, store, issuer.now)
	grant := issuer.verify(t, issuer.claims)
	budgetIdentity := budgetIdentityDigest(ledger.authorityStoreRef, ledger.beadPrefix, grant)
	wrongIdentity := ledgerState{
		SchemaVersion:     ledgerSchemaVersion,
		AuthorityStoreRef: ledger.authorityStoreRef,
		BeadPrefix:        ledger.beadPrefix,
		BudgetIdentity:    digestString("another budget identity"),
		Records:           []ledgerRecord{},
	}
	raw, err := encodeLedgerState(wrongIdentity)
	if err != nil {
		t.Fatal(err)
	}
	beadID := ledgerBeadID(ledger.authorityStoreRef, ledger.beadPrefix, budgetIdentity)
	created, err := store.Create(beads.Bead{
		ID: beadID, Type: "gate", Title: ledgerTitle, Labels: []string{ledgerLabel},
		Metadata: beads.StringMap{ledgerMetadataKey: raw},
	})
	if err != nil || created.ID != beadID {
		t.Fatalf("plant deterministic-ID scope winner = %#v, %v", created, err)
	}
	if _, err := ledger.Reserve(grant, 1); !errors.Is(err, ErrLedgerUnavailable) {
		t.Fatalf("Reserve against wrong-identity create winner = %v, want unavailable", err)
	}
	unchanged, err := store.Get(beadID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Metadata[ledgerMetadataKey] != raw {
		t.Fatal("ledger overwrote the deterministic create winner")
	}
}

func containsLedgerBead(items []beads.Bead, id string) bool {
	for _, item := range items {
		if item.ID == id {
			return true
		}
	}
	return false
}

func assertLedgerNotReady(t *testing.T, store beads.Store, id string) {
	t.Helper()
	ready, err := store.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if containsLedgerBead(ready, id) {
		t.Fatalf("ledger bead %q is claimable from Ready projection", id)
	}
}

func assertNoLedgerRows(t *testing.T, store beads.Store) {
	t.Helper()
	rows, err := store.ListOpen()
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("unexpected ledger bookkeeping after refusal: %#v", rows)
	}
}
