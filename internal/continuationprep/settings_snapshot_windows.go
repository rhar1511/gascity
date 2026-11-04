//go:build windows

package continuationprep

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func captureContinuationSettingsSnapshot(path string) (continuationSettingsSnapshot, error) {
	// Preserve absolute, UNC and extended-length file paths without reopening
	// a path later to discover its file identity.
	name, err := filepath.Abs(path)
	if err != nil {
		return continuationSettingsSnapshot{}, err
	}
	if !strings.HasPrefix(name, `\\?\`) && !strings.HasPrefix(name, `\\.\`) {
		if strings.HasPrefix(name, `\\`) {
			name = `\\?\UNC\` + name[2:]
		} else {
			name = `\\?\` + name
		}
	}
	nativePath, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return continuationSettingsSnapshot{}, err
	}
	handle, err := windows.CreateFile(nativePath, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return continuationSettingsSnapshot{}, fmt.Errorf("opening settings metadata %q: %w", path, err)
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		return continuationSettingsSnapshot{}, errors.Join(os.ErrInvalid, windows.CloseHandle(handle))
	}
	info, err := file.Stat()
	if err != nil {
		return continuationSettingsSnapshot{}, errors.Join(err, file.Close())
	}
	return continuationSettingsSnapshot{info: info, handle: file}, nil
}
