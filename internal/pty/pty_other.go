//go:build !windows

package pty

// Start reports that pseudo-consoles are unavailable. The agent is
// Windows-only; this keeps the package buildable so the portable helpers and
// the packages that import it can be tested on other platforms.
func Start(Config) (Process, error) { return nil, ErrUnsupported }

// parseEnvBlock has no meaning without a Windows environment block.
func utf16BlockToStrings(*uint16) []string { return nil }
