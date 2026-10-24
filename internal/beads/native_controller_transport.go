package beads

// WithNativeControllerTransport attaches a controller-configured HTTP transport.
// The caller validates its exact scope and protected writer before publishing
// the store. Ordinary queries and mutations retain the native implementation.
func WithNativeControllerTransport(transport *BdStore) NativeDoltStoreOption {
	return func(s *NativeDoltStore) {
		if s == nil {
			return
		}
		s.controllerTransportMu.Lock()
		defer s.controllerTransportMu.Unlock()
		s.mu.Lock()
		defer s.mu.Unlock()
		if !s.closed {
			s.controllerTransport = transport
		}
	}
}

func (s *NativeDoltStore) controllerTransportForCapabilities() *BdStore {
	if s == nil {
		return nil
	}
	s.controllerTransportMu.RLock()
	defer s.controllerTransportMu.RUnlock()
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed || s.readOnlyReason != "" {
		return nil
	}
	return s.controllerTransport
}

// withControllerTransport owns the lifetime bracket for every returned handle.
// CloseStore acquires this lock before the native lock, so a protected writer
// can read native records inside the bracket without a recursive-lock deadlock.
func (s *NativeDoltStore) withControllerTransport(fn func(*BdStore) error) error {
	if s == nil {
		return ErrStoreClosed
	}
	s.controllerTransportMu.RLock()
	defer s.controllerTransportMu.RUnlock()
	s.mu.RLock()
	closed, reason, transport := s.closed, s.readOnlyReason, s.controllerTransport
	s.mu.RUnlock()
	if closed {
		return ErrStoreClosed
	}
	if reason != "" {
		return ErrProxiedNativeReadOnly
	}
	if transport == nil {
		return ErrPrivateEvidenceTransportUnsupported
	}
	return fn(transport)
}

type nativeControllerCapabilities struct{ store *NativeDoltStore }

// DecisionFrontierRecordWriterHandle exposes the scoped transport through the native lifetime fence.
func (s *NativeDoltStore) DecisionFrontierRecordWriterHandle() (DecisionFrontierRecordWriter, bool) {
	if _, ok := DecisionFrontierRecordWriterFor(s.controllerTransportForCapabilities()); !ok {
		return nil, false
	}
	return nativeControllerCapabilities{store: s}, true
}

// PrivateEvidenceMetadataCASWriterHandle exposes the scoped transport through the native lifetime fence.
func (s *NativeDoltStore) PrivateEvidenceMetadataCASWriterHandle() (PrivateEvidenceMetadataCASWriter, bool) {
	if _, ok := PrivateEvidenceMetadataCASWriterFor(s.controllerTransportForCapabilities()); !ok {
		return nil, false
	}
	return nativeControllerCapabilities{store: s}, true
}

// PrivateEvidenceArchiveReaderHandle exposes the scoped transport through the native lifetime fence.
func (s *NativeDoltStore) PrivateEvidenceArchiveReaderHandle() (PrivateEvidenceArchiveReader, bool) {
	if _, ok := PrivateEvidenceArchiveReaderFor(s.controllerTransportForCapabilities()); !ok {
		return nil, false
	}
	return nativeControllerCapabilities{store: s}, true
}

// ControllerMetadataTransitionWriterHandle exposes the scoped transport through the native lifetime fence.
func (s *NativeDoltStore) ControllerMetadataTransitionWriterHandle() (ControllerMetadataTransitionWriter, bool) {
	if _, ok := ControllerMetadataTransitionWriterFor(s.controllerTransportForCapabilities()); !ok {
		return nil, false
	}
	return nativeControllerCapabilities{store: s}, true
}

// ControllerMetadataTransitionReceiptReaderHandle exposes the scoped transport through the native lifetime fence.
func (s *NativeDoltStore) ControllerMetadataTransitionReceiptReaderHandle() (ControllerMetadataTransitionReceiptReader, bool) {
	if _, ok := ControllerMetadataTransitionReceiptReaderFor(s.controllerTransportForCapabilities()); !ok {
		return nil, false
	}
	return nativeControllerCapabilities{store: s}, true
}

