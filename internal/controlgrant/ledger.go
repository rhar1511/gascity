package controlgrant

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

const (
	ledgerSchemaVersion  = "gc.workflow-control-ledger.v2"
	ledgerMetadataKey    = "gc.workflow_control_ledger.v2"
	ledgerTitle          = "workflow control reservation ledger"
	ledgerLabel          = "gc:control-ledger"
	ledgerBeadIDSuffix   = "-control-ledger-"
	maxLedgerBeadIDBytes = 255 // beads v1.3.0 issues.id is VARCHAR(255).
	maxLedgerCASAttempts = 64
	maxLedgerRecords     = 4096
	maxLedgerBytes       = 1 << 20
	maxOutcomeCodeBytes  = 128
)

var (
	// ErrLedgerUnavailable means durable idempotency or metadata CAS is absent,
	// corrupt, or cannot be read with the required scope.
	ErrLedgerUnavailable = errors.New("workflow control reservation ledger unavailable")
	// ErrLedgerConflict means an idempotency key, grant identity, or receipt was
	// reused with different signed request or outcome data.
	ErrLedgerConflict = errors.New("workflow control reservation ledger conflict")
	// ErrBudgetExhausted means the signed invocation or effect allowance is full.
	ErrBudgetExhausted = errors.New("workflow control grant budget exhausted")
	// ErrLedgerFull means bounded durable receipt storage has reached capacity.
	ErrLedgerFull = errors.New("workflow control reservation ledger full")
	// ErrGrantNotVerified means a value did not come unchanged from Verifier.Verify.
	ErrGrantNotVerified = errors.New("workflow control grant was not verified")
)

// ReceiptState describes the durable state of one idempotent invocation.
type ReceiptState string

const (
	// ReceiptReserved records budget before the operation starts. It is
	// uncertain until a known outcome is durably recorded, even for the caller
	// that received the one-time Execute permit. A reservation left in this state
	// after a restart cannot execute again.
	ReceiptReserved ReceiptState = "reserved"
	// ReceiptKnown records an outcome whose actual effect count is known.
	ReceiptKnown ReceiptState = "known"
	// ReceiptUnknown records an uncertain outcome and charges the full reservation.
	ReceiptUnknown ReceiptState = "unknown"
)

// Receipt is the caller-facing view of a durable invocation record.
type Receipt struct {
	GrantID         string
	TokenID         string
	IdempotencyKey  string
	State           ReceiptState
	EffectUnit      EffectUnit
	EffectsReserved uint64
	EffectsCharged  uint64
	OutcomeCode     string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	Uncertain       bool
}

// Reservation tells a caller whether this call won the durable reservation.
// Only Execute=true permits an effect. Existing receipts are always returned
// with Execute=false, including a reservation left behind by a crash.
type Reservation struct {
	Execute  bool
	Existing bool
	Receipt  Receipt
}

// Ledger persists invocation reservations and operation-specific effect
// allowance in one metadata-CAS value. It is only a budget and idempotency
// primitive: it does not fence multi-store effects or make a control dispatcher
// transactional.
type Ledger struct {
	store             beads.Store
	authorityStoreRef string
	beadPrefix        string
	now               func() time.Time
}

// NewLedger binds to the server-owned durable authority ledger store. All
// target scopes must share this store, its canonical authorityStoreRef, and
// its exact beadPrefix so a GrantID and Principal cannot reset their budget by
// targeting another control or bead store. Target StoreRef and ControlID are
// bound by the signed policy digest inside the ledger. Stores must honor stable
// caller-supplied IDs and metadata CAS; there is no in-memory or unconditional
// fallback.
func NewLedger(store beads.Store, authorityStoreRef, beadPrefix string, now func() time.Time) (*Ledger, error) {
	if store == nil || !validAuthorityStoreRef(authorityStoreRef) || !validLedgerBeadPrefix(beadPrefix) {
		return nil, fmt.Errorf("ledger needs a store, canonical authority-store identity, and exact bead namespace prefix: %w", ErrLedgerUnavailable)
	}
	if !beads.StableCreateIDFor(store) {
		return nil, fmt.Errorf("ledger store does not support stable create IDs: %w", ErrLedgerUnavailable)
	}
	if _, ok := beads.MetadataCASWriterFor(store); !ok {
		return nil, fmt.Errorf("ledger store does not support metadata CAS: %w", ErrLedgerUnavailable)
	}
	if now == nil {
		now = time.Now
	}
	return &Ledger{store: store, authorityStoreRef: authorityStoreRef, beadPrefix: beadPrefix, now: now}, nil
}

