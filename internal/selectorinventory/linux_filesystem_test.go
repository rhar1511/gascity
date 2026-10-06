//go:build linux

package selectorinventory

import (
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLinuxFilesystemReaderUsesInjectedRootsAndDoesNotFollowSymlinks(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "systemd")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	wantData := []byte("[Service]\nExecStart=/opt/agent/dispatch_once.sh\n")
	if err := os.WriteFile(filepath.Join(root, "worker.service"), wantData, 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(dir, "outside.service")
	if err := os.WriteFile(outside, []byte("private target\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked.service")); err != nil {
		t.Fatal(err)
	}
	reader, err := NewLinuxFilesystemReader(map[string][]LinuxFilesystemRoot{
		SourceSystemdSystemUnits: {{Path: root, Prefix: "system"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := reader.ReadTree(SourceSystemdSystemUnits, Limits{MaxEntries: 8, MaxBytes: 4096})
	if input.Status != StatusAvailable || input.RootSymlink {
		t.Fatalf("ReadTree() status/root symlink = %q/%v", input.Status, input.RootSymlink)
	}
	if len(input.Entries) != 2 {
		t.Fatalf("ReadTree() entries = %#v, want regular file and symlink", input.Entries)
	}
	if !reflect.DeepEqual(input.Entries[0], TreeEntry{
		RelativePath: "system/linked.service", Kind: EntrySymlink,
		Watermark: input.Entries[0].Watermark,
	}) || input.Entries[0].Watermark == "" {
		t.Fatalf("symlink entry = %#v", input.Entries[0])
	}
	if input.Entries[1].RelativePath != "system/worker.service" || input.Entries[1].Kind != EntryRegular ||
		!reflect.DeepEqual(input.Entries[1].Data, wantData) || input.Entries[1].Watermark == "" {
		t.Fatalf("regular entry = %#v", input.Entries[1])
	}
	for _, entry := range input.Entries {
		if string(entry.Data) == "private target\n" {
			t.Fatal("reader followed a symlink and copied its target")
		}
	}
}

func TestLinuxFilesystemReaderFailsClosedForUnknownMissingAndOversizedScopes(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "cron")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "one"), []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "two"), []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := NewLinuxFilesystemReader(map[string][]LinuxFilesystemRoot{
		SourceSystemCronDirectory: {{Path: root, Prefix: "cron"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := reader.ReadTree(SourceSystemdSystemUnits, Limits{MaxEntries: 2, MaxBytes: 100}); got.Status != StatusUnsupported {
		t.Fatalf("unconfigured fixed source status = %q, want unsupported", got.Status)
	}
	if got := reader.ReadTree("unrecognized.source", Limits{MaxEntries: 2, MaxBytes: 100}); got.Status != StatusUnsupported {
		t.Fatalf("unknown source status = %q, want unsupported", got.Status)
	}
	if got := reader.ReadTree(SourceSystemCronDirectory, Limits{MaxEntries: 1, MaxBytes: 100}); got.Status != StatusParseError {
		t.Fatalf("entry overflow status = %q, want parse error", got.Status)
	}
	if got := reader.ReadTree(SourceSystemCronDirectory, Limits{MaxEntries: 2, MaxBytes: 4}); got.Status != StatusParseError {
		t.Fatalf("byte overflow status = %q, want parse error", got.Status)
	}
}

func TestLinuxFilesystemReaderRejectsRootSymlinkAndInvalidConfiguration(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "root-link")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	reader, err := NewLinuxFilesystemReader(map[string][]LinuxFilesystemRoot{
		SourceSystemCronDirectory: {{Path: link, Prefix: "cron"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := reader.ReadTree(SourceSystemCronDirectory, Limits{MaxEntries: 2, MaxBytes: 100})
	if input.Status != StatusAvailable || !input.RootSymlink || len(input.Entries) != 0 {
		t.Fatalf("root symlink result = %#v", input)
	}
	if _, err := NewLinuxFilesystemReader(map[string][]LinuxFilesystemRoot{
		"unrecognized.source": {{Path: target, Prefix: "cron"}},
	}); err == nil {
		t.Fatal("constructor accepted an unknown source ID")
	}
	if _, err := NewLinuxFilesystemReader(map[string][]LinuxFilesystemRoot{
		SourceSystemCronDirectory: {{Path: "relative/path", Prefix: "cron"}},
	}); err == nil {
		t.Fatal("constructor accepted a relative path")
	}
	if _, err := NewLinuxFilesystemReader(map[string][]LinuxFilesystemRoot{
		SourceSystemCronDirectory: {{Path: target, Prefix: "../escape"}},
	}); err == nil {
		t.Fatal("constructor accepted an unsafe relative prefix")
	}
}

func TestLinuxFilesystemReaderRejectsOverlappingRootsAndPrefixes(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "cron")
	child := filepath.Join(parent, "nested")
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		roots []LinuxFilesystemRoot
	}{
		{name: "duplicate physical path", roots: []LinuxFilesystemRoot{{Path: parent, Prefix: "a"}, {Path: parent, Prefix: "b"}}},
		{name: "nested physical path", roots: []LinuxFilesystemRoot{{Path: parent, Prefix: "a"}, {Path: child, Prefix: "b"}}},
		{name: "duplicate virtual prefix", roots: []LinuxFilesystemRoot{{Path: parent, Prefix: "a"}, {Path: child, Prefix: "a"}}},
		{name: "nested virtual prefix", roots: []LinuxFilesystemRoot{{Path: parent, Prefix: "a"}, {Path: child, Prefix: "a/b"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewLinuxFilesystemReader(map[string][]LinuxFilesystemRoot{
				SourceSystemCronDirectory: tc.roots,
			}); err == nil {
				t.Fatal("constructor accepted overlapping source-local roots")
			}
		})
	}
}

func TestLinuxFilesystemReaderRejectsDuplicatePhysicalRootAliases(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")
	if err := os.WriteFile(first, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, second); err != nil {
		t.Fatal(err)
	}
	reader, err := NewLinuxFilesystemReader(map[string][]LinuxFilesystemRoot{
		SourceSystemCronDirectory: {{Path: first, Prefix: "a"}, {Path: second, Prefix: "b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if input := reader.ReadTree(SourceSystemCronDirectory, Limits{MaxEntries: 8, MaxBytes: 4096}); input.Status == StatusAvailable {
		t.Fatalf("reader accepted two names for the same physical root: %#v", input)
	}
}

func TestLinuxFilesystemReaderClassifiesSocketRootWithoutOpeningItForReading(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.socket")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Skipf("Unix sockets unavailable in test environment: %v", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Errorf("close fixture Unix listener: %v", err)
		}
	})
	reader, err := NewLinuxFilesystemReader(map[string][]LinuxFilesystemRoot{
		SourceSystemdSystemUnits: {{Path: path, Prefix: "system"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := reader.ReadTree(SourceSystemdSystemUnits, Limits{MaxEntries: 8, MaxBytes: 4096})
	if input.Status != StatusAvailable || len(input.Entries) != 1 || input.Entries[0].Kind != EntryOther {
		t.Fatalf("socket source result = %#v, want one safely classified non-regular entry", input)
	}
}

func TestLinuxFilesystemReaderClassifiesSocketEntryWithoutOpeningItForReading(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "systemd")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(root, "worker.socket"))
	if err != nil {
		t.Skipf("Unix sockets unavailable in test environment: %v", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Errorf("close fixture Unix listener: %v", err)
		}
	})
	reader, err := NewLinuxFilesystemReader(map[string][]LinuxFilesystemRoot{
		SourceSystemdSystemUnits: {{Path: root, Prefix: "system"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := reader.ReadTree(SourceSystemdSystemUnits, Limits{MaxEntries: 8, MaxBytes: 4096})
	if input.Status != StatusAvailable || len(input.Entries) != 1 || input.Entries[0].Kind != EntryOther {
		t.Fatalf("socket directory entry result = %#v, want one safely classified non-regular entry", input)
	}
}

func TestLinuxFilesystemPathRevalidationDetectsAncestorReplacement(t *testing.T) {
	dir := t.TempDir()
	ancestor := filepath.Join(dir, "source")
	root := filepath.Join(ancestor, "units")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	opened, err := openLinuxTreePathNoFollow(root)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.close()
	backup := filepath.Join(dir, "old-source")
	if err := os.Rename(ancestor, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := opened.revalidate(); err == nil {
		t.Fatal("path validation accepted a replaced ancestor")
	}
}

func TestLinuxFilesystemSnapshotRevalidatesEarlierRegularFilesAtEnd(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "source")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(root, "first")
	if err := os.WriteFile(first, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "later"), []byte("later"), 0o600); err != nil {
		t.Fatal(err)
	}
	opened, err := openLinuxTreePathNoFollow(root)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.close()
	readable, err := openLinuxTreeReadable(opened.fd, true)
	if err != nil {
		t.Fatal(err)
	}
	defer readable.close()
	state := linuxTreeBuild{entryLimit: 8, byteLimit: 4096}
	if err := state.readDirectory(readable.fd, opened.fd, "", "source"); err != nil {
		t.Fatal(err)
	}
	defer state.close()
	if err := os.WriteFile(first, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := state.revalidate(); err == nil {
		t.Fatal("whole-tree validation accepted an earlier file change")
	}
}

func TestLinuxFilesystemDirectoryEnumerationCapsReadAtRemainingBudgetPlusOne(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"one", "two", "three"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	opened, err := openLinuxTreePathNoFollow(root)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.close()
	readable, err := openLinuxTreeReadable(opened.fd, true)
	if err != nil {
		t.Fatal(err)
	}
	defer readable.close()
	names, err := readLinuxTreeNames(readable.fd, 1)
	if err == nil || len(names) > 2 {
		t.Fatalf("bounded read returned %d names and error %v, want at most budget+1 and overflow", len(names), err)
	}
}

type linuxDirectoryNamesFixture struct {
	names     []string
	requested int
}

func (f *linuxDirectoryNamesFixture) Readdirnames(n int) ([]string, error) {
	f.requested = n
	if n < len(f.names) {
		return append([]string(nil), f.names[:n]...), nil
	}
	return append([]string(nil), f.names...), nil
}

func TestLinuxFilesystemNamesReaderRequestsOnlyRemainingPlusOne(t *testing.T) {
	fixture := &linuxDirectoryNamesFixture{names: []string{"a", "b", "c", "d", "e"}}
	_, err := readLinuxTreeNamesFrom(fixture, 2)
	if err == nil || fixture.requested != 3 {
		t.Fatalf("bounded listing requested %d names and returned %v; want exactly budget+1 and overflow", fixture.requested, err)
	}
}

func TestLinuxFilesystemReaderRejectsExcessiveActiveTreeDepth(t *testing.T) {
	root := t.TempDir()
	current := root
	for i := 0; i < 40; i++ {
		current = filepath.Join(current, "d")
		if err := os.Mkdir(current, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	reader, err := NewLinuxFilesystemReader(map[string][]LinuxFilesystemRoot{
		SourceSystemdSystemUnits: {{Path: root, Prefix: "system"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := reader.ReadTree(SourceSystemdSystemUnits, Limits{MaxEntries: 512, MaxBytes: 1 << 20})
	if input.Status != StatusParseError {
		t.Fatalf("deep tree status = %q, want fail-closed depth limit", input.Status)
	}
}

func TestLinuxFilesystemReaderRejectsPhysicalAliasesAcrossRootsAndDescendants(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "tree")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(root, "unit.service")
	if err := os.WriteFile(inside, []byte("[Service]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(dir, "standalone.service")
	if err := os.Link(inside, outside); err != nil {
		t.Fatal(err)
	}
	reader, err := NewLinuxFilesystemReader(map[string][]LinuxFilesystemRoot{
		SourceSystemdSystemUnits: {{Path: root, Prefix: "tree"}, {Path: outside, Prefix: "single"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := reader.ReadTree(SourceSystemdSystemUnits, Limits{MaxEntries: 16, MaxBytes: 4096})
	if input.Status != StatusParseError {
		t.Fatalf("snapshot with one physical file under two names returned %q, want parse error", input.Status)
	}
}

func TestLinuxFilesystemReaderAllowsPhysicalReuseAcrossSourceIDs(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "unit.service"), []byte("[Service]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := NewLinuxFilesystemReader(map[string][]LinuxFilesystemRoot{
		SourceSystemCronDirectory: {{Path: root, Prefix: "cron"}},
		SourceSystemdSystemUnits:  {{Path: root, Prefix: "systemd"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, sourceID := range []string{SourceSystemCronDirectory, SourceSystemdSystemUnits} {
		input := reader.ReadTree(sourceID, Limits{MaxEntries: 8, MaxBytes: 4096})
		if input.Status != StatusAvailable {
			t.Fatalf("source %q returned %q for its own snapshot", sourceID, input.Status)
		}
	}
}

func TestLinuxFilesystemErrorStatusDependsOnOperation(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		phase linuxTreeErrorPhase
		want  Status
	}{
		{name: "initial missing path", err: unix.ENOENT, phase: linuxTreeErrorInitialLookup, want: StatusAbsent},
		{name: "disappeared observed object", err: unix.ENOENT, phase: linuxTreeErrorAfterObservation, want: StatusRaced},
		{name: "proc descriptor reopen unavailable", err: unix.ENOENT, phase: linuxTreeErrorProcReopen, want: StatusUnavailable},
		{name: "proc descriptor permissions unavailable", err: unix.EACCES, phase: linuxTreeErrorProcReopen, want: StatusUnavailable},
		{name: "descriptor exhaustion after observation", err: unix.EMFILE, phase: linuxTreeErrorAfterObservation, want: StatusUnavailable},
		{name: "system descriptor exhaustion", err: unix.ENFILE, phase: linuxTreeErrorAfterObservation, want: StatusUnavailable},
		{name: "memory exhaustion", err: unix.ENOMEM, phase: linuxTreeErrorAfterObservation, want: StatusUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := linuxTreeOperationFailure(tc.err, tc.phase)
			if got := linuxTreeErrorStatus(err); got != tc.want {
				t.Fatalf("linuxTreeErrorStatus(%v, %v) = %q, want %q", tc.err, tc.phase, got, tc.want)
			}
		})
	}
}

func TestLinuxFilesystemRegularReadCapsBytesAtBothLimits(t *testing.T) {
	cases := []struct {
		name      string
		remaining int
		wantRead  int
	}{
		{name: "per-file limit", remaining: maxSingleFileBytes + 100, wantRead: maxSingleFileBytes + 1},
		{name: "remaining tree budget", remaining: 7, wantRead: 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := bytes.NewReader(bytes.Repeat([]byte{'x'}, maxSingleFileBytes*8))
			if _, err := readLinuxTreeData(input, tc.remaining); err == nil {
				t.Fatal("oversized input was accepted")
			} else if !errors.Is(err, errLinuxTreeLimit) {
				t.Fatalf("read error = %v, want tree limit", err)
			}
			if got := maxSingleFileBytes*8 - input.Len(); got != tc.wantRead {
				t.Fatalf("read consumed %d bytes, want exactly limit+1 (%d)", got, tc.wantRead)
			}
		})
	}
}
