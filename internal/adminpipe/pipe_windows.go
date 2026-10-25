//go:build windows

package adminpipe

import (
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/Microsoft/go-winio"
)

// pipeSDDL restricts the pipe to SYSTEM, the Administrators group and the
// owner of the process that created it.
//
// D: a DACL with no inheritance; A;;GA;;; grants all access to
//
//	SY  = Local System
//	BA  = Builtin Administrators
//	OW  = Creator Owner (the account the service runs as)
//
// Nothing else is granted, so another local user cannot reach the pipe and
// clear lockdown (docs/03-SECURITY.md §4).
const pipeSDDL = "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;OW)"

// Listen creates the admin pipe.
func Listen() (net.Listener, error) {
	ln, err := winio.ListenPipe(Name, &winio.PipeConfig{
		SecurityDescriptor: pipeSDDL,
		MessageMode:        false,
		InputBufferSize:    64 << 10,
		OutputBufferSize:   64 << 10,
	})
	if err != nil {
		return nil, fmt.Errorf("adminpipe: listen on %s: %w", Name, err)
	}
	return ln, nil
}

// dial connects to the pipe.
func dial(timeout time.Duration) (net.Conn, error) {
	conn, err := winio.DialPipe(Name, &timeout)
	if err != nil {
		// A missing pipe is the normal "service is not running" case, which
		// deserves a clear message rather than a Windows error number.
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, winio.ErrTimeout) {
			return nil, ErrNotRunning
		}
		return nil, fmt.Errorf("adminpipe: connect: %w", err)
	}
	return conn, nil
}
