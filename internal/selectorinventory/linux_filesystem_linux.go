//go:build linux

package selectorinventory

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	maxLinuxFilesystemRoots = 128
	maxLinuxPathComponents  = 64
	maxLinuxTreeDepth       = 32
)

var (
	errLinuxTreeRace  = errors.New("filesystem changed during snapshot")
	errLinuxTreeLimit = errors.New("filesystem snapshot limit exceeded")
	errLinuxTreeAlias = errors.New("physical filesystem object appears more than once")
	errLinuxSymlink   = errors.New("symlink in configured filesystem path")
)

type linuxTreeErrorPhase uint8

const (
	linuxTreeErrorInitialLookup linuxTreeErrorPhase = iota
	linuxTreeErrorAfterObservation
	linuxTreeErrorProcReopen
)

type linuxTreeOperationError struct {
	err   error
	phase linuxTreeErrorPhase
}

func (e *linuxTreeOperationError) Error() string { return e.err.Error() }
func (e *linuxTreeOperationError) Unwrap() error { return e.err }

func linuxTreeOperationFailure(err error, phase linuxTreeErrorPhase) error {
	if err == nil {
		return nil
	}
	var existing *linuxTreeOperationError
	if errors.As(err, &existing) && existing.phase == linuxTreeErrorProcReopen {
		return err
	}
	return &linuxTreeOperationError{err: err, phase: phase}
}

// LinuxFilesystemRoot is one explicitly configured filesystem input. Prefix
// is a caller-selected stable relative name used to keep entries from several
// roots distinct. The reader has no default paths and performs no discovery.
type LinuxFilesystemRoot struct {
	Path   string
	Prefix string
}

// LinuxFilesystemReader implements FilesystemReader using only caller-injected
// roots for the fixed selectorinventory filesystem source IDs. It opens path
// components with O_PATH, classifies nodes before reading them, bounds
// enumeration, and revalidates all paths and entries after the complete tree
// snapshot. Missing mappings remain unsupported so required scopes fail
// closed in Collector. The caller supplies the path manifest; this reader
// cannot prove that the manifest includes every host path in a scope.
type LinuxFilesystemReader struct {
	roots map[string][]LinuxFilesystemRoot
}

// NewLinuxFilesystemReader copies and validates the configured mapping. It
// does not read paths, create files, or inspect the host during construction.
func NewLinuxFilesystemReader(roots map[string][]LinuxFilesystemRoot) (*LinuxFilesystemReader, error) {
	reader := &LinuxFilesystemReader{roots: make(map[string][]LinuxFilesystemRoot, len(roots))}
	for sourceID, configured := range roots {
		if !isFilesystemSourceID(sourceID) || len(configured) == 0 || len(configured) > maxLinuxFilesystemRoots {
			return nil, errors.New("selectorinventory: invalid Linux filesystem source mapping")
		}
		copyRoots := make([]LinuxFilesystemRoot, len(configured))
		for i, root := range configured {
			if root.Path == "" || root.Path == "/" || !filepath.IsAbs(root.Path) || filepath.Clean(root.Path) != root.Path ||
				pathComponentCount(root.Path) > maxLinuxPathComponents || !safeRelativePath(root.Prefix) {
				return nil, errors.New("selectorinventory: invalid Linux filesystem root")
			}
			copyRoots[i] = root
		}
		if err := validateLinuxSourceRoots(copyRoots); err != nil {
			return nil, err
		}
		reader.roots[sourceID] = copyRoots
	}
	return reader, nil
}

