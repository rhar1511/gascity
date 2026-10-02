package beads

import (
	"encoding/json"
	"os"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
)

type controllerTransitionWrapperTestStore struct {
	Store
	mu           sync.Mutex
	getCalls     int
	transitionFn func(string, ControllerMetadataTransitionRequest)
}

type revisionTransitionWrapperTestStore struct {
	Store
	transitionFn func(string)
}

type revisionTransitionPatchWrapperTestStore struct {
	Store
	getCalls     int
	patchCalls   int
	receiptCalls int
	patchFn      func(string, RevisionTransitionPatchRequest) (RevisionTransitionPatchResult, error)
	receiptFn    func(string) (RevisionTransitionPatchReceipt, bool, error)
}

func (s *revisionTransitionPatchWrapperTestStore) Get(id string) (Bead, error) {
	s.getCalls++
	return s.Store.Get(id)
}

func (s *revisionTransitionPatchWrapperTestStore) TransitionPatch(issueID string, request RevisionTransitionPatchRequest) (RevisionTransitionPatchResult, error) {
	s.patchCalls++
	if s.patchFn != nil {
		return s.patchFn(issueID, request)
	}
	return RevisionTransitionPatchResult{Applied: true}, nil
}

func (s *revisionTransitionPatchWrapperTestStore) ReadRevisionTransitionPatchReceipt(receiptID string) (RevisionTransitionPatchReceipt, bool, error) {
	s.receiptCalls++
	if s.receiptFn != nil {
		return s.receiptFn(receiptID)
	}
	return RevisionTransitionPatchReceipt{}, false, nil
}

func (s *revisionTransitionWrapperTestStore) RevisionTransitionWriterHandle() (RevisionTransitionWriter, bool) {
	return s, s != nil
}

func (s *revisionTransitionWrapperTestStore) CompareAndSetMetadataKeyWithReceipt(id, _, _, _ string, _ int64, _ RevisionTransitionReceipt) (Bead, bool, error) {
	if s.transitionFn != nil {
		s.transitionFn(id)
	}
	bead, err := s.Get(id)
	return bead, err == nil, err
}

func (s *controllerTransitionWrapperTestStore) Get(id string) (Bead, error) {
	s.mu.Lock()
	s.getCalls++
	s.mu.Unlock()
	return s.Store.Get(id)
}

func (s *controllerTransitionWrapperTestStore) TransitionMetadata(id string, request ControllerMetadataTransitionRequest) (ControllerMetadataTransitionResult, error) {
	if s.transitionFn != nil {
		s.transitionFn(id, request)
	}
	return ControllerMetadataTransitionResult{Applied: true}, nil
}

func TestControllerMetadataTransitionCapabilityForwardsThroughTypedStores(t *testing.T) {
	backing := &controllerTransitionWrapperTestStore{Store: NewMemStore()}
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
			writer, ok := ControllerMetadataTransitionWriterFor(tc.wrap(backing))
			if !ok {
				t.Fatal("typed store hid the controller transition writer")
			}
			if _, err := writer.TransitionMetadata("issue-1", ControllerMetadataTransitionRequest{}); err != nil {
				t.Fatalf("forward transition writer: %v", err)
			}
		})
	}
}