// Reserve atomically charges one invocation and holds effects of the grant's
// signed operation-specific unit before execution. A duplicate exact request
// returns its receipt without permission to execute again.
func (l *Ledger) Reserve(grant VerifiedGrant, effects uint64) (Reservation, error) {
	if l == nil || l.store == nil {
		return Reservation{}, ErrLedgerUnavailable
	}
	if !validVerifiedGrant(grant) {
		return Reservation{}, ErrGrantNotVerified
	}
	if effects == 0 || grant.Budget.InvocationLimit == 0 || grant.Budget.EffectLimit == 0 ||
		grant.Budget.EffectUnit != grant.Claims.EffectUnit || effects > grant.Budget.EffectLimit {
		return Reservation{}, fmt.Errorf("invalid invocation or effect reservation: %w", ErrMalformed)
	}
	requestHash := requestDigest(grant, effects)
	idemHash := digestString(grant.Claims.IdempotencyKey)
	budgetHash := budgetIdentityDigest(l.authorityStoreRef, l.beadPrefix, grant)
	policyHash := budgetPolicyDigest(grant)
	tokenHash := digestString(grant.Claims.TokenID)
	beadID := ledgerBeadID(l.authorityStoreRef, l.beadPrefix, budgetHash)

	for range maxLedgerCASAttempts {
		_, rawState, state, exists, err := l.loadLedger(budgetHash)
		if err != nil {
			return Reservation{}, err
		}
		if prior, ok := findByIdempotency(state, idemHash); ok {
			if prior.RequestDigest != requestHash || prior.BudgetIdentity != budgetHash || prior.PolicyDigest != policyHash || prior.TokenDigest != tokenHash {
				return Reservation{}, ErrLedgerConflict
			}
			return Reservation{Existing: true, Receipt: receiptForRecord(grant, prior)}, nil
		}
		if prior, ok := findByToken(state, budgetHash, tokenHash); ok {
			if prior.RequestDigest != requestHash || prior.IdempotencyDigest != idemHash {
				return Reservation{}, ErrLedgerConflict
			}
			return Reservation{Existing: true, Receipt: receiptForRecord(grant, prior)}, nil
		}
		if err := checkGrantPolicy(state, budgetHash, policyHash, grant); err != nil {
			return Reservation{}, err
		}
		current := l.now()
		if !current.Before(time.Unix(grant.Claims.ExpiresAt, 0)) || current.Before(time.Unix(grant.Claims.IssuedAt, 0)) {
			return Reservation{}, ErrExpired
		}
		if len(state.Records) >= maxLedgerRecords {
			return Reservation{}, ErrLedgerFull
		}
		invocations, effectsUsed, err := grantUsage(state, budgetHash)
		if err != nil {
			return Reservation{}, err
		}
		if invocations >= grant.Budget.InvocationLimit || effectsUsed > grant.Budget.EffectLimit || effects > grant.Budget.EffectLimit-effectsUsed {
			return Reservation{}, ErrBudgetExhausted
		}

		now := current.UTC().UnixNano()
		record := ledgerRecord{
			IdempotencyDigest: idemHash,
			RequestDigest:     requestHash,
			BudgetIdentity:    budgetHash,
			PolicyDigest:      policyHash,
			TokenDigest:       tokenHash,
			State:             ReceiptReserved,
			EffectUnit:        grant.Budget.EffectUnit,
			EffectsReserved:   effects,
			InvocationLimit:   grant.Budget.InvocationLimit,
			EffectLimit:       grant.Budget.EffectLimit,
			CreatedAt:         now,
			UpdatedAt:         now,
		}
		next := cloneLedgerState(state)
		next.Records = append(next.Records, record)
		nextRaw, err := encodeLedgerState(next)
		if err != nil {
			return Reservation{}, err
		}
		if !exists {
			created, createErr := l.store.Create(beads.Bead{
				ID: beadID, Type: "gate", Title: ledgerTitle, Labels: []string{ledgerLabel},
				Metadata: beads.StringMap{ledgerMetadataKey: nextRaw},
			})
			if createErr == nil {
				if created.ID != beadID {
					return Reservation{}, fmt.Errorf("stable create returned ledger ID %q, want %q: %w", created.ID, beadID, ErrLedgerUnavailable)
				}
				if _, _, _, decodeErr := l.decodeLedgerBead(created, budgetHash); decodeErr != nil {
					return Reservation{}, decodeErr
				}
				return Reservation{Execute: true, Receipt: receiptForRecord(grant, record)}, nil
			}
			// A create can lose a deterministic-ID race or fail after committing.
			// Read and validate the winner; a caller without a confirmed create
			// result never receives an execution permit.
			_, _, _, found, readErr := l.loadLedger(budgetHash)
			if readErr != nil {
				return Reservation{}, readErr
			}
			if !found {
				return Reservation{}, fmt.Errorf("create workflow control reservation ledger: %w: %w", ErrLedgerUnavailable, createErr)
			}
			continue
		}
		outcome, err := beads.ApplyMetadataCAS(l.store, beadID, ledgerMetadataKey, rawState, nextRaw)
		if err != nil {
			return Reservation{}, fmt.Errorf("reserve workflow control invocation: %w: %w", ErrLedgerUnavailable, err)
		}
		switch outcome {
		case beads.MetadataCASSwapped:
			return Reservation{Execute: true, Receipt: receiptForRecord(grant, record)}, nil
		case beads.MetadataCASAlreadyNext:
			// A concurrent caller may have written the identical reservation. Its
			// CAS winner is the only caller permitted to execute it.
			fresh, _, _, found, readErr := l.loadLedger(budgetHash)
			if readErr != nil {
				return Reservation{}, readErr
			}
			if !found {
				return Reservation{}, ErrLedgerUnavailable
			}
			if existing, ok := findByIdempotencyFromBead(fresh, idemHash, requestHash, tokenHash, l.authorityStoreRef, l.beadPrefix, budgetHash); ok {
				return Reservation{Existing: true, Receipt: receiptForRecord(grant, existing)}, nil
			}
			continue
		case beads.MetadataCASConflict:
			continue
		default:
			return Reservation{}, fmt.Errorf("unexpected metadata CAS outcome %q: %w", outcome, ErrLedgerUnavailable)
		}
	}
	return Reservation{}, fmt.Errorf("metadata CAS contention exceeded %d attempts: %w", maxLedgerCASAttempts, ErrLedgerUnavailable)
}

