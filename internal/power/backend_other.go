//go:build !windows

package power

import (
	"errors"
	"time"
)

// noopBackend lets the package build and its logic be tested off Windows. The
// agent only ships on Windows, so every call reports that it is unavailable
// rather than pretending to work.
type noopBackend struct{}

func defaultBackend() Backend { return noopBackend{} }

var errNotWindows = errors.New("power: only implemented on Windows")

func (noopBackend) Suspend(bool) error                 { return errNotWindows }
func (noopBackend) Shutdown(bool, time.Duration) error { return errNotWindows }
func (noopBackend) CancelShutdown() error              { return errNotWindows }
func (noopBackend) LockWorkstation() error             { return errNotWindows }
func (noopBackend) KeepAwake(bool) error               { return errNotWindows }
func (noopBackend) InteractiveSession() bool           { return false }
