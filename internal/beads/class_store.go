package beads

// This file declares the strongly-typed per-class store wrappers that form the
// compile-time seam over the otherwise class-agnostic Store interface.
//
// Each type embeds the Store interface (field name Store), so it promotes every
// Store method and therefore IS a Store for all Store operations. The point is
// purely static: a function that handles a statically-known coordination class
// takes/returns its typed store, and the compiler then refuses to let a caller
// hand it a store belonging to a different class. At runtime each typed value
// wraps the SAME underlying store value the call site already used — no new
// backend, no extra caching or policy layer — so behavior is byte-identical.
//
// Optional capabilities (e.g. Counter, GraphApplyStore, GraphApplyFor,
// StorageCreateStore, Backing/ReadyLive) are NOT promoted through the embedding:
// a type assertion on a typed store value asserts on the wrapper, not the
// underlying store, and will fail. Capabilities with an explicit handle
// provider are resolved through that provider; for other optional capabilities,
// assert on the embedded .Store field instead (e.g. `c, ok := s.Store.(beads.Counter)`).
// Likewise pass the unwrapped .Store field when calling a generic Store helper
// that is shared across multiple classes.

// WorkStore is a strongly-typed view over a single Store holding work beads
// (the city's general task ledger). It is backed by the same underlying store
// it wraps; the wrapper exists so the compiler enforces that a work-class
// consumer cannot be handed another class's store. Access optional capabilities
// by asserting on the embedded .Store field.
type WorkStore struct {
	Store
}

// GraphStore is a strongly-typed view over a single Store holding graph beads
// (controller graph / molecule state). It is backed by the same underlying
// store it wraps; the wrapper exists so the compiler enforces that a graph-class
// consumer cannot be handed another class's store. Access optional capabilities
// by asserting on the embedded .Store field.
type GraphStore struct {
	Store
}

// SessionStore is a strongly-typed view over a single Store holding session
// beads (session lifecycle projection). It is backed by the same underlying
// store it wraps; the wrapper exists so the compiler enforces that a
// session-class consumer cannot be handed another class's store. Access optional
// capabilities by asserting on the embedded .Store field.
type SessionStore struct {
	Store
}

// MailStore is a strongly-typed view over a single Store holding mail beads
// (inter-agent messages). It is backed by the same underlying store it wraps;
// the wrapper exists so the compiler enforces that a mail-class consumer cannot
// be handed another class's store. Access optional capabilities by asserting on
// the embedded .Store field.
type MailStore struct {
	Store
}

// OrdersStore is a strongly-typed view over a single Store holding order beads
// (scheduled/event-gated formula triggers). It is backed by the same underlying
// store it wraps; the wrapper exists so the compiler enforces that an
// orders-class consumer cannot be handed another class's store. Access optional
// capabilities by asserting on the embedded .Store field.
type OrdersStore struct {
	Store
}

// NudgesStore is a strongly-typed view over a single Store holding nudge beads
// (session nudges). It is backed by the same underlying store it wraps; the
// wrapper exists so the compiler enforces that a nudges-class consumer cannot be
// handed another class's store. Access optional capabilities by asserting on the
// embedded .Store field.
type NudgesStore struct {
	Store
}

// The typed class wrappers declare their embedded store as the
// conditional-writes resolution target, so ResolveConditionalWriter works on
// a typed handle without the caller remembering to unwrap — the one optional
// capability where forgetting the unwrap would not fail loudly but silently
// resolve unset→legacy (fatal under require). All other optional capabilities
// keep the assert-on-.Store convention above.

// ConditionalWritesResolveTarget declares the wrapped store as the
// conditional-writes resolution target.
func (s WorkStore) ConditionalWritesResolveTarget() Store { return s.Store }

// ConditionalWritesResolveTarget declares the wrapped store as the
// conditional-writes resolution target.
func (s GraphStore) ConditionalWritesResolveTarget() Store { return s.Store }

// ConditionalWritesResolveTarget declares the wrapped store as the
// conditional-writes resolution target.
func (s SessionStore) ConditionalWritesResolveTarget() Store { return s.Store }