// RecordKnownOutcome settles a reserved invocation with its known actual effect
// count. Repeating the same transition is idempotent; a different terminal
// outcome conflicts and never changes the prior receipt.
func (l *Ledger) RecordKnownOutcome(grant VerifiedGrant, actualEffects uint64, outcomeCode string) (Receipt, error) {
	return l.recordOutcome(grant, ReceiptKnown, actualEffects, outcomeCode)
}

// RecordUnknownOutcome settles a reserved invocation whose effects may have
// happened. The full planned allowance is charged so uncertainty cannot restore
// budget or authorize a retry.
func (l *Ledger) RecordUnknownOutcome(grant VerifiedGrant, outcomeCode string) (Receipt, error) {
	return l.recordOutcome(grant, ReceiptUnknown, 0, outcomeCode)
}

func (l *Ledger) recordOutcome(grant VerifiedGrant, status ReceiptState, actualEffects uint64, outcomeCode string) (Receipt, error) {
	if l == nil || l.store == nil {
		return Receipt{}, ErrLedgerUnavailable
	}
	if !validVerifiedGrant(grant) {
		return Receipt{}, ErrGrantNotVerified
	}
	if !validOutcomeCode(outcomeCode) {
		return Receipt{}, fmt.Errorf("invalid workflow control outcome code: %w", ErrMalformed)
	}
	idemHash := digestString(grant.Claims.IdempotencyKey)
	budgetHash := budgetIdentityDigest(l.authorityStoreRef, l.beadPrefix, grant)
	beadID := ledgerBeadID(l.authorityStoreRef, l.beadPrefix, budgetHash)
	for range maxLedgerCASAttempts {
		_, rawState, state, exists, err := l.loadLedger(budgetHash)
		if err != nil {
			return Receipt{}, err
		}
		if !exists {
			return Receipt{}, ErrLedgerConflict
		}
		record, ok := findByIdempotency(state, idemHash)
		if !ok {
			return Receipt{}, ErrLedgerConflict
		}
		if record.BudgetIdentity != budgetIdentityDigest(l.authorityStoreRef, l.beadPrefix, grant) || record.PolicyDigest != budgetPolicyDigest(grant) ||
			record.TokenDigest != digestString(grant.Claims.TokenID) ||
			record.RequestDigest != requestDigest(grant, record.EffectsReserved) {
			return Receipt{}, ErrLedgerConflict
		}
		charged := record.EffectsReserved
		if status == ReceiptKnown {
			if actualEffects > record.EffectsReserved {
				return Receipt{}, ErrLedgerConflict
			}
			charged = actualEffects
		}
		if record.State != ReceiptReserved {
			if record.State == status && record.EffectsCharged == charged && record.OutcomeCode == outcomeCode {
				return receiptForRecord(grant, record), nil
			}
			return Receipt{}, ErrLedgerConflict
		}

		next := cloneLedgerState(state)
		index := recordIndexByIdempotency(next, idemHash)
		if index < 0 {
			return Receipt{}, ErrLedgerUnavailable
		}
		next.Records[index].State = status
		next.Records[index].EffectsCharged = charged
		next.Records[index].OutcomeCode = outcomeCode
		updatedAt := l.now().UTC().UnixNano()
		if updatedAt < next.Records[index].CreatedAt {
			updatedAt = next.Records[index].CreatedAt
		}
		next.Records[index].UpdatedAt = updatedAt
		updated := next.Records[index]
		nextRaw, err := encodeLedgerState(next)
		if err != nil {
			return Receipt{}, err
		}
		outcome, err := beads.ApplyMetadataCAS(l.store, beadID, ledgerMetadataKey, rawState, nextRaw)
		if err != nil {
			return Receipt{}, fmt.Errorf("record workflow control outcome: %w: %w", ErrLedgerUnavailable, err)
		}
		switch outcome {
		case beads.MetadataCASSwapped:
			return receiptForRecord(grant, updated), nil
		case beads.MetadataCASAlreadyNext, beads.MetadataCASConflict:
			continue
		default:
			return Receipt{}, fmt.Errorf("unexpected metadata CAS outcome %q: %w", outcome, ErrLedgerUnavailable)
		}
	}
	return Receipt{}, fmt.Errorf("metadata CAS contention exceeded %d attempts: %w", maxLedgerCASAttempts, ErrLedgerUnavailable)
}

