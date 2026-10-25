//go:build windows

package power

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// SetThreadExecutionState flags.
const (
	esContinuous       = 0x80000000
	esSystemRequired   = 0x00000001
	esAwaymodeRequired = 0x00000040
)

// seShutdownName is the privilege needed to suspend, hibernate, shut down or
// restart. x/sys/windows does not export the SE_*_NAME string constants.
const seShutdownName = "SeShutdownPrivilege"

var (
	modpowrprof         = windows.NewLazySystemDLL("powrprof.dll")
	procSetSuspendState = modpowrprof.NewProc("SetSuspendState")

	moduser32              = windows.NewLazySystemDLL("user32.dll")
	procLockWorkStation    = moduser32.NewProc("LockWorkStation")
	procSetThreadExecState = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetThreadExecutionState")

	modkernel32              = windows.NewLazySystemDLL("kernel32.dll")
	procProcessIdToSessionId = modkernel32.NewProc("ProcessIdToSessionId")
)

// winBackend is the real implementation.
type winBackend struct {
	// keepAwakeMu guards the execution-state call, which is per-thread state in
	// the OS and must not be raced.
	keepAwakeMu sync.Mutex
	// keepAwakeDone stops the holder goroutine.
	keepAwakeDone chan struct{}
}

func defaultBackend() Backend { return &winBackend{} }

// Suspend puts the machine to sleep or hibernation.
func (b *winBackend) Suspend(hibernate bool) error {
	if err := enablePrivilege(seShutdownName); err != nil {
		return err
	}
	// Releasing the keep-awake hold first, or the request is refused.
	if err := b.KeepAwake(false); err != nil {
		return err
	}
	var hib uintptr
	if hibernate {
		hib = 1
	}
	// SetSuspendState(hibernate, force, wakeupEventsDisabled). force is 0 so
	// applications get a chance to object; wakeup events stay enabled.
	r, _, errno := procSetSuspendState.Call(hib, 0, 0)
	if r == 0 {
		return fmt.Errorf("power: suspend: %w", errno)
	}
	return nil
}

// Shutdown powers the machine off or restarts it.
//
// shutdown.exe is used rather than InitiateSystemShutdownEx because it owns the
// delay itself: the action then survives the agent being stopped in the
// meantime, and Windows shows its own countdown to anyone at the console.
func (b *winBackend) Shutdown(restart bool, delay time.Duration) error {
	exe, err := systemExe("shutdown.exe")
	if err != nil {
		return err
	}
	flag := "/s"
	if restart {
		flag = "/r"
	}
	secs := strconv.Itoa(int(delay.Seconds()))
	// /t <secs> is the delay; no /f, so open applications may object, which is
	// what a user at the keyboard would expect.
	cmd := exec.Command(exe, flag, "/t", secs, "/c", "Requested from Burrow")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("power: shutdown: %w: %s", err, string(out))
	}
	return nil
}

// CancelShutdown aborts a pending shutdown.
func (b *winBackend) CancelShutdown() error {
	exe, err := systemExe("shutdown.exe")
	if err != nil {
		return err
	}
	out, err := exec.Command(exe, "/a").CombinedOutput()
	if err != nil {
		return fmt.Errorf("power: cancel shutdown: %w: %s", err, string(out))
	}
	return nil
}

// LockWorkstation locks the desktop.
//
// This only affects the caller's own session, so it does nothing useful from a
// service in session 0. The Manager checks InteractiveSession first and reports
// ErrLockUnavailable rather than appearing to succeed.
func (b *winBackend) LockWorkstation() error {
	r, _, errno := procLockWorkStation.Call()
	if r == 0 {
		return fmt.Errorf("power: lock: %w", errno)
	}
	return nil
}

// KeepAwake holds off system sleep.
//
// SetThreadExecutionState applies to the calling thread, and the continuous
// flag keeps it in force until it is cleared. A dedicated goroutine locked to
// one OS thread holds it, because Go would otherwise be free to move the
// calling goroutine to a different thread and the hold would lapse.
func (b *winBackend) KeepAwake(on bool) error {
	b.keepAwakeMu.Lock()
	defer b.keepAwakeMu.Unlock()

	if !on {
		if b.keepAwakeDone != nil {
			close(b.keepAwakeDone)
			b.keepAwakeDone = nil
		}
		return nil
	}
	if b.keepAwakeDone != nil {
		return nil // already held
	}

	done := make(chan struct{})
	errc := make(chan error, 1)
	go func() {
		// The hold is thread state, so this goroutine must not migrate.
		lockOSThread()
		defer unlockOSThread()

		r, _, errno := procSetThreadExecState.Call(uintptr(esContinuous | esSystemRequired | esAwaymodeRequired))
		if r == 0 {
			errc <- fmt.Errorf("power: keep awake: %w", errno)
			return
		}
		errc <- nil
		<-done
		// Clearing the continuous flag releases the hold.
		procSetThreadExecState.Call(uintptr(esContinuous))
	}()

	if err := <-errc; err != nil {
		close(done)
		return err
	}
	b.keepAwakeDone = done
	return nil
}

// InteractiveSession reports whether this process is in a session with a
// desktop. A Windows service is in session 0, which has none.
func (b *winBackend) InteractiveSession() bool {
	var session uint32
	r, _, _ := procProcessIdToSessionId.Call(uintptr(windows.GetCurrentProcessId()),
		uintptr(unsafe.Pointer(&session)))
	if r == 0 {
		return false
	}
	return session != 0
}

// enablePrivilege turns on a privilege the process holds but has disabled.
// Shutdown and suspend both need SeShutdownPrivilege, which is present but off
// by default.
func enablePrivilege(name string) error {
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(),
		windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &tok); err != nil {
		return fmt.Errorf("power: open token: %w", err)
	}
	defer tok.Close()

	var luid windows.LUID
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return fmt.Errorf("power: privilege name: %w", err)
	}
	if err := windows.LookupPrivilegeValue(nil, n, &luid); err != nil {
		return fmt.Errorf("power: look up %s: %w", name, err)
	}

	privs := windows.Tokenprivileges{
		PrivilegeCount: 1,
	}
	privs.Privileges[0] = windows.LUIDAndAttributes{
		Luid:       luid,
		Attributes: windows.SE_PRIVILEGE_ENABLED,
	}
	if err := windows.AdjustTokenPrivileges(tok, false, &privs, 0, nil, nil); err != nil {
		return fmt.Errorf("power: enable %s: %w", name, err)
	}
	return nil
}

// systemExe resolves a System32 executable by absolute path, so the action does
// not depend on PATH and cannot pick up something else called shutdown.exe.
func systemExe(name string) (string, error) {
	dir, err := windows.GetSystemDirectory()
	if err != nil {
		return "", fmt.Errorf("power: system directory: %w", err)
	}
	p := filepath.Join(dir, name)
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("power: %s not found: %w", name, err)
	}
	return p, nil
}
