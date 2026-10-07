//go:build linux

package selectorwriter

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestLinuxIdentityReaderUsesOnlyInjectedRegularFiles(t *testing.T) {
	dir := t.TempDir()
	paths := LinuxIdentityPaths{
		HostID:   filepath.Join(dir, "machine-id"),
		BootID:   filepath.Join(dir, "boot-id"),
		ProcStat: filepath.Join(dir, "proc-stat"),
	}
	writeFixture(t, paths.HostID, "  machine-fixture-id\n")
	writeFixture(t, paths.BootID, "boot-fixture-id\n")
	writeFixture(t, paths.ProcStat, "cpu  1 2 3\nbtime 1790000000\nprocesses 2\n")

	reader, err := NewLinuxIdentityReader(paths)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.ReadIdentity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := HostIdentity{
		HostID: "machine-fixture-id", BootID: "boot-fixture-id",
		BootStartedAt: time.Unix(1790000000, 0).UTC(),
	}
	if got != want {
		t.Fatalf("ReadIdentity() = %#v, want %#v", got, want)
	}
}

func TestLinuxIdentityReaderRejectsSymlinkAndMalformedBootTime(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target")
		link := filepath.Join(dir, "machine-id")
		writeFixture(t, target, "machine-id\n")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		paths := LinuxIdentityPaths{
			HostID: link, BootID: filepath.Join(dir, "boot-id"), ProcStat: filepath.Join(dir, "proc-stat"),
		}
		writeFixture(t, paths.BootID, "boot-id\n")
		writeFixture(t, paths.ProcStat, "btime 1790000000\n")
		reader, err := NewLinuxIdentityReader(paths)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := reader.ReadIdentity(context.Background()); err == nil {
			t.Fatal("ReadIdentity() accepted a symlink")
		}
	})

	t.Run("duplicate boot time", func(t *testing.T) {
		dir := t.TempDir()
		paths := LinuxIdentityPaths{
			HostID:   filepath.Join(dir, "machine-id"),
			BootID:   filepath.Join(dir, "boot-id"),
			ProcStat: filepath.Join(dir, "proc-stat"),
		}
		writeFixture(t, paths.HostID, "machine-id\n")
		writeFixture(t, paths.BootID, "boot-id\n")
		writeFixture(t, paths.ProcStat, "btime 1790000000\nbtime 1790000001\n")
		reader, err := NewLinuxIdentityReader(paths)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := reader.ReadIdentity(context.Background()); err == nil {
			t.Fatal("ReadIdentity() accepted duplicate btime fields")
		}
	})
}

func TestNewLinuxIdentityReaderRequiresAbsoluteDistinctInjectedPaths(t *testing.T) {
	if _, err := NewLinuxIdentityReader(LinuxIdentityPaths{HostID: "machine-id", BootID: "/boot", ProcStat: "/stat"}); err == nil {
		t.Fatal("constructor accepted a relative host-id path")
	}
	if _, err := NewLinuxIdentityReader(LinuxIdentityPaths{HostID: "/same", BootID: "/same", ProcStat: "/stat"}); err == nil {
		t.Fatal("constructor accepted a path reused for two identities")
	}
}

func TestLinuxIdentityPathRevalidationDetectsFileAndAncestorReplacement(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "machine-id")
		writeFixture(t, path, "first-id\n")
		opened, err := openLinuxIdentityPath(path)
		if err != nil {
			t.Fatal(err)
		}
		defer opened.close()
		if _, err := opened.read(); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path, filepath.Join(dir, "old-machine-id")); err != nil {
			t.Fatal(err)
		}
		writeFixture(t, path, "replacement-id\n")
		if err := opened.revalidate(); err == nil {
			t.Fatal("identity path validation accepted replacement file")
		}
	})

	t.Run("ancestor", func(t *testing.T) {
		dir := t.TempDir()
		ancestor := filepath.Join(dir, "identity")
		if err := os.Mkdir(ancestor, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(ancestor, "machine-id")
		writeFixture(t, path, "first-id\n")
		opened, err := openLinuxIdentityPath(path)
		if err != nil {
			t.Fatal(err)
		}
		defer opened.close()
		backup := filepath.Join(dir, "old-identity")
		if err := os.Rename(ancestor, backup); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(ancestor, 0o700); err != nil {
			t.Fatal(err)
		}
		writeFixture(t, path, "replacement-id\n")
		if err := opened.revalidate(); err == nil {
			t.Fatal("identity path validation accepted replacement ancestor")
		}
	})
}

