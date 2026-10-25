//go:build windows

package agent

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func systemDir() (string, error) {
	d, err := windows.GetSystemDirectory()
	if err != nil {
		return "", fmt.Errorf("agent: system directory: %w", err)
	}
	return d, nil
}

// authorizedKeysPath returns the file Windows OpenSSH will actually read for
// this account.
//
// For a member of Administrators, sshd ignores %USERPROFILE%\.ssh\authorized_keys
// and reads %ProgramData%\ssh\administrators_authorized_keys instead. Writing
// to the user file for an admin account is a silent failure: the key is stored
// and the login still refused (docs/03-SECURITY.md §5).
func authorizedKeysPath() (string, error) {
	admin, err := currentUserIsAdmin()
	if err != nil {
		return "", err
	}
	if admin {
		programData := os.Getenv("ProgramData")
		if programData == "" {
			programData = `C:\ProgramData`
		}
		return filepath.Join(programData, "ssh", "administrators_authorized_keys"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("agent: home directory: %w", err)
	}
	return filepath.Join(home, ".ssh", "authorized_keys"), nil
}

// currentUserIsAdmin reports whether this process's token has an enabled
// Administrators membership.
func currentUserIsAdmin() (bool, error) {
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &tok); err != nil {
		return false, fmt.Errorf("agent: open token: %w", err)
	}
	defer tok.Close()

	adminSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return false, fmt.Errorf("agent: administrators sid: %w", err)
	}
	groups, err := tok.GetTokenGroups()
	if err != nil {
		return false, fmt.Errorf("agent: token groups: %w", err)
	}
	for _, g := range groups.AllGroups() {
		if !g.Sid.Equals(adminSID) {
			continue
		}
		// Deny-only membership does not count: sshd checks the effective
		// group, and so must this.
		return g.Attributes&windows.SE_GROUP_USE_FOR_DENY_ONLY == 0, nil
	}
	return false, nil
}