// ReadTree returns one bounded no-follow tree snapshot for a fixed source ID.
func (r *LinuxFilesystemReader) ReadTree(sourceID string, limits Limits) TreeInput {
	if r == nil || !isFilesystemSourceID(sourceID) {
		return TreeInput{Status: StatusUnsupported}
	}
	roots, ok := r.roots[sourceID]
	if !ok || len(roots) == 0 {
		return TreeInput{Status: StatusUnsupported}
	}
	if limits.MaxEntries <= 0 || limits.MaxBytes <= 0 {
		return TreeInput{Status: StatusUnavailable}
	}
	entryLimit := min(limits.MaxEntries, maxTreeEntries)
	byteLimit := min(limits.MaxBytes, maxTreeBytes)
	state := linuxTreeBuild{entryLimit: entryLimit, byteLimit: byteLimit}
	defer state.close()
	openedRoots := make([]*linuxOpenedTreePath, 0, len(roots))
	defer func() {
		for _, opened := range openedRoots {
			opened.close()
		}
	}()
	for _, root := range roots {
		opened, err := openLinuxTreePathNoFollow(root.Path)
		if errors.Is(err, errLinuxSymlink) {
			return TreeInput{Status: StatusAvailable, RootSymlink: true, Entries: []TreeEntry{}}
		}
		if err != nil {
			return TreeInput{Status: linuxTreeErrorStatus(err)}
		}
		openedRoots = append(openedRoots, opened)
		var info unix.Stat_t
		if err := unix.Fstat(opened.fd, &info); err != nil {
			return TreeInput{Status: linuxTreeErrorStatus(linuxTreeOperationFailure(err, linuxTreeErrorAfterObservation))}
		}
		if err := state.addPhysicalObject(info); err != nil {
			return TreeInput{Status: linuxTreeErrorStatus(err)}
		}
		var scanErr error
		switch info.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			readable, err := openLinuxTreeReadable(opened.fd, true)
			if err != nil {
				return TreeInput{Status: linuxTreeErrorStatus(err)}
			}
			scanErr = state.readDirectory(readable.fd, opened.fd, "", root.Prefix)
			readable.close()
		case unix.S_IFREG:
			readable, err := openLinuxTreeReadable(opened.fd, false)
			if err != nil {
				return TreeInput{Status: linuxTreeErrorStatus(err)}
			}
			data, after, readErr := readLinuxTreeFile(readable.fd, info, byteLimit-state.usedBytes)
			readable.close()
			if readErr != nil {
				scanErr = readErr
			} else {
				scanErr = state.add(TreeEntry{
					RelativePath: path.Join(root.Prefix, filepath.Base(root.Path)),
					Kind:         EntryRegular, Data: data, Watermark: linuxTreeWatermark(after),
				})
			}
		default:
			scanErr = state.add(TreeEntry{
				RelativePath: path.Join(root.Prefix, filepath.Base(root.Path)),
				Kind:         EntryOther, Watermark: linuxTreeWatermark(info),
			})
		}
		if scanErr != nil {
			return TreeInput{Status: linuxTreeErrorStatus(scanErr)}
		}
	}
	if err := state.revalidate(); err != nil {
		return TreeInput{Status: linuxTreeErrorStatus(err)}
	}
	for _, opened := range openedRoots {
		if err := opened.revalidate(); err != nil {
			return TreeInput{Status: linuxTreeErrorStatus(err)}
		}
	}
	sort.Slice(state.entries, func(i, j int) bool { return state.entries[i].RelativePath < state.entries[j].RelativePath })
	return TreeInput{Status: StatusAvailable, Entries: state.entries}
}

func validateLinuxSourceRoots(roots []LinuxFilesystemRoot) error {
	for i := range roots {
		for j := 0; j < i; j++ {
			if linuxPathsOverlap(roots[i].Path, roots[j].Path) || linuxPrefixesOverlap(roots[i].Prefix, roots[j].Prefix) {
				return errors.New("selectorinventory: overlapping Linux filesystem roots")
			}
		}
	}
	return nil
}

