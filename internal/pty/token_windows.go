//go:build windows

package pty

import (
	"fmt"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The Burrow service runs as the owner's Windows account, which is normally an
// administrator. A service logged on as an admin holds a full admin token with
// no UAC prompt in front of it, and any child it spawns inherits that. Claude
// sessions must not silently hold admin rights, so every child is launched with
// a restricted token instead (docs/02-ARCHITECTURE.md §5, D5).
//
// The restriction has three parts:
//   - DISABLE_MAX_PRIVILEGE strips every privilege but SeChangeNotifyPrivilege
//   - BUILTIN\Administrators is marked deny-only, so it can no longer be used
//     to grant access (it still shows in `whoami /groups`, as "Group used for
//     deny only" — that string is the acceptance test)
//   - the integrity level is lowered to Medium, so the process cannot write to
//     high-integrity objects
const disableMaxPrivilege = 0x1

var (
	modadvapi32               = windows.NewLazySystemDLL("advapi32.dll")
	procCreateRestrictedToken = modadvapi32.NewProc("CreateRestrictedToken")
	procSetTokenInformation   = modadvapi32.NewProc("SetTokenInformation")
)

// mediumIntegritySID is S-1-16-8192, the Medium Mandatory Level.
func mediumIntegritySID() (*windows.SID, error) {
	sid, err := windows.StringToSid("S-1-16-8192")
	if err != nil {
		return nil, fmt.Errorf("pty: medium integrity sid: %w", err)
	}
	return sid, nil
}

// RestrictedToken derives a restricted primary token from the current process
// token. The caller owns the returned token and must Close it.
func RestrictedToken() (windows.Token, error) {
	// The process token is opened with exactly the rights needed and no more.
	// Asking for TOKEN_ALL_ACCESS here fails with "access denied" on a normal
	// user account, and CreateRestrictedToken only needs TOKEN_DUPLICATE.
	var self windows.Token
	// TOKEN_ADJUST_DEFAULT is included because the restricted token inherits
	// the access rights of this handle, and lowering its integrity label needs
	// that right on the new token.
	if err := windows.OpenProcessToken(windows.CurrentProcess(),
		windows.TOKEN_DUPLICATE|windows.TOKEN_QUERY|windows.TOKEN_ASSIGN_PRIMARY|
			windows.TOKEN_ADJUST_DEFAULT, &self); err != nil {
		return 0, fmt.Errorf("pty: open own token: %w", err)
	}
	defer self.Close()

	// CreateRestrictedToken returns a token of the same type as its input, and
	// a process token is already a primary token, so no DuplicateTokenEx step
	// is needed to get something CreateProcessAsUser will accept.
	dup := self

	adminSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return 0, fmt.Errorf("pty: administrators sid: %w", err)
	}
	disable := []windows.SIDAndAttributes{{Sid: adminSID, Attributes: 0}}

	var restricted windows.Token
	r, _, errno := procCreateRestrictedToken.Call(
		uintptr(dup),
		uintptr(disableMaxPrivilege),
		uintptr(len(disable)),
		uintptr(unsafe.Pointer(&disable[0])),
		0, 0, // delete no privileges explicitly; DISABLE_MAX_PRIVILEGE covers it
		0, 0, // restrict no SIDs: a restricting SID would also block reads of
		//        the user's own files, which the session legitimately needs
		uintptr(unsafe.Pointer(&restricted)),
	)
	if r == 0 {
		return 0, fmt.Errorf("pty: CreateRestrictedToken: %w", errno)
	}

	if err := setMediumIntegrity(restricted); err != nil {
		restricted.Close()
		return 0, err
	}
	if err := setUserOwnerAndDefaultDACL(restricted); err != nil {
		restricted.Close()
		return 0, err
	}
	return restricted, nil
}

// tokenOwner and tokenDefaultDACL mirror TOKEN_OWNER and TOKEN_DEFAULT_DACL,
// which x/sys/windows does not declare.
type tokenOwner struct{ Owner *windows.SID }
type tokenDefaultDACL struct{ DefaultDacl *windows.ACL }

