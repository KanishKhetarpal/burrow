// Package pty runs a child process on a pseudo-console.
//
// Claude Code is an interactive terminal program: it draws a TUI, asks y/n
// questions and expects a real console. A plain pipe would make it fall back to
// non-interactive behaviour, so sessions run on a Windows pseudo-console
// (ConPTY) and the PWA sees the same bytes a terminal would
// (docs/02-ARCHITECTURE.md §2.1).
package pty

import (
	"errors"
	"io"
	"strings"
)

// ErrUnsupported is returned on platforms without a pseudo-console
// implementation. The agent only ships on Windows; this keeps the package
// buildable elsewhere so the pure-Go parts can be tested.
var ErrUnsupported = errors.New("pty: pseudo-console is only implemented on Windows")

// Size is a terminal size in character cells.
type Size struct {
	Cols uint16
	Rows uint16
}

// DefaultSize is used until the client reports its real size. 120x30 is wide
// enough that Claude Code's output does not wrap awkwardly in the phone's
// terminal view before the first resize arrives.
var DefaultSize = Size{Cols: 120, Rows: 30}

// Config describes a process to start.
type Config struct {
	// Path is the executable. It is resolved against the child's PATH.
	Path string
	// Args are the arguments after the executable name.
	Args []string
	// Dir is the working directory.
	Dir string
	// Env holds variables to add to (or override in) the user environment.
	Env map[string]string
	// Size is the initial terminal size.
	Size Size
	// Restricted launches the child with a restricted token: no privileges,
	// Administrators deny-only, Medium integrity. Always true in production.
	Restricted bool
}

// Process is a running child on a pseudo-console.
type Process interface {
	// Read returns raw PTY output, escape sequences included.
	io.Reader
	// Write sends keystrokes to the child.
	io.Writer
	// Resize tells the console its new size.
	Resize(Size) error
	// PID is the child's process id.
	PID() int
	// Wait blocks until the child exits and returns its exit code.
	Wait() (int, error)
	// Kill terminates the child and its whole process tree.
	Kill() error
	// Close releases the console and handles. Safe to call more than once.
	Close() error
}

// ---------- environment helpers (portable, so they can be unit tested) ----------

// parseEnvBlock splits a Windows environment block ("K=V\0K=V\0\0").
func parseEnvBlock(block *uint16) []string {
	if block == nil {
		return nil
	}
	return utf16BlockToStrings(block)
}

// upsertEnv sets key to value, replacing any existing entry case-insensitively,
// because Windows environment variable names are not case-sensitive.
func upsertEnv(env []string, key, value string) []string {
	prefix := strings.ToLower(key) + "="
	for i, e := range env {
		if strings.HasPrefix(strings.ToLower(e), prefix) {
			env[i] = key + "=" + value
			return env
		}
	}
	return append(env, key+"="+value)
}

// lookupEnv finds a value in a parsed environment, case-insensitively.
func lookupEnv(env []string, key string) (string, bool) {
	prefix := strings.ToLower(key) + "="
	for _, e := range env {
		if strings.HasPrefix(strings.ToLower(e), prefix) {
			return e[len(prefix):], true
		}
	}
	return "", false
}

// appendNull adds the terminating empty string that UTF16PtrFromStringSlice
// turns into the block's final NUL.
func appendNull(env []string) []string {
	return append(env, "")
}
