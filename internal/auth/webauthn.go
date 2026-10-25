package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"burrow/internal/audit"
	"burrow/internal/store"
)

// owner is the single WebAuthn user. Burrow has exactly one human, so each
// paired device registers another credential against the same user handle; a
// login then accepts any of them and the credential identifies the device.
type owner struct {
	login string
	creds []webauthn.Credential
}

// ownerHandle is the fixed user handle. It is opaque to the authenticator and
// must stay stable, or previously registered passkeys stop matching.
var ownerHandle = []byte("burrow-owner-v1\x00")

func (o *owner) WebAuthnID() []byte                         { return ownerHandle }
func (o *owner) WebAuthnName() string                       { return o.login }
func (o *owner) WebAuthnDisplayName() string                { return o.login }
func (o *owner) WebAuthnCredentials() []webauthn.Credential { return o.creds }

// loadOwner builds the WebAuthn user from the stored credentials of every live
// device.
func (m *Manager) loadOwner(ctx context.Context) (*owner, error) {
	cfg := m.cfg()
	if cfg == nil {
		return nil, errors.New("auth: no config")
	}
	rows, err := m.store.ListCredentials(ctx)
	if err != nil {
		return nil, err
	}
	o := &owner{login: cfg.Owner.TailscaleLogin}
	for _, row := range rows {
		cred, err := credentialFromStore(row)
		if err != nil {
			// A credential that cannot be decoded is unusable; skipping it is
			// better than refusing every login.
			continue
		}
		o.creds = append(o.creds, cred)
	}
	return o, nil
}

func credentialFromStore(row store.Credential) (webauthn.Credential, error) {
	raw, err := decodeCredentialID(row.ID)
	if err != nil {
		return webauthn.Credential{}, err
	}
	cred := webauthn.Credential{
		ID:              raw,
		PublicKey:       row.PublicKey,
		AttestationType: row.Attestation,
	}
	cred.Authenticator.AAGUID = row.AAGUID
	cred.Authenticator.SignCount = row.SignCount
	cred.Flags.BackupEligible = row.BackupEligible
	cred.Flags.BackupState = row.BackupState
	for _, t := range row.Transports {
		cred.Transport = append(cred.Transport, protocol.AuthenticatorTransport(t))
	}
	return cred, nil
}

// ---------- pairing (registration) ----------

// PairBegin starts registration for a node in an open pairing window.
//
// The 6-digit code is checked here, before a challenge is issued, so a wrong
// code costs an attempt and produces nothing.
func (m *Manager) PairBegin(ctx context.Context, nodeID, login, code, deviceName, deviceKind string) (string, *protocol.CredentialCreation, error) {
	if m.Lockdown() {
		return "", nil, ErrLockdown
	}
	wa, err := m.webAuthn()
	if err != nil {
		return "", nil, err
	}
	cfg := m.cfg()
	if cfg == nil {
		return "", nil, errors.New("auth: no config")
	}

	if err := m.checkPairingCode(code); err != nil {
		m.log(audit.Entry{
			Actor:  "node:" + nodeID,
			Action: audit.ActionPair,
			Result: audit.ResultDenied,
			Meta:   map[string]any{"reason": "wrong or expired pairing code"},
		})
		return "", nil, err
	}

	count, err := m.store.CountDevices(ctx)
	if err != nil {
		return "", nil, err
	}
	if count >= cfg.Device.MaxDevices {
		return "", nil, ErrTooManyDevices
	}

	switch deviceKind {
	case "phone", "laptop":
	default:
		return "", nil, fmt.Errorf("auth: device kind must be phone or laptop")
	}
	if deviceName == "" {
		deviceName = deviceKind
	}

	o, err := m.loadOwner(ctx)
	if err != nil {
		return "", nil, err
	}

	// Exclude already-registered credentials so the authenticator offers to
	// create a new one rather than silently reusing an existing passkey.
	exclude := make([]protocol.CredentialDescriptor, 0, len(o.creds))
	for _, c := range o.creds {
		exclude = append(exclude, c.Descriptor())
	}

	creation, session, err := wa.BeginRegistration(o,
		webauthn.WithExclusions(exclude),
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementRequired,
			UserVerification: protocol.VerificationRequired,
		}),
	)
	if err != nil {
		return "", nil, fmt.Errorf("auth: begin registration: %w", err)
	}

	id, err := m.putCeremony(&ceremony{
		kind:       ceremonyRegister,
		session:    *session,
		nodeID:     nodeID,
		deviceName: deviceName,
		deviceKind: deviceKind,
	})
	if err != nil {
		return "", nil, err
	}
	return id, creation, nil
}

