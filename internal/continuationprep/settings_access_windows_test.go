//go:build windows

package continuationprep

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/sys/windows"
)

// Deny only data-read access on this test-owned file. Keep the control handle
// and original Go-owned descriptor alive so cleanup restores this exact object,
// even if its path changes. No privileges or impersonation are enabled.
func writeUnreadablePreparationSettings(t *testing.T, city string) {
	t.Helper()
	writePreparationSettings(t, city, 0o600)
	path := filepath.Join(city, ".gc", "settings.json")
	if _, err := os.ReadFile(path); err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	control, err := windows.CreateFile(name, windows.READ_CONTROL|windows.WRITE_DAC|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := windows.CloseHandle(control); err != nil {
			t.Errorf("closing ACL control handle: %v", err)
		}
	})
	original, err := windows.GetSecurityInfo(control, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil || original == nil {
		t.Fatalf("capturing original test DACL: %v", err)
	}
	originalControl, originalRevision, err := original.Control()
	if err != nil {
		t.Fatal(err)
	}
	originalACL, originalDefaulted, err := original.DACL()
	if err != nil || originalACL == nil || originalDefaulted || originalControl&windows.SE_DACL_PRESENT == 0 {
		t.Fatalf("exact fixture restoration requires a present nonnull nondefaulted DACL: %v", err)
	}
	originalSDDL := original.String()
	if originalSDDL == "" {
		t.Fatal("cannot capture exact DACL representation")
	}
	const controlMask = windows.SE_DACL_PRESENT | windows.SE_DACL_DEFAULTED | windows.SE_DACL_PROTECTED |
		windows.SE_DACL_AUTO_INHERITED | windows.SE_DACL_AUTO_INHERIT_REQ
	restoreInfo := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION)
	if originalControl&windows.SE_DACL_PROTECTED != 0 {
		restoreInfo |= windows.PROTECTED_DACL_SECURITY_INFORMATION
	} else {
		restoreInfo |= windows.UNPROTECTED_DACL_SECURITY_INFORMATION
	}
	restoreNeeded := false
	// Register restoration before the write, including a write that reports an
	// error. The earlier cleanup closes the handle only after restoration.
	t.Cleanup(func() {
		if !restoreNeeded {
			return
		}
		err := windows.SetSecurityInfo(control, windows.SE_FILE_OBJECT, restoreInfo, nil, nil, originalACL, nil)
		runtime.KeepAlive(original)
		if err != nil {
			t.Errorf("restoring original test DACL: %v", err)
			return
		}
		restored, err := windows.GetSecurityInfo(control, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil || restored == nil {
			t.Errorf("verifying restored DACL: %v", err)
			return
		}
		restoredControl, revision, err := restored.Control()
		if err != nil {
			t.Errorf("verifying restored control flags: %v", err)
			return
		}
		restoredACL, defaulted, err := restored.DACL()
		if err != nil || restoredACL == nil || defaulted != originalDefaulted ||
			revision != originalRevision || restoredControl&controlMask != originalControl&controlMask ||
			restored.String() != originalSDDL {
			t.Errorf("original DACL/presence/default/inheritance/protection did not restore exactly: %v", err)
			return
		}
		if _, err := os.ReadFile(path); err != nil {
			t.Errorf("restored test file remains unreadable: %v", err)
		}
	})
	user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	denied, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.FILE_READ_DATA,
		AccessMode:        windows.DENY_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
		},
	}}, originalACL)
	runtime.KeepAlive(user)
	runtime.KeepAlive(original)
	if err != nil {
		t.Fatal(err)
	}
	restoreNeeded = true
	err = windows.SetSecurityInfo(control, windows.SE_FILE_OBJECT, restoreInfo, nil, nil, denied, nil)
	runtime.KeepAlive(denied)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := captureContinuationSettingsSnapshot(path)
	if err != nil {
		t.Fatalf("DACL must preserve metadata capture: %v", err)
	}
	if err := snapshot.close(); err != nil {
		t.Fatal(err)
	}
	if snapshot.handle == nil {
		t.Fatal("Windows metadata snapshot did not retain a handle")
	}
	if _, err := snapshot.handle.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("metadata probe descriptor not closed: %v", err)
	}
	if _, err := os.ReadFile(path); !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("test DACL did not cause real data-read access denial: %v", err)
	}
}

func TestWindowsSettingsSnapshotRetainsIdentityAndCloses(t *testing.T) {
	city := t.TempDir()
	writePreparationSettings(t, city, 0o600)
	path := filepath.Join(city, ".gc", "settings.json")
	before, err := captureContinuationSettingsSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			if err := before.close(); err != nil {
				t.Error(err)
			}
		}
	})
	if before.handle == nil || !before.info.Mode().IsRegular() {
		t.Fatal("snapshot did not retain eager regular-file identity")
	}
	read, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	current, statErr := read.Stat()
	closeErr := read.Close()
	if statErr != nil || closeErr != nil || !os.SameFile(before.info, current) {
		t.Fatalf("unchanged file identity unavailable: %v / %v", statErr, closeErr)
	}
	if err := os.Rename(path, path+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	read, err = os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement, statErr := read.Stat()
	closeErr = read.Close()
	if statErr != nil || closeErr != nil || os.SameFile(before.info, replacement) {
		t.Fatalf("snapshot accepted changed identity: %v / %v", statErr, closeErr)
	}
	if err := before.close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	if _, err := before.handle.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("metadata descriptor remained open: %v", err)
	}
}

func TestWindowsSettingsUnreadableFixtureRestoresExactDACL(t *testing.T) {
	// The production-helper fixture checks real access denial and metadata
	// access, and its registered cleanup verifies exact DACL restoration and
	// an ordinary data-read open before the temporary directory is removed.
	writeUnreadablePreparationSettings(t, t.TempDir())
}
