package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/qualification"
)

const (
	qualificationMaxInputFiles = 20000
	qualificationMaxInputBytes = 256 << 20
)

type capturedQualificationRoot struct {
	id          string
	kind        string
	path        string
	pin         string
	pinStatus   string
	files       map[string]string
	registered  bool
	unavailable string
}

// qualificationCapture observes the exact successful reads performed by the
// loader and snapshots each resolved pack root before its contents are parsed.
// Conflicting reads or unbound external roots make the closure unavailable.
type qualificationCapture struct {
	mu           sync.Mutex
	fs           fsys.FS
	cityRoot     string
	reads        map[string]string
	totalBytes   int64
	readConflict bool
	roots        map[string]*capturedQualificationRoot
	rootIDs      map[string]string
	aliases      []*capturedQualificationRoot
	issues       []string
	environment  map[string]string
}

type qualificationCaptureFS struct {
	fs      fsys.FS
	capture *qualificationCapture
}

func (f qualificationCaptureFS) MkdirAll(path string, perm os.FileMode) error {
	return f.fs.MkdirAll(path, perm)
}

func (f qualificationCaptureFS) WriteFile(path string, data []byte, perm os.FileMode) error {
	return f.fs.WriteFile(path, data, perm)
}

func (f qualificationCaptureFS) ReadFile(path string) ([]byte, error) {
	data, err := f.fs.ReadFile(path)
	if err == nil {
		f.capture.recordRead(path, data)
	}
	return data, err
}

func (f qualificationCaptureFS) Stat(path string) (os.FileInfo, error) {
	return f.fs.Stat(path)
}

func (f qualificationCaptureFS) Lstat(path string) (os.FileInfo, error) {
	return f.fs.Lstat(path)
}

func (f qualificationCaptureFS) ReadDir(path string) ([]os.DirEntry, error) {
	return f.fs.ReadDir(path)
}

func (f qualificationCaptureFS) Rename(oldpath, newpath string) error {
	return f.fs.Rename(oldpath, newpath)
}
func (f qualificationCaptureFS) Remove(path string) error { return f.fs.Remove(path) }
func (f qualificationCaptureFS) Chmod(path string, mode os.FileMode) error {
	return f.fs.Chmod(path, mode)
}

func newQualificationCapture(fs fsys.FS, cityRoot string) *qualificationCapture {
	implicitPath := implicitImportPath()
	implicitState, implicitPathDigest, implicitPathIssue := qualificationImplicitPathIdentity(implicitPath)
	capture := &qualificationCapture{
		fs:       fs,
		cityRoot: canonicalQualificationPath(cityRoot),
		reads:    make(map[string]string),
		roots:    make(map[string]*capturedQualificationRoot),
		rootIDs:  make(map[string]string),
		environment: map[string]string{
			"implicit_import_file_status":      implicitState,
			"implicit_import_file_path_sha256": implicitPathDigest,
		},
	}
	if implicitPathIssue != "" {
		capture.issues = append(capture.issues, implicitPathIssue)
	}
	if capture.cityRoot == "" {
		capture.issues = append(capture.issues, "city_root_unresolved")
		return capture
	}
	capture.addRoot("city", "city", capture.cityRoot, "", "content", "")
	return capture
}