type ledgerState struct {
	SchemaVersion     string         `json:"schema_version"`
	AuthorityStoreRef string         `json:"authority_store_ref"`
	BeadPrefix        string         `json:"bead_prefix"`
	BudgetIdentity    string         `json:"budget_identity"`
	Records           []ledgerRecord `json:"records"`
}

type ledgerRecord struct {
	IdempotencyDigest string       `json:"idempotency_digest"`
	RequestDigest     string       `json:"request_digest"`
	BudgetIdentity    string       `json:"budget_identity"`
	PolicyDigest      string       `json:"policy_digest"`
	TokenDigest       string       `json:"token_digest"`
	State             ReceiptState `json:"state"`
	EffectUnit        EffectUnit   `json:"effect_unit"`
	EffectsReserved   uint64       `json:"effects_reserved"`
	EffectsCharged    uint64       `json:"effects_charged"`
	InvocationLimit   uint64       `json:"invocation_limit"`
	EffectLimit       uint64       `json:"effect_limit"`
	OutcomeCode       string       `json:"outcome_code,omitempty"`
	CreatedAt         int64        `json:"created_at_unix_nano"`
	UpdatedAt         int64        `json:"updated_at_unix_nano"`
}

func (l *Ledger) loadLedger(budgetIdentity string) (beads.Bead, string, ledgerState, bool, error) {
	beadID := ledgerBeadID(l.authorityStoreRef, l.beadPrefix, budgetIdentity)
	if beadID == "" {
		return beads.Bead{}, "", ledgerState{}, false, ErrLedgerUnavailable
	}
	bead, err := l.store.Get(beadID)
	if errors.Is(err, beads.ErrNotFound) {
		return beads.Bead{}, "", ledgerState{
			SchemaVersion:     ledgerSchemaVersion,
			AuthorityStoreRef: l.authorityStoreRef,
			BeadPrefix:        l.beadPrefix,
			BudgetIdentity:    budgetIdentity,
			Records:           []ledgerRecord{},
		}, false, nil
	}
	if err != nil {
		return beads.Bead{}, "", ledgerState{}, false, fmt.Errorf("read reservation ledger: %w: %w", ErrLedgerUnavailable, err)
	}
	loaded, raw, state, err := l.decodeLedgerBead(bead, budgetIdentity)
	if err != nil {
		return beads.Bead{}, "", ledgerState{}, false, err
	}
	return loaded, raw, state, true, nil
}