// ConditionalWritesResolveTarget declares the wrapped store as the
// conditional-writes resolution target.
func (s MailStore) ConditionalWritesResolveTarget() Store { return s.Store }

// ConditionalWritesResolveTarget declares the wrapped store as the
// conditional-writes resolution target.
func (s OrdersStore) ConditionalWritesResolveTarget() Store { return s.Store }

// ConditionalWritesResolveTarget declares the wrapped store as the
// conditional-writes resolution target.
func (s NudgesStore) ConditionalWritesResolveTarget() Store { return s.Store }

// PrivateEvidenceMetadataCASWriterHandle forwards the narrowly scoped private
// evidence writer without bypassing any inner wrapper that owns write policy.
func (s WorkStore) PrivateEvidenceMetadataCASWriterHandle() (PrivateEvidenceMetadataCASWriter, bool) {
	return PrivateEvidenceMetadataCASWriterFor(s.Store)
}

// PrivateEvidenceMetadataCASWriterHandle forwards the private-evidence writer
// from the wrapped store.
func (s GraphStore) PrivateEvidenceMetadataCASWriterHandle() (PrivateEvidenceMetadataCASWriter, bool) {
	return PrivateEvidenceMetadataCASWriterFor(s.Store)
}

// PrivateEvidenceMetadataCASWriterHandle forwards the private-evidence writer
// from the wrapped store.
func (s SessionStore) PrivateEvidenceMetadataCASWriterHandle() (PrivateEvidenceMetadataCASWriter, bool) {
	return PrivateEvidenceMetadataCASWriterFor(s.Store)
}

// PrivateEvidenceMetadataCASWriterHandle forwards the private-evidence writer
// from the wrapped store.
func (s MailStore) PrivateEvidenceMetadataCASWriterHandle() (PrivateEvidenceMetadataCASWriter, bool) {
	return PrivateEvidenceMetadataCASWriterFor(s.Store)
}

// PrivateEvidenceMetadataCASWriterHandle forwards the private-evidence writer
// from the wrapped store.
func (s OrdersStore) PrivateEvidenceMetadataCASWriterHandle() (PrivateEvidenceMetadataCASWriter, bool) {
	return PrivateEvidenceMetadataCASWriterFor(s.Store)
}

// PrivateEvidenceMetadataCASWriterHandle forwards the private-evidence writer
// from the wrapped store.
func (s NudgesStore) PrivateEvidenceMetadataCASWriterHandle() (PrivateEvidenceMetadataCASWriter, bool) {
	return PrivateEvidenceMetadataCASWriterFor(s.Store)
}

// PrivateEvidenceArchiveReaderHandle forwards the private-evidence reader
// from the wrapped store.
func (s WorkStore) PrivateEvidenceArchiveReaderHandle() (PrivateEvidenceArchiveReader, bool) {
	return PrivateEvidenceArchiveReaderFor(s.Store)
}

// PrivateEvidenceArchiveReaderHandle forwards the private-evidence reader
// from the wrapped store.
func (s GraphStore) PrivateEvidenceArchiveReaderHandle() (PrivateEvidenceArchiveReader, bool) {
	return PrivateEvidenceArchiveReaderFor(s.Store)
}

// PrivateEvidenceArchiveReaderHandle forwards the private-evidence reader
// from the wrapped store.
func (s SessionStore) PrivateEvidenceArchiveReaderHandle() (PrivateEvidenceArchiveReader, bool) {
	return PrivateEvidenceArchiveReaderFor(s.Store)
}

// PrivateEvidenceArchiveReaderHandle forwards the private-evidence reader
// from the wrapped store.
func (s MailStore) PrivateEvidenceArchiveReaderHandle() (PrivateEvidenceArchiveReader, bool) {
	return PrivateEvidenceArchiveReaderFor(s.Store)
}

// PrivateEvidenceArchiveReaderHandle forwards the private-evidence reader
// from the wrapped store.
func (s OrdersStore) PrivateEvidenceArchiveReaderHandle() (PrivateEvidenceArchiveReader, bool) {
	return PrivateEvidenceArchiveReaderFor(s.Store)
}