func TestCachingStoreControllerMetadataTransitionEvictsOwner(t *testing.T) {
	mem := NewMemStore()
	owner, err := mem.Create(Bead{Title: "transition owner", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	backing := &controllerTransitionWrapperTestStore{Store: mem}
	backing.transitionFn = func(id string, _ ControllerMetadataTransitionRequest) {
		if err := backing.SetMetadata(id, "gc.transition_test", "updated"); err != nil {
			t.Errorf("update transition owner: %v", err)
		}
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

	writer, ok := ControllerMetadataTransitionWriterFor(cache)
	if !ok {
		t.Fatal("CachingStore hid the controller transition writer")
	}
	if _, err := writer.TransitionMetadata(owner.ID, ControllerMetadataTransitionRequest{}); err != nil {
		t.Fatalf("cached transition: %v", err)
	}
	got, err := cache.Get(owner.ID)
	if err != nil {
		t.Fatalf("Get after transition: %v", err)
	}
	if got.Metadata["gc.transition_test"] != "updated" {
		t.Fatalf("cached owner metadata = %v, want transition result", got.Metadata)
	}
	backing.mu.Lock()
	getsAfter := backing.getCalls
	backing.mu.Unlock()
	if getsAfter <= getsBefore {
		t.Fatalf("backing Get calls = %d before and %d after; transition did not evict cached owner", getsBefore, getsAfter)
	}
}

func TestProxiedControllerMetadataTransitionUsesGenerationBracket(t *testing.T) {
	root := t.TempDir()
	writeProxyRecordForControllerTransitionTest(t, root, 4001, 45123, "generation-one")
	writeStore := &controllerTransitionWrapperTestStore{Store: NewMemStore()}
	writeStore.transitionFn = func(string, ControllerMetadataTransitionRequest) {
		writeProxyRecordForControllerTransitionTest(t, root, 4002, 45987, "generation-two")
	}
	native := newNativeDoltStoreForTest(newNativeDoltMemStorage(), WithProxiedReadOnly())
	store, err := NewProxiedStore(native, writeStore, PinForTest("/scope", root, "beads"))
	if err != nil {
		t.Fatalf("NewProxiedStore: %v", err)
	}
	writer, ok := ControllerMetadataTransitionWriterFor(store)
	if !ok {
		t.Fatal("ProxiedStore hid the controller transition writer")
	}
	if _, err := writer.TransitionMetadata("issue-1", ControllerMetadataTransitionRequest{}); err != nil {
		t.Fatalf("proxied transition: %v", err)
	}
	if !store.Demoted() {
		t.Fatal("proxy generation change during controller transition did not stand down the native read leaf")
	}
	if verdict := store.Verdict(); verdict == nil || verdict.Verdict != ProxiedVerdictProxyGone {
		t.Fatalf("proxied transition verdict = %v, want proxy_gone", verdict)
	}
}

func TestCachingRevisionTransitionEvictsOwner(t *testing.T) {
	mem := NewMemStore()
	owner, err := mem.Create(Bead{Title: "revision transition owner", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	backing := &revisionTransitionWrapperTestStore{Store: mem}
	backing.transitionFn = func(id string) {
		if err := backing.SetMetadata(id, "gc.transition_test", "updated"); err != nil {
			t.Errorf("update transition owner: %v", err)
		}
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}
	if _, err := cache.Get(owner.ID); err != nil {
		t.Fatalf("initial cached Get: %v", err)
	}
	writer, ok := RevisionTransitionWriterFor(cache)
	if !ok || writer == nil {
		t.Fatal("CachingStore hid the complete revision transition writer")
	}
	if _, won, err := writer.CompareAndSetMetadataKeyWithReceipt(owner.ID, "gc.test", "", "next", owner.Revision, RevisionTransitionReceipt{}); err != nil || !won {
		t.Fatalf("cached revision transition: won=%v err=%v", won, err)
	}
	got, err := cache.Get(owner.ID)
	if err != nil || got.Metadata["gc.transition_test"] != "updated" {
		t.Fatalf("cached owner after transition = %+v, %v", got, err)
	}
}

func TestProxiedRevisionTransitionUsesOneGenerationBracket(t *testing.T) {
	root := t.TempDir()
	writeProxyRecordForControllerTransitionTest(t, root, 4001, 45123, "generation-one")
	mem := NewMemStore()
	owner, err := mem.Create(Bead{Title: "revision transition owner", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	writeStore := &revisionTransitionWrapperTestStore{Store: mem}
	writeStore.transitionFn = func(string) {
		writeProxyRecordForControllerTransitionTest(t, root, 4002, 45987, "generation-two")
	}
	native := newNativeDoltStoreForTest(newNativeDoltMemStorage(), WithProxiedReadOnly())
	store, err := NewProxiedStore(native, writeStore, PinForTest("/scope", root, "beads"))
	if err != nil {
		t.Fatalf("NewProxiedStore: %v", err)
	}
	writer, ok := RevisionTransitionWriterFor(store)
	if !ok || writer == nil {
		t.Fatal("ProxiedStore hid the complete revision transition writer")
	}
	if _, won, err := writer.CompareAndSetMetadataKeyWithReceipt(owner.ID, "gc.test", "", "next", owner.Revision, RevisionTransitionReceipt{}); err != nil || !won {
		t.Fatalf("proxied revision transition: won=%v err=%v", won, err)
	}
	if !store.Demoted() {
		t.Fatal("proxy generation change during revision transition did not stand down the native read leaf")
	}
}

func TestCachingRevisionTransitionPatchEvictsAndForwardsReceiptRead(t *testing.T) {
	mem := NewMemStore()
	owner, err := mem.Create(Bead{Title: "patch transition owner", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	backing := &revisionTransitionPatchWrapperTestStore{Store: mem}
	backing.patchFn = func(id string, request RevisionTransitionPatchRequest) (RevisionTransitionPatchResult, error) {
		if id != owner.ID || request.ReceiptID != "patch/receipt-1" {
			t.Errorf("patch request forwarding = %q / %q", id, request.ReceiptID)
		}
		if err := backing.SetMetadata(id, "gc.patch_test", "updated"); err != nil {
			return RevisionTransitionPatchResult{}, err
		}
		return RevisionTransitionPatchResult{Applied: true}, nil
	}
	backing.receiptFn = func(id string) (RevisionTransitionPatchReceipt, bool, error) {
		if id != "patch/receipt-1" {
			t.Errorf("receipt ID forwarded = %q", id)
		}
		return RevisionTransitionPatchReceipt{ReceiptID: id, IssueID: owner.ID}, true, nil
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}
	if _, err := cache.Get(owner.ID); err != nil {
		t.Fatalf("initial cached Get: %v", err)
	}
	getsBefore := backing.getCalls
	writer, ok := RevisionTransitionPatchWriterFor(cache)
	if !ok {
		t.Fatal("CachingStore hid the patch writer")
	}
	if _, err := writer.TransitionPatch(owner.ID, RevisionTransitionPatchRequest{ReceiptID: "patch/receipt-1"}); err != nil {
		t.Fatalf("cached patch: %v", err)
	}
	got, err := cache.Get(owner.ID)
	if err != nil || got.Metadata["gc.patch_test"] != "updated" {
		t.Fatalf("owner after patch = %+v, %v", got, err)
	}
	if backing.getCalls <= getsBefore {
		t.Fatalf("backing Get calls = %d before and %d after; patch did not evict the issue cache", getsBefore, backing.getCalls)
	}
	reader, ok := RevisionTransitionPatchReceiptReaderFor(cache)
	if !ok {
		t.Fatal("CachingStore hid the exact receipt reader")
	}
	if receipt, found, err := reader.ReadRevisionTransitionPatchReceipt("patch/receipt-1"); err != nil || !found || receipt.IssueID != owner.ID {
		t.Fatalf("receipt read = (%+v, %t, %v)", receipt, found, err)
	}
	if backing.receiptCalls != 1 {
		t.Fatalf("receipt reads = %d, want a direct backing read", backing.receiptCalls)
	}
}

func TestProxiedRevisionTransitionPatchUsesGenerationBracket(t *testing.T) {
	root := t.TempDir()
	writeProxyRecordForControllerTransitionTest(t, root, 4101, 46123, "generation-one")
	writeStore := &revisionTransitionPatchWrapperTestStore{Store: NewMemStore()}
	writeStore.patchFn = func(string, RevisionTransitionPatchRequest) (RevisionTransitionPatchResult, error) {
		writeProxyRecordForControllerTransitionTest(t, root, 4102, 46987, "generation-two")
		return RevisionTransitionPatchResult{Applied: true}, nil
	}
	native := newNativeDoltStoreForTest(newNativeDoltMemStorage(), WithProxiedReadOnly())
	store, err := NewProxiedStore(native, writeStore, PinForTest("/scope", root, "beads"))
	if err != nil {
		t.Fatalf("NewProxiedStore: %v", err)
	}
	writer, ok := RevisionTransitionPatchWriterFor(store)
	if !ok {
		t.Fatal("ProxiedStore hid the patch writer")
	}
	if _, err := writer.TransitionPatch("issue-1", RevisionTransitionPatchRequest{}); err != nil {
		t.Fatalf("proxied patch: %v", err)
	}
	if !store.Demoted() {
		t.Fatal("proxy generation change during patch did not stand down the native read leaf")
	}
	if verdict := store.Verdict(); verdict == nil || verdict.Verdict != ProxiedVerdictProxyGone {
		t.Fatalf("proxied patch verdict = %v, want proxy_gone", verdict)
	}
}

func TestProxiedRevisionTransitionPatchReceiptReadUsesGenerationBracket(t *testing.T) {
	root := t.TempDir()
	writeProxyRecordForControllerTransitionTest(t, root, 4201, 47123, "generation-one")
	writeStore := &revisionTransitionPatchWrapperTestStore{Store: NewMemStore()}
	writeStore.receiptFn = func(receiptID string) (RevisionTransitionPatchReceipt, bool, error) {
		writeProxyRecordForControllerTransitionTest(t, root, 4202, 47987, "generation-two")
		return RevisionTransitionPatchReceipt{ReceiptID: receiptID}, true, nil
	}
	native := newNativeDoltStoreForTest(newNativeDoltMemStorage(), WithProxiedReadOnly())
	store, err := NewProxiedStore(native, writeStore, PinForTest("/scope", root, "beads"))
	if err != nil {
		t.Fatalf("NewProxiedStore: %v", err)
	}
	reader, ok := RevisionTransitionPatchReceiptReaderFor(store)
	if !ok {
		t.Fatal("ProxiedStore hid the exact receipt reader")
	}
	if _, found, err := reader.ReadRevisionTransitionPatchReceipt("patch/receipt-1"); err != nil || !found {
		t.Fatalf("proxied receipt read: found=%t err=%v", found, err)
	}
	if !store.Demoted() {
		t.Fatal("proxy generation change during receipt read did not stand down the native read leaf")
	}
}

func writeProxyRecordForControllerTransitionTest(t *testing.T, root string, pid, port int, generation string) {
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
	_ ControllerMetadataTransitionWriter     = (*controllerTransitionWrapperTestStore)(nil)
	_ RevisionTransitionWriterHandleProvider = (*revisionTransitionWrapperTestStore)(nil)
	_ RevisionTransitionPatchWriter          = (*revisionTransitionPatchWrapperTestStore)(nil)
	_ RevisionTransitionPatchReceiptReader   = (*revisionTransitionPatchWrapperTestStore)(nil)
)