func (l *Ledger) decodeLedgerBead(bead beads.Bead, budgetIdentity string) (beads.Bead, string, ledgerState, error) {
	if bead.ID != ledgerBeadID(l.authorityStoreRef, l.beadPrefix, budgetIdentity) || bead.Type != "gate" || !hasLedgerLabel(bead) {
		return beads.Bead{}, "", ledgerState{}, fmt.Errorf("deterministic ledger row has wrong identity, type, or label: %w", ErrLedgerUnavailable)
	}
	raw := bead.Metadata[ledgerMetadataKey]
	state, err := decodeLedgerState(raw, l.authorityStoreRef, l.beadPrefix, budgetIdentity)
	if err != nil {
		return beads.Bead{}, "", ledgerState{}, fmt.Errorf("decode reservation ledger: %w: %w", ErrLedgerUnavailable, err)
	}
	return bead, raw, state, nil
}

func encodeLedgerState(state ledgerState) (string, error) {
	if err := validateLedgerState(state, state.AuthorityStoreRef, state.BeadPrefix, state.BudgetIdentity); err != nil {
		return "", fmt.Errorf("validate reservation ledger: %w: %w", ErrLedgerUnavailable, err)
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return "", fmt.Errorf("encode reservation ledger: %w", ErrLedgerUnavailable)
	}
	if len(raw) > maxLedgerBytes {
		return "", ErrLedgerFull
	}
	return string(raw), nil
}

func decodeLedgerState(raw, authorityStoreRef, beadPrefix, budgetIdentity string) (ledgerState, error) {
	if raw == "" || len(raw) > maxLedgerBytes {
		return ledgerState{}, errors.New("reservation ledger is empty or oversized")
	}
	var state ledgerState
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&state); err != nil {
		return ledgerState{}, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return ledgerState{}, errors.New("reservation ledger has trailing JSON")
		}
		return ledgerState{}, err
	}
	canonical, err := json.Marshal(state)
	if err != nil || !bytes.Equal(canonical, []byte(raw)) {
		return ledgerState{}, errors.New("reservation ledger is not canonical JSON")
	}
	if err := validateLedgerState(state, authorityStoreRef, beadPrefix, budgetIdentity); err != nil {
		return ledgerState{}, err
	}
	return state, nil
}