// PrivateEvidenceArchiveReaderHandle forwards the private-evidence reader
// from the wrapped store.
func (s NudgesStore) PrivateEvidenceArchiveReaderHandle() (PrivateEvidenceArchiveReader, bool) {
	return PrivateEvidenceArchiveReaderFor(s.Store)
}

// ControllerMetadataTransitionWriterHandle forwards the scope's explicitly
// enabled Q43 transition transport through the wrapped store.
func (s WorkStore) ControllerMetadataTransitionWriterHandle() (ControllerMetadataTransitionWriter, bool) {
	return ControllerMetadataTransitionWriterFor(s.Store)
}

// ControllerMetadataTransitionWriterHandle forwards the scope's explicitly
// enabled Q43 transition transport through the wrapped store.
func (s GraphStore) ControllerMetadataTransitionWriterHandle() (ControllerMetadataTransitionWriter, bool) {
	return ControllerMetadataTransitionWriterFor(s.Store)
}

// ControllerMetadataTransitionWriterHandle forwards the scope's explicitly
// enabled Q43 transition transport through the wrapped store.
func (s SessionStore) ControllerMetadataTransitionWriterHandle() (ControllerMetadataTransitionWriter, bool) {
	return ControllerMetadataTransitionWriterFor(s.Store)
}

// ControllerMetadataTransitionWriterHandle forwards the scope's explicitly
// enabled Q43 transition transport through the wrapped store.
func (s MailStore) ControllerMetadataTransitionWriterHandle() (ControllerMetadataTransitionWriter, bool) {
	return ControllerMetadataTransitionWriterFor(s.Store)
}

// ControllerMetadataTransitionWriterHandle forwards the scope's explicitly
// enabled Q43 transition transport through the wrapped store.
func (s OrdersStore) ControllerMetadataTransitionWriterHandle() (ControllerMetadataTransitionWriter, bool) {
	return ControllerMetadataTransitionWriterFor(s.Store)
}

// ControllerMetadataTransitionWriterHandle forwards the scope's explicitly
// enabled Q43 transition transport through the wrapped store.
func (s NudgesStore) ControllerMetadataTransitionWriterHandle() (ControllerMetadataTransitionWriter, bool) {
	return ControllerMetadataTransitionWriterFor(s.Store)
}

// DecisionFrontierSourceReaderHandle forwards the authoritative source reader
// through this typed store view.
func (s WorkStore) DecisionFrontierSourceReaderHandle() (DecisionFrontierSourceReader, bool) {
	return DecisionFrontierSourceReaderFor(s.Store)
}

// DecisionFrontierSourceReaderHandle forwards the authoritative source reader
// through this typed store view.
func (s GraphStore) DecisionFrontierSourceReaderHandle() (DecisionFrontierSourceReader, bool) {
	return DecisionFrontierSourceReaderFor(s.Store)
}

// DecisionFrontierSourceReaderHandle forwards the authoritative source reader
// through this typed store view.
func (s SessionStore) DecisionFrontierSourceReaderHandle() (DecisionFrontierSourceReader, bool) {
	return DecisionFrontierSourceReaderFor(s.Store)
}

// DecisionFrontierSourceReaderHandle forwards the authoritative source reader
// through this typed store view.
func (s MailStore) DecisionFrontierSourceReaderHandle() (DecisionFrontierSourceReader, bool) {
	return DecisionFrontierSourceReaderFor(s.Store)
}

// DecisionFrontierSourceReaderHandle forwards the authoritative source reader
// through this typed store view.
func (s OrdersStore) DecisionFrontierSourceReaderHandle() (DecisionFrontierSourceReader, bool) {
	return DecisionFrontierSourceReaderFor(s.Store)
}

// DecisionFrontierSourceReaderHandle forwards the authoritative source reader
// through this typed store view.
func (s NudgesStore) DecisionFrontierSourceReaderHandle() (DecisionFrontierSourceReader, bool) {
	return DecisionFrontierSourceReaderFor(s.Store)
}

// RevisionTransitionReceiptReaderHandle forwards exact receipt reads through
// this typed store view only when the underlying store explicitly supports it.
func (s WorkStore) RevisionTransitionReceiptReaderHandle() (RevisionTransitionReceiptReader, bool) {
	return RevisionTransitionReceiptReaderFor(s.Store)
}

