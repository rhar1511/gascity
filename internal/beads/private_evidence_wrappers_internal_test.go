package beads

import (
	"encoding/json"
	"os"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
)

type privateEvidenceWrapperTestStore struct {
	Store
	mu       sync.Mutex
	values   map[string]string
	archives []Bead
	getCalls int
	onCAS    func()
}

func newPrivateEvidenceWrapperTestStore(store Store) *privateEvidenceWrapperTestStore {
	return &privateEvidenceWrapperTestStore{Store: store, values: make(map[string]string)}
}

func (s *privateEvidenceWrapperTestStore) Get(id string) (Bead, error) {
	s.mu.Lock()
	s.getCalls++
	s.mu.Unlock()
	return s.Store.Get(id)
}

func (s *privateEvidenceWrapperTestStore) CompareAndSetPrivateEvidenceMetadataKey(id, key, expected, next string) (bool, error) {
	lookup := id + "\x00" + key
	s.mu.Lock()
	current, present := s.values[lookup]
	if (expected == "" && present) || (expected != "" && (!present || current != expected)) {
		s.mu.Unlock()
		return false, nil
	}
	if err := s.SetMetadata(id, key, next); err != nil {
		s.mu.Unlock()
		return false, err
	}
	s.values[lookup] = next
	onCAS := s.onCAS
	s.mu.Unlock()
	if onCAS != nil {
		onCAS()
	}
	return true, nil
}

func (s *privateEvidenceWrapperTestStore) ReadPrivateEvidenceMetadataKey(id, key string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, present := s.values[id+"\x00"+key]
	return value, present, nil
}

func (s *privateEvidenceWrapperTestStore) ListPrivateEvidenceOwnerIndexes(ownerID string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string)
	for composite, value := range s.values {
		prefix := ownerID + "\x00"
		if len(composite) >= len(prefix) && composite[:len(prefix)] == prefix {
			out[composite[len(prefix):]] = value
		}
	}
	return out, nil
}

func (s *privateEvidenceWrapperTestStore) ListPrivateEvidenceArchives(string, string) ([]Bead, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Bead(nil), s.archives...), nil
}

func TestPrivateEvidenceCapabilitiesForwardThroughTypedStores(t *testing.T) {
	backing := newPrivateEvidenceWrapperTestStore(NewMemStore())
	typed := []struct {
		name string
		wrap func(Store) Store
	}{
		{"work", func(store Store) Store { return WorkStore{Store: store} }},
		{"graph", func(store Store) Store { return GraphStore{Store: store} }},
		{"session", func(store Store) Store { return SessionStore{Store: store} }},
		{"mail", func(store Store) Store { return MailStore{Store: store} }},
		{"orders", func(store Store) Store { return OrdersStore{Store: store} }},
		{"nudges", func(store Store) Store { return NudgesStore{Store: store} }},
	}
	for _, tc := range typed {
		t.Run(tc.name, func(t *testing.T) {
			store := tc.wrap(backing)
			owner, err := backing.Create(Bead{Title: "typed private evidence owner", Type: "task"})
			if err != nil {
				t.Fatal(err)
			}
			writer, ok := PrivateEvidenceMetadataCASWriterFor(store)
			if !ok {
				t.Fatal("typed store hid the private evidence writer")
			}
			reader, ok := PrivateEvidenceArchiveReaderFor(store)
			if !ok {
				t.Fatal("typed store hid the private evidence archive reader")
			}
			indexes, err := reader.ListPrivateEvidenceOwnerIndexes(owner.ID)
			if err != nil || len(indexes) != 0 {
				t.Fatalf("archive reader forwarding returned %v, %v", indexes, err)
			}
			const key = "gc.attempt_evidence.index.test"
			swapped, err := writer.CompareAndSetPrivateEvidenceMetadataKey(owner.ID, key, "", tc.name)
			if err != nil || !swapped {
				t.Fatalf("typed store CAS = (%t, %v), want (true, nil)", swapped, err)
			}
			value, present, err := writer.ReadPrivateEvidenceMetadataKey(owner.ID, key)
			if err != nil || !present || value != tc.name {
				t.Fatalf("typed store readback = (%q, %t, %v)", value, present, err)
			}
		})
	}
}

