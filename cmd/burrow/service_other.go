//go:build !windows

package main

import (
	"errors"

	"burrow/internal/agent"
)

// The service, its installer and the console password prompt are Windows-only.
// These stubs exist so the command compiles elsewhere for vetting and for the
// portable tests.

var errWindowsOnly = errors.New("this command is only available on Windows")

func runAsService(*agent.Agent, func(string, ...any)) error { return errWindowsOnly }

func cmdInstall([]string) error   { return errWindowsOnly }
func cmdUninstall([]string) error { return errWindowsOnly }

func readPasswordNoEcho() (string, error) { return "", errWindowsOnly }

func serviceHint() string { return "" }