func (c *qualificationCapture) recordRead(path string, data []byte) {
	canonical := canonicalQualificationPath(path)
	if canonical == "" || !utf8.ValidString(canonical) {
		c.mu.Lock()
		c.issues = append(c.issues, "input_path_unresolved")
		if canonical != "" {
			c.issues = append(c.issues, "input_path_encoding_unsupported")
		}
		c.mu.Unlock()
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	digest := digestBytes(data)
	if prior, ok := c.reads[canonical]; ok {
		if prior != digest {
			c.readConflict = true
			c.issues = append(c.issues, "input_changed_during_load")
		}
		return
	}
	c.totalBytes += int64(len(data))
	if c.totalBytes > qualificationMaxInputBytes {
		c.issues = append(c.issues, "qualification_input_bytes_exceeded")
	}
	c.reads[canonical] = digest
}

func (c *qualificationCapture) recordExternalRead(path string, data []byte) {
	c.recordRead(path, data)
}

func (c *qualificationCapture) recordPackRoot(path, source, pin, pinStatus string) {
	canonical := canonicalQualificationPath(path)
	if canonical == "" {
		c.mu.Lock()
		c.issues = append(c.issues, "pack_root_unresolved")
		c.mu.Unlock()
		return
	}
	unavailable := ""
	if source == "" && pathIsWithin(canonical, c.cityRoot) {
		pinStatus = "content"
	}
	switch pinStatus {
	case "locked":
		if source == "" || !isFullPackCommit(pin) {
			pinStatus = "unbound"
		}
	case "bundled":
		if strings.TrimSpace(pin) == "" {
			pinStatus = "unbound"
		}
	case "content":
		pin = ""
	default:
		pinStatus = "unbound"
	}
	if pinStatus == "unbound" {
		unavailable = "pack_pin_unbound"
		pin = ""
	}
	idSeed := source + "\x00" + pin
	if pinStatus == "content" || source == "" {
		idSeed = "local\x00" + canonical
	} else if pinStatus == "unbound" {
		idSeed = source + "\x00unbound"
	}
	id := "pack:" + shortDigest(idSeed)
	c.mu.Lock()
	if unavailable != "" {
		c.issues = append(c.issues, unavailable)
	}
	if priorPath, exists := c.rootIDs[id]; exists && priorPath != canonical {
		c.issues = append(c.issues, "duplicate_root_id")
		id = id + ":alias:" + shortDigest(canonical)
		c.addRootLocked(id, "pack", canonical, pin, pinStatus, firstNonEmpty(unavailable, "duplicate_root_id"))
	} else {
		c.addRootLocked(id, "pack", canonical, pin, pinStatus, unavailable)
	}
	c.mu.Unlock()
	if source == "" && !pathIsWithin(canonical, c.cityRoot) {
		c.mu.Lock()
		c.issues = append(c.issues, "external_pack_unbound")
		c.mu.Unlock()
	}
	c.mu.Lock()
	root := c.roots[canonical]
	if root == nil || root.registered {
		c.mu.Unlock()
		return
	}
	root.registered = true
	c.mu.Unlock()
	c.snapshotPackTree(root)
}

func (c *qualificationCapture) addRoot(id, kind, path, pin, pinStatus, unavailable string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.addRootLocked(id, kind, path, pin, pinStatus, unavailable)
}

func (c *qualificationCapture) addRootLocked(id, kind, path, pin, pinStatus, unavailable string) {
	if existingPath, exists := c.rootIDs[id]; exists && existingPath != path {
		c.issues = append(c.issues, "duplicate_root_id")
		if existing := c.roots[existingPath]; existing != nil {
			existing.unavailable = "duplicate_root_id"
		}
		return
	}
	c.rootIDs[id] = path
	if existing := c.roots[path]; existing != nil {
		if existing.id != id || existing.pin != pin || existing.pinStatus != pinStatus {
			existing.unavailable = "conflicting_root_binding"
			c.issues = append(c.issues, "conflicting_root_binding")
			aliasID := id
			if aliasID == existing.id {
				aliasID += ":alias:" + shortDigest(path)
			}
			alias := &capturedQualificationRoot{
				id:          aliasID,
				kind:        kind,
				path:        path,
				pin:         pin,
				pinStatus:   pinStatus,
				files:       make(map[string]string, len(existing.files)),
				registered:  existing.registered,
				unavailable: "conflicting_root_binding",
			}
			for rel, digest := range existing.files {
				alias.files[rel] = digest
			}
			c.aliases = append(c.aliases, alias)
			c.rootIDs[aliasID] = path
		}
		return
	}
	c.roots[path] = &capturedQualificationRoot{
		id:          id,
		kind:        kind,
		path:        path,
		pin:         pin,
		pinStatus:   pinStatus,
		files:       make(map[string]string),
		unavailable: unavailable,
	}
}

func (c *qualificationCapture) snapshotPackTree(root *capturedQualificationRoot) {
	if root == nil {
		return
	}
	count, total := 0, int64(0)
	var walk func(string) error
	walk = func(dir string) error {
		entries, err := c.fs.ReadDir(dir)
		if err != nil {
			return err
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			if entry.Name() == ".git" {
				continue
			}
			if entry.IsDir() && unclassifiedQualificationDir(entry.Name()) {
				c.mu.Lock()
				root.unavailable = "pack_input_directory_unclassified"
				c.issues = append(c.issues, "pack_input_directory_unclassified")
				c.mu.Unlock()
				continue
			}
			path := filepath.Join(dir, entry.Name())
			info, err := c.fs.Lstat(path)
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("symlinked pack input %q", filepath.Base(path))
			}
			if info.IsDir() {
				if err := walk(path); err != nil {
					return err
				}
				continue
			}
			if !info.Mode().IsRegular() {
				continue
			}
			count++
			total += info.Size()
			if count > qualificationMaxInputFiles || total > qualificationMaxInputBytes {
				return fmt.Errorf("pack input snapshot exceeds limits")
			}
			data, err := c.fs.ReadFile(path)
			if err != nil {
				return err
			}
			c.recordRead(path, data)
			rel, err := filepath.Rel(root.path, canonicalQualificationPath(path))
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("pack input escapes resolved root")
			}
			if !utf8.ValidString(rel) {
				return fmt.Errorf("pack input path is not valid UTF-8")
			}
			root.files[filepath.ToSlash(rel)] = digestBytes(data)
		}
		return nil
	}
	if err := walk(root.path); err != nil {
		c.mu.Lock()
		root.unavailable = "pack_input_snapshot_incomplete"
		c.issues = append(c.issues, "pack_input_snapshot_incomplete")
		c.mu.Unlock()
	}
}