func (s GraphStore) RevisionTransitionReceiptReaderHandle() (RevisionTransitionReceiptReader, bool) {
	return RevisionTransitionReceiptReaderFor(s.Store)
}

func (s SessionStore) RevisionTransitionReceiptReaderHandle() (RevisionTransitionReceiptReader, bool) {
	return RevisionTransitionReceiptReaderFor(s.Store)
}

func (s MailStore) RevisionTransitionReceiptReaderHandle() (RevisionTransitionReceiptReader, bool) {
	return RevisionTransitionReceiptReaderFor(s.Store)
}

func (s OrdersStore) RevisionTransitionReceiptReaderHandle() (RevisionTransitionReceiptReader, bool) {
	return RevisionTransitionReceiptReaderFor(s.Store)
}

func (s NudgesStore) RevisionTransitionReceiptReaderHandle() (RevisionTransitionReceiptReader, bool) {
	return RevisionTransitionReceiptReaderFor(s.Store)
}

// ControllerMetadataTransitionReceiptReaderHandle forwards full durable Q43
// receipt envelopes through this typed store view.
func (s WorkStore) ControllerMetadataTransitionReceiptReaderHandle() (ControllerMetadataTransitionReceiptReader, bool) {
	return ControllerMetadataTransitionReceiptReaderFor(s.Store)
}

func (s GraphStore) ControllerMetadataTransitionReceiptReaderHandle() (ControllerMetadataTransitionReceiptReader, bool) {
	return ControllerMetadataTransitionReceiptReaderFor(s.Store)
}

func (s SessionStore) ControllerMetadataTransitionReceiptReaderHandle() (ControllerMetadataTransitionReceiptReader, bool) {
	return ControllerMetadataTransitionReceiptReaderFor(s.Store)
}

func (s MailStore) ControllerMetadataTransitionReceiptReaderHandle() (ControllerMetadataTransitionReceiptReader, bool) {
	return ControllerMetadataTransitionReceiptReaderFor(s.Store)
}

func (s OrdersStore) ControllerMetadataTransitionReceiptReaderHandle() (ControllerMetadataTransitionReceiptReader, bool) {
	return ControllerMetadataTransitionReceiptReaderFor(s.Store)
}

func (s NudgesStore) ControllerMetadataTransitionReceiptReaderHandle() (ControllerMetadataTransitionReceiptReader, bool) {
	return ControllerMetadataTransitionReceiptReaderFor(s.Store)
}

// RevisionTransitionWriterHandle forwards the complete atomic source
// transition through this typed store view.
func (s WorkStore) RevisionTransitionWriterHandle() (RevisionTransitionWriter, bool) {
	return RevisionTransitionWriterFor(s.Store)
}

func (s GraphStore) RevisionTransitionWriterHandle() (RevisionTransitionWriter, bool) {
	return RevisionTransitionWriterFor(s.Store)
}

func (s SessionStore) RevisionTransitionWriterHandle() (RevisionTransitionWriter, bool) {
	return RevisionTransitionWriterFor(s.Store)
}

func (s MailStore) RevisionTransitionWriterHandle() (RevisionTransitionWriter, bool) {
	return RevisionTransitionWriterFor(s.Store)
}

func (s OrdersStore) RevisionTransitionWriterHandle() (RevisionTransitionWriter, bool) {
	return RevisionTransitionWriterFor(s.Store)
}

func (s NudgesStore) RevisionTransitionWriterHandle() (RevisionTransitionWriter, bool) {
	return RevisionTransitionWriterFor(s.Store)
}

// DecisionFrontierRecordWriterHandle forwards the protected record writer
// through the typed class view without inventing support.
func (s WorkStore) DecisionFrontierRecordWriterHandle() (DecisionFrontierRecordWriter, bool) {
	return DecisionFrontierRecordWriterFor(s.Store)
}

func (s GraphStore) DecisionFrontierRecordWriterHandle() (DecisionFrontierRecordWriter, bool) {
	return DecisionFrontierRecordWriterFor(s.Store)
}

func (s SessionStore) DecisionFrontierRecordWriterHandle() (DecisionFrontierRecordWriter, bool) {
	return DecisionFrontierRecordWriterFor(s.Store)
}

