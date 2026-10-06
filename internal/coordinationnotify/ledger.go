package coordinationnotify

import (
	"regexp"
	"sort"
	"sync"
)

// Attempt identifies one transport event and its elapsed time from the first
// attempt. Callers persist the event ID before invoking a channel adapter.
type Attempt struct {
	EventID        string `json:"event_id"`
	Channel        string `json:"channel"`
	ElapsedSeconds int    `json:"elapsed_seconds"`
}

// Receipt records a channel attempt against one decision epoch.
type Receipt struct {
	DedupKey         string `json:"dedup_key"`
	HoldEpoch        uint64 `json:"hold_epoch"`
	EventID          string `json:"event_id"`
	Channel          string `json:"channel"`
	Attempt          uint64 `json:"attempt"`
	ElapsedSeconds   int    `json:"elapsed_seconds"`
	Suppressed       bool   `json:"suppressed"`
	SuppressionCause string `json:"suppression_cause,omitempty"`
}

// LedgerSnapshot is a deterministic serializable receipt projection. Persist
// snapshots at the consumer's storage boundary; this package owns no I/O.
type LedgerSnapshot struct {
	Receipts []Receipt `json:"receipts"`
}

// Ledger tracks same-epoch receipts in memory for deterministic replay. A
// caller persists Snapshot with its normal durable store.
type Ledger struct {
	mu       sync.Mutex
	events   map[string]bool
	attempts map[string]uint64
	receipts []Receipt
}

// NewLedger returns an empty dedup ledger.
func NewLedger() *Ledger {
	return &Ledger{events: make(map[string]bool), attempts: make(map[string]uint64)}
}

// NewLedgerFromSnapshot restores dedup state from a persisted receipt snapshot.
func NewLedgerFromSnapshot(snapshot LedgerSnapshot) *Ledger {
	l := NewLedger()
	for _, receipt := range snapshot.Receipts {
		if receipt.Suppressed {
			receipt = safeSuppressedReceipt(receipt)
			l.receipts = append(l.receipts, receipt)
			continue
		}
		if !dedupKeyPattern.MatchString(receipt.DedupKey) || receipt.HoldEpoch == 0 ||
			!eventIDPattern.MatchString(receipt.EventID) || !validChannel(receipt.Channel) {
			continue
		}
		l.receipts = append(l.receipts, receipt)
		if receipt.Attempt == 0 {
			continue
		}
		key := ledgerKey(receipt.DedupKey, receipt.HoldEpoch, receipt.Channel)
		l.events[eventKey(key, receipt.EventID)] = true
		if receipt.Attempt > l.attempts[key] {
			l.attempts[key] = receipt.Attempt
		}
	}
	return l
}

// Record appends one accepted attempt or one fail-closed suppression receipt.
// Distinct event IDs permit bounded retries; replaying an event never sends it
// twice, and suppressed-only snapshots never create accepted dedup state.
func (l *Ledger) Record(envelope *Envelope, attempt Attempt) Receipt {
	if l == nil {
		return suppressedReceipt(envelope, attempt, "invalid_attempt")
	}
	if envelope == nil || !dedupKeyPattern.MatchString(envelope.DedupKey) || envelope.HoldEpoch == 0 ||
		!validChannel(envelope.Channel) || !validChannel(envelope.Delivery.FailoverChannel) ||
		envelope.Channel == envelope.Delivery.FailoverChannel || validateDeliveryPolicy(envelope.Delivery) != nil ||
		!eventIDPattern.MatchString(attempt.EventID) || attempt.ElapsedSeconds < 0 ||
		(attempt.Channel != envelope.Channel && attempt.Channel != envelope.Delivery.FailoverChannel) {
		receipt := suppressedReceipt(envelope, attempt, "invalid_attempt")
		l.mu.Lock()
		l.receipts = append(l.receipts, receipt)
		l.mu.Unlock()
		return receipt
	}
	key := ledgerKey(envelope.DedupKey, envelope.HoldEpoch, attempt.Channel)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.events == nil {
		l.events = make(map[string]bool)
	}
	if l.attempts == nil {
		l.attempts = make(map[string]uint64)
	}
	if l.events[eventKey(key, attempt.EventID)] {
		receipt := suppressedReceipt(envelope, attempt, "duplicate_event")
		receipt.Attempt = l.attempts[key]
		l.receipts = append(l.receipts, receipt)
		return receipt
	}
	next := l.attempts[key] + 1
	maxAttempts := uint64(envelope.Delivery.MaxAttempts)
	if attempt.Channel == envelope.Delivery.FailoverChannel {
		maxAttempts = 1
	}
	if next > maxAttempts {
		receipt := suppressedReceipt(envelope, attempt, "max_attempts")
		receipt.Attempt = next
		l.receipts = append(l.receipts, receipt)
		return receipt
	}
	if attempt.ElapsedSeconds > envelope.Delivery.DeadlineSeconds {
		receipt := suppressedReceipt(envelope, attempt, "deadline")
		receipt.Attempt = next
		l.receipts = append(l.receipts, receipt)
		return receipt
	}
	if attempt.Channel == envelope.Channel && next > 1 && attempt.ElapsedSeconds < minimumRetryElapsed(envelope.Delivery.BackoffSeconds, int(next)-1) {
		receipt := suppressedReceipt(envelope, attempt, "backoff")
		receipt.Attempt = next
		l.receipts = append(l.receipts, receipt)
		return receipt
	}
	l.events[eventKey(key, attempt.EventID)] = true
	l.attempts[key] = next
	receipt := Receipt{
		DedupKey: envelope.DedupKey, HoldEpoch: envelope.HoldEpoch,
		EventID: attempt.EventID, Channel: attempt.Channel, Attempt: next,
		ElapsedSeconds: attempt.ElapsedSeconds,
	}
	l.receipts = append(l.receipts, receipt)
	return receipt
}

