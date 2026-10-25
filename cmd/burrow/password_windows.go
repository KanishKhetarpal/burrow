//go:build windows

package main

import (
	"fmt"
	"os"

	"golang.org/x/term"
)

// readPasswordNoEcho reads a line from the console with echo turned off.
//
// The value goes straight from here into the service control manager. It is
// never echoed, never logged, and never placed on a command line, where any
// local process could read it out of the process list
// (docs/03-SECURITY.md §4).
func readPasswordNoEcho() (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		// Reading a password from a pipe would mean it came from a script or a
		// file, which is exactly what must not happen.
		return "", fmt.Errorf("a password must be typed at a console, not piped in")
	}
	b, err := term.ReadPassword(fd)
	if err != nil {
		return "", fmt.Errorf("could not read the password: %w", err)
	}
	return string(b), nil
}
