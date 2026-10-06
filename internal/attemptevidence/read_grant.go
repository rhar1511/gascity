package attemptevidence

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// ReadGrantScope identifies the exact retained access boundary for a signed
// read grant. The issuer must obtain these fields from trusted original
// permission records and authorize its authenticated reader before signing.
// This function computes an identifier; it does not grant permission.
//
// The digest is SHA-256 of six UTF-8 fields separated by NUL, in this order:
// gc-attempt-read.v1, store ref, work ID, repository root, workspace root,
// attempt ID. Fields must be nonempty and cannot contain NUL. Their exact
// archived spelling is retained; no alias or current-work lookup is applied.
func ReadGrantScope(request ReadAuthorizationRequest) (string, error) {
	fields := []string{
		"gc-attempt-read.v1", request.Scope.StoreRef, request.Scope.WorkID,
		request.Scope.RepositoryRoot, request.Scope.WorkspaceRoot, request.AttemptID,
	}
	for _, field := range fields {
		if strings.TrimSpace(field) == "" || strings.ContainsRune(field, '\x00') {
			return "", errors.New("attempt read grant requires a complete original scope")
		}
	}
	digest := sha256.Sum256([]byte(strings.Join(fields, "\x00")))
	return "gc-attempt-read.v1:" + hex.EncodeToString(digest[:]), nil
}