// PairFinish completes registration and stores the device and credential.
func (m *Manager) PairFinish(ctx context.Context, ceremonyID, nodeID, login string, r *http.Request) (store.Device, error) {
	wa, err := m.webAuthn()
	if err != nil {
		return store.Device{}, err
	}
	c, err := m.takeCeremony(ceremonyID, ceremonyRegister, nodeID)
	if err != nil {
		return store.Device{}, err
	}
	o, err := m.loadOwner(ctx)
	if err != nil {
		return store.Device{}, err
	}

	cred, err := wa.FinishRegistration(o, c.session, r)
	if err != nil {
		m.log(audit.Entry{
			Actor:  "node:" + nodeID,
			Action: audit.ActionPair,
			Result: audit.ResultDenied,
			Meta:   map[string]any{"reason": "registration ceremony failed"},
		})
		return store.Device{}, errors.Join(ErrCeremony, err)
	}
	// userVerification: required is requested, but the response is what counts:
	// a credential created without user verification would let anyone holding
	// the unlocked device in.
	if !cred.Flags.UserVerified {
		return store.Device{}, fmt.Errorf("%w: user verification was not performed", ErrCeremony)
	}

	now := m.now()
	devID, err := randomID(12)
	if err != nil {
		return store.Device{}, err
	}
	dev := store.Device{
		ID:       devID,
		Name:     c.deviceName,
		Kind:     c.deviceKind,
		NodeID:   nodeID,
		Login:    login,
		PairedAt: now,
	}
	if err := m.store.AddDevice(ctx, dev); err != nil {
		return store.Device{}, err
	}

	transports := make([]string, 0, len(cred.Transport))
	for _, t := range cred.Transport {
		transports = append(transports, string(t))
	}
	if err := m.store.AddCredential(ctx, store.Credential{
		ID:             credentialID(cred.ID),
		DeviceID:       devID,
		PublicKey:      cred.PublicKey,
		Attestation:    cred.AttestationType,
		Transports:     transports,
		AAGUID:         cred.Authenticator.AAGUID,
		SignCount:      cred.Authenticator.SignCount,
		BackupEligible: cred.Flags.BackupEligible,
		BackupState:    cred.Flags.BackupState,
		CreatedAt:      now,
	}); err != nil {
		return store.Device{}, err
	}

	m.consumePairing()
	m.log(audit.Entry{
		Actor:  "device:" + dev.Name,
		Action: audit.ActionPair,
		Target: dev.Kind,
		Result: audit.ResultOK,
		Meta:   map[string]any{"deviceId": devID},
	})
	return dev, nil
}

// ---------- login ----------

// LoginBegin issues an assertion challenge for a paired node.
func (m *Manager) LoginBegin(ctx context.Context, nodeID string) (string, *protocol.CredentialAssertion, error) {
	if m.Lockdown() {
		return "", nil, ErrLockdown
	}
	if until, err := m.lockedUntil(ctx, nodeID); err != nil {
		return "", nil, err
	} else if until != nil {
		return "", nil, fmt.Errorf("%w until %s", ErrLockedOut, until.Format(time.RFC3339))
	}

	wa, err := m.webAuthn()
	if err != nil {
		return "", nil, err
	}
	o, err := m.loadOwner(ctx)
	if err != nil {
		return "", nil, err
	}
	if len(o.creds) == 0 {
		return "", nil, ErrNoSession
	}

	assertion, session, err := wa.BeginLogin(o, webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return "", nil, fmt.Errorf("auth: begin login: %w", err)
	}
	id, err := m.putCeremony(&ceremony{kind: ceremonyLogin, session: *session, nodeID: nodeID})
	if err != nil {
		return "", nil, err
	}
	return id, assertion, nil
}

