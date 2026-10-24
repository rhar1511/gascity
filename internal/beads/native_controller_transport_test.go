package beads

import (
	"errors"
	"net/http"
	"reflect"
	"testing"
)

func TestNativeControllerTransportRetainedHandleRefusesAfterClose(t *testing.T) {
	complete, _ := newRemoteDecisionFrontierMetadataCASFixture(t,
		remoteDecisionFrontierMetadataCASMapRecord(t, 7),
		&remoteDecisionFrontierMetadataPermitIssuerStub{token: "metadata-permit"},
		&remoteDecisionFrontierMetadataTransitionWriterStub{toRevision: 47})
	transport := NewBdStore("", nil, WithBdStoreDecisionFrontierRecordWriter(complete))
	native := newNativeDoltStoreForTest(nil, WithNativeControllerTransport(transport))
	writer, ok := DecisionFrontierRecordWriterFor(native)
	if !ok {
		t.Fatal("configured native store did not expose its protected writer")
	}
	if err := native.CloseStore(); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.CreateDecisionFrontierRecord(remoteDecisionFrontierTestMapRecord()); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("retained writer after close: %v, want ErrStoreClosed", err)
	}
	if issuer := complete.permitIssuer.(*remoteDecisionFrontierTestPermitIssuer); len(issuer.requests) != 0 {
		t.Fatal("closed native store issued a protected permit")
	}
	if batch := complete.batchWriter.(*remoteDecisionFrontierTestBatchWriter); len(batch.requests) != 0 {
		t.Fatal("closed native store submitted a protected mutation")
	}
}

func nativeControllerTestTransport(t *testing.T) *BdStore {
	t.Helper()
	complete, _ := newRemoteDecisionFrontierMetadataCASFixture(t,
		remoteDecisionFrontierMetadataCASMapRecord(t, 7),
		&remoteDecisionFrontierMetadataPermitIssuerStub{token: "metadata-permit"},
		&remoteDecisionFrontierMetadataTransitionWriterStub{toRevision: 47})
	transport := NewBdStore("", nil,
		WithBdStorePrivateEvidenceHTTP(PrivateEvidenceHTTPConfig{
			Endpoint: "http://127.0.0.1:1", ProjectID: "project-a", Database: "gc_fixture",
			ScopeRef: "rig:fixture", TokenFile: privateEvidenceTokenFile(t), RevisionTransitions: true, RevisionTransitionPatches: true,
		}), WithBdStoreDecisionFrontierRecordWriter(complete))
	if transport.privateEvidenceHTTPInitErr != nil {
		t.Fatal(transport.privateEvidenceHTTPInitErr)
	}
	if _, ok := RevisionTransitionPatchWriterFor(transport); !ok {
		t.Fatal("configured leaf transport missing typed patch writer")
	}
	if _, ok := RevisionTransitionPatchReceiptReaderFor(transport); !ok {
		t.Fatal("configured leaf transport missing exact patch receipt reader")
	}
	transport.privateEvidenceHTTP.client.Transport = privateEvidenceHandlerTransport{handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("unexpected HTTP operation outside the native lifetime")
	})}
	return transport
}

