//go:build !windows

package files

import (
	"fmt"
	"path/filepath"
)

// finalPath resolves symlinks on non-Windows platforms.
//
// The agent only ever runs on Windows; this exists so the package compiles and
// its table and fuzz tests can run on a Linux CI box as well.
func finalPath(p string) (string, error) {
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("files: resolve %q: %w", p, err)
	}
	return real, nil
}
