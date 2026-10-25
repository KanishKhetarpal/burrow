//go:build windows

package main

import "testing"

// An account that does not exist must be refused, which proves the check
// really asks Windows rather than accepting anything. A made-up account is
// used so the test never counts towards the owner's lockout threshold.
func TestCheckWindowsPasswordRefusesAnUnknownAccount(t *testing.T) {
	if err := checkWindowsPassword(`.\burrow-no-such-user-7f3a`, "not-a-password"); err == nil {
		t.Fatal("checkWindowsPassword accepted an account that does not exist")
	}
}