func validateLedgerState(state ledgerState, authorityStoreRef, beadPrefix, budgetIdentity string) error {
	if state.SchemaVersion != ledgerSchemaVersion || state.AuthorityStoreRef != authorityStoreRef || state.BeadPrefix != beadPrefix || state.BudgetIdentity != budgetIdentity ||
		!validAuthorityStoreRef(state.AuthorityStoreRef) || !validLedgerBeadPrefix(state.BeadPrefix) || !isSHA256Hex(state.BudgetIdentity) || state.Records == nil || len(state.Records) > maxLedgerRecords {
		return errors.New("reservation ledger authority identity, version, or record count is invalid")
	}
	type budgetSummary struct {
		policyDigest    string
		invocationLimit uint64
		effectLimit     uint64
		effectUnit      EffectUnit
		invocations     uint64
		effects         uint64
	}
	var summary budgetSummary
	haveSummary := false
	idempotency := make(map[string]struct{}, len(state.Records))
	tokens := make(map[string]struct{}, len(state.Records))
	for _, record := range state.Records {
		if !isSHA256Hex(record.IdempotencyDigest) || !isSHA256Hex(record.RequestDigest) || !isSHA256Hex(record.BudgetIdentity) ||
			record.BudgetIdentity != budgetIdentity ||
			!isSHA256Hex(record.PolicyDigest) || !isSHA256Hex(record.TokenDigest) || !validEffectUnit(record.EffectUnit) ||
			record.EffectsReserved == 0 || record.InvocationLimit == 0 || record.EffectLimit == 0 || record.EffectsReserved > record.EffectLimit ||
			record.EffectsCharged > record.EffectsReserved || record.CreatedAt <= 0 || record.UpdatedAt < record.CreatedAt {
			return errors.New("reservation ledger record identity, budget, or time is invalid")
		}
		if _, duplicate := idempotency[record.IdempotencyDigest]; duplicate {
			return errors.New("reservation ledger repeats an idempotency key")
		}
		idempotency[record.IdempotencyDigest] = struct{}{}
		tokenKey := record.BudgetIdentity + ":" + record.TokenDigest
		if _, duplicate := tokens[tokenKey]; duplicate {
			return errors.New("reservation ledger repeats a grant token")
		}
		tokens[tokenKey] = struct{}{}
		switch record.State {
		case ReceiptReserved:
			if record.EffectsCharged != 0 || record.OutcomeCode != "" {
				return errors.New("reserved record has a terminal outcome")
			}
		case ReceiptKnown:
			if !validOutcomeCode(record.OutcomeCode) {
				return errors.New("known outcome record has an invalid outcome code")
			}
		case ReceiptUnknown:
			if record.EffectsCharged != record.EffectsReserved || !validOutcomeCode(record.OutcomeCode) {
				return errors.New("unknown outcome record did not charge its full reservation")
			}
		default:
			return errors.New("reservation ledger has an unknown state")
		}

		if !haveSummary {
			summary = budgetSummary{policyDigest: record.PolicyDigest, invocationLimit: record.InvocationLimit, effectLimit: record.EffectLimit, effectUnit: record.EffectUnit}
			haveSummary = true
		} else if summary.policyDigest != record.PolicyDigest || summary.invocationLimit != record.InvocationLimit ||
			summary.effectLimit != record.EffectLimit || summary.effectUnit != record.EffectUnit {
			return errors.New("reservation ledger has conflicting policy for one grant identity")
		}
		if summary.invocations == summary.invocationLimit || summary.effects > summary.effectLimit {
			return errors.New("reservation ledger exceeds a grant allowance")
		}
		summary.invocations++
		charged := record.EffectsCharged
		if record.State == ReceiptReserved {
			charged = record.EffectsReserved
		}
		if charged > summary.effectLimit-summary.effects {
			return errors.New("reservation ledger exceeds its effect allowance")
		}
		summary.effects += charged
	}
	return nil
}

func cloneLedgerState(state ledgerState) ledgerState {
	state.Records = append([]ledgerRecord(nil), state.Records...)
	return state
}

func findByIdempotency(state ledgerState, digest string) (ledgerRecord, bool) {
	for _, record := range state.Records {
		if record.IdempotencyDigest == digest {
			return record, true
		}
	}
	return ledgerRecord{}, false
}

func findByToken(state ledgerState, budgetIdentity, tokenDigest string) (ledgerRecord, bool) {
	for _, record := range state.Records {
		if record.BudgetIdentity == budgetIdentity && record.TokenDigest == tokenDigest {
			return record, true
		}
	}
	return ledgerRecord{}, false
}

