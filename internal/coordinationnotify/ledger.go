package coordinationnotify

import (
	"sort"
	"sync"
)

// Receipt records a channel attempt against one decision epoch.
type Receipt struct {
	DedupKey   string `json:"dedup_key"`
	HoldEpoch  uint64 `json:"hold_epoch"`
	Channel    string `json:"channel"`
	Attempt    uint64 `json:"attempt"`
	Suppressed bool   `json:"suppressed"`
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
	seen     map[string]bool
	attempts map[string]uint64
	receipts []Receipt
}

// NewLedger returns an empty dedup ledger.
func NewLedger() *Ledger {
	return &Ledger{seen: make(map[string]bool), attempts: make(map[string]uint64)}
}

// NewLedgerFromSnapshot restores dedup state from a persisted receipt snapshot.
func NewLedgerFromSnapshot(snapshot LedgerSnapshot) *Ledger {
	l := NewLedger()
	for _, receipt := range snapshot.Receipts {
		if receipt.DedupKey == "" || receipt.HoldEpoch == 0 || receipt.Channel == "" {
			continue
		}
		key := ledgerKey(receipt.DedupKey, receipt.HoldEpoch, receipt.Channel)
		l.seen[key] = true
		if receipt.Attempt > l.attempts[key] {
			l.attempts[key] = receipt.Attempt
		}
		l.receipts = append(l.receipts, receipt)
	}
	return l
}

// Record appends one attempt receipt and suppresses repeated attempts for the
// same dedup key, hold epoch, and channel.
func (l *Ledger) Record(envelope *Envelope, channel string) Receipt {
	if l == nil || envelope == nil || envelope.DedupKey == "" || envelope.HoldEpoch == 0 ||
		(channel != envelope.Channel && channel != envelope.Delivery.FailoverChannel) {
		return Receipt{Suppressed: true}
	}
	key := ledgerKey(envelope.DedupKey, envelope.HoldEpoch, channel)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen == nil {
		l.seen = make(map[string]bool)
	}
	if l.attempts == nil {
		l.attempts = make(map[string]uint64)
	}
	suppressed := l.seen[key]
	l.seen[key] = true
	l.attempts[key]++
	receipt := Receipt{
		DedupKey: envelope.DedupKey, HoldEpoch: envelope.HoldEpoch,
		Channel: channel, Attempt: l.attempts[key], Suppressed: suppressed,
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
		return a.Attempt < b.Attempt
	})
	return LedgerSnapshot{Receipts: rows}
}

func ledgerKey(key string, epoch uint64, channel string) string {
	return key + "\x00" + uintString(epoch) + "\x00" + channel
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
