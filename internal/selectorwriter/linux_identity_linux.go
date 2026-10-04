//go:build linux

package selectorwriter

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	maxLinuxIdentityFileBytes      = 64 << 10
	maxLinuxIdentityPathComponents = 64
)

// LinuxIdentityPaths contains explicit inputs selected by trusted startup
// code. This adapter has no default host paths and performs no directory scan.
type LinuxIdentityPaths struct {
	HostID   string
	BootID   string
	ProcStat string
}

// LinuxIdentityReader reads a caller-selected stable host ID, the kernel boot
// ID, and the current boot start time. Every path must be absolute and is read
// without following symlinks. The Collector reads this adapter before and
// after its source snapshots to detect a reboot during collection.
type LinuxIdentityReader struct{ paths LinuxIdentityPaths }

// NewLinuxIdentityReader constructs an adapter from explicit paths. It does
// not probe the host while constructing the reader.
func NewLinuxIdentityReader(paths LinuxIdentityPaths) (*LinuxIdentityReader, error) {
	values := []string{paths.HostID, paths.BootID, paths.ProcStat}
	seen := make(map[string]struct{}, len(values))
	for _, path := range values {
		if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return nil, errors.New("selectorwriter: invalid Linux identity path")
		}
		if _, ok := seen[path]; ok {
			return nil, errors.New("selectorwriter: repeated Linux identity path")
		}
		seen[path] = struct{}{}
	}
	return &LinuxIdentityReader{paths: paths}, nil
}

// ReadIdentity implements IdentityReader. Any missing, inaccessible,
// symlinked, changing, or malformed input fails closed.
func (r *LinuxIdentityReader) ReadIdentity(ctx context.Context) (HostIdentity, error) {
	if ctx == nil || r == nil {
		return HostIdentity{}, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return HostIdentity{}, ErrUnavailable
	}
	paths := make([]*linuxOpenedIdentityPath, 0, 3)
	seenInputs := make(map[linuxIdentityObjectID]struct{}, 3)
	defer func() {
		for _, opened := range paths {
			opened.close()
		}
	}()
	for _, path := range []string{r.paths.HostID, r.paths.BootID, r.paths.ProcStat} {
		opened, err := openLinuxIdentityPath(path)
		if err != nil {
			return HostIdentity{}, ErrUnavailable
		}
		var info unix.Stat_t
		if statErr := unix.Fstat(opened.fd, &info); statErr != nil {
			opened.close()
			return HostIdentity{}, ErrUnavailable
		}
		identity := linuxIdentityObjectID{device: info.Dev, inode: info.Ino}
		if _, duplicate := seenInputs[identity]; duplicate {
			opened.close()
			return HostIdentity{}, ErrUnavailable
		}
		seenInputs[identity] = struct{}{}
		paths = append(paths, opened)
	}
	hostID, err := paths[0].read()
	if err != nil {
		return HostIdentity{}, ErrUnavailable
	}
	bootID, err := paths[1].read()
	if err != nil {
		return HostIdentity{}, ErrUnavailable
	}
	procStat, err := paths[2].read()
	if err != nil {
		return HostIdentity{}, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return HostIdentity{}, ErrUnavailable
	}
	for _, opened := range paths {
		if err := opened.revalidate(); err != nil {
			return HostIdentity{}, ErrUnavailable
		}
	}
	bootStartedAt, err := parseLinuxBootStartedAt(procStat)
	if err != nil {
		return HostIdentity{}, ErrUnavailable
	}
	identity := HostIdentity{
		HostID: strings.TrimSpace(string(hostID)), BootID: strings.TrimSpace(string(bootID)),
		BootStartedAt: bootStartedAt,
	}
	if !validIdentity(identity) {
		return HostIdentity{}, ErrUnavailable
	}
	return identity, nil
}

type linuxIdentityPathComponent struct {
	path string
	info unix.Stat_t
}

type linuxIdentityObjectID struct {
	device uint64
	inode  uint64
}

type linuxOpenedIdentityPath struct {
	path       string
	fd         int
	components []linuxIdentityPathComponent
}