// setUserOwnerAndDefaultDACL makes the user, not Administrators, the owner of
// everything the restricted process creates, and grants the user access by
// default.
//
// An elevated token — which is what a service logged on as an administrator
// holds — has BUILTIN\Administrators as its default owner and a default DACL
// that grants Administrators and SYSTEM only. Once Administrators is deny-only,
// the child cannot open its own process or token: `whoami /groups` fails with
// "Access is denied" and so would anything else that inspects itself. A
// non-elevated token does not show this, which is why it only surfaced when
// the tests first ran from an elevated terminal.
func setUserOwnerAndDefaultDACL(t windows.Token) error {
	tu, err := t.GetTokenUser()
	if err != nil {
		return fmt.Errorf("pty: read token user: %w", err)
	}
	user := tu.User.Sid

	owner := tokenOwner{Owner: user}
	r, _, errno := procSetTokenInformation.Call(
		uintptr(t),
		uintptr(windows.TokenOwner),
		uintptr(unsafe.Pointer(&owner)),
		unsafe.Sizeof(owner),
	)
	if r == 0 {
		return fmt.Errorf("pty: set token owner: %w", errno)
	}

	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return fmt.Errorf("pty: system sid: %w", err)
	}
	grant := func(sid *windows.SID) windows.EXPLICIT_ACCESS {
		return windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		}
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{grant(user), grant(system)}, nil)
	if err != nil {
		return fmt.Errorf("pty: build default dacl: %w", err)
	}
	dacl := tokenDefaultDACL{DefaultDacl: acl}
	r, _, errno = procSetTokenInformation.Call(
		uintptr(t),
		uintptr(windows.TokenDefaultDacl),
		uintptr(unsafe.Pointer(&dacl)),
		unsafe.Sizeof(dacl),
	)
	runtime.KeepAlive(acl)
	runtime.KeepAlive(tu)
	if r == 0 {
		return fmt.Errorf("pty: set default dacl: %w", errno)
	}
	return nil
}

// setMediumIntegrity lowers a token's mandatory integrity label.
func setMediumIntegrity(t windows.Token) error {
	sid, err := mediumIntegritySID()
	if err != nil {
		return err
	}
	label := windows.Tokenmandatorylabel{
		Label: windows.SIDAndAttributes{Sid: sid, Attributes: windows.SE_GROUP_INTEGRITY},
	}
	r, _, errno := procSetTokenInformation.Call(
		uintptr(t),
		uintptr(windows.TokenIntegrityLevel),
		uintptr(unsafe.Pointer(&label)),
		uintptr(label.Size()),
	)
	if r == 0 {
		return fmt.Errorf("pty: lower integrity level: %w", errno)
	}
	return nil
}

// AdminDenyOnly reports whether a token has BUILTIN\Administrators marked
// deny-only. Used by `burrow selftest` and by the PTY test to prove the
// restriction took effect rather than assuming it.
func AdminDenyOnly(t windows.Token) (bool, error) {
	groups, err := t.GetTokenGroups()
	if err != nil {
		return false, fmt.Errorf("pty: read token groups: %w", err)
	}
	adminSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return false, fmt.Errorf("pty: administrators sid: %w", err)
	}
	for _, g := range groups.AllGroups() {
		if g.Sid.Equals(adminSID) {
			return g.Attributes&windows.SE_GROUP_USE_FOR_DENY_ONLY != 0, nil
		}
	}
	// Not a member at all is at least as restricted as deny-only.
	return true, nil
}

// environmentBlock builds the user environment for a child process. Without it
// a service-spawned child inherits the service's bare environment and misses
// PATH entries, USERPROFILE and APPDATA, which Claude Code needs to find its
// own configuration.
func environmentBlock(t windows.Token, extra map[string]string) (*uint16, func(), error) {
	var block *uint16
	if err := windows.CreateEnvironmentBlock(&block, t, false); err != nil {
		return nil, nil, fmt.Errorf("pty: create environment block: %w", err)
	}
	free := func() { windows.DestroyEnvironmentBlock(block) }
	if len(extra) == 0 {
		return block, free, nil
	}

	// Merge the extra variables in, then build a fresh block and release the
	// one the OS gave us.
	env := parseEnvBlock(block)
	free()
	for k, v := range extra {
		env = upsertEnv(env, k, v)
	}
	merged, backing, err := encodeEnvBlock(env)
	if err != nil {
		return nil, nil, err
	}
	// The closure holds a reference to the backing slice, so it stays reachable
	// for as long as the caller keeps the free function — which is as long as
	// the pointer is in use.
	return merged, func() { runtime.KeepAlive(backing) }, nil
}

// encodeEnvBlock turns "K=V" strings into a double-NUL-terminated UTF-16 block.
// It returns the pointer to pass to CreateProcess and the backing slice, which
// the caller must keep reachable.
func encodeEnvBlock(env []string) (*uint16, []uint16, error) {
	var buf []uint16
	for _, e := range env {
		if e == "" {
			continue // an empty entry would terminate the block early
		}
		if strings.ContainsRune(e, 0) {
			return nil, nil, fmt.Errorf("pty: environment entry contains a NUL")
		}
		u, err := windows.UTF16FromString(e)
		if err != nil {
			return nil, nil, fmt.Errorf("pty: encode environment entry: %w", err)
		}
		buf = append(buf, u...) // UTF16FromString already appends the NUL
	}
	buf = append(buf, 0) // the final empty entry closes the block
	return &buf[0], buf, nil
}
