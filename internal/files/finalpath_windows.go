//go:build windows

package files

import (
	"fmt"
	"strings"

	"golang.org/x/sys/windows"
)

// GetFinalPathNameByHandle flags. x/sys/windows does not export these, so they
// are spelled out from the Win32 headers. Both happen to be zero — the default
// is a normalised path with a drive-letter volume name — but naming them keeps
// the call site readable and makes a future change to a different form obvious.
const (
	fileNameNormalized = 0x0
	volumeNameDOS      = 0x0
)

// finalPath returns the canonical path of an existing file or directory, with
// every symlink, junction and 8.3 short name resolved.
//
// This is the containment check that matters on Windows: filepath.EvalSymlinks
// does not follow directory junctions, which are the usual way to escape a
// folder. GetFinalPathNameByHandle asks the filesystem what the handle actually
// refers to, so a junction out of the root shows up as its target.
func finalPath(p string) (string, error) {
	u16, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return "", fmt.Errorf("files: path is not valid UTF-16: %w", err)
	}

	// FILE_FLAG_BACKUP_SEMANTICS is required to open a directory handle.
	// No access is requested (0): the handle is only used to ask for the name,
	// so this works on files the caller may not read.
	h, err := windows.CreateFile(
		u16,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return "", fmt.Errorf("files: open %q: %w", p, err)
	}
	defer windows.CloseHandle(h)

	buf := make([]uint16, windows.MAX_PATH)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), fileNameNormalized|volumeNameDOS)
		if err != nil {
			return "", fmt.Errorf("files: final path of %q: %w", p, err)
		}
		if int(n) < len(buf) {
			break
		}
		// n is the required length, excluding the terminator.
		buf = make([]uint16, n+1)
	}

	out := windows.UTF16ToString(buf)
	// The result carries the extended-length prefix; strip it so comparisons
	// and anything shown to a human use the ordinary form.
	out = strings.TrimPrefix(out, `\\?\`)
	return out, nil
}