func TestNativeControllerTransportCapabilityLifetime(t *testing.T) {
	contracts := []struct {
		name     string
		lookup   func(*NativeDoltStore) (any, bool)
		contract reflect.Type
	}{
		{"DecisionFrontierRecordWriter", func(s *NativeDoltStore) (any, bool) { return DecisionFrontierRecordWriterFor(s) }, reflect.TypeOf((*DecisionFrontierRecordWriter)(nil)).Elem()},
		{"PrivateEvidenceMetadataCASWriter", func(s *NativeDoltStore) (any, bool) { return PrivateEvidenceMetadataCASWriterFor(s) }, reflect.TypeOf((*PrivateEvidenceMetadataCASWriter)(nil)).Elem()},
		{"PrivateEvidenceArchiveReader", func(s *NativeDoltStore) (any, bool) { return PrivateEvidenceArchiveReaderFor(s) }, reflect.TypeOf((*PrivateEvidenceArchiveReader)(nil)).Elem()},
		{"ControllerMetadataTransitionWriter", func(s *NativeDoltStore) (any, bool) { return ControllerMetadataTransitionWriterFor(s) }, reflect.TypeOf((*ControllerMetadataTransitionWriter)(nil)).Elem()},
		{"ControllerMetadataTransitionReceiptReader", func(s *NativeDoltStore) (any, bool) { return ControllerMetadataTransitionReceiptReaderFor(s) }, reflect.TypeOf((*ControllerMetadataTransitionReceiptReader)(nil)).Elem()},
		{"DecisionFrontierSourceReader", func(s *NativeDoltStore) (any, bool) { return DecisionFrontierSourceReaderFor(s) }, reflect.TypeOf((*DecisionFrontierSourceReader)(nil)).Elem()},
		{"RevisionTransitionPatchWriter", func(s *NativeDoltStore) (any, bool) { return RevisionTransitionPatchWriterFor(s) }, reflect.TypeOf((*RevisionTransitionPatchWriter)(nil)).Elem()},
		{"RevisionTransitionPatchReceiptReader", func(s *NativeDoltStore) (any, bool) { return RevisionTransitionPatchReceiptReaderFor(s) }, reflect.TypeOf((*RevisionTransitionPatchReceiptReader)(nil)).Elem()},
		{"RevisionTransitionReceiptReader", func(s *NativeDoltStore) (any, bool) { return RevisionTransitionReceiptReaderFor(s) }, reflect.TypeOf((*RevisionTransitionReceiptReader)(nil)).Elem()},
		{"RevisionTransitionWriter", func(s *NativeDoltStore) (any, bool) { return RevisionTransitionWriterFor(s) }, reflect.TypeOf((*RevisionTransitionWriter)(nil)).Elem()},
	}
	for _, state := range []string{"nil", "unconfigured", "disabled", "enabled", "read-only", "closed"} {
		t.Run(state, func(t *testing.T) {
			var native *NativeDoltStore
			if state != "nil" {
				native = newNativeDoltStoreForTest(nil)
			}
			if state == "disabled" {
				WithNativeControllerTransport(NewBdStore("", nil))(native)
			}
			if state == "enabled" || state == "read-only" || state == "closed" {
				WithNativeControllerTransport(nativeControllerTestTransport(t))(native)
			}
			if state == "read-only" {
				WithProxiedReadOnly()(native)
			}
			if state == "closed" {
				if err := native.CloseStore(); err != nil {
					t.Fatal(err)
				}
			}
			for _, contract := range contracts {
				handle, available := contract.lookup(native)
				want := state == "enabled"
				if available != want || (want && handle == nil) {
					t.Errorf("%s available=%v handle=%T, want %v", contract.name, available, handle, want)
				}
			}
		})
	}
	for _, terminal := range []string{"closed", "read-only"} {
		t.Run("retained_"+terminal, func(t *testing.T) {
			native := newNativeDoltStoreForTest(nil, WithNativeControllerTransport(nativeControllerTestTransport(t)))
			handles := make([]any, len(contracts))
			for i, c := range contracts {
				var ok bool
				handles[i], ok = c.lookup(native)
				if !ok {
					t.Fatalf("missing %s", c.name)
				}
			}
			want := ErrStoreClosed
			if terminal == "closed" {
				if err := native.CloseStore(); err != nil {
					t.Fatal(err)
				}
			} else {
				WithProxiedReadOnly()(native)
				want = ErrProxiedNativeReadOnly
			}
			for i, c := range contracts {
				for j := 0; j < c.contract.NumMethod(); j++ {
					name := c.contract.Method(j).Name
					t.Run(c.name+"/"+name, func(t *testing.T) {
						method := reflect.ValueOf(handles[i]).MethodByName(name)
						args := make([]reflect.Value, method.Type().NumIn())
						for k := range args {
							args[k] = reflect.Zero(method.Type().In(k))
						}
						results := method.Call(args)
						err, _ := results[len(results)-1].Interface().(error)
						if !errors.Is(err, want) {
							t.Fatalf("retained %s: %v, want %v", name, err, want)
						}
					})
				}
			}
		})
	}
}

