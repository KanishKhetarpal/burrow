//go:build !windows

package agent

import (
	"errors"
	"os"
	"path/filepath"
)

func systemDir() (string, error) { return "", errors.New("agent: Windows only") }

func authorizedKeysPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ssh", "authorized_keys"), nil
}