// LoginFinish verifies an assertion and creates a session.
func (m *Manager) LoginFinish(ctx context.Context, ceremonyID, nodeID string, r *http.Request) (store.AuthSession, store.Device, error) {
	cfg := m.cfg()
	if cfg == nil {
		return store.AuthSession{}, store.Device{}, errors.New("auth: no config")
	}
	cred, err := m.assert(ctx, ceremonyID, ceremonyLogin, nodeID, r)
	if err != nil {
		m.recordFailure(ctx, nodeID, err)
		return store.AuthSession{}, store.Device{}, err
	}

	row, err := m.store.CredentialByID(ctx, credentialID(cred.ID))
	if err != nil {
		m.recordFailure(ctx, nodeID, nil)
		return store.AuthSession{}, store.Device{}, ErrCeremony
	}
	dev, err := m.store.DeviceByID(ctx, row.DeviceID)
	if err != nil {
		return store.AuthSession{}, store.Device{}, ErrCeremony
	}
	// The passkey must be used from the device it was paired on. Without this,
	// a passkey synced to another of the owner's devices via iCloud Keychain
	// would authenticate from a node that was never paired.
	if dev.NodeID != nodeID {
		m.log(audit.Entry{
			Actor:  "node:" + nodeID,
			Action: audit.ActionLoginFail,
			Result: audit.ResultDenied,
			Meta:   map[string]any{"reason": "credential belongs to a different device"},
		})
		m.recordFailure(ctx, nodeID, nil)
		return store.AuthSession{}, store.Device{}, ErrCeremony
	}

	now := m.now()
	sessID, err := randomID(32)
	if err != nil {
		return store.AuthSession{}, store.Device{}, err
	}
	sess := store.AuthSession{
		ID:         sessID,
		DeviceID:   dev.ID,
		NodeID:     nodeID,
		CreatedAt:  now,
		LastSeenAt: now,
		// A login is itself a fresh user-verified assertion, so it starts the
		// step-up window.
		StepupAt: &now,
	}
	if err := m.store.CreateAuthSession(ctx, sess); err != nil {
		return store.AuthSession{}, store.Device{}, err
	}
	if err := m.store.ClearLoginFailures(ctx, nodeID); err != nil {
		return store.AuthSession{}, store.Device{}, err
	}
	if err := m.store.TouchDevice(ctx, dev.ID, now); err != nil {
		return store.AuthSession{}, store.Device{}, err
	}

	m.log(audit.Entry{Actor: "device:" + dev.Name, Action: audit.ActionLogin, Result: audit.ResultOK})
	return sess, dev, nil
}

// ---------- step-up ----------

// StepupBegin issues a challenge for a dangerous action.
func (m *Manager) StepupBegin(ctx context.Context, nodeID string) (string, *protocol.CredentialAssertion, error) {
	id, assertion, err := m.LoginBegin(ctx, nodeID)
	if err != nil {
		return "", nil, err
	}
	// LoginBegin registered a login ceremony; re-key it as a step-up so a
	// step-up challenge cannot be redeemed for a new session, or vice versa.
	m.mu.Lock()
	if c, ok := m.ceremonies[id]; ok {
		c.kind = ceremonyStepup
	}
	m.mu.Unlock()
	return id, assertion, nil
}

// StepupFinish verifies the assertion and stamps the session.
func (m *Manager) StepupFinish(ctx context.Context, ceremonyID, nodeID, sessionID string, r *http.Request) error {
	sess, err := m.store.AuthSessionByID(ctx, sessionID)
	if err != nil {
		return ErrNoSession
	}
	cred, err := m.assert(ctx, ceremonyID, ceremonyStepup, nodeID, r)
	if err != nil {
		m.recordFailure(ctx, nodeID, err)
		return err
	}
	row, err := m.store.CredentialByID(ctx, credentialID(cred.ID))
	if err != nil || row.DeviceID != sess.DeviceID {
		// Stepping up with another device's passkey must not lift this
		// session's restrictions.
		return ErrCeremony
	}
	now := m.now()
	if err := m.store.MarkStepup(ctx, sessionID, now); err != nil {
		return err
	}
	m.log(audit.Entry{Actor: "device:" + sess.DeviceID, Action: audit.ActionStepup, Result: audit.ResultOK})
	return nil
}

