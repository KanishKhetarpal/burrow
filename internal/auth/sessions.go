package auth

import (
	"context"
	"errors"
	"net/http"
	"time"

	"burrow/internal/store"
)

// CookieName is prefixed `__Host-`, which binds it to this exact host over
// HTTPS with Path=/ and forbids a Domain attribute. A subdomain cannot set or
// read it (docs/03-SECURITY.md §3.3).
const CookieName = "__Host-burrow"

// SetSessionCookie writes the session cookie.
func SetSessionCookie(w http.ResponseWriter, sessionID string, absoluteLifetime time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    sessionID,
		Path:     "/",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		// MaxAge matches the absolute lifetime. The idle timeout is enforced
		// server-side, because a cookie cannot express it.
		MaxAge: int(absoluteLifetime.Seconds()),
	})
}

// ClearSessionCookie expires the cookie on logout.
func ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

// Session is a validated session plus its device.
type Session struct {
	store.AuthSession
	Device store.Device
}

// StepupFresh reports whether the session's last passkey assertion is recent
// enough for a step-up endpoint.
func (s Session) StepupFresh(now time.Time, window time.Duration) bool {
	if s.StepupAt == nil {
		return false
	}
	return now.Sub(*s.StepupAt) <= window
}

// Validate checks a session id against every expiry rule and the node it is
// bound to.
//
// Binding to the node matters: a stolen cookie replayed from another device on
// the tailnet must not work, even if that device is the other paired one.
func (m *Manager) Validate(ctx context.Context, sessionID, nodeID string) (Session, error) {
	if m.Lockdown() {
		return Session{}, ErrLockdown
	}
	cfg := m.cfg()
	if cfg == nil {
		return Session{}, errors.New("auth: no config")
	}
	if sessionID == "" {
		return Session{}, ErrNoSession
	}

	sess, err := m.store.AuthSessionByID(ctx, sessionID)
	if err != nil {
		return Session{}, ErrNoSession
	}
	if sess.NodeID != nodeID {
		return Session{}, ErrNoSession
	}

	now := m.now()
	a := cfg.Agent.Auth
	if now.Sub(sess.CreatedAt) > a.AbsoluteTimeout() {
		return Session{}, ErrNoSession
	}
	if now.Sub(sess.LastSeenAt) > a.IdleTimeout() {
		return Session{}, ErrNoSession
	}

	dev, err := m.store.DeviceByID(ctx, sess.DeviceID)
	if err != nil || dev.RevokedAt != nil {
		return Session{}, ErrNoSession
	}

	// Slide the idle window. Done after every check so an expired session is
	// never revived by the act of presenting it.
	if err := m.store.TouchAuthSession(ctx, sess.ID, now); err != nil {
		return Session{}, err
	}
	sess.LastSeenAt = now

	return Session{AuthSession: sess, Device: dev}, nil
}

// Logout revokes a session.
func (m *Manager) Logout(ctx context.Context, sessionID string) error {
	return m.store.RevokeAuthSession(ctx, sessionID, m.now())
}

// RevokeDevice revokes a device and all its sessions.
func (m *Manager) RevokeDevice(ctx context.Context, deviceID string) error {
	return m.store.RevokeDevice(ctx, deviceID, m.now())
}

// StepupWindow is the configured freshness window for step-up.
func (m *Manager) StepupWindow() time.Duration {
	cfg := m.cfg()
	if cfg == nil {
		return 5 * time.Minute
	}
	return cfg.Agent.Auth.StepupWindow()
}

// AbsoluteLifetime is the configured hard session lifetime.
func (m *Manager) AbsoluteLifetime() time.Duration {
	cfg := m.cfg()
	if cfg == nil {
		return 12 * time.Hour
	}
	return cfg.Agent.Auth.AbsoluteTimeout()
}

// Now exposes the manager's clock, so handlers and tests agree on the time.
func (m *Manager) Now() time.Time { return m.now() }

// PurgeExpired drops sessions past their absolute lifetime. Called periodically
// by the agent.
func (m *Manager) PurgeExpired(ctx context.Context) error {
	_, err := m.store.PurgeExpiredAuthSessions(ctx, m.now().Add(-m.AbsoluteLifetime()))
	return err
}

// DeviceByNode returns the live device paired to a Tailscale node.
func (m *Manager) DeviceByNode(ctx context.Context, nodeID string) (store.Device, error) {
	return m.store.DeviceByNodeID(ctx, nodeID)
}

// ListDevices returns every live paired device.
func (m *Manager) ListDevices(ctx context.Context) ([]store.Device, error) {
	return m.store.ListDevices(ctx)
}
