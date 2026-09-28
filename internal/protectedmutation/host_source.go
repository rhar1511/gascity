package protectedmutation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	// HostAuthoritySchemaV1 identifies the protected-mutation host keyring.
	HostAuthoritySchemaV1 = 1
	// HostKeyringFile is intentionally separate from every other authority's
	// keyring and cannot reuse compatibility or selector roles.
	HostKeyringFile = "protected-mutation-keyring.json"
	// HostRevocationsFile contains the current signed protected-mutation
	// revocation snapshot.
	HostRevocationsFile = "protected-mutation-revocations.json"

	maxHostKeyringBytes = 1 << 20
)

type hostKeyringFile struct {
	SchemaVersion int          `json:"schema_version"`
	Keys          []TrustedKey `json:"keys"`
}

// FileHostAuthoritySource reads a dedicated host-owned trust directory. The
// directory must be selected by trusted controller startup code; this package
// does not read an environment variable or register the source anywhere. Mode
// checks are defense in depth; deployment must also keep the mount and its
// parents unavailable to workers that share the controller's OS identity.
type FileHostAuthoritySource struct {
	directory string
	root      *os.Root
	rootInfo  os.FileInfo
}

var _ Source = FileHostAuthoritySource{}

// NewFileHostAuthoritySource pins one absolute, protected, non-symlinked host
// directory. Worker, provider, city, pack, and request data must not choose it.
func NewFileHostAuthoritySource(directory string) (FileHostAuthoritySource, error) {
	directory = strings.TrimSpace(directory)
	if directory == "" || !filepath.IsAbs(directory) {
		return FileHostAuthoritySource{}, fmt.Errorf("protected mutation authority directory must be absolute: %w", ErrUnavailable)
	}
	info, err := os.Lstat(directory)
	if err != nil || !protectedHostDirectory(info) {
		return FileHostAuthoritySource{}, fmt.Errorf("protected mutation authority directory is not a protected real directory: %w", ErrUnavailable)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return FileHostAuthoritySource{}, fmt.Errorf("open protected mutation authority directory: %w", ErrUnavailable)
	}
	rootInfo, err := root.Stat(".")
	if err != nil || !protectedHostDirectory(rootInfo) || !os.SameFile(info, rootInfo) {
		closeErr := root.Close()
		return FileHostAuthoritySource{}, errors.Join(
			fmt.Errorf("protected mutation authority directory changed while opening: %w", ErrUnavailable),
			closeErr,
		)
	}
	return FileHostAuthoritySource{directory: filepath.Clean(directory), root: root, rootInfo: rootInfo}, nil
}

// Load rereads the protected keyring and signed revocation snapshot. Missing,
// writable, symlinked, replaced, changing, oversized, or non-strict files make
// the authority unavailable; there is no fallback source.
func (s FileHostAuthoritySource) Load(ctx context.Context) (Bundle, error) {
	if s.directory == "" || s.root == nil || s.rootInfo == nil {
		return Bundle{}, ErrUnavailable
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return Bundle{}, err
		}
	}
	if err := s.verifyRootIdentity(); err != nil {
		return Bundle{}, err
	}
	var keyring hostKeyringFile
	if err := readProtectedHostJSON(s.root, HostKeyringFile, maxHostKeyringBytes, &keyring); err != nil {
		return Bundle{}, fmt.Errorf("read protected mutation keyring: %w: %v", ErrUnavailable, err)
	}
	if keyring.SchemaVersion != HostAuthoritySchemaV1 {
		return Bundle{}, fmt.Errorf("protected mutation keyring schema is unsupported: %w", ErrUnavailable)
	}
	var revocations SignedRevocations
	if err := readProtectedHostJSON(s.root, HostRevocationsFile, maxRevocationBytes, &revocations); err != nil {
		return Bundle{}, fmt.Errorf("read protected mutation revocations: %w: %v", ErrUnavailable, err)
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return Bundle{}, err
		}
	}
	return Bundle{Keys: keyring.Keys, Revocations: revocations}, nil
}

// Close releases the pinned directory handle.
func (s FileHostAuthoritySource) Close() error {
	if s.root == nil {
		return nil
	}
	return s.root.Close()
}

func (s FileHostAuthoritySource) verifyRootIdentity() error {
	pathInfo, err := os.Lstat(s.directory)
	if err != nil || !protectedHostDirectory(pathInfo) || !os.SameFile(s.rootInfo, pathInfo) {
		return fmt.Errorf("protected mutation authority root path changed: %w", ErrUnavailable)
	}
	openedInfo, err := s.root.Stat(".")
	if err != nil || !protectedHostDirectory(openedInfo) || !os.SameFile(s.rootInfo, openedInfo) {
		return fmt.Errorf("protected mutation authority root handle changed: %w", ErrUnavailable)
	}
	return nil
}

func protectedHostDirectory(info os.FileInfo) bool {
	return info != nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0o022 == 0
}

func readProtectedHostJSON(root *os.Root, name string, limit int64, destination any) error {
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !protectedHostFile(info, limit) {
		return errors.New("authority file is not a protected regular file")
	}
	file, err := root.Open(name)
	if err != nil {
		return err
	}
	openedInfo, err := file.Stat()
	if err != nil {
		return closeProtectedHostFile(file, err)
	}
	if !protectedHostFile(openedInfo, limit) || !os.SameFile(info, openedInfo) {
		return closeProtectedHostFile(file, errors.New("authority file changed while opening"))
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return closeProtectedHostFile(file, fmt.Errorf("read authority file: %w", err))
	}
	if int64(len(data)) > limit {
		return closeProtectedHostFile(file, errors.New("authority file exceeded size limit"))
	}
	readInfo, err := file.Stat()
	if err != nil {
		return closeProtectedHostFile(file, fmt.Errorf("stat authority file after reading: %w", err))
	}
	if !protectedHostFile(readInfo, limit) || !os.SameFile(openedInfo, readInfo) ||
		openedInfo.Size() != readInfo.Size() || !openedInfo.ModTime().Equal(readInfo.ModTime()) {
		return closeProtectedHostFile(file, errors.New("authority file changed while reading"))
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close authority file: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("authority file has trailing JSON")
		}
		return err
	}
	return nil
}

func closeProtectedHostFile(file *os.File, operationErr error) error {
	return errors.Join(operationErr, file.Close())
}

func protectedHostFile(info os.FileInfo, limit int64) bool {
	return info != nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 &&
		info.Mode().Perm()&0o022 == 0 && info.Size() >= 1 && info.Size() <= limit
}
