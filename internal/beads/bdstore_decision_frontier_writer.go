package beads

var (
	_ DecisionFrontierRecordWriterHandleProvider = (*BdStore)(nil)
)

// WithBdStoreDecisionFrontierRecordWriter attaches a caller-constructed
// protected remote decision-frontier writer to a BdStore. It does not load
// credentials, build permit issuers, or contact the Beads service; callers must
// supply the complete trusted adapter explicitly.
func WithBdStoreDecisionFrontierRecordWriter(writer *RemoteDecisionFrontierRecordWriter) BdStoreOption {
	return func(store *BdStore) {
		if store != nil {
			store.decisionFrontierRecordWriter = writer
		}
	}
}

// DecisionFrontierRecordWriterHandle fails closed for an unattached or
// incomplete remote adapter.
func (store *BdStore) DecisionFrontierRecordWriterHandle() (DecisionFrontierRecordWriter, bool) {
	if store == nil || store.decisionFrontierRecordWriter == nil {
		return nil, false
	}
	return store.decisionFrontierRecordWriter.DecisionFrontierRecordWriterHandle()
}
