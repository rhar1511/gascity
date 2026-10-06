package compatibility

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

	"github.com/gastownhall/gascity/internal/qualification"
)

type hostCompatibilityKeyringFile struct {
	SchemaVersion int                          `json:"schema_version"`
	Keys          []HostCompatibilityPublicKey `json:"keys"`
}

type hostCompatibilityRecordsFile struct {
	SchemaVersion int                             `json:"schema_version"`
	Records       []SignedHostCompatibilityRecord `json:"records"`
}

// FileHostCompatibilityAuthoritySource reads a host-mounted trust directory.
// The directory path must come from a trusted supervisor/server startup
// provider. Worker, provider, city, and pack configuration must not select or
// replace this source. The source rereads every file on each authority call so
// signed revocation and expiry changes take effect without stale in-memory
// policy. Mode checks are defense in depth; host deployment must ensure the
// mount and its parent are not writable by workers, including workers running
// under the same OS identity as the server.
type FileHostCompatibilityAuthoritySource struct {
	directory string
	root      *os.Root
	rootInfo  os.FileInfo
}

var _ HostCompatibilityAuthoritySource = FileHostCompatibilityAuthoritySource{}

// NewFileHostCompatibilityAuthoritySource creates a reader for the fixed host
// trust mount. It rejects relative paths and a symlinked/non-directory root.
func NewFileHostCompatibilityAuthoritySource(directory string) (FileHostCompatibilityAuthoritySource, error) {
	directory = strings.TrimSpace(directory)
	if directory == "" || !filepath.IsAbs(directory) {
		return FileHostCompatibilityAuthoritySource{}, fmt.Errorf("host compatibility authority directory must be absolute: %w", qualification.ErrUnavailable)
	}
	info, err := os.Lstat(directory)
	if err != nil || !protectedHostCompatibilityDirectory(info) {
		return FileHostCompatibilityAuthoritySource{}, fmt.Errorf("host compatibility authority directory is not a protected real directory: %w", qualification.ErrUnavailable)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return FileHostCompatibilityAuthoritySource{}, fmt.Errorf("opening host compatibility authority directory: %w", qualification.ErrUnavailable)
	}
	rootInfo, err := root.Stat(".")
	if err != nil || !protectedHostCompatibilityDirectory(rootInfo) || !os.SameFile(info, rootInfo) {
		closeErr := root.Close()
		return FileHostCompatibilityAuthoritySource{}, errors.Join(
			fmt.Errorf("host compatibility authority directory changed while opening: %w", qualification.ErrUnavailable),
			closeErr,
		)
	}
	return FileHostCompatibilityAuthoritySource{directory: filepath.Clean(directory), root: root, rootInfo: rootInfo}, nil
}

// Load reads the dedicated keyring, exact-scope signed records, and current
// signed revocation snapshot from the host mount. Missing or unsafe files are
// unavailable; there is no fallback to city or pack configuration.
func (s FileHostCompatibilityAuthoritySource) Load(ctx context.Context) (HostCompatibilityAuthorityBundle, error) {
	if strings.TrimSpace(s.directory) == "" || s.root == nil || s.rootInfo == nil {
		return HostCompatibilityAuthorityBundle{}, qualification.ErrUnavailable
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return HostCompatibilityAuthorityBundle{}, err
		}
	}
	if err := s.verifyRootIdentity(); err != nil {
		return HostCompatibilityAuthorityBundle{}, err
	}
	var keyring hostCompatibilityKeyringFile
	if err := readHostCompatibilityJSON(s.root, HostCompatibilityKeyringFile, &keyring); err != nil {
		return HostCompatibilityAuthorityBundle{}, fmt.Errorf("read host compatibility keyring: %w", qualification.ErrUnavailable)
	}
	if keyring.SchemaVersion != HostCompatibilityAuthoritySchemaV1 {
		return HostCompatibilityAuthorityBundle{}, fmt.Errorf("host compatibility keyring schema is unsupported: %w", qualification.ErrUnavailable)
	}
	var records hostCompatibilityRecordsFile
	if err := readHostCompatibilityJSON(s.root, HostCompatibilityRecordsFile, &records); err != nil {
		return HostCompatibilityAuthorityBundle{}, fmt.Errorf("read host compatibility records: %w", qualification.ErrUnavailable)
	}
	if records.SchemaVersion != HostCompatibilityAuthoritySchemaV1 {
		return HostCompatibilityAuthorityBundle{}, fmt.Errorf("host compatibility records schema is unsupported: %w", qualification.ErrUnavailable)
	}
	var revocations SignedHostCompatibilityRevocations
	if err := readHostCompatibilityJSON(s.root, HostCompatibilityRevocationsFile, &revocations); err != nil {
		return HostCompatibilityAuthorityBundle{}, fmt.Errorf("read host compatibility revocations: %w", qualification.ErrUnavailable)
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return HostCompatibilityAuthorityBundle{}, err
		}
	}
	return HostCompatibilityAuthorityBundle{Keys: keyring.Keys, Records: records.Records, Revocation: revocations}, nil
}

