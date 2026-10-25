//go:build !windows

package files

import "os"

// recycleBin has no equivalent outside Windows. The agent is Windows-only, so
// this exists for the tests that run elsewhere; it removes the path instead.
func recycleBin(path string) error {
	return os.RemoveAll(path)
}