func linuxPathsOverlap(left, right string) bool {
	relative, err := filepath.Rel(left, right)
	if err != nil {
		return true
	}
	if relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
		return true
	}
	relative, err = filepath.Rel(right, left)
	if err != nil {
		return true
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func linuxPrefixesOverlap(left, right string) bool {
	return left == right || strings.HasPrefix(left, right+"/") || strings.HasPrefix(right, left+"/")
}

func pathComponentCount(value string) int {
	if !filepath.IsAbs(value) {
		return maxLinuxPathComponents + 1
	}
	trimmed := strings.TrimPrefix(value, string(filepath.Separator))
	if trimmed == "" {
		return 0
	}
	return len(strings.Split(trimmed, string(filepath.Separator)))
}

func isFilesystemSourceID(sourceID string) bool {
	for _, source := range filesystemSources {
		if source.id == sourceID {
			return true
		}
	}
	return false
}

type linuxTreeObjectIdentity struct {
	device uint64
	inode  uint64
}

type linuxTreePathComponent struct {
	path string
	info unix.Stat_t
}

type linuxOpenedTreePath struct {
	path       string
	fd         int
	components []linuxTreePathComponent
}

func (p *linuxOpenedTreePath) close() {
	if p != nil && p.fd >= 0 {
		_ = unix.Close(p.fd)
		p.fd = -1
	}
}

func (p *linuxOpenedTreePath) revalidate() error {
	if p == nil || p.fd < 0 {
		return errLinuxTreeRace
	}
	var finalInfo unix.Stat_t
	if err := unix.Fstat(p.fd, &finalInfo); err != nil {
		return linuxTreeOperationFailure(err, linuxTreeErrorAfterObservation)
	}
	if len(p.components) == 0 || !sameLinuxTreeVersion(p.components[len(p.components)-1].info, finalInfo) {
		return errLinuxTreeRace
	}
	fresh, err := openLinuxTreePathNoFollow(p.path)
	if err != nil {
		return linuxTreeOperationFailure(err, linuxTreeErrorAfterObservation)
	}
	defer fresh.close()
	if len(fresh.components) != len(p.components) {
		return errLinuxTreeRace
	}
	for i := range p.components {
		if p.components[i].path != fresh.components[i].path || !sameLinuxTreeVersion(p.components[i].info, fresh.components[i].info) {
			return errLinuxTreeRace
		}
	}
	return nil
}

func openLinuxTreeRelative(rootFD int, relative string) (int, unix.Stat_t, error) {
	if !safeRelativePath(relative) {
		return -1, unix.Stat_t{}, errLinuxTreeRace
	}
	var rootInfo unix.Stat_t
	if err := unix.Fstat(rootFD, &rootInfo); err != nil {
		return -1, unix.Stat_t{}, linuxTreeOperationFailure(err, linuxTreeErrorAfterObservation)
	}
	if rootInfo.Mode&unix.S_IFMT != unix.S_IFDIR {
		return -1, unix.Stat_t{}, errLinuxTreeRace
	}
	currentFD := rootFD
	ownedFD := -1
	parts := strings.Split(relative, "/")
	for index, component := range parts {
		next, err := unix.Openat(currentFD, component, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			if ownedFD >= 0 {
				_ = unix.Close(ownedFD)
			}
			return -1, unix.Stat_t{}, linuxTreeOperationFailure(err, linuxTreeErrorAfterObservation)
		}
		var info unix.Stat_t
		if err := unix.Fstat(next, &info); err != nil {
			_ = unix.Close(next)
			if ownedFD >= 0 {
				_ = unix.Close(ownedFD)
			}
			return -1, unix.Stat_t{}, linuxTreeOperationFailure(err, linuxTreeErrorAfterObservation)
		}
		if index < len(parts)-1 && info.Mode&unix.S_IFMT != unix.S_IFDIR {
			_ = unix.Close(next)
			if ownedFD >= 0 {
				_ = unix.Close(ownedFD)
			}
			return -1, unix.Stat_t{}, errLinuxTreeRace
		}
		if ownedFD >= 0 {
			_ = unix.Close(ownedFD)
		}
		ownedFD = next
		currentFD = next
		if index == len(parts)-1 {
			return ownedFD, info, nil
		}
	}
	return -1, unix.Stat_t{}, errLinuxTreeRace
}

func openLinuxTreePathNoFollow(configuredPath string) (*linuxOpenedTreePath, error) {
	if configuredPath == "" || configuredPath == "/" || !filepath.IsAbs(configuredPath) || filepath.Clean(configuredPath) != configuredPath ||
		pathComponentCount(configuredPath) > maxLinuxPathComponents {
		return nil, errors.New("invalid configured path")
	}
	fd, err := unix.Open("/", unix.O_PATH|unix.O_CLOEXEC|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, linuxTreeOperationFailure(err, linuxTreeErrorProcReopen)
	}
	var rootInfo unix.Stat_t
	if err := unix.Fstat(fd, &rootInfo); err != nil {
		_ = unix.Close(fd)
		return nil, linuxTreeOperationFailure(err, linuxTreeErrorAfterObservation)
	}
	opened := &linuxOpenedTreePath{
		path:       configuredPath,
		fd:         fd,
		components: []linuxTreePathComponent{{path: "/", info: rootInfo}},
	}
	parts := strings.Split(strings.TrimPrefix(configuredPath, "/"), "/")
	currentPath := ""
	for index, component := range parts {
		if component == "" || component == "." || component == ".." {
			opened.close()
			return nil, errors.New("invalid configured path component")
		}
		next, openErr := unix.Openat(opened.fd, component, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if openErr != nil {
			opened.close()
			if errors.Is(openErr, unix.ELOOP) {
				return nil, errLinuxSymlink
			}
			return nil, openErr
		}
		var info, namedInfo unix.Stat_t
		if statErr := unix.Fstat(next, &info); statErr != nil {
			_ = unix.Close(next)
			opened.close()
			return nil, linuxTreeOperationFailure(statErr, linuxTreeErrorAfterObservation)
		}
		if info.Mode&unix.S_IFMT == unix.S_IFLNK {
			_ = unix.Close(next)
			opened.close()
			return nil, errLinuxSymlink
		}
		if statErr := unix.Fstatat(opened.fd, component, &namedInfo, unix.AT_SYMLINK_NOFOLLOW); statErr != nil {
			_ = unix.Close(next)
			opened.close()
			return nil, linuxTreeOperationFailure(statErr, linuxTreeErrorAfterObservation)
		}
		if !sameLinuxTreeVersion(info, namedInfo) {
			_ = unix.Close(next)
			opened.close()
			return nil, errLinuxTreeRace
		}
		if index < len(parts)-1 && info.Mode&unix.S_IFMT != unix.S_IFDIR {
			_ = unix.Close(next)
			opened.close()
			return nil, unix.ENOTDIR
		}
		_ = unix.Close(opened.fd)
		opened.fd = next
		if currentPath == "" {
			currentPath = "/" + component
		} else {
			currentPath += "/" + component
		}
		opened.components = append(opened.components, linuxTreePathComponent{path: currentPath, info: info})
	}
	return opened, nil
}

type linuxTreeReadable struct{ fd int }

func (f *linuxTreeReadable) close() {
	if f != nil && f.fd >= 0 {
		_ = unix.Close(f.fd)
		f.fd = -1
	}
}

func openLinuxTreeReadable(pathFD int, directory bool) (*linuxTreeReadable, error) {
	var before unix.Stat_t
	if err := unix.Fstat(pathFD, &before); err != nil {
		return nil, linuxTreeOperationFailure(err, linuxTreeErrorAfterObservation)
	}
	wantMode := uint32(unix.S_IFREG)
	if directory {
		wantMode = unix.S_IFDIR
	}
	if before.Mode&unix.S_IFMT != wantMode {
		return nil, errors.New("refusing to read a non-regular filesystem node")
	}
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NONBLOCK
	if directory {
		flags |= unix.O_DIRECTORY
	}
	procFDPath := fmt.Sprintf("/proc/self/fd/%d", pathFD)
	fd, err := unix.Open(procFDPath, flags, 0)
	if err != nil {
		return nil, linuxTreeOperationFailure(err, linuxTreeErrorProcReopen)
	}
	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil {
		_ = unix.Close(fd)
		return nil, linuxTreeOperationFailure(err, linuxTreeErrorProcReopen)
	}
	if !sameLinuxTreeVersion(before, after) {
		_ = unix.Close(fd)
		return nil, errLinuxTreeRace
	}
	return &linuxTreeReadable{fd: fd}, nil
}

type linuxTreeEntryCheck struct {
	fd       int
	rootFD   int
	relative string
	info     unix.Stat_t
}

type linuxTreeBuild struct {
	// The 512-entry snapshot and 128 configured-root bounds cap retained
	// descriptors at 640. Directory recursion is separately capped at 32;
	// the active directory stack uses at most 65 descriptors and a regular-file
	// read adds at most three. Peak reader-owned use is therefore at most 708;
	// path revalidation uses at most two transient descriptors. Deeper trees
	// fail closed to preserve this bound.
	entries    []TreeEntry
	checks     []linuxTreeEntryCheck
	physical   map[linuxTreeObjectIdentity]struct{}
	entryLimit int
	byteLimit  int
	usedBytes  int
}

func (b *linuxTreeBuild) close() {
	if b == nil {
		return
	}
	for i := range b.checks {
		_ = unix.Close(b.checks[i].fd)
	}
	b.checks = nil
}

func (b *linuxTreeBuild) add(entry TreeEntry) error {
	if len(b.entries) >= b.entryLimit {
		return errLinuxTreeLimit
	}
	if !safeRelativePath(entry.RelativePath) {
		return errors.New("unsafe generated relative path")
	}
	entryBytes := len(entry.RelativePath) + len(entry.Watermark) + len(entry.Data)
	if entryBytes > b.byteLimit-b.usedBytes {
		return errLinuxTreeLimit
	}
	b.usedBytes += entryBytes
	b.entries = append(b.entries, entry)
	return nil
}

func (b *linuxTreeBuild) addCheck(fd, rootFD int, relative string, info unix.Stat_t) error {
	checkFD, err := unix.Dup(fd)
	if err != nil {
		return err
	}
	b.checks = append(b.checks, linuxTreeEntryCheck{fd: checkFD, rootFD: rootFD, relative: relative, info: info})
	return nil
}

func (b *linuxTreeBuild) addPhysicalObject(info unix.Stat_t) error {
	if b.physical == nil {
		b.physical = make(map[linuxTreeObjectIdentity]struct{})
	}
	identity := linuxTreeObjectIdentity{device: info.Dev, inode: info.Ino}
	if _, duplicate := b.physical[identity]; duplicate {
		return errLinuxTreeAlias
	}
	b.physical[identity] = struct{}{}
	return nil
}

func (b *linuxTreeBuild) revalidate() error {
	for _, check := range b.checks {
		var current unix.Stat_t
		if err := unix.Fstat(check.fd, &current); err != nil {
			return linuxTreeOperationFailure(err, linuxTreeErrorAfterObservation)
		}
		if !sameLinuxTreeVersion(check.info, current) {
			return errLinuxTreeRace
		}
		currentPathFD, named, err := openLinuxTreeRelative(check.rootFD, check.relative)
		if err != nil {
			return err
		}
		_ = unix.Close(currentPathFD)
		if !sameLinuxTreeVersion(check.info, named) {
			return errLinuxTreeRace
		}
	}
	return nil
}

func (b *linuxTreeBuild) readDirectory(fd, rootFD int, relativeBase, prefix string) error {
	return b.readDirectoryDepth(fd, rootFD, relativeBase, prefix, 0)
}

func (b *linuxTreeBuild) readDirectoryDepth(fd, rootFD int, relativeBase, prefix string, depth int) error {
	if depth > maxLinuxTreeDepth {
		return errLinuxTreeLimit
	}
	var before, after unix.Stat_t
	if err := unix.Fstat(fd, &before); err != nil {
		return linuxTreeOperationFailure(err, linuxTreeErrorAfterObservation)
	}
	if before.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errLinuxTreeRace
	}
	names, err := readLinuxTreeNames(fd, b.entryLimit-len(b.entries))
	if err != nil {
		return err
	}
	for _, name := range names {
		if name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
			return errLinuxTreeRace
		}
		relative := path.Join(relativeBase, name)
		virtualPath := path.Join(prefix, relative)
		var entryInfo unix.Stat_t
		if err := unix.Fstatat(fd, name, &entryInfo, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return linuxTreeOperationFailure(err, linuxTreeErrorAfterObservation)
		}
		childFD, err := unix.Openat(fd, name, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			if errors.Is(err, unix.ELOOP) {
				return errLinuxTreeRace
			}
			return linuxTreeOperationFailure(err, linuxTreeErrorAfterObservation)
		}
		var openedInfo unix.Stat_t
		if err := unix.Fstat(childFD, &openedInfo); err != nil {
			_ = unix.Close(childFD)
			return linuxTreeOperationFailure(err, linuxTreeErrorAfterObservation)
		}
		if !sameLinuxTreeVersion(entryInfo, openedInfo) {
			_ = unix.Close(childFD)
			return errLinuxTreeRace
		}
		if err := b.addPhysicalObject(openedInfo); err != nil {
			_ = unix.Close(childFD)
			return err
		}
		kind := entryKindFromMode(openedInfo.Mode)
		if kind == EntryDirectory && depth >= maxLinuxTreeDepth {
			_ = unix.Close(childFD)
			return errLinuxTreeLimit
		}
		if err := b.addCheck(childFD, rootFD, relative, openedInfo); err != nil {
			_ = unix.Close(childFD)
			return err
		}
		switch kind {
		case EntryDirectory:
			if err := b.add(TreeEntry{RelativePath: virtualPath, Kind: kind, Watermark: linuxTreeWatermark(openedInfo)}); err != nil {
				_ = unix.Close(childFD)
				return err
			}
			readable, err := openLinuxTreeReadable(childFD, true)
			if err != nil {
				_ = unix.Close(childFD)
				return err
			}
			err = b.readDirectoryDepth(readable.fd, rootFD, relative, prefix, depth+1)
			readable.close()
			_ = unix.Close(childFD)
			if err != nil {
				return err
			}
		case EntryRegular:
			readable, err := openLinuxTreeReadable(childFD, false)
			if err != nil {
				_ = unix.Close(childFD)
				return err
			}
			data, afterInfo, readErr := readLinuxTreeFile(readable.fd, openedInfo, b.byteLimit-b.usedBytes)
			readable.close()
			_ = unix.Close(childFD)
			if readErr != nil {
				return readErr
			}
			if err := b.add(TreeEntry{RelativePath: virtualPath, Kind: kind, Data: data, Watermark: linuxTreeWatermark(afterInfo)}); err != nil {
				return err
			}
		case EntrySymlink, EntryOther:
			if err := b.add(TreeEntry{RelativePath: virtualPath, Kind: kind, Watermark: linuxTreeWatermark(openedInfo)}); err != nil {
				_ = unix.Close(childFD)
				return err
			}
			_ = unix.Close(childFD)
		}
	}
	if err := unix.Fstat(fd, &after); err != nil {
		return linuxTreeOperationFailure(err, linuxTreeErrorAfterObservation)
	}
	if !sameLinuxTreeVersion(before, after) {
		return errLinuxTreeRace
	}
	return nil
}