func (c *qualificationCapture) closure() qualification.InputClosure {
	c.mu.Lock()
	defer c.mu.Unlock()
	assigned := make(map[string]bool, len(c.reads))
	for path, digest := range c.reads {
		var owner *capturedQualificationRoot
		for _, candidate := range c.allRootsLocked() {
			if pathIsWithin(path, candidate.path) && (owner == nil || len(candidate.path) > len(owner.path)) {
				owner = candidate
			}
		}
		if owner != nil {
			rel, err := filepath.Rel(owner.path, path)
			if err == nil {
				owner.files[filepath.ToSlash(rel)] = digest
				assigned[path] = true
			}
		}
	}
	// Add every successful read outside the city and pinned pack roots as an
	// explicit unbound input. It remains visible by identity but cannot qualify.
	for path, digest := range c.reads {
		if assigned[path] {
			continue
		}
		parent := filepath.Dir(path)
		id := "external:" + shortDigest(parent)
		root := c.roots[parent]
		if root == nil {
			root = &capturedQualificationRoot{id: id, kind: "external", path: parent, pinStatus: "unbound", files: make(map[string]string), unavailable: "unbound_external_input"}
			c.roots[parent] = root
			if priorPath, exists := c.rootIDs[id]; exists && priorPath != parent {
				c.issues = append(c.issues, "duplicate_root_id")
				root.id += ":alias:" + shortDigest(parent)
			}
			c.rootIDs[root.id] = parent
		}
		rel, err := filepath.Rel(parent, path)
		if err == nil {
			root.files[filepath.ToSlash(rel)] = digest
		}
	}
	if len(c.reads) == 0 {
		c.issues = append(c.issues, "loader_inputs_not_captured")
	}
	for _, root := range c.allRootsLocked() {
		if root.kind == "external" || root.unavailable != "" || root.pinStatus == "unbound" {
			if root.unavailable == "" {
				root.unavailable = "input_root_unbound"
			}
			c.issues = append(c.issues, root.unavailable)
		}
	}
	if c.readConflict {
		c.issues = append(c.issues, "input_changed_during_load")
	}
	roots := make([]qualification.InputRoot, 0, len(c.roots))
	for _, captured := range c.allRootsLocked() {
		roots = append(roots, qualification.InputRoot{
			ID:                 captured.id,
			Kind:               captured.kind,
			Pin:                captured.pin,
			PinStatus:          captured.pinStatus,
			ResolvedPathSHA256: qualificationPathDigest(captured.path),
			InputsSHA256:       digestInputFiles(captured.files),
			InputCount:         len(captured.files),
			UnavailableReason:  captured.unavailable,
		})
		if !utf8.ValidString(captured.path) || qualificationPathDigest(captured.path) == "" {
			c.issues = append(c.issues, "input_root_path_encoding_unsupported")
		}
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].ID < roots[j].ID })
	closure := qualification.InputClosure{SchemaVersion: qualification.SchemaVersion, Status: qualification.StatusAvailable, Roots: roots}
	environmentDigest, envErr := qualification.DigestJSON(c.environment)
	if envErr == nil {
		closure.EnvironmentSHA = environmentDigest
	}
	digest, err := qualification.InputClosureDigest(closure)
	if err != nil {
		closure.Status = qualification.StatusUnavailable
		closure.Reason = "input_closure_digest_failed"
		return closure
	}
	closure.SHA256 = digest
	if envErr != nil {
		c.issues = append(c.issues, "environment_digest_failed")
	}
	if len(c.issues) > 0 {
		sort.Strings(c.issues)
		closure.Status = qualification.StatusUnavailable
		closure.Reason = c.issues[0]
	}
	return closure
}