func findByIdempotencyFromBead(bead beads.Bead, digest, requestHash, tokenHash string, authorityStoreRef, beadPrefix, budgetIdentity string) (ledgerRecord, bool) {
	state, err := decodeLedgerState(bead.Metadata[ledgerMetadataKey], authorityStoreRef, beadPrefix, budgetIdentity)
	if err != nil {
		return ledgerRecord{}, false
	}
	record, ok := findByIdempotency(state, digest)
	if !ok || record.RequestDigest != requestHash || record.TokenDigest != tokenHash {
		return ledgerRecord{}, false
	}
	return record, true
}

func recordIndexByIdempotency(state ledgerState, digest string) int {
	for index, record := range state.Records {
		if record.IdempotencyDigest == digest {
			return index
		}
	}
	return -1
}

func checkGrantPolicy(state ledgerState, budgetIdentity, policyDigest string, grant VerifiedGrant) error {
	for _, record := range state.Records {
		if record.BudgetIdentity != budgetIdentity {
			continue
		}
		if record.PolicyDigest != policyDigest || record.InvocationLimit != grant.Budget.InvocationLimit ||
			record.EffectLimit != grant.Budget.EffectLimit || record.EffectUnit != grant.Budget.EffectUnit {
			return ErrLedgerConflict
		}
	}
	return nil
}

func grantUsage(state ledgerState, budgetIdentity string) (uint64, uint64, error) {
	var invocations, effects uint64
	for _, record := range state.Records {
		if record.BudgetIdentity != budgetIdentity {
			continue
		}
		if invocations == ^uint64(0) {
			return 0, 0, ErrLedgerUnavailable
		}
		invocations++
		charged := record.EffectsCharged
		if record.State == ReceiptReserved {
			charged = record.EffectsReserved
		}
		if charged > ^uint64(0)-effects {
			return 0, 0, ErrLedgerUnavailable
		}
		effects += charged
	}
	return invocations, effects, nil
}

func receiptForRecord(grant VerifiedGrant, record ledgerRecord) Receipt {
	return Receipt{
		GrantID: grant.Claims.GrantID, TokenID: grant.Claims.TokenID, IdempotencyKey: grant.Claims.IdempotencyKey,
		State: record.State, EffectUnit: record.EffectUnit, EffectsReserved: record.EffectsReserved,
		EffectsCharged: record.EffectsCharged, OutcomeCode: record.OutcomeCode,
		CreatedAt: time.Unix(0, record.CreatedAt).UTC(), UpdatedAt: time.Unix(0, record.UpdatedAt).UTC(),
		Uncertain: record.State == ReceiptReserved || record.State == ReceiptUnknown,
	}
}

func requestDigest(grant VerifiedGrant, effects uint64) string {
	canonical, _ := json.Marshal(struct {
		Claims    Claims    `json:"claims"`
		Principal Principal `json:"principal"`
		Budget    Budget    `json:"budget"`
		Effects   uint64    `json:"effects"`
	}{Claims: grant.Claims, Principal: grant.Principal, Budget: grant.Budget, Effects: effects})
	return digestBytes(canonical)
}

func budgetIdentityDigest(authorityStoreRef, beadPrefix string, grant VerifiedGrant) string {
	canonical, _ := json.Marshal(struct {
		AuthorityStoreRef string    `json:"authority_store_ref"`
		BeadPrefix        string    `json:"bead_prefix"`
		GrantID           string    `json:"grant_id"`
		Principal         Principal `json:"principal"`
	}{AuthorityStoreRef: authorityStoreRef, BeadPrefix: beadPrefix, GrantID: grant.Claims.GrantID, Principal: grant.Principal})
	return digestBytes(canonical)
}