func readLinuxTreeNames(fd int, remaining int) ([]string, error) {
	if remaining < 0 {
		return nil, errLinuxTreeLimit
	}
	dup, err := unix.Dup(fd)
	if err != nil {
		return nil, err
	}
	directory := os.NewFile(uintptr(dup), "linux-selectorinventory-directory")
	if directory == nil {
		_ = unix.Close(dup)
		return nil, errors.New("invalid directory descriptor")
	}
	names, readErr := readLinuxTreeNamesFrom(directory, remaining)
	closeErr := directory.Close()
	if closeErr != nil {
		return nil, linuxTreeOperationFailure(closeErr, linuxTreeErrorAfterObservation)
	}
	if readErr != nil {
		return nil, linuxTreeOperationFailure(readErr, linuxTreeErrorAfterObservation)
	}
	return names, nil
}

type linuxDirectoryNamesReader interface {
	Readdirnames(int) ([]string, error)
}

func readLinuxTreeNamesFrom(directory linuxDirectoryNamesReader, remaining int) ([]string, error) {
	if directory == nil || remaining < 0 {
		return nil, errLinuxTreeLimit
	}
	request := remaining + 1
	if request > maxTreeEntries+1 {
		request = maxTreeEntries + 1
	}
	names, readErr := directory.Readdirnames(request)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, readErr
	}
	if len(names) > remaining {
		return nil, errLinuxTreeLimit
	}
	sort.Strings(names)
	return names, nil
}

