package protectedmutation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFileHostAuthoritySourceReadsProtectedBundle(t *testing.T) {
	fixture := newGrantFixture(t)
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeHostAuthorityFiles(t, directory, fixture.source.bundle)

	source, err := NewFileHostAuthoritySource(directory)
	if err != nil {
		t.Fatalf("NewFileHostAuthoritySource: %v", err)
	}
	t.Cleanup(func() {
		if err := source.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	bundle, err := source.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(bundle.Keys) != 2 || len(bundle.Revocations.Payload) == 0 || bundle.Revocations.Signature == "" {
		t.Fatalf("loaded incomplete bundle: %#v", bundle)
	}

	verifier, err := NewVerifier(source, fixture.verifier.options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(context.Background(), tokenForClaims(t, fixture.claims, fixture.grantPrivate), fixture.want); err != nil {
		t.Fatalf("Verify from file source: %v", err)
	}
}

func TestFileHostAuthoritySourceRejectsUnsafePathsAndFiles(t *testing.T) {
	fixture := newGrantFixture(t)
	if _, err := NewFileHostAuthoritySource("relative/path"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("relative source = %v, want ErrUnavailable", err)
	}

	t.Run("writable keyring", func(t *testing.T) {
		directory := t.TempDir()
		if err := os.Chmod(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		writeHostAuthorityFiles(t, directory, fixture.source.bundle)
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
		writeHostAuthorityFiles(t, directory, fixture.source.bundle)
		outside := filepath.Join(root, "outside.json")
		data, err := os.ReadFile(filepath.Join(directory, HostRevocationsFile))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(outside, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(directory, HostRevocationsFile)); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(directory, HostRevocationsFile)); err != nil {
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
}

func TestFileHostAuthoritySourceRejectsReplacedRootAndNoncanonicalJSON(t *testing.T) {
	fixture := newGrantFixture(t)
	t.Run("replaced root", func(t *testing.T) {
		root := t.TempDir()
		directory := filepath.Join(root, "authority")
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		writeHostAuthorityFiles(t, directory, fixture.source.bundle)
		source, err := NewFileHostAuthoritySource(directory)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = source.Close() })
		moved := directory + ".old"
		if err := os.Rename(directory, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		writeHostAuthorityFiles(t, directory, fixture.source.bundle)
		if _, err := source.Load(context.Background()); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("Load replaced root = %v, want ErrUnavailable", err)
		}
	})

	t.Run("unknown keyring field", func(t *testing.T) {
		directory := t.TempDir()
		if err := os.Chmod(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		writeHostAuthorityFiles(t, directory, fixture.source.bundle)
		path := filepath.Join(directory, HostKeyringFile)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data[:len(data)-1], []byte(`,"unexpected":true}`)...)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		source, err := NewFileHostAuthoritySource(directory)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = source.Close() })
		if _, err := source.Load(context.Background()); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("Load unknown keyring field = %v, want ErrUnavailable", err)
		}
	})
}

func writeHostAuthorityFiles(t *testing.T, directory string, bundle Bundle) {
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
