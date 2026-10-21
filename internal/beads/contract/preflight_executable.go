package contract

// BeadsExecutableObservation belongs to one context invocation. Every command
// attempt begins afresh, including retries that fail before selecting a binary.
// Identity is published only by the current attempt's completed inspection.
// Callers must not share an observation between concurrent invocations.
type BeadsExecutableObservation struct {
	generation uint64
	artifact   BeadsArtifact
}

// BeginAttempt invalidates prior identity before a new command attempt.
func (o *BeadsExecutableObservation) BeginAttempt() { o.generation++; o.artifact = BeadsArtifact{} }

// Artifact returns the identity published by the current attempt.
func (o *BeadsExecutableObservation) Artifact() BeadsArtifact { return o.artifact }