func readLinuxTreeFile(fd int, before unix.Stat_t, remaining int) ([]byte, unix.Stat_t, error) {
	if remaining < 0 {
		return nil, unix.Stat_t{}, errLinuxTreeLimit
	}
	if before.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, unix.Stat_t{}, errors.New("refusing to read a non-regular filesystem node")
	}
	var readableBefore unix.Stat_t
	if err := unix.Fstat(fd, &readableBefore); err != nil {
		return nil, unix.Stat_t{}, linuxTreeOperationFailure(err, linuxTreeErrorAfterObservation)
	}
	if !sameLinuxTreeVersion(before, readableBefore) {
		return nil, unix.Stat_t{}, errLinuxTreeRace
	}
	dup, err := unix.Dup(fd)
	if err != nil {
		return nil, unix.Stat_t{}, err
	}
	file := os.NewFile(uintptr(dup), "linux-selectorinventory-file")
	if file == nil {
		_ = unix.Close(dup)
		return nil, unix.Stat_t{}, errors.New("invalid file descriptor")
	}
	data, err := readLinuxTreeData(file, remaining)
	closeErr := file.Close()
	if err != nil {
		return nil, unix.Stat_t{}, linuxTreeOperationFailure(err, linuxTreeErrorAfterObservation)
	}
	if closeErr != nil {
		return nil, unix.Stat_t{}, linuxTreeOperationFailure(closeErr, linuxTreeErrorAfterObservation)
	}
	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil {
		return nil, unix.Stat_t{}, linuxTreeOperationFailure(err, linuxTreeErrorAfterObservation)
	}
	if !sameLinuxTreeVersion(before, after) {
		return nil, unix.Stat_t{}, errLinuxTreeRace
	}
	return data, after, nil
}

