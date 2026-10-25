//go:build windows

package files

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// SHFileOperation flags.
const (
	foDelete          = 0x0003
	fofAllowUndo      = 0x0040 // this is what sends it to the Recycle Bin
	fofNoConfirmation = 0x0010
	fofNoErrorUI      = 0x0400
	fofSilent         = 0x0004
	fofNoConfirmMkDir = 0x0200
)

// shFileOpStruct mirrors SHFILEOPSTRUCTW.
type shFileOpStruct struct {
	hwnd                  windows.Handle
	wFunc                 uint32
	pFrom                 *uint16
	pTo                   *uint16
	fFlags                uint16
	fAnyOperationsAborted int32
	hNameMappings         uintptr
	lpszProgressTitle     *uint16
}

var (
	modshell32          = windows.NewLazySystemDLL("shell32.dll")
	procSHFileOperation = modshell32.NewProc("SHFileOperationW")
)

// recycleBin moves a path to the Recycle Bin.
//
// SHFileOperation with FOF_ALLOWUNDO is the documented way to do this. The
// newer IFileOperation needs COM apartment initialisation, which is awkward in
// a service; this call does not. The UI flags matter because the agent runs in
// session 0, where no dialog could ever be answered.
func recycleBin(path string) error {
	// pFrom is a double-NUL-terminated list of paths, not a plain string.
	from, err := doubleNullTerminated(path)
	if err != nil {
		return err
	}

	op := shFileOpStruct{
		wFunc: foDelete,
		pFrom: &from[0],
		fFlags: fofAllowUndo | fofNoConfirmation | fofNoErrorUI |
			fofSilent | fofNoConfirmMkDir,
	}
	r, _, _ := procSHFileOperation.Call(uintptr(unsafe.Pointer(&op)))
	if r != 0 {
		return fmt.Errorf("files: move to Recycle Bin failed (code %d)", r)
	}
	if op.fAnyOperationsAborted != 0 {
		return fmt.Errorf("files: move to Recycle Bin was aborted")
	}
	return nil
}

// doubleNullTerminated encodes one path as the NUL-separated, NUL-terminated
// list SHFileOperation expects.
func doubleNullTerminated(path string) ([]uint16, error) {
	u, err := windows.UTF16FromString(path)
	if err != nil {
		return nil, fmt.Errorf("files: encode path: %w", err)
	}
	return append(u, 0), nil
}
