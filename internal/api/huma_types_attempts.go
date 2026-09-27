package api

// BeadAttemptsDiffInput is the Huma input for
// GET /v0/city/{cityName}/bead/{id}/attempts/diff.
type BeadAttemptsDiffInput struct {
	CityScope
	ID string `path:"id" doc:"Bead ID."`
}

// BeadAttemptHistoryInput is the Huma input for
// GET /v0/city/{cityName}/bead/{id}/attempts/{sessionID}/history.
type BeadAttemptHistoryInput struct {
	CityScope
	ID        string `path:"id" minLength:"1" doc:"Bead ID."`
	SessionID string `path:"sessionID" minLength:"1" doc:"Session bead ID for the selected execution attempt."`
}