func readLinuxTreeData(reader io.Reader, remaining int) ([]byte, error) {
	if reader == nil || remaining < 0 {
		return nil, errLinuxTreeLimit
	}
	limit := min(remaining, maxSingleFileBytes)
	data, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, errLinuxTreeLimit
	}
	return data, nil
}

func entryKindFromMode(mode uint32) TreeEntryKind {
	switch mode & unix.S_IFMT {
	case unix.S_IFREG:
		return EntryRegular
	case unix.S_IFDIR:
		return EntryDirectory
	case unix.S_IFLNK:
		return EntrySymlink
	default:
		return EntryOther
	}
}

func sameLinuxTreeVersion(left, right unix.Stat_t) bool {
	return left.Dev == right.Dev && left.Ino == right.Ino && left.Mode == right.Mode && left.Uid == right.Uid &&
		left.Gid == right.Gid && left.Nlink == right.Nlink && left.Rdev == right.Rdev && left.Size == right.Size &&
		left.Mtim == right.Mtim && left.Ctim == right.Ctim
}

func linuxTreeWatermark(info unix.Stat_t) string {
	return fmt.Sprintf("%d:%d:%o:%d:%d:%d:%d:%d:%d:%d", info.Dev, info.Ino, info.Mode, info.Uid, info.Gid,
		info.Size, info.Mtim.Sec, info.Mtim.Nsec, info.Ctim.Sec, info.Ctim.Nsec)
}

