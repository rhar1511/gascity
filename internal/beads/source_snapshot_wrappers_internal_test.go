package beads

import (
	"errors"
	"sync"
	"testing"
)

type sourceSnapshotPlainStore struct{ Store }

type sourceSnapshotWrapperTestStore struct {
	Store
	mu           sync.Mutex
	getCalls     int
	snapshotCall func(string) (Bead, error)
}

func (s *sourceSnapshotWrapperTestStore) Get(id string) (Bead, error) {
	s.mu.Lock()
	s.getCalls++
	s.mu.Unlock()
	return s.Store.Get(id)
}

func (s *sourceSnapshotWrapperTestStore) DecisionFrontierSourceSnapshot(id string) (Bead, error) {
	if s.snapshotCall == nil {
		return Bead{ID: id, Title: "authoritative snapshot"}, nil
	}
	return s.snapshotCall(id)
}

func (s *sourceSnapshotWrapperTestStore) DecisionFrontierSourceReaderHandle() (DecisionFrontierSourceReader, bool) {
	return s, s != nil
}

func TestDecisionFrontierSourceReaderForwardsThroughTypedStores(t *testing.T) {
	backing := &sourceSnapshotWrapperTestStore{Store: NewMemStore()}
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
			reader, ok := DecisionFrontierSourceReaderFor(tc.wrap(backing))
			if !ok || reader == nil {
				t.Fatal("typed store hid the source-snapshot reader")
			}
			bead, err := reader.DecisionFrontierSourceSnapshot("source-1")
			if err != nil || bead.ID != "source-1" || bead.Title != "authoritative snapshot" {
				t.Fatalf("source snapshot = %+v, %v", bead, err)
			}
		})
	}
}

func TestCachingStoreDecisionFrontierSourceSnapshotDoesNotPopulateCache(t *testing.T) {
	backing := &sourceSnapshotWrapperTestStore{Store: NewMemStore()}
	cache := NewCachingStoreForTest(backing, nil)
	reader, ok := DecisionFrontierSourceReaderFor(cache)
	if !ok || reader == nil {
		t.Fatal("CachingStore hid the source-snapshot reader")
	}
	if bead, err := reader.DecisionFrontierSourceSnapshot("source-1"); err != nil || bead.ID != "source-1" {
		t.Fatalf("source snapshot = %+v, %v", bead, err)
	}
	backing.mu.Lock()
	getsBefore := backing.getCalls
	backing.mu.Unlock()
	if getsBefore != 0 {
		t.Fatalf("source snapshot called ordinary Get %d times, want zero", getsBefore)
	}
	if _, err := cache.Get("source-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after source snapshot error = %v, want not found from backing", err)
	}
	backing.mu.Lock()
	getsAfter := backing.getCalls
	backing.mu.Unlock()
	if getsAfter != 1 {
		t.Fatalf("ordinary Get calls after source snapshot = %d, want a cache miss to reach backing once", getsAfter)
	}
}

func TestProxiedDecisionFrontierSourceSnapshotUsesWriteLeafAndGenerationBracket(t *testing.T) {
	root := t.TempDir()
	writeProxyRecordForControllerTransitionTest(t, root, 4001, 45123, "generation-one")
	writeStore := &sourceSnapshotWrapperTestStore{Store: sourceSnapshotPlainStore{Store: NewMemStore()}}
	writeStore.snapshotCall = func(id string) (Bead, error) {
		writeProxyRecordForControllerTransitionTest(t, root, 4002, 45987, "generation-two")
		return Bead{ID: id, Title: "bd write-leaf snapshot"}, nil
	}
	native := newNativeDoltStoreForTest(newNativeDoltMemStorage(), WithProxiedReadOnly())
	store, err := NewProxiedStore(native, writeStore, PinForTest("/scope", root, "beads"))
	if err != nil {
		t.Fatalf("NewProxiedStore: %v", err)
	}
	reader, ok := DecisionFrontierSourceReaderFor(store)
	if !ok || reader == nil {
		t.Fatal("ProxiedStore hid the source-snapshot reader on its write leaf")
	}
	bead, err := reader.DecisionFrontierSourceSnapshot("source-1")
	if err != nil {
		t.Fatalf("proxied source snapshot: %v", err)
	}
	if bead.Title != "bd write-leaf snapshot" {
		t.Fatalf("proxied snapshot came from %q, want authoritative bd write leaf", bead.Title)
	}
	if !store.Demoted() {
		t.Fatal("proxy generation change during source snapshot did not stand down the native read leaf")
	}
	if verdict := store.Verdict(); verdict == nil || verdict.Verdict != ProxiedVerdictProxyGone {
		t.Fatalf("proxy source snapshot verdict = %v, want proxy_gone", verdict)
	}
}

func TestProxiedDecisionFrontierSourceReaderIsUnavailableWithoutWriteLeafCapability(t *testing.T) {
	root := t.TempDir()
	writeProxyRecordForControllerTransitionTest(t, root, 4001, 45123, "generation-one")
	native := newNativeDoltStoreForTest(newNativeDoltMemStorage(), WithProxiedReadOnly())
	store, err := NewProxiedStore(native, sourceSnapshotPlainStore{Store: NewMemStore()}, PinForTest("/scope", root, "beads"))
	if err != nil {
		t.Fatalf("NewProxiedStore: %v", err)
	}
	if reader, ok := DecisionFrontierSourceReaderFor(store); ok || reader != nil {
		t.Fatal("ProxiedStore exposed source snapshots without write-leaf support")
	}
}

var (
	_ DecisionFrontierSourceReader               = (*sourceSnapshotWrapperTestStore)(nil)
	_ DecisionFrontierSourceReaderHandleProvider = (*sourceSnapshotWrapperTestStore)(nil)
)