func TestCachingStorePrivateEvidenceCASInvalidatesCachedOwner(t *testing.T) {
	backing := newPrivateEvidenceWrapperTestStore(NewMemStore())
	owner, err := backing.Create(Bead{Title: "private evidence owner", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}
	if _, err := cache.Get(owner.ID); err != nil {
		t.Fatalf("initial cached Get: %v", err)
	}
	backing.mu.Lock()
	getsBefore := backing.getCalls
	backing.mu.Unlock()

	writer, ok := PrivateEvidenceMetadataCASWriterFor(cache)
	if !ok {
		t.Fatal("CachingStore hid the private evidence writer")
	}
	const key, next = "gc.attempt_evidence.index.test", "sealed-private-value"
	swapped, err := writer.CompareAndSetPrivateEvidenceMetadataKey(owner.ID, key, "", next)
	if err != nil || !swapped {
		t.Fatalf("cached private evidence CAS = (%t, %v), want (true, nil)", swapped, err)
	}
	got, err := cache.Get(owner.ID)
	if err != nil {
		t.Fatalf("Get after private evidence CAS: %v", err)
	}
	if got.Metadata[key] != next {
		t.Fatal("cache returned the owner row from before its private evidence CAS")
	}
	backing.mu.Lock()
	getsAfter := backing.getCalls
	backing.mu.Unlock()
	if getsAfter <= getsBefore {
		t.Fatalf("backing Get calls = %d before and %d after CAS; cache did not evict the owner", getsBefore, getsAfter)
	}
	if _, ok := PrivateEvidenceArchiveReaderFor(cache); !ok {
		t.Fatal("CachingStore hid the private evidence archive reader")
	}
}

func TestProxiedPrivateEvidenceCASUsesGenerationBracket(t *testing.T) {
	root := t.TempDir()
	writeProxyRecordForPrivateEvidenceTest(t, root, 4001, 45123, "generation-one")
	writeStore := newPrivateEvidenceWrapperTestStore(NewMemStore())
	owner, err := writeStore.Create(Bead{Title: "private evidence owner", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	writeStore.onCAS = func() {
		writeProxyRecordForPrivateEvidenceTest(t, root, 4002, 45987, "generation-two")
	}
	native := newNativeDoltStoreForTest(newNativeDoltMemStorage(), WithProxiedReadOnly())
	store, err := NewProxiedStore(native, writeStore, PinForTest("/scope", root, "beads"))
	if err != nil {
		t.Fatalf("NewProxiedStore: %v", err)
	}
	writer, ok := PrivateEvidenceMetadataCASWriterFor(store)
	if !ok {
		t.Fatal("ProxiedStore hid the private evidence writer")
	}
	swapped, err := writer.CompareAndSetPrivateEvidenceMetadataKey(owner.ID, "gc.attempt_evidence.index.test", "", "sealed")
	if err != nil || !swapped {
		t.Fatalf("proxied private evidence CAS = (%t, %v), want (true, nil)", swapped, err)
	}
	if !store.Demoted() {
		t.Fatal("proxy generation change during private CAS did not stand down the native read leaf")
	}
	if verdict := store.Verdict(); verdict == nil || verdict.Verdict != ProxiedVerdictProxyGone {
		t.Fatalf("proxied private CAS verdict = %v, want proxy_gone", verdict)
	}
	value, present, err := writeStore.ReadPrivateEvidenceMetadataKey(owner.ID, "gc.attempt_evidence.index.test")
	if err != nil || !present || value != "sealed" {
		t.Fatalf("bd write leaf private CAS value = (%q, %t, %v)", value, present, err)
	}
}

func writeProxyRecordForPrivateEvidenceTest(t *testing.T, root string, pid, port int, generation string) {
	t.Helper()
	rootID, err := proxyendpoint.RootID(root)
	if err != nil {
		t.Fatalf("proxy root id: %v", err)
	}
	record, err := json.Marshal(proxyendpoint.Record{
		PID: pid, Port: port, UpstreamID: "upstream", Schema: proxyendpoint.SchemaV2,
		Kind: proxyendpoint.RecordKind, Birth: proxyendpoint.BirthToken("boot", generation),
		RootID: rootID, ControlPort: port + 1,
	})
	if err != nil {
		t.Fatalf("marshal proxy record: %v", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create proxy root: %v", err)
	}
	if err := os.WriteFile(proxyendpoint.PIDPath(root), record, 0o600); err != nil {
		t.Fatalf("write proxy record: %v", err)
	}
}

var (
	_ PrivateEvidenceMetadataCASWriter = (*privateEvidenceWrapperTestStore)(nil)
	_ PrivateEvidenceArchiveReader     = (*privateEvidenceWrapperTestStore)(nil)
)