func TestNativeControllerTransportPreservesNativeQueries(t *testing.T) {
	native := newNativeDoltStoreForTest(newNativeDoltMemStorage(), WithNativeControllerTransport(nativeControllerTestTransport(t)))
	t.Cleanup(func() {
		if err := native.CloseStore(); err != nil {
			t.Error(err)
		}
	})
	created, err := native.Create(Bead{ID: "unit-1", Title: "native query record", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := native.Get(created.ID)
	if err != nil || got.Title != "native query record" {
		t.Fatalf("native Get: %+v, %v", got, err)
	}
}

func TestNativeControllerTransportPatchCapabilitiesThroughCache(t *testing.T) {
	native := newNativeDoltStoreForTest(nil, WithNativeControllerTransport(nativeControllerTestTransport(t)))
	cached := NewCachingStore(native, nil)
	writer, writerOK := RevisionTransitionPatchWriterFor(cached)
	reader, readerOK := RevisionTransitionPatchReceiptReaderFor(cached)
	if !writerOK || writer == nil || !readerOK || reader == nil {
		t.Fatal("cached native store lost patch capabilities")
	}
	if err := native.CloseStore(); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.TransitionPatch("unit-1", RevisionTransitionPatchRequest{}); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("retained cached writer: %v, want closed", err)
	}
	if _, _, err := reader.ReadRevisionTransitionPatchReceipt("receipt-1"); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("retained cached reader: %v, want closed", err)
	}
}

func TestNativeControllerTransportPatchForwarding(t *testing.T) {
	request := controllerTransitionPatchTestRequest()
	rawReceipt := controllerTransitionPatchTestReceipt(t, request, request.Actor)
	transport := &controllerTransitionPatchTestTransport{handler: controllerTransitionPatchTestHandler(rawReceipt, http.StatusOK)}
	leaf := controllerTransitionPatchTestStore(t, transport)
	expectedWrite, err := leaf.TransitionPatch("gc-1", request)
	if err != nil {
		t.Fatal(err)
	}
	expectedRead, expectedFound, err := leaf.ReadRevisionTransitionPatchReceipt(request.ReceiptID)
	if err != nil || !expectedFound {
		t.Fatalf("leaf receipt: found=%v err=%v", expectedFound, err)
	}
	native := newNativeDoltStoreForTest(nil, WithNativeControllerTransport(leaf))
	t.Cleanup(func() {
		if err := native.CloseStore(); err != nil {
			t.Error(err)
		}
	})
	cached := NewCachingStore(native, nil)
	writer, writerOK := RevisionTransitionPatchWriterFor(cached)
	reader, readerOK := RevisionTransitionPatchReceiptReaderFor(cached)
	if !writerOK || !readerOK {
		t.Fatal("native cache path lost lifecycle patch contracts")
	}
	gotWrite, err := writer.TransitionPatch("gc-1", request)
	if err != nil || !reflect.DeepEqual(gotWrite, expectedWrite) {
		t.Fatalf("forwarded write differs: err=%v", err)
	}
	gotRead, found, err := reader.ReadRevisionTransitionPatchReceipt(request.ReceiptID)
	if err != nil || !found || !reflect.DeepEqual(gotRead, expectedRead) {
		t.Fatalf("forwarded receipt differs: found=%v err=%v", found, err)
	}
	if transport.postCount != 2 || transport.receiptGetCount != 2 || !reflect.DeepEqual(transport.postBodies[0], transport.postBodies[1]) {
		t.Fatal("native cache forwarding changed exact request or duplicated an operation")
	}
}