// DecisionFrontierSourceReaderHandle exposes the scoped transport through the native lifetime fence.
func (s *NativeDoltStore) DecisionFrontierSourceReaderHandle() (DecisionFrontierSourceReader, bool) {
	if _, ok := DecisionFrontierSourceReaderFor(s.controllerTransportForCapabilities()); !ok {
		return nil, false
	}
	return nativeControllerCapabilities{store: s}, true
}

// RevisionTransitionReceiptReaderHandle exposes the scoped transport through the native lifetime fence.
func (s *NativeDoltStore) RevisionTransitionReceiptReaderHandle() (RevisionTransitionReceiptReader, bool) {
	if _, ok := RevisionTransitionReceiptReaderFor(s.controllerTransportForCapabilities()); !ok {
		return nil, false
	}
	return nativeControllerCapabilities{store: s}, true
}

// RevisionTransitionWriterHandle exposes the scoped transport through the native lifetime fence.
func (s *NativeDoltStore) RevisionTransitionWriterHandle() (RevisionTransitionWriter, bool) {
	if _, ok := RevisionTransitionWriterFor(s.controllerTransportForCapabilities()); !ok {
		return nil, false
	}
	return nativeControllerCapabilities{store: s}, true
}

func (w nativeControllerCapabilities) CreateDecisionFrontierRecord(record Bead) (Bead, error) {
	var result Bead
	err := w.store.withControllerTransport(func(transport *BdStore) error {
		writer, ok := DecisionFrontierRecordWriterFor(transport)
		if !ok {
			return ErrDecisionFrontierCapabilityUnsupported
		}
		var err error
		result, err = writer.CreateDecisionFrontierRecord(record)
		return err
	})
	return result, err
}

func (w nativeControllerCapabilities) CompareAndSetDecisionFrontierRecordMetadataKey(id, key, expected, next string) (bool, error) {
	var result bool
	err := w.store.withControllerTransport(func(transport *BdStore) error {
		writer, ok := DecisionFrontierRecordWriterFor(transport)
		if !ok {
			return ErrDecisionFrontierCapabilityUnsupported
		}
		var err error
		result, err = writer.CompareAndSetDecisionFrontierRecordMetadataKey(id, key, expected, next)
		return err
	})
	return result, err
}

func (w nativeControllerCapabilities) EnsureDecisionFrontierLink(sourceID, targetID, depType string) error {
	err := w.store.withControllerTransport(func(transport *BdStore) error {
		writer, ok := DecisionFrontierRecordWriterFor(transport)
		if !ok {
			return ErrDecisionFrontierCapabilityUnsupported
		}
		err := writer.EnsureDecisionFrontierLink(sourceID, targetID, depType)
		return err
	})
	return err
}

func (w nativeControllerCapabilities) CompareAndSetPrivateEvidenceMetadataKey(id, key, expected, next string) (bool, error) {
	var result bool
	err := w.store.withControllerTransport(func(transport *BdStore) error {
		var err error
		result, err = transport.CompareAndSetPrivateEvidenceMetadataKey(id, key, expected, next)
		return err
	})
	return result, err
}

func (w nativeControllerCapabilities) ReadPrivateEvidenceMetadataKey(id, key string) (string, bool, error) {
	var result string
	var found bool
	err := w.store.withControllerTransport(func(transport *BdStore) error {
		var err error
		result, found, err = transport.ReadPrivateEvidenceMetadataKey(id, key)
		return err
	})
	return result, found, err
}

func (w nativeControllerCapabilities) ListPrivateEvidenceOwnerIndexes(ownerID string) (map[string]string, error) {
	var result map[string]string
	err := w.store.withControllerTransport(func(transport *BdStore) error {
		var err error
		result, err = transport.ListPrivateEvidenceOwnerIndexes(ownerID)
		return err
	})
	return result, err
}

func (w nativeControllerCapabilities) ListPrivateEvidenceArchives(ownerID, attemptID string) ([]Bead, error) {
	var result []Bead
	err := w.store.withControllerTransport(func(transport *BdStore) error {
		var err error
		result, err = transport.ListPrivateEvidenceArchives(ownerID, attemptID)
		return err
	})
	return result, err
}

