package beads

import "testing"

type decisionFrontierRecordWriterTestStub struct {
	created Bead
	casWon  bool
	linked  [3]string
	calls   []string
	onWrite func()
	err     error
}

func (w *decisionFrontierRecordWriterTestStub) CreateDecisionFrontierRecord(record Bead) (Bead, error) {
	w.calls = append(w.calls, "create")
	w.created = cloneBead(record)
	if w.onWrite != nil {
		w.onWrite()
	}
	if w.err != nil {
		return Bead{}, w.err
	}
	return cloneBead(record), nil
}

func (w *decisionFrontierRecordWriterTestStub) CompareAndSetDecisionFrontierRecordMetadataKey(string, string, string, string) (bool, error) {
	w.calls = append(w.calls, "cas")
	if w.onWrite != nil {
		w.onWrite()
	}
	return w.casWon, w.err
}

func (w *decisionFrontierRecordWriterTestStub) EnsureDecisionFrontierLink(sourceID, targetID, depType string) error {
	w.calls = append(w.calls, "link")
	w.linked = [3]string{sourceID, targetID, depType}
	if w.onWrite != nil {
		w.onWrite()
	}
	return w.err
}

type decisionFrontierRecordWriterTestStore struct {
	Store
	writer    DecisionFrontierRecordWriter
	available bool
}

func (s *decisionFrontierRecordWriterTestStore) DecisionFrontierRecordWriterHandle() (DecisionFrontierRecordWriter, bool) {
	if s == nil || !s.available || s.writer == nil {
		return nil, false
	}
	return s.writer, true
}

func TestDecisionFrontierRecordWriterForForwardsThroughTypedStores(t *testing.T) {
	writer := &decisionFrontierRecordWriterTestStub{}
	backing := &decisionFrontierRecordWriterTestStore{Store: NewMemStore(), writer: writer, available: true}
	stores := []struct {
		name  string
		store Store
	}{
		{name: "work", store: WorkStore{Store: backing}},
		{name: "graph", store: GraphStore{Store: backing}},
		{name: "session", store: SessionStore{Store: backing}},
		{name: "mail", store: MailStore{Store: backing}},
		{name: "orders", store: OrdersStore{Store: backing}},
		{name: "nudges", store: NudgesStore{Store: backing}},
	}
	for _, tc := range stores {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := DecisionFrontierRecordWriterFor(tc.store)
			if !ok || got != writer {
				t.Fatalf("DecisionFrontierRecordWriterFor(%T) = (%T, %t), want the backing writer", tc.store, got, ok)
			}
		})
	}

	backing.available = false
	for _, tc := range stores {
		t.Run(tc.name+" unavailable", func(t *testing.T) {
			if got, ok := DecisionFrontierRecordWriterFor(tc.store); ok || got != nil {
				t.Fatalf("unavailable backing advertised a writer: (%T, %t)", got, ok)
			}
		})
	}
}

func TestCachingStoreDecisionFrontierWriterEvictsAffectedRecords(t *testing.T) {
	writer := &decisionFrontierRecordWriterTestStub{casWon: true}
	backing := &decisionFrontierRecordWriterTestStore{Store: NewMemStore(), writer: writer, available: true}
	cache := NewCachingStoreForTest(backing, nil)
	cache.beads["created"] = Bead{ID: "created", Title: "stale"}
	cache.deps["created"] = []Dep{{IssueID: "created", DependsOnID: "old-target"}}
	cache.beads["cas-record"] = Bead{ID: "cas-record", Title: "stale"}
	cache.beads["link-source"] = Bead{ID: "link-source", Title: "stale"}
	cache.beads["link-target"] = Bead{ID: "link-target", Title: "stale"}

	capability, ok := DecisionFrontierRecordWriterFor(cache)
	if !ok || capability == nil {
		t.Fatal("cache hid the backing's complete record-writer capability")
	}
	if _, err := capability.CreateDecisionFrontierRecord(Bead{ID: "created", Title: "private record"}); err != nil {
		t.Fatalf("CreateDecisionFrontierRecord: %v", err)
	}
	assertCacheEntryEvicted(t, cache, "created")
	if _, err := capability.CompareAndSetDecisionFrontierRecordMetadataKey("cas-record", "state", "pending", "resolved"); err != nil {
		t.Fatalf("CompareAndSetDecisionFrontierRecordMetadataKey: %v", err)
	}
	assertCacheEntryEvicted(t, cache, "cas-record")
	if err := capability.EnsureDecisionFrontierLink("link-source", "link-target", "blocks"); err != nil {
		t.Fatalf("EnsureDecisionFrontierLink: %v", err)
	}
	assertCacheEntryEvicted(t, cache, "link-source")
	assertCacheEntryEvicted(t, cache, "link-target")
}

func assertCacheEntryEvicted(t *testing.T, cache *CachingStore, id string) {
	t.Helper()
	cache.mu.RLock()
	_, cached := cache.beads[id]
	_, dirty := cache.dirty[id]
	cache.mu.RUnlock()
	if cached || !dirty {
		t.Fatalf("cache state for %q = cached:%t dirty:%t, want evicted and dirty", id, cached, dirty)
	}
}

func TestDecisionFrontierWriterHandleDoesNotOverclaimIncompleteProviders(t *testing.T) {
	incomplete := &decisionFrontierRecordWriterTestStore{Store: NewMemStore()}
	if got, ok := DecisionFrontierRecordWriterFor(incomplete); ok || got != nil {
		t.Fatalf("incomplete provider advertised writer: (%T, %t)", got, ok)
	}
	cache := NewCachingStoreForTest(incomplete, nil)
	if got, ok := DecisionFrontierRecordWriterFor(cache); ok || got != nil {
		t.Fatalf("cache overclaimed an incomplete writer: (%T, %t)", got, ok)
	}
}

func TestDecisionFrontierRecordWriterForRejectsTypedNilProviderResult(t *testing.T) {
	var typedNil *decisionFrontierRecordWriterTestStub
	provider := &decisionFrontierRecordWriterTestStore{
		Store: NewMemStore(), writer: typedNil, available: true,
	}
	if writer, ok := DecisionFrontierRecordWriterFor(provider); ok || writer != nil {
		t.Fatalf("typed-nil provider result was accepted: (%T, %t)", writer, ok)
	}
}
