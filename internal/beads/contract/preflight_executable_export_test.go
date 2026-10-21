//go:build integration && linux

package contract

// InspectedBeadsModulePathForTest binds the external inspection fixture to the
// same module identity as the private predicate; it adds no production API.
const InspectedBeadsModulePathForTest = beadsModulePath

// InspectBeadsExecutableForTest exposes the private one-use inspection boundary
// to external integration tests without creating an import cycle through beads.
func InspectBeadsExecutableForTest(path string) (func() (BeadsArtifact, error), error) {
	_, finish, err := openBeadsExecutable(path)
	return finish, err
}

// BeadsArtifactSourceReasonForTest retains the exact private rejection predicate.
func BeadsArtifactSourceReasonForTest(artifact BeadsArtifact) string {
	return artifact.unconfirmedSourceReason()
}
