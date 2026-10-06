package sessionlog

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/fsys"
)

// OpenedTranscript is a transcript opened beneath a provider search root.
// Path preserves the lexical path supplied by the caller for stable IDs and
// diagnostics; it is not used to reopen the file.
type OpenedTranscript struct {
	file     *os.File
	root     *os.Root
	relative string
	provider string
	path     string
}

// OpenTranscript opens path beneath one of the provider's configured search
// roots. The returned handle owns the descriptor used by Stat and the Read
// methods. Configured roots may be symlinks. Codex and Kimi additionally allow
// their direct, provider-discovered symlinked session roots.
func OpenTranscript(provider string, searchPaths []string, path string) (*OpenedTranscript, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("empty session log path")
	}
	cleanPath := filepath.Clean(path)
	absolutePath, err := filepath.Abs(cleanPath)
	if err != nil {
		return nil, fmt.Errorf("resolving session log path: %w", err)
	}

	var lastErr error
	matchedRoot := false
	for _, rootPath := range providerTranscriptSearchPaths(provider, searchPaths) {
		relative, ok := transcriptPathRelativeToRoot(rootPath, absolutePath)
		if !ok {
			continue
		}
		matchedRoot = true

		root, err := os.OpenRoot(rootPath)
		if err != nil {
			lastErr = err
			continue
		}
		file, err := root.Open(relative)
		if err != nil {
			_ = root.Close()
			lastErr = err
			continue
		}
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			_ = root.Close()
			return nil, fmt.Errorf("checking opened session log: %w", err)
		}
		if info.IsDir() {
			_ = file.Close()
			_ = root.Close()
			return nil, fmt.Errorf("session log path is a directory")
		}
		return &OpenedTranscript{file: file, root: root, relative: relative, provider: provider, path: cleanPath}, nil
	}
	if matchedRoot && lastErr != nil {
		return nil, fmt.Errorf("opening session log beneath configured search paths: %w", lastErr)
	}
	return nil, fmt.Errorf("session log path is outside configured search paths")
}

// Path returns the caller's cleaned lexical transcript path. It does not
// resolve symlinks and remains suitable for transcript and stream identity.
func (t *OpenedTranscript) Path() string {
	if t == nil {
		return ""
	}
	return t.path
}

// Stat describes the already-open transcript descriptor, not a path lookup.
func (t *OpenedTranscript) Stat() (os.FileInfo, error) {
	if t == nil || t.file == nil {
		return nil, os.ErrInvalid
	}
	return t.file.Stat()
}

// Read parses the already-open descriptor as a provider transcript.
func (t *OpenedTranscript) Read(tailCompactions int) (*Session, error) {
	if t == nil || t.file == nil {
		return nil, os.ErrInvalid
	}
	return ReadProviderFileFrom(t.provider, t.path, t.file, tailCompactions)
}

// ReadRaw parses the already-open descriptor without display-type filtering.
func (t *OpenedTranscript) ReadRaw(tailCompactions int) (*Session, error) {
	if t == nil || t.file == nil {
		return nil, os.ErrInvalid
	}
	return ReadProviderFileRawFrom(t.provider, t.path, t.file, tailCompactions)
}

// TailMeta reads provider-specific tail metadata from the opened descriptor.
func (t *OpenedTranscript) TailMeta() (*TailMeta, error) {
	if t == nil || t.file == nil {
		return nil, os.ErrInvalid
	}
	if ProviderFamily(t.provider) == "codex" {
		return ExtractCodexTailMetaFrom(t.file)
	}
	return ExtractTailMetaFrom(t.file)
}

// TailUsage reads the provider-specific usage window from the opened descriptor.
func (t *OpenedTranscript) TailUsage() ([]TailUsage, error) {
	if t == nil || t.file == nil {
		return nil, os.ErrInvalid
	}
	if ProviderFamily(t.provider) == "codex" {
		return ExtractCodexTailUsageFrom(t.file)
	}
	return ExtractTailUsageFrom(t.file)
}

