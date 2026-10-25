// Package auth is Burrow's human-presence layer: passkey pairing, login,
// step-up, sessions and lockdown.
//
// It sits behind the identity gate, which has already established that the
// request comes from an allowed device. This layer establishes that the person
// holding that device is present (docs/03-SECURITY.md §3.3-3.4).
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"burrow/internal/audit"
	"burrow/internal/config"
	"burrow/internal/store"
)

// Errors the HTTP layer maps to API codes.
var (
	ErrNoSession      = errors.New("auth: no valid session")
	ErrStepupRequired = errors.New("auth: step-up required")
	ErrLockedOut      = errors.New("auth: locked out after too many failures")
	ErrPairingClosed  = errors.New("auth: no pairing window is open")
	ErrPairingCode    = errors.New("auth: wrong pairing code")
	ErrTooManyDevices = errors.New("auth: device limit reached")
	ErrLockdown       = errors.New("auth: locked down")
	ErrCeremony       = errors.New("auth: ceremony failed")
)

// Clock lets tests drive expiry without sleeping.
type Clock func() time.Time

// Manager owns all auth state.
type Manager struct {
	store *store.Store
	cfg   func() *config.Config
	log   func(audit.Entry)
	now   Clock

	mu sync.Mutex
	// wa is rebuilt whenever the agent FQDN becomes known, since the FQDN is
	// the WebAuthn relying-party ID.
	wa      *webauthn.WebAuthn
	waFQDN  string
	pairing *pairingWindow
	// ceremonies holds in-flight WebAuthn challenges, keyed by a random id
	// handed to the client. Keeping them server-side means a client cannot
	// replay or pick its own challenge.
	ceremonies map[string]*ceremony
	lockdown   bool
}

type ceremony struct {
	kind    ceremonyKind
	session webauthn.SessionData
	nodeID  string
	expires time.Time
	// deviceName and deviceKind are carried from pair/begin to pair/finish.
	deviceName string
	deviceKind string
}

type ceremonyKind int

const (
	ceremonyRegister ceremonyKind = iota
	ceremonyLogin
	ceremonyStepup
)

// ceremonyTTL bounds how long a challenge stays usable.
const ceremonyTTL = 5 * time.Minute

// NewManager builds the auth manager.
func NewManager(st *store.Store, cfg func() *config.Config, log func(audit.Entry), now Clock) *Manager {
	if log == nil {
		log = func(audit.Entry) {}
	}
	if now == nil {
		now = time.Now
	}
	return &Manager{
		store:      st,
		cfg:        cfg,
		log:        log,
		now:        now,
		ceremonies: map[string]*ceremony{},
	}
}

// SetRelyingParty points WebAuthn at the agent's FQDN. It must be called once
// the tsnet node is up and before any ceremony.
func (m *Manager) SetRelyingParty(fqdn string) error {
	if fqdn == "" {
		return errors.New("auth: relying-party id is empty")
	}
	wa, err := webauthn.New(&webauthn.Config{
		RPID:          fqdn,
		RPDisplayName: "Burrow",
		RPOrigins:     []string{"https://" + fqdn},
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			// A resident (discoverable) key means the phone can log in without
			// being told which credential to use, and user verification means
			// Face ID or Windows Hello, not just a tap.
			ResidentKey:      protocol.ResidentKeyRequirementRequired,
			UserVerification: protocol.VerificationRequired,
		},
		// Attestation would identify the authenticator model; it adds nothing
		// here and is extra data to hold.
		AttestationPreference: protocol.PreferNoAttestation,
		Timeouts: webauthn.TimeoutsConfig{
			Login: webauthn.TimeoutConfig{
				Enforce:    true,
				Timeout:    ceremonyTTL,
				TimeoutUVD: ceremonyTTL,
			},
			Registration: webauthn.TimeoutConfig{
				Enforce:    true,
				Timeout:    ceremonyTTL,
				TimeoutUVD: ceremonyTTL,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("auth: configure webauthn: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.wa = wa
	m.waFQDN = fqdn
	return nil
}

// RelyingPartyID reports the configured RP ID, for diagnostics.
func (m *Manager) RelyingPartyID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.waFQDN
}

func (m *Manager) webAuthn() (*webauthn.WebAuthn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.wa == nil {
		return nil, errors.New("auth: relying party is not configured yet")
	}
	return m.wa, nil
}

// ---------- ceremony bookkeeping ----------

func (m *Manager) putCeremony(c *ceremony) (string, error) {
	id, err := randomID(16)
	if err != nil {
		return "", err
	}
	c.expires = m.now().Add(ceremonyTTL)
	m.mu.Lock()
	defer m.mu.Unlock()
	// Sweep expired entries so an abandoned ceremony cannot accumulate.
	for k, v := range m.ceremonies {
		if m.now().After(v.expires) {
			delete(m.ceremonies, k)
		}
	}
	m.ceremonies[id] = c
	return id, nil
}

// takeCeremony consumes a ceremony: a challenge is single-use, so a failed
// finish cannot be retried against the same challenge.
func (m *Manager) takeCeremony(id string, kind ceremonyKind, nodeID string) (*ceremony, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.ceremonies[id]
	if !ok {
		return nil, ErrCeremony
	}
	delete(m.ceremonies, id)
	if m.now().After(c.expires) || c.kind != kind || c.nodeID != nodeID {
		return nil, ErrCeremony
	}
	return c, nil
}

// ---------- lockdown ----------

const settingLockdown = "lockdown"

// LoadLockdown restores the lockdown flag at startup, so a reboot does not
// quietly clear it.
func (m *Manager) LoadLockdown(ctx context.Context) error {
	var on bool
	err := m.store.GetSetting(ctx, settingLockdown, &on)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.lockdown = on
	m.mu.Unlock()
	return nil
}

// Lockdown reports whether the agent is locked down.
func (m *Manager) Lockdown() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lockdown
}

// EnterLockdown kills every session and refuses everything but /healthz until
// cleared from the PC console.
func (m *Manager) EnterLockdown(ctx context.Context, actor string) error {
	m.mu.Lock()
	m.lockdown = true
	m.ceremonies = map[string]*ceremony{}
	m.pairing = nil
	m.mu.Unlock()

	if err := m.store.SetSetting(ctx, settingLockdown, true); err != nil {
		return err
	}
	if err := m.store.RevokeAllAuthSessions(ctx, m.now()); err != nil {
		return err
	}
	m.log(audit.Entry{Actor: actor, Action: audit.ActionLockdownEnter, Result: audit.ResultOK})
	return nil
}

// ExitLockdown clears lockdown. Only the admin pipe calls this, so it cannot be
// reached from a paired device.
func (m *Manager) ExitLockdown(ctx context.Context, actor string) error {
	m.mu.Lock()
	m.lockdown = false
	m.mu.Unlock()
	if err := m.store.SetSetting(ctx, settingLockdown, false); err != nil {
		return err
	}
	m.log(audit.Entry{Actor: actor, Action: audit.ActionLockdownExit, Result: audit.ResultOK})
	return nil
}

// ---------- helpers ----------

func randomID(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// RandomHex returns n random bytes as hex. Used for the agent hostname.
func RandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: random: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// credentialID encodes a raw WebAuthn credential ID for storage and for
// allowCredentials.
func credentialID(raw []byte) string {
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCredentialID(s string) ([]byte, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("auth: bad credential id: %w", err)
	}
	return b, nil
}
