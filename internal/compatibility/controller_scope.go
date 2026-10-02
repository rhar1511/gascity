package compatibility

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// ControllerScopeID derives a stable opaque identity for a controller rooted
// at one city directory. The value is the literal prefix
// "city-controller-v1:" followed by lowercase SHA-256 of the UTF-8 bytes of
// the cleaned, slash-normalized absolute path after symlink resolution. The
// path itself is never returned or placed in action records.
func ControllerScopeID(cityPath string) (string, error) {
	if strings.TrimSpace(cityPath) == "" {
		return "", fmt.Errorf("controller city path is empty")
	}
	abs, err := filepath.Abs(cityPath)
	if err != nil {
		return "", fmt.Errorf("resolve controller city path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve controller city path symlinks: %w", err)
	}
	canonical := filepath.ToSlash(filepath.Clean(resolved))
	if !utf8.ValidString(canonical) {
		return "", fmt.Errorf("controller city path is not valid UTF-8")
	}
	return "city-controller-v1:" + sha256String(canonical), nil
}
