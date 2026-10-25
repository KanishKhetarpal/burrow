//go:build !windows

package adminpipe

import (
	"net"
	"time"
)

// Listen is unavailable outside Windows; the agent is Windows-only.
func Listen() (net.Listener, error) { return nil, ErrNotRunning }

func dial(time.Duration) (net.Conn, error) { return nil, ErrNotRunning }