func TestLinuxIdentityReaderRejectsPhysicalAliasesAcrossIdentityInputs(t *testing.T) {
	dir := t.TempDir()
	paths := LinuxIdentityPaths{
		HostID:   filepath.Join(dir, "machine-id"),
		BootID:   filepath.Join(dir, "boot-id"),
		ProcStat: filepath.Join(dir, "proc-stat"),
	}
	writeFixture(t, paths.HostID, "shared-identity\n")
	if err := os.Link(paths.HostID, paths.BootID); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, paths.ProcStat, "btime 1790000000\n")
	reader, err := NewLinuxIdentityReader(paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadIdentity(context.Background()); err == nil {
		t.Fatal("ReadIdentity() accepted two logical identities backed by one physical file")
	}
}

func writeFixture(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxIdentityPathRevalidationAllowsUnrelatedDirectoryEntries(t *testing.T) {
	for _, kind := range []string{"file", "directory"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "machine-id")
			writeFixture(t, path, "stable-identity\n")
			opened, err := openLinuxIdentityPath(path)
			if err != nil {
				t.Fatal(err)
			}
			defer opened.close()
			if _, err := opened.read(); err != nil {
				t.Fatal(err)
			}
			sibling := filepath.Join(dir, "unrelated-entry")
			if kind == "directory" {
				if err := os.Mkdir(sibling, 0o700); err != nil {
					t.Fatal(err)
				}
			} else {
				writeFixture(t, sibling, "unrelated-data\n")
			}
			if err := opened.revalidate(); err != nil {
				t.Fatalf("unchanged identity rejected after unrelated %s creation: %v", kind, err)
			}
			if err := os.Remove(sibling); err != nil {
				t.Fatal(err)
			}
			if err := opened.revalidate(); err != nil {
				t.Fatalf("unchanged identity rejected after unrelated %s removal: %v", kind, err)
			}
		})
	}
}

func TestLinuxIdentityPathRevalidationStillRejectsContentAndAncestorModeChanges(t *testing.T) {
	for _, kind := range []string{"content", "ancestor mode"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "machine-id")
			writeFixture(t, path, "stable-identity\n")
			opened, err := openLinuxIdentityPath(path)
			if err != nil {
				t.Fatal(err)
			}
			defer opened.close()
			if _, err := opened.read(); err != nil {
				t.Fatal(err)
			}
			if kind == "content" {
				writeFixture(t, path, "different-length-identity\n")
			} else {
				if err := os.Chmod(dir, 0o500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Chmod(dir, 0o700); err != nil {
						t.Error(err)
					}
				})
			}
			if err := opened.revalidate(); err == nil {
				t.Fatalf("accepted changed %s", kind)
			}
		})
	}
}

func TestLinuxIdentityPathComponentComparisonPreservesObjectAndAccessMetadata(t *testing.T) {
	original := unix.Stat_t{Dev: 1, Ino: 2, Mode: unix.S_IFDIR | 0o700, Nlink: 2, Rdev: 3, Size: 4, Uid: 5, Gid: 6}
	original.Mtim.Sec = 7
	original.Ctim.Sec = 8
	cases := []struct {
		name   string
		change func(*unix.Stat_t)
		want   bool
	}{
		{"unchanged", func(*unix.Stat_t) {}, true},
		{"directory entries", func(s *unix.Stat_t) { s.Nlink++; s.Size++; s.Mtim.Sec++; s.Ctim.Sec++ }, true},
		{"device", func(s *unix.Stat_t) { s.Dev++ }, false},
		{"inode", func(s *unix.Stat_t) { s.Ino++ }, false},
		{"permissions", func(s *unix.Stat_t) { s.Mode ^= 0o100 }, false},
		{"type", func(s *unix.Stat_t) { s.Mode = unix.S_IFREG | 0o700 }, false},
		{"special device", func(s *unix.Stat_t) { s.Rdev++ }, false},
		{"owner", func(s *unix.Stat_t) { s.Uid++ }, false},
		{"group", func(s *unix.Stat_t) { s.Gid++ }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			changed := original
			tc.change(&changed)
			if got := sameLinuxPathComponent(original, changed); got != tc.want {
				t.Fatalf("forward comparison = %v, want %v", got, tc.want)
			}
			if got := sameLinuxPathComponent(changed, original); got != tc.want {
				t.Fatalf("reverse comparison = %v, want %v", got, tc.want)
			}
		})
	}
	t.Run("regular file retains full version", func(t *testing.T) {
		file := original
		file.Mode = unix.S_IFREG | 0o600
		for _, mutate := range []func(*unix.Stat_t){func(s *unix.Stat_t) { s.Nlink++ }, func(s *unix.Stat_t) { s.Size++ }, func(s *unix.Stat_t) { s.Mtim.Sec++ }, func(s *unix.Stat_t) { s.Ctim.Sec++ }} {
			changed := file
			mutate(&changed)
			if sameLinuxPathComponent(file, changed) {
				t.Fatal("accepted modified regular-file version")
			}
		}
	})
}