func linuxTreeErrorStatus(err error) Status {
	var operationErr *linuxTreeOperationError
	phase := linuxTreeErrorInitialLookup
	if errors.As(err, &operationErr) {
		phase = operationErr.phase
	}
	return linuxTreeErrorStatusFor(err, phase)
}

func linuxTreeErrorStatusFor(err error, phase linuxTreeErrorPhase) Status {
	switch {
	case errors.Is(err, errLinuxTreeLimit):
		return StatusParseError
	case errors.Is(err, errLinuxTreeAlias):
		return StatusParseError
	case errors.Is(err, errLinuxTreeRace):
		return StatusRaced
	case phase == linuxTreeErrorProcReopen:
		return StatusUnavailable
	case phase == linuxTreeErrorAfterObservation && (errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, errLinuxSymlink)):
		return StatusRaced
	case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM):
		return StatusPermissionDenied
	case errors.Is(err, unix.EMFILE), errors.Is(err, unix.ENFILE), errors.Is(err, unix.ENOMEM):
		return StatusUnavailable
	case errors.Is(err, unix.ENOENT) && phase == linuxTreeErrorAfterObservation:
		return StatusRaced
	case errors.Is(err, unix.ENOENT) && phase == linuxTreeErrorInitialLookup:
		return StatusAbsent
	default:
		return StatusUnavailable
	}
}