func budgetPolicyDigest(grant VerifiedGrant) string {
	c := grant.Claims
	canonical, _ := json.Marshal(struct {
		GrantID             string     `json:"grant_id"`
		Principal           Principal  `json:"principal"`
		Kind                Kind       `json:"kind"`
		City                string     `json:"city"`
		StoreRef            string     `json:"store_ref"`
		ControlID           string     `json:"control_id"`
		ControlRevision     int64      `json:"control_revision"`
		WorkID              string     `json:"work_id"`
		WorkRevision        int64      `json:"work_revision"`
		Owner               string     `json:"owner"`
		ExecutionGeneration uint64     `json:"execution_generation"`
		InvocationLimit     uint64     `json:"invocation_limit"`
		EffectLimit         uint64     `json:"effect_limit"`
		EffectUnit          EffectUnit `json:"effect_unit"`
	}{
		GrantID: c.GrantID, Principal: grant.Principal, Kind: c.Kind, City: c.City, StoreRef: c.StoreRef,
		ControlID: c.ControlID, ControlRevision: c.ControlRevision, WorkID: c.WorkID, WorkRevision: c.WorkRevision,
		Owner: c.Owner, ExecutionGeneration: c.ExecutionGeneration, InvocationLimit: grant.Budget.InvocationLimit,
		EffectLimit: grant.Budget.EffectLimit, EffectUnit: grant.Budget.EffectUnit,
	})
	return digestBytes(canonical)
}

func digestString(value string) string { return digestBytes([]byte(value)) }

func digestBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func isSHA256Hex(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

func validOutcomeCode(value string) bool {
	return len(value) > 0 && len(value) <= maxOutcomeCodeBytes && validAtom(value)
}

func validEffectUnit(unit EffectUnit) bool {
	switch unit {
	case EffectRetryAttempts, EffectRalphAttempts, EffectFanoutChildren, EffectDrainMembers:
		return true
	default:
		return false
	}
}

// validAuthorityStoreRef accepts the canonical server identity grammar
// `<kind>:<scope>`. Both components use lowercase ASCII letters and digits,
// with single internal `-`, `_`, or `.` separators; kind starts with a letter
// and scope starts with an alphanumeric. This is intentionally narrower than
// validAtom and does not parse client-facing workflow StoreRefs.
func validAuthorityStoreRef(value string) bool {
	if len(value) == 0 || len(value) > maxLedgerBeadIDBytes || strings.Count(value, ":") != 1 {
		return false
	}
	kind, scope, _ := strings.Cut(value, ":")
	return validAuthorityStoreComponent(kind, true) && validAuthorityStoreComponent(scope, false)
}

func validAuthorityStoreComponent(value string, startsWithLetter bool) bool {
	if value == "" {
		return false
	}
	previousSeparator := false
	for index := 0; index < len(value); index++ {
		character := value[index]
		isLetter := character >= 'a' && character <= 'z'
		isDigit := character >= '0' && character <= '9'
		if isLetter || isDigit {
			if index == 0 && startsWithLetter && !isLetter {
				return false
			}
			previousSeparator = false
			continue
		}
		switch character {
		case '-', '_', '.':
			if index == 0 || index == len(value)-1 || previousSeparator {
				return false
			}
			previousSeparator = true
		default:
			return false
		}
	}
	return true
}

func hasLedgerLabel(bead beads.Bead) bool {
	for _, label := range bead.Labels {
		if label == ledgerLabel {
			return true
		}
	}
	return false
}

func validLedgerBeadPrefix(prefix string) bool {
	if prefix == "" || len(prefix)+len(ledgerBeadIDSuffix)+sha256.Size*2 > maxLedgerBeadIDBytes {
		return false
	}
	for index, r := range prefix {
		isLetter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
		isDigit := r >= '0' && r <= '9'
		if !(isLetter || isDigit || index > 0 && (r == '-' || r == '_' || r == '.')) {
			return false
		}
	}
	return !strings.HasSuffix(prefix, "-")
}

func ledgerBeadID(authorityStoreRef, beadPrefix, budgetIdentity string) string {
	if !validAuthorityStoreRef(authorityStoreRef) || !validLedgerBeadPrefix(beadPrefix) || !isSHA256Hex(budgetIdentity) {
		return ""
	}
	id := beadPrefix + ledgerBeadIDSuffix + digestString(authorityStoreRef+"\x00"+beadPrefix+"\x00"+budgetIdentity)
	if len(id) > maxLedgerBeadIDBytes {
		return ""
	}
	return id
}
