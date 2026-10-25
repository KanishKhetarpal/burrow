package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"math/big"
	"time"
)

// pairingWindow is an open invitation to pair one device.
//
// It can only be opened from the PC console (`burrow pair`), which is what
// makes the 6-digit code safe: an attacker on the tailnet cannot start a window,
// and a window that is open lasts minutes and allows five guesses
// (docs/03-SECURITY.md T10).
type pairingWindow struct {
	code     string
	expires  time.Time
	attempts int
	// used marks the window consumed, so one `burrow pair` pairs one device.
	used bool
}

// PairingMaxAttempts is how many wrong codes close the window.
const PairingMaxAttempts = 5

// OpenPairing starts a pairing window and returns the code to show on the PC
// screen. The code is returned to the caller (the CLI, over the local admin
// pipe) and never sent to a network client.
func (m *Manager) OpenPairing() (code string, expires time.Time, err error) {
	if m.Lockdown() {
		return "", time.Time{}, ErrLockdown
	}
	cfg := m.cfg()
	if cfg == nil {
		return "", time.Time{}, fmt.Errorf("auth: no config")
	}

	code, err = sixDigitCode()
	if err != nil {
		return "", time.Time{}, err
	}
	expires = m.now().Add(cfg.Agent.Auth.PairingWindow())

	m.mu.Lock()
	m.pairing = &pairingWindow{code: code, expires: expires}
	m.mu.Unlock()
	return code, expires, nil
}

// ClosePairing ends the window early.
func (m *Manager) ClosePairing() {
	m.mu.Lock()
	m.pairing = nil
	m.mu.Unlock()
}

// PairingOpen reports whether a usable window exists. The identity gate calls
// this on every request from an unpaired node.
func (m *Manager) PairingOpen() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pairingUsableLocked()
}

func (m *Manager) pairingUsableLocked() bool {
	p := m.pairing
	if p == nil || p.used {
		return false
	}
	if m.now().After(p.expires) {
		m.pairing = nil
		return false
	}
	if p.attempts >= PairingMaxAttempts {
		m.pairing = nil
		return false
	}
	return true
}

// checkPairingCode verifies a submitted code and counts the attempt.
//
// A wrong code burns one of five attempts; the fifth closes the window, so the
// operator has to go back to the PC to try again.
func (m *Manager) checkPairingCode(code string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.pairingUsableLocked() {
		return ErrPairingClosed
	}
	p := m.pairing
	p.attempts++
	// Constant-time compare: the code is a shared secret for the window's life.
	if subtle.ConstantTimeCompare([]byte(code), []byte(p.code)) != 1 {
		if p.attempts >= PairingMaxAttempts {
			m.pairing = nil
		}
		return ErrPairingCode
	}
	return nil
}

// consumePairing marks the window used after a successful registration.
func (m *Manager) consumePairing() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pairing != nil {
		m.pairing.used = true
		m.pairing = nil
	}
}

// sixDigitCode returns a uniformly random 6-digit code.
func sixDigitCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", fmt.Errorf("auth: generate pairing code: %w", err)
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}
