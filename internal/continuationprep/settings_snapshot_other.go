//go:build !windows

package continuationprep

import "os"

func captureContinuationSettingsSnapshot(path string) (continuationSettingsSnapshot, error) {
	info, err := os.Stat(path)
	if err != nil {
		return continuationSettingsSnapshot{}, err
	}
	return continuationSettingsSnapshot{info: info}, nil
}
