package selectorattestation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFileHostAuthoritySourceReadsProtectedBundle(t *testing.T) {
	fixture := newFixture(t)
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeHostFiles(t, directory, fixture.source.bundle)
	source, err := NewFileHostAuthoritySource(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := source.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	verifier, err := NewVerifier(source, fixture.verifier.options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.VerifyObservation(context.Background(), observationToken(t, fixture.observation, fixture.observationKey), fixture.observationWant); err != nil {
		t.Fatalf("VerifyObservation from file source: %v", err)
	}
	if _, err := verifier.VerifyReview(context.Background(), reviewToken(t, fixture.review, fixture.reviewKey), fixture.reviewWant); err != nil {
		t.Fatalf("VerifyReview from file source: %v", err)
	}
}

func TestFileHostAuthoritySourceRejectsUnsafeFilesAndReplacedRoot(t *testing.T) {
	fixture := newFixture(t)
	if _, err := NewFileHostAuthoritySource("relative/path"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("relative source = %v, want ErrUnavailable", err)
	}

	t.Run("writable keyring", func(t *testing.T) {
		directory := protectedDirectory(t)
		writeHostFiles(t, directory, fixture.source.bundle)
		source, err := NewFileHostAuthoritySource(directory)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = source.Close() })
		if err := os.Chmod(filepath.Join(directory, HostKeyringFile), 0o666); err != nil {
			t.Fatal(err)
		}
		if _, err := source.Load(context.Background()); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("Load writable keyring = %v, want ErrUnavailable", err)
		}
	})

	t.Run("symlinked revocations", func(t *testing.T) {
		root := t.TempDir()
		directory := filepath.Join(root, "authority")
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		writeHostFiles(t, directory, fixture.source.bundle)
		path := filepath.Join(directory, HostRevocationsFile)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(root, "outside.json")
		if err := os.WriteFile(outside, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, path); err != nil {
			t.Fatal(err)
		}
		source, err := NewFileHostAuthoritySource(directory)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = source.Close() })
		if _, err := source.Load(context.Background()); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("Load symlinked revocations = %v, want ErrUnavailable", err)
		}
	})

	t.Run("replaced root", func(t *testing.T) {
		root := t.TempDir()
		directory := filepath.Join(root, "authority")
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		writeHostFiles(t, directory, fixture.source.bundle)
		source, err := NewFileHostAuthoritySource(directory)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = source.Close() })
		if err := os.Rename(directory, directory+".old"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		writeHostFiles(t, directory, fixture.source.bundle)
		if _, err := source.Load(context.Background()); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("Load replaced root = %v, want ErrUnavailable", err)
		}
	})
}

func TestFileHostAuthoritySourceRejectsStrictAndBoundedJSONFailures(t *testing.T) {
	fixture := newFixture(t)
	t.Run("noncanonical keyring", func(t *testing.T) {
		directory := protectedDirectory(t)
		writeHostFiles(t, directory, fixture.source.bundle)
		path := filepath.Join(directory, HostKeyringFile)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		source, err := NewFileHostAuthoritySource(directory)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = source.Close() })
		if _, err := source.Load(context.Background()); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("Load noncanonical keyring = %v, want ErrUnavailable", err)
		}
	})

	t.Run("unknown field", func(t *testing.T) {
		directory := protectedDirectory(t)
		writeHostFiles(t, directory, fixture.source.bundle)
		path := filepath.Join(directory, HostKeyringFile)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data[:len(data)-1], []byte(`,"unknown":true}`)...)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		source, err := NewFileHostAuthoritySource(directory)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = source.Close() })
		if _, err := source.Load(context.Background()); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("Load unknown field = %v, want ErrUnavailable", err)
		}
	})

	t.Run("oversized keyring", func(t *testing.T) {
		directory := protectedDirectory(t)
		writeHostFiles(t, directory, fixture.source.bundle)
		path := filepath.Join(directory, HostKeyringFile)
		if err := os.WriteFile(path, make([]byte, maxHostKeyringBytes+1), 0o600); err != nil {
			t.Fatal(err)
		}
		source, err := NewFileHostAuthoritySource(directory)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = source.Close() })
		if _, err := source.Load(context.Background()); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("Load oversized keyring = %v, want ErrUnavailable", err)
		}
	})
}

func protectedDirectory(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func writeHostFiles(t *testing.T, directory string, bundle Bundle) {
	t.Helper()
	keyring, err := json.Marshal(hostKeyringFile{SchemaVersion: HostAuthoritySchemaV1, Keys: bundle.Keys})
	if err != nil {
		t.Fatal(err)
	}
	revocations, err := json.Marshal(bundle.Revocations)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		HostKeyringFile:     keyring,
		HostRevocationsFile: revocations,
	} {
		if err := os.WriteFile(filepath.Join(directory, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