// Close releases the directory handle pinned when this source was created.
// The host startup owner must check the returned error during shutdown.
func (s FileHostCompatibilityAuthoritySource) Close() error {
	if s.root == nil {
		return nil
	}
	return s.root.Close()
}

func (s FileHostCompatibilityAuthoritySource) verifyRootIdentity() error {
	pathInfo, err := os.Lstat(s.directory)
	if err != nil || !protectedHostCompatibilityDirectory(pathInfo) || !os.SameFile(s.rootInfo, pathInfo) {
		return fmt.Errorf("host compatibility authority root path changed: %w", qualification.ErrUnavailable)
	}
	openedInfo, err := s.root.Stat(".")
	if err != nil || !protectedHostCompatibilityDirectory(openedInfo) || !os.SameFile(s.rootInfo, openedInfo) {
		return fmt.Errorf("host compatibility authority root handle changed: %w", qualification.ErrUnavailable)
	}
	return nil
}

func protectedHostCompatibilityDirectory(info os.FileInfo) bool {
	return info != nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0o022 == 0
}

func readHostCompatibilityJSON(root *os.Root, name string, target any) error {
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !protectedHostCompatibilityFile(info) {
		return fmt.Errorf("host compatibility file is not a protected regular file")
	}
	file, err := root.Open(name)
	if err != nil {
		return err
	}
	openedInfo, err := file.Stat()
	if err != nil {
		return closeHostCompatibilityFile(file, err)
	}
	if !protectedHostCompatibilityFile(openedInfo) || !os.SameFile(info, openedInfo) {
		return closeHostCompatibilityFile(file, fmt.Errorf("host compatibility file changed while opening"))
	}
	data, err := io.ReadAll(io.LimitReader(file, maxHostCompatibilityFileBytes+1))
	if err != nil {
		return closeHostCompatibilityFile(file, fmt.Errorf("read host compatibility file: %w", err))
	}
	if len(data) > maxHostCompatibilityFileBytes {
		return closeHostCompatibilityFile(file, fmt.Errorf("host compatibility file exceeded size limit"))
	}
	readInfo, err := file.Stat()
	if err != nil {
		return closeHostCompatibilityFile(file, fmt.Errorf("stat host compatibility file after reading: %w", err))
	}
	if !os.SameFile(openedInfo, readInfo) || openedInfo.Size() != readInfo.Size() || !openedInfo.ModTime().Equal(readInfo.ModTime()) {
		return closeHostCompatibilityFile(file, fmt.Errorf("host compatibility file changed while reading"))
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close host compatibility file: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("host compatibility file has trailing JSON")
		}
		return err
	}
	return nil
}

func closeHostCompatibilityFile(file *os.File, operationErr error) error {
	return errors.Join(operationErr, file.Close())
}

func protectedHostCompatibilityFile(info os.FileInfo) bool {
	return info != nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0o022 == 0 && info.Size() >= 1 && info.Size() <= maxHostCompatibilityFileBytes
}