func (s MailStore) DecisionFrontierRecordWriterHandle() (DecisionFrontierRecordWriter, bool) {
	return DecisionFrontierRecordWriterFor(s.Store)
}

func (s OrdersStore) DecisionFrontierRecordWriterHandle() (DecisionFrontierRecordWriter, bool) {
	return DecisionFrontierRecordWriterFor(s.Store)
}

func (s NudgesStore) DecisionFrontierRecordWriterHandle() (DecisionFrontierRecordWriter, bool) {
	return DecisionFrontierRecordWriterFor(s.Store)
}

var (
	_ ConditionalWritesResolveTargeter                        = WorkStore{}
	_ ConditionalWritesResolveTargeter                        = GraphStore{}
	_ ConditionalWritesResolveTargeter                        = SessionStore{}
	_ ConditionalWritesResolveTargeter                        = MailStore{}
	_ ConditionalWritesResolveTargeter                        = OrdersStore{}
	_ ConditionalWritesResolveTargeter                        = NudgesStore{}
	_ ControllerMetadataTransitionWriterHandleProvider        = WorkStore{}
	_ ControllerMetadataTransitionWriterHandleProvider        = GraphStore{}
	_ ControllerMetadataTransitionWriterHandleProvider        = SessionStore{}
	_ ControllerMetadataTransitionWriterHandleProvider        = MailStore{}
	_ ControllerMetadataTransitionWriterHandleProvider        = OrdersStore{}
	_ ControllerMetadataTransitionWriterHandleProvider        = NudgesStore{}
	_ DecisionFrontierSourceReaderHandleProvider              = WorkStore{}
	_ DecisionFrontierSourceReaderHandleProvider              = GraphStore{}
	_ DecisionFrontierSourceReaderHandleProvider              = SessionStore{}
	_ DecisionFrontierSourceReaderHandleProvider              = MailStore{}
	_ DecisionFrontierSourceReaderHandleProvider              = OrdersStore{}
	_ DecisionFrontierSourceReaderHandleProvider              = NudgesStore{}
	_ RevisionTransitionReceiptReaderHandleProvider           = WorkStore{}
	_ RevisionTransitionReceiptReaderHandleProvider           = GraphStore{}
	_ RevisionTransitionReceiptReaderHandleProvider           = SessionStore{}
	_ RevisionTransitionReceiptReaderHandleProvider           = MailStore{}
	_ RevisionTransitionReceiptReaderHandleProvider           = OrdersStore{}
	_ RevisionTransitionReceiptReaderHandleProvider           = NudgesStore{}
	_ ControllerMetadataTransitionReceiptReaderHandleProvider = WorkStore{}
	_ ControllerMetadataTransitionReceiptReaderHandleProvider = GraphStore{}
	_ ControllerMetadataTransitionReceiptReaderHandleProvider = SessionStore{}
	_ ControllerMetadataTransitionReceiptReaderHandleProvider = MailStore{}
	_ ControllerMetadataTransitionReceiptReaderHandleProvider = OrdersStore{}
	_ ControllerMetadataTransitionReceiptReaderHandleProvider = NudgesStore{}
	_ RevisionTransitionWriterHandleProvider                  = WorkStore{}
	_ RevisionTransitionWriterHandleProvider                  = GraphStore{}
	_ RevisionTransitionWriterHandleProvider                  = SessionStore{}
	_ RevisionTransitionWriterHandleProvider                  = MailStore{}
	_ RevisionTransitionWriterHandleProvider                  = OrdersStore{}
	_ RevisionTransitionWriterHandleProvider                  = NudgesStore{}
	_ DecisionFrontierRecordWriterHandleProvider              = WorkStore{}
	_ DecisionFrontierRecordWriterHandleProvider              = GraphStore{}
	_ DecisionFrontierRecordWriterHandleProvider              = SessionStore{}
	_ DecisionFrontierRecordWriterHandleProvider              = MailStore{}
	_ DecisionFrontierRecordWriterHandleProvider              = OrdersStore{}
	_ DecisionFrontierRecordWriterHandleProvider              = NudgesStore{}
)