func (c *qualificationCapture) allRootsLocked() []*capturedQualificationRoot {
	roots := make([]*capturedQualificationRoot, 0, len(c.roots)+len(c.aliases))
	for _, root := range c.roots {
		roots = append(roots, root)
	}
	return append(roots, c.aliases...)
}

func digestInputFiles(files map[string]string) string {
	type item struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
	}
	items := make([]item, 0, len(files))
	for path, digest := range files {
		items = append(items, item{Path: path, SHA256: digest})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Path < items[j].Path })
	digest, err := qualification.DigestJSON(items)
	if err != nil {
		return ""
	}
	return digest
}

func qualificationPathDigest(path string) string {
	if !utf8.ValidString(path) {
		return ""
	}
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:])
}

// qualificationImplicitPathIdentity binds the resolved path and presence of
// the one environment-derived file that can participate in config loading.
// It returns only a hash of the canonical path, never the path itself.
func qualificationImplicitPathIdentity(path string) (state, pathSHA256, issue string) {
	if path == "" {
		return "disabled", "", ""
	}
	canonical := canonicalQualificationCandidate(path)
	if canonical == "" || !utf8.ValidString(canonical) {
		return "unresolved", "", "implicit_import_path_unresolved"
	}
	pathSHA256 = qualificationPathDigest(canonical)
	if pathSHA256 == "" {
		return "unresolved", "", "implicit_import_path_encoding_unsupported"
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "absent", pathSHA256, ""
	}
	if err != nil {
		return "unresolved", pathSHA256, "implicit_import_presence_unresolved"
	}
	if !info.Mode().IsRegular() {
		return "unsupported", pathSHA256, "implicit_import_not_regular_file"
	}
	return "present", pathSHA256, ""
}

func shortDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:12])
}

func isFullPackCommit(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func canonicalQualificationPath(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return ""
	}
	return filepath.Clean(resolved)
}

// canonicalQualificationCandidate resolves all existing path components and
// appends a missing suffix. This is used only to bind an absent optional input
// path; captured input files and roots use canonicalQualificationPath, which
// requires the final path to exist.
func canonicalQualificationCandidate(path string) string {
	if strings.TrimSpace(path) == "" || !utf8.ValidString(path) {
		return ""
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved)
	}
	if _, err := os.Lstat(abs); err == nil || !errors.Is(err, os.ErrNotExist) {
		// An existing but unresolved final path is usually a dangling or
		// inaccessible symlink. Do not turn it into an ordinary absent path.
		return ""
	}
	missing := []string{filepath.Base(abs)}
	parent := filepath.Dir(abs)
	for {
		if resolved, err := filepath.EvalSymlinks(parent); err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved)
		}
		if _, err := os.Lstat(parent); err == nil || !errors.Is(err, os.ErrNotExist) {
			return ""
		}
		next := filepath.Dir(parent)
		if next == parent {
			return ""
		}
		missing = append(missing, filepath.Base(parent))
		parent = next
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func pathIsWithin(path, root string) bool {
	if path == "" || root == "" {
		return false
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// These common state/cache directories are not assumed irrelevant. If a pack
// ships one, qualification fails until the loader's use is explicitly
// classified; silently skipping it would make the input closure incomplete.
func unclassifiedQualificationDir(name string) bool {
	switch name {
	case ".beads", ".gc", ".cache", "state", "tmp", "node_modules", "__pycache__":
		return true
	default:
		return false
	}
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
