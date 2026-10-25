//go:build windows

package power

import "runtime"

// Thin wrappers so the keep-awake holder's intent is readable at the call site:
// SetThreadExecutionState's continuous hold is per-OS-thread state, and the Go
// scheduler is otherwise free to move the goroutine elsewhere.
func lockOSThread()   { runtime.LockOSThread() }
func unlockOSThread() { runtime.UnlockOSThread() }