// Snapshot returns receipts in stable key/epoch/channel/attempt order.
func (l *Ledger) Snapshot() LedgerSnapshot {
	if l == nil {
		return LedgerSnapshot{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	rows := append([]Receipt(nil), l.receipts...)
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.DedupKey != b.DedupKey {
			return a.DedupKey < b.DedupKey
		}
		if a.HoldEpoch != b.HoldEpoch {
			return a.HoldEpoch < b.HoldEpoch
		}
		if a.Channel != b.Channel {
			return a.Channel < b.Channel
		}
		if a.Attempt != b.Attempt {
			return a.Attempt < b.Attempt
		}
		return a.EventID < b.EventID
	})
	return LedgerSnapshot{Receipts: rows}
}

func ledgerKey(key string, epoch uint64, channel string) string {
	return key + "\x00" + uintString(epoch) + "\x00" + channel
}

var (
	eventIDPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]{0,127}$`)
	dedupKeyPattern = regexp.MustCompile(`^coord-(?:hold|abstain)-[a-z][a-z0-9]*(?:-[a-z0-9]+)*(?:\.[a-z0-9]+)*$`)
)

func eventKey(key, eventID string) string {
	return key + "\x00" + eventID
}

func suppressedReceipt(envelope *Envelope, attempt Attempt, cause string) Receipt {
	receipt := Receipt{
		EventID: attempt.EventID, Channel: attempt.Channel,
		ElapsedSeconds: attempt.ElapsedSeconds, Suppressed: true, SuppressionCause: cause,
	}
	if envelope != nil {
		receipt.DedupKey = envelope.DedupKey
		receipt.HoldEpoch = envelope.HoldEpoch
	}
	return safeSuppressedReceipt(receipt)
}

func safeSuppressedReceipt(receipt Receipt) Receipt {
	if receipt.SuppressionCause != "duplicate_event" && receipt.SuppressionCause != "max_attempts" &&
		receipt.SuppressionCause != "deadline" && receipt.SuppressionCause != "backoff" {
		receipt.SuppressionCause = "invalid_attempt"
	}
	if !dedupKeyPattern.MatchString(receipt.DedupKey) || receipt.HoldEpoch == 0 {
		receipt.DedupKey = ""
		receipt.HoldEpoch = 0
	}
	if receipt.SuppressionCause == "invalid_attempt" || !eventIDPattern.MatchString(receipt.EventID) ||
		!validChannel(receipt.Channel) || receipt.ElapsedSeconds < 0 {
		receipt.EventID = "invalid-event"
		receipt.Channel = "invalid-channel"
		receipt.Attempt = 0
		receipt.ElapsedSeconds = 0
		receipt.SuppressionCause = "invalid_attempt"
	}
	return receipt
}

func validChannel(channel string) bool {
	return channel == "discord" || channel == "pr_update"
}

func validateDeliveryPolicy(delivery Delivery) error {
	return validateDelivery(DeliveryPolicy(delivery))
}

func minimumRetryElapsed(schedule []int, delays int) int {
	if delays > len(schedule) {
		delays = len(schedule)
	}
	total := 0
	for _, delay := range schedule[:delays] {
		total += delay
	}
	return total
}

func uintString(value uint64) string {
	if value == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for value > 0 {
		i--
		buf[i] = byte('0' + value%10)
		value /= 10
	}
	return string(buf[i:])
}