func openLinuxIdentityPath(path string) (*linuxOpenedIdentityPath, error) {
	if path == "" || path == "/" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("invalid absolute identity path")
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) == 0 || len(parts) > maxLinuxIdentityPathComponents {
		return nil, errors.New("identity path component limit exceeded")
	}
	fd, err := unix.Open("/", unix.O_PATH|unix.O_CLOEXEC|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	var rootInfo unix.Stat_t
	if err := unix.Fstat(fd, &rootInfo); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	opened := &linuxOpenedIdentityPath{
		path:       path,
		fd:         fd,
		components: []linuxIdentityPathComponent{{path: "/", info: rootInfo}},
	}
	currentPath := ""
	for index, part := range parts {
		if part == "" || part == "." || part == ".." {
			opened.close()
			return nil, errors.New("invalid identity path component")
		}
		next, openErr := unix.Openat(opened.fd, part, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if openErr != nil {
			opened.close()
			if errors.Is(openErr, unix.ELOOP) {
				return nil, errLinuxIdentitySymlink
			}
			return nil, openErr
		}
		var info, namedInfo unix.Stat_t
		if statErr := unix.Fstat(next, &info); statErr != nil {
			_ = unix.Close(next)
			opened.close()
			return nil, statErr
		}
		if info.Mode&unix.S_IFMT == unix.S_IFLNK {
			_ = unix.Close(next)
			opened.close()
			return nil, errLinuxIdentitySymlink
		}
		if statErr := unix.Fstatat(opened.fd, part, &namedInfo, unix.AT_SYMLINK_NOFOLLOW); statErr != nil || !sameLinuxFileVersion(info, namedInfo) {
			_ = unix.Close(next)
			opened.close()
			return nil, ErrUnavailable
		}
		if index < len(parts)-1 && info.Mode&unix.S_IFMT != unix.S_IFDIR {
			_ = unix.Close(next)
			opened.close()
			return nil, unix.ENOTDIR
		}
		_ = unix.Close(opened.fd)
		opened.fd = next
		if currentPath == "" {
			currentPath = "/" + part
		} else {
			currentPath += "/" + part
		}
		opened.components = append(opened.components, linuxIdentityPathComponent{path: currentPath, info: info})
	}
	var finalInfo unix.Stat_t
	if err := unix.Fstat(opened.fd, &finalInfo); err != nil || finalInfo.Mode&unix.S_IFMT != unix.S_IFREG {
		opened.close()
		return nil, errors.New("identity input is not a regular file")
	}
	return opened, nil
}

var errLinuxIdentitySymlink = errors.New("symlink in identity path")

func (p *linuxOpenedIdentityPath) close() {
	if p != nil && p.fd >= 0 {
		_ = unix.Close(p.fd)
		p.fd = -1
	}
}

func (p *linuxOpenedIdentityPath) read() ([]byte, error) {
	if p == nil || p.fd < 0 {
		return nil, ErrUnavailable
	}
	var before unix.Stat_t
	if err := unix.Fstat(p.fd, &before); err != nil || before.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, ErrUnavailable
	}
	fd, err := unix.Open(fmt.Sprintf("/proc/self/fd/%d", p.fd), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrUnavailable
	}
	var openedInfo unix.Stat_t
	if err := unix.Fstat(fd, &openedInfo); err != nil || !sameLinuxFileVersion(before, openedInfo) {
		_ = unix.Close(fd)
		return nil, ErrUnavailable
	}
	dup, err := unix.Dup(fd)
	if err != nil {
		_ = unix.Close(fd)
		return nil, ErrUnavailable
	}
	file := os.NewFile(uintptr(dup), "linux-selectorwriter-identity")
	if file == nil {
		_ = unix.Close(dup)
		_ = unix.Close(fd)
		return nil, ErrUnavailable
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxLinuxIdentityFileBytes+1))
	closeErr := file.Close()
	var after unix.Stat_t
	statErr := unix.Fstat(fd, &after)
	_ = unix.Close(fd)
	if readErr != nil || closeErr != nil || statErr != nil || len(data) == 0 || len(data) > maxLinuxIdentityFileBytes ||
		!sameLinuxFileVersion(before, after) {
		return nil, ErrUnavailable
	}
	return data, nil
}

func (p *linuxOpenedIdentityPath) revalidate() error {
	if p == nil || p.fd < 0 {
		return ErrUnavailable
	}
	var finalInfo unix.Stat_t
	if err := unix.Fstat(p.fd, &finalInfo); err != nil || len(p.components) == 0 || !sameLinuxFileVersion(p.components[len(p.components)-1].info, finalInfo) {
		return ErrUnavailable
	}
	fresh, err := openLinuxIdentityPath(p.path)
	if err != nil {
		return ErrUnavailable
	}
	defer fresh.close()
	if len(fresh.components) != len(p.components) {
		return ErrUnavailable
	}
	for i := range p.components {
		if p.components[i].path != fresh.components[i].path || !sameLinuxFileVersion(p.components[i].info, fresh.components[i].info) {
			return ErrUnavailable
		}
	}
	return nil
}

func sameLinuxFileVersion(left, right unix.Stat_t) bool {
	return left.Dev == right.Dev && left.Ino == right.Ino && left.Mode == right.Mode && left.Nlink == right.Nlink &&
		left.Rdev == right.Rdev && left.Size == right.Size && left.Mtim == right.Mtim && left.Ctim == right.Ctim
}

func parseLinuxBootStartedAt(data []byte) (time.Time, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	var bootTime time.Time
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || fields[0] != "btime" {
			continue
		}
		if len(fields) != 2 || !bootTime.IsZero() {
			return time.Time{}, errors.New("invalid or duplicate btime field")
		}
		seconds, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || seconds <= 0 {
			return time.Time{}, errors.New("invalid btime value")
		}
		bootTime = time.Unix(seconds, 0).UTC()
	}
	if err := scanner.Err(); err != nil {
		return time.Time{}, fmt.Errorf("read proc-stat input: %w", err)
	}
	if bootTime.IsZero() {
		return time.Time{}, errors.New("btime field unavailable")
	}
	return bootTime, nil
}
