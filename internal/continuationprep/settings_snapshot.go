package continuationprep

import "os"

// Windows retains a metadata handle because path Stat may defer identity lookup.
// Other platforms carry an eager Stat snapshot without an owned handle.
type continuationSettingsSnapshot struct {
	info   os.FileInfo
	handle *os.File
}

func (s continuationSettingsSnapshot) close() error {
	if s.handle != nil {
		return s.handle.Close()
	}
	return nil
}