// assert runs the shared half of login and step-up.
func (m *Manager) assert(ctx context.Context, ceremonyID string, kind ceremonyKind, nodeID string, r *http.Request) (*webauthn.Credential, error) {
	wa, err := m.webAuthn()
	if err != nil {
		return nil, err
	}
	c, err := m.takeCeremony(ceremonyID, kind, nodeID)
	if err != nil {
		return nil, err
	}
	o, err := m.loadOwner(ctx)
	if err != nil {
		return nil, err
	}
	// User verification (Face ID, Windows Hello) is enforced inside
	// FinishLogin: every ceremony begins with VerificationRequired, and the
	// library rejects an assertion without the UV flag. The check is on the
	// session here, not on the returned credential: since go-webauthn v0.18
	// cred.Flags.UserVerified is the stored "uvInitialized" value, which this
	// store does not keep, so testing it refused every login and step-up.
	if c.session.UserVerification != protocol.VerificationRequired {
		return nil, fmt.Errorf("%w: ceremony did not require user verification", ErrCeremony)
	}
	cred, err := wa.FinishLogin(o, c.session, r)
	if err != nil {
		return nil, errors.Join(ErrCeremony, err)
	}
	// A sign counter that has not advanced on an authenticator that uses one
	// suggests a cloned credential. go-webauthn flags it; refuse rather than
	// warn.
	if cred.Authenticator.CloneWarning {
		return nil, fmt.Errorf("%w: authenticator clone warning", ErrCeremony)
	}
	if err := m.store.UpdateCredentialUse(ctx, credentialID(cred.ID), cred.Authenticator.SignCount, m.now()); err != nil {
		return nil, err
	}
	return cred, nil
}

func (m *Manager) lockedUntil(ctx context.Context, nodeID string) (*time.Time, error) {
	until, err := m.store.LoginLockedUntil(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	if until == nil || m.now().After(*until) {
		return nil, nil
	}
	return until, nil
}

// cause, when known, goes into the audit entry. The API response stays vague
// on purpose (it would tell an attacker which check failed), so without this a
// failing passkey left the owner nothing to diagnose.
func (m *Manager) recordFailure(ctx context.Context, nodeID string, cause error) {
	cfg := m.cfg()
	if cfg == nil {
		return
	}
	f, err := m.store.RecordLoginFailure(ctx, nodeID, m.now(),
		cfg.Agent.Auth.LoginMaxFailures, cfg.Agent.Auth.Lockout())
	if err != nil {
		return
	}
	meta := map[string]any{"failures": f.Count}
	if cause != nil {
		meta["error"] = cause.Error()
	}
	if f.LockedUntil != nil {
		meta["lockedUntil"] = f.LockedUntil.Format(time.RFC3339)
	}
	m.log(audit.Entry{
		Actor:  "node:" + nodeID,
		Action: audit.ActionLoginFail,
		Result: audit.ResultDenied,
		Meta:   meta,
	})
}

// LoginFinishForPairing creates a session for a device that has just completed
// registration.
//
// Registration already proved user presence with a verified passkey, so
// requiring a second ceremony immediately afterwards would only make pairing
// feel broken. The session is still node-bound and still expires normally.
func (m *Manager) LoginFinishForPairing(ctx context.Context, dev store.Device, nodeID string) (store.AuthSession, store.Device, error) {
	if dev.NodeID != nodeID {
		return store.AuthSession{}, store.Device{}, ErrCeremony
	}
	now := m.now()
	id, err := randomID(32)
	if err != nil {
		return store.AuthSession{}, store.Device{}, err
	}
	sess := store.AuthSession{
		ID:         id,
		DeviceID:   dev.ID,
		NodeID:     nodeID,
		CreatedAt:  now,
		LastSeenAt: now,
		StepupAt:   &now,
	}
	if err := m.store.CreateAuthSession(ctx, sess); err != nil {
		return store.AuthSession{}, store.Device{}, err
	}
	m.log(audit.Entry{Actor: "device:" + dev.Name, Action: audit.ActionLogin,
		Result: audit.ResultOK, Meta: map[string]any{"viaPairing": true}})
	return sess, dev, nil
}