// TailUsageSince reads provider-specific usage from the opened descriptor,
// growing the scan window until cursorID is found or the configured cap hits.
func (t *OpenedTranscript) TailUsageSince(cursorID string) ([]TailUsage, error) {
	if t == nil || t.file == nil {
		return nil, os.ErrInvalid
	}
	if ProviderFamily(t.provider) == "codex" {
		return ExtractCodexTailUsageFrom(t.file)
	}
	return extractTailUsageSinceFrom(t.file, t.path, cursorID, maxUsageScanBytes)
}

// ReadSeeker exposes the opened descriptor to callers that need a provider
// tail parser not covered by Read/ReadRaw. The descriptor remains owned by the
// OpenedTranscript and must not be closed by the caller.
func (t *OpenedTranscript) ReadSeeker() io.ReadSeeker {
	if t == nil {
		return nil
	}
	return t.file
}

// OpenRelative opens another file beneath the same authorized search root.
// It is used for provider-owned sidecar files such as Claude subagent logs.
func (t *OpenedTranscript) OpenRelative(relative string) (*os.File, error) {
	if t == nil || t.root == nil {
		return nil, os.ErrInvalid
	}
	clean := filepath.Clean(strings.TrimSpace(relative))
	if clean == "" || clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("relative transcript path escapes configured search root")
	}
	return t.root.Open(clean)
}

// replaceAtomic confines the replacement to the retained search root. The
// lexical diagnostic path must not regain filesystem authority during reset.
func (t *OpenedTranscript) replaceAtomic(data []byte, perm os.FileMode) error {
	if t == nil || t.root == nil || t.file == nil {
		return os.ErrInvalid
	}
	return fsys.WriteFileAtomic(transcriptRootFS{t.root}, t.relative, data, perm)
}

type transcriptRootFS struct{ *os.Root }

// WriteFile creates only the atomic writer's new temporary file. A preexisting
// temp name must not redirect or overwrite another transcript.
func (r transcriptRootFS) WriteFile(name string, data []byte, perm os.FileMode) error {
	f, err := r.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

func (r transcriptRootFS) ReadDir(name string) ([]os.DirEntry, error) {
	f, err := r.Open(name)
	if err != nil {
		return nil, err
	}
	entries, err := f.ReadDir(-1)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return entries, err
}

// Close releases the opened transcript descriptor.
func (t *OpenedTranscript) Close() error {
	if t == nil || t.file == nil {
		return nil
	}
	err := t.file.Close()
	t.file = nil
	if t.root != nil {
		if rootErr := t.root.Close(); err == nil {
			err = rootErr
		}
		t.root = nil
	}
	return err
}

func transcriptPathRelativeToRoot(rootPath, transcriptPath string) (string, bool) {
	rootAbs, err := filepath.Abs(filepath.Clean(rootPath))
	if err != nil {
		return "", false
	}
	if relative, ok := relativeWithinRoot(rootAbs, transcriptPath); ok {
		return relative, true
	}

	// Discovery may return a physical path after traversing a configured-root
	// alias or a provider-recognized linked account root. Resolve only the
	// candidate's parent for root selection; the file itself is always opened
	// later through Root.Open, which enforces confinement at use time.
	resolvedRoot, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", false
	}
	resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(transcriptPath))
	if err != nil {
		return "", false
	}
	resolvedCandidate := filepath.Join(resolvedParent, filepath.Base(transcriptPath))
	return relativeWithinRoot(filepath.Clean(resolvedRoot), resolvedCandidate)
}

func relativeWithinRoot(root, path string) (string, bool) {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	return relative, true
}

// openProviderFileBeneath shares confinement for discovery-side metadata and
// directory reads. The returned descriptor survives closing the root handle.
func openProviderFileBeneath(rootPath, path string) (*os.File, error) {
	relative, err := filepath.Rel(filepath.Clean(rootPath), filepath.Clean(path))
	if err != nil || relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, os.ErrPermission
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	f, err := root.Open(relative)
	rootErr := root.Close()
	if err != nil {
		return nil, err
	}
	if rootErr != nil {
		_ = f.Close()
		return nil, rootErr
	}
	return f, nil
}
