package api

// AttemptEvidenceListInput identifies a work item whose archived execution
// attempts should be read. Access is checked against each archive's stored
// repository/work/attempt permission scope.
type AttemptEvidenceListInput struct {
	CityScope
	ID string `path:"id" doc:"Work bead ID that owns the captured execution attempts."`
}

// AttemptEvidenceGetInput identifies one exact attempt archive. AttemptID is
// immutable and has no latest-attempt fallback.
type AttemptEvidenceGetInput struct {
	CityScope
	ID        string `path:"id" doc:"Work bead ID that owns the captured execution attempt."`
	AttemptID string `path:"attemptID" doc:"Exact immutable attempt ID."`
}

// AttemptEvidenceArtifactGetInput identifies one content-addressed payload
// referenced by an exact immutable attempt.
type AttemptEvidenceArtifactGetInput struct {
	CityScope
	ID        string `path:"id" doc:"Work bead ID that owns the captured execution attempt."`
	AttemptID string `path:"attemptID" doc:"Exact immutable attempt ID."`
	SHA256    string `path:"sha256" doc:"Exact content digest listed by this attempt."`
}