func (w nativeControllerCapabilities) TransitionMetadata(issueID string, request ControllerMetadataTransitionRequest) (ControllerMetadataTransitionResult, error) {
	var result ControllerMetadataTransitionResult
	err := w.store.withControllerTransport(func(transport *BdStore) error {
		var err error
		result, err = transport.TransitionMetadata(issueID, request)
		return err
	})
	return result, err
}

func (w nativeControllerCapabilities) ControllerMetadataTransitionReceipt(issueID, receiptID string) (ControllerMetadataTransitionReceipt, bool, error) {
	var result ControllerMetadataTransitionReceipt
	var found bool
	err := w.store.withControllerTransport(func(transport *BdStore) error {
		var err error
		result, found, err = transport.ControllerMetadataTransitionReceipt(issueID, receiptID)
		return err
	})
	return result, found, err
}

func (w nativeControllerCapabilities) DecisionFrontierSourceSnapshot(id string) (Bead, error) {
	var result Bead
	err := w.store.withControllerTransport(func(transport *BdStore) error {
		var err error
		result, err = transport.DecisionFrontierSourceSnapshot(id)
		return err
	})
	return result, err
}

func (w nativeControllerCapabilities) DecisionFrontierRevisionTransitionReceipt(issueID, receiptID string) (RevisionTransitionReceipt, bool, error) {
	var result RevisionTransitionReceipt
	var found bool
	err := w.store.withControllerTransport(func(transport *BdStore) error {
		var err error
		result, found, err = transport.DecisionFrontierRevisionTransitionReceipt(issueID, receiptID)
		return err
	})
	return result, found, err
}

func (w nativeControllerCapabilities) CompareAndSetMetadataKeyWithReceipt(id, key, expected, next string, expectedRevision int64, receipt RevisionTransitionReceipt) (Bead, bool, error) {
	var result Bead
	var won bool
	err := w.store.withControllerTransport(func(transport *BdStore) error {
		writer, ok := RevisionTransitionWriterFor(transport)
		if !ok {
			return ErrControllerMetadataRevisionTransitionUnavailable
		}
		var err error
		result, won, err = writer.CompareAndSetMetadataKeyWithReceipt(id, key, expected, next, expectedRevision, receipt)
		return err
	})
	return result, won, err
}

// RevisionTransitionPatchWriterHandle forwards typed patch writes through the native lifetime fence.
func (s *NativeDoltStore) RevisionTransitionPatchWriterHandle() (RevisionTransitionPatchWriter, bool) {
	if _, ok := RevisionTransitionPatchWriterFor(s.controllerTransportForCapabilities()); !ok {
		return nil, false
	}
	return nativeControllerCapabilities{store: s}, true
}

// RevisionTransitionPatchReceiptReaderHandle forwards exact receipt reads through the native lifetime fence.
func (s *NativeDoltStore) RevisionTransitionPatchReceiptReaderHandle() (RevisionTransitionPatchReceiptReader, bool) {
	if _, ok := RevisionTransitionPatchReceiptReaderFor(s.controllerTransportForCapabilities()); !ok {
		return nil, false
	}
	return nativeControllerCapabilities{store: s}, true
}

func (w nativeControllerCapabilities) TransitionPatch(issueID string, request RevisionTransitionPatchRequest) (RevisionTransitionPatchResult, error) {
	var result RevisionTransitionPatchResult
	err := w.store.withControllerTransport(func(transport *BdStore) error {
		writer, ok := RevisionTransitionPatchWriterFor(transport)
		if !ok {
			return ErrRevisionTransitionPatchUnavailable
		}
		var err error
		result, err = writer.TransitionPatch(issueID, request)
		return err
	})
	return result, err
}

func (w nativeControllerCapabilities) ReadRevisionTransitionPatchReceipt(receiptID string) (RevisionTransitionPatchReceipt, bool, error) {
	var result RevisionTransitionPatchReceipt
	var found bool
	err := w.store.withControllerTransport(func(transport *BdStore) error {
		reader, ok := RevisionTransitionPatchReceiptReaderFor(transport)
		if !ok {
			return ErrRevisionTransitionPatchUnavailable
		}
		var err error
		result, found, err = reader.ReadRevisionTransitionPatchReceipt(receiptID)
		return err
	})
	return result, found, err
}
