package web

import (
	"errors"
	"net/http"
	"strings"

	"burrow/internal/audit"
	"burrow/internal/auth"
	"burrow/internal/config"
)

func (s *Server) handleAuthState(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Config()
	node, _ := NodeFrom(r.Context())

	authenticated := false
	if c, err := r.Cookie(auth.CookieName); err == nil {
		if _, err := s.deps.Auth.Validate(r.Context(), c.Value, node.StableID); err == nil {
			authenticated = true
		}
	}
	_, paired := DeviceFrom(r.Context())

	WriteJSON(w, http.StatusOK, map[string]any{
		"paired":        paired,
		"pairingOpen":   s.deps.Auth.PairingOpen(),
		"authenticated": authenticated,
		"lockdown":      s.deps.Auth.Lockdown(),
		"maxDevices":    cfg.Device.MaxDevices,
	})
}

type pairBeginRequest struct {
	Code       string `json:"code"`
	DeviceName string `json:"deviceName"`
	Kind       string `json:"kind"`
}

func (s *Server) handlePairBegin(w http.ResponseWriter, r *http.Request) {
	// The gate allows an unpaired owner node here only while a window opened
	// from the PC console is live.
	if !IsPairingCandidate(r.Context()) {
		if _, already := DeviceFrom(r.Context()); already {
			WriteError(w, http.StatusConflict, CodeConflict, "this device is already paired")
			return
		}
		notFound(w)
		return
	}
	var req pairBeginRequest
	if !DecodeJSON(w, r, &req) {
		return
	}
	node, _ := NodeFrom(r.Context())

	ceremonyID, creation, err := s.deps.Auth.PairBegin(r.Context(),
		node.StableID, node.Login, req.Code, req.DeviceName, req.Kind)
	if err != nil {
		s.writeAuthError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"ceremonyId": ceremonyID,
		"publicKey":  creation.Response,
	})
}

func (s *Server) handlePairFinish(w http.ResponseWriter, r *http.Request) {
	if !IsPairingCandidate(r.Context()) {
		notFound(w)
		return
	}
	// The ceremony id travels in a header rather than the body, because the
	// body is the raw WebAuthn attestation that the library parses itself.
	ceremonyID := r.Header.Get("X-Burrow-Ceremony")
	if ceremonyID == "" {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "missing ceremony id")
		return
	}
	node, _ := NodeFrom(r.Context())

	dev, err := s.deps.Auth.PairFinish(r.Context(), ceremonyID, node.StableID, node.Login, r)
	if err != nil {
		s.writeAuthError(w, err)
		return
	}

	// Pairing records the device in the config so the identity gate accepts it
	// from the next request on.
	if err := s.persistDevice(dev.Name, dev.Kind, node.StableID); err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "could not save the device")
		return
	}

	sess, _, err := s.deps.Auth.LoginFinishForPairing(r.Context(), dev, node.StableID)
	if err != nil {
		// The device is paired; it just has to unlock once more.
		WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "signedIn": false})
		return
	}
	auth.SetSessionCookie(w, sess.ID, s.deps.Auth.AbsoluteLifetime())
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "signedIn": true})
}

func (s *Server) handleLoginBegin(w http.ResponseWriter, r *http.Request) {
	node, _ := NodeFrom(r.Context())
	ceremonyID, assertion, err := s.deps.Auth.LoginBegin(r.Context(), node.StableID)
	if err != nil {
		s.writeAuthError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"ceremonyId": ceremonyID,
		"publicKey":  assertion.Response,
	})
}

func (s *Server) handleLoginFinish(w http.ResponseWriter, r *http.Request) {
	ceremonyID := r.Header.Get("X-Burrow-Ceremony")
	if ceremonyID == "" {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "missing ceremony id")
		return
	}
	node, _ := NodeFrom(r.Context())

	sess, dev, err := s.deps.Auth.LoginFinish(r.Context(), ceremonyID, node.StableID, r)
	if err != nil {
		s.writeAuthError(w, err)
		return
	}
	auth.SetSessionCookie(w, sess.ID, s.deps.Auth.AbsoluteLifetime())
	WriteJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"device": map[string]string{"id": dev.ID, "name": dev.Name, "kind": dev.Kind},
	})
}

func (s *Server) handleStepupBegin(w http.ResponseWriter, r *http.Request) {
	node, _ := NodeFrom(r.Context())
	ceremonyID, assertion, err := s.deps.Auth.StepupBegin(r.Context(), node.StableID)
	if err != nil {
		s.writeAuthError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"ceremonyId": ceremonyID,
		"publicKey":  assertion.Response,
	})
}

func (s *Server) handleStepupFinish(w http.ResponseWriter, r *http.Request) {
	ceremonyID := r.Header.Get("X-Burrow-Ceremony")
	if ceremonyID == "" {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "missing ceremony id")
		return
	}
	node, _ := NodeFrom(r.Context())
	sess, ok := SessionFrom(r.Context())
	if !ok {
		WriteError(w, http.StatusUnauthorized, CodeAuthRequired, "sign in required")
		return
	}
	if err := s.deps.Auth.StepupFinish(r.Context(), ceremonyID, node.StableID, sess.ID, r); err != nil {
		s.writeAuthError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	sess, _ := SessionFrom(r.Context())
	if err := s.deps.Auth.Logout(r.Context(), sess.ID); err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "could not sign out")
		return
	}
	auth.ClearSessionCookie(w)
	s.log(audit.Entry{Actor: Actor(r), Action: audit.ActionLogout, Result: audit.ResultOK})
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	sess, _ := SessionFrom(r.Context())
	cfg := s.deps.Config()

	var stepupUntil any
	if sess.StepupAt != nil {
		stepupUntil = sess.StepupAt.Add(s.deps.Auth.StepupWindow())
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"login": cfg.Owner.TailscaleLogin,
		"device": map[string]string{
			"id": sess.Device.ID, "name": sess.Device.Name, "kind": sess.Device.Kind,
		},
		"stepupValidUntil": stepupUntil,
		// Tells the PWA whether to offer the bypass option at all.
		"bypassPermissionsAllowed": cfg.Claude.AllowBypassPermissions,
	})
}

type sshKeyRequest struct {
	PublicKey string `json:"publicKey"`
}

// allowedKeyTypes are the only SSH key types accepted. RSA is absent
// deliberately: there is no reason to add a weaker key here, and the two
// devices both support ed25519 (docs/04-API.md §2).
var allowedKeyTypes = []string{
	"ssh-ed25519",
	"ecdsa-sha2-nistp256",
	"sk-ssh-ed25519@openssh.com",
	"sk-ecdsa-sha2-nistp256@openssh.com",
}

func (s *Server) handleAddSSHKey(w http.ResponseWriter, r *http.Request) {
	var req sshKeyRequest
	if !DecodeJSON(w, r, &req) {
		return
	}
	key := strings.TrimSpace(req.PublicKey)
	if key == "" {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "no public key")
		return
	}
	// A newline would let one request append several authorized_keys lines,
	// including options like command= that change what the key can do.
	if strings.ContainsAny(key, "\r\n") {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "a key must be a single line")
		return
	}
	fields := strings.Fields(key)
	if len(fields) < 2 {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "malformed public key")
		return
	}
	allowed := false
	for _, t := range allowedKeyTypes {
		if fields[0] == t {
			allowed = true
			break
		}
	}
	if !allowed {
		WriteError(w, http.StatusBadRequest, CodeBadRequest,
			"key type not allowed; use ed25519, ecdsa-nistp256 or a security key")
		return
	}
	if s.deps.SSHKeyAdder == nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "adding SSH keys is not configured")
		return
	}

	fp, err := s.deps.SSHKeyAdder(r.Context(), key)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "could not add the key")
		return
	}
	s.log(audit.Entry{
		Actor: Actor(r), Action: audit.ActionSSHKeyAdd,
		Target: fp, Result: audit.ResultOK,
		Meta: map[string]any{"type": fields[0]},
	})
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "fingerprint": fp})
}

// persistDevice adds a newly paired device to the live config and writes it
// back, so the allowlist survives a restart.
func (s *Server) persistDevice(name, kind, nodeID string) error {
	cfg := s.deps.Config()
	cfg.Device.Allowed = append(cfg.Device.Allowed, config.AllowedDevice{
		Name:   name,
		Kind:   config.DeviceKind(kind),
		NodeID: nodeID,
	})
	s.log(audit.Entry{
		Actor: "device:" + name, Action: audit.ActionConfigChange,
		Target: "device.allowed", Result: audit.ResultOK,
	})
	return config.SaveDevices(cfg)
}

// writeAuthError maps the auth package's errors to API codes.
func (s *Server) writeAuthError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrLockdown):
		notFound(w)
	case errors.Is(err, auth.ErrLockedOut):
		WriteError(w, http.StatusTooManyRequests, CodeLockedOut,
			"too many failed attempts; try again shortly")
	case errors.Is(err, auth.ErrPairingClosed):
		WriteError(w, http.StatusForbidden, CodeForbidden,
			"the pairing window is closed; run `burrow pair` on the PC")
	case errors.Is(err, auth.ErrPairingCode):
		WriteError(w, http.StatusForbidden, CodeForbidden, "wrong code")
	case errors.Is(err, auth.ErrTooManyDevices):
		WriteError(w, http.StatusConflict, CodeConflict,
			"the device limit is reached; revoke one first")
	case errors.Is(err, auth.ErrStepupRequired):
		WriteError(w, http.StatusForbidden, CodeStepupRequired, "confirm with Face ID to continue")
	case errors.Is(err, auth.ErrNoSession):
		WriteError(w, http.StatusUnauthorized, CodeAuthRequired, "sign in required")
	case errors.Is(err, auth.ErrCeremony):
		// Deliberately vague: the detail would tell an attacker which part of
		// the ceremony failed.
		WriteError(w, http.StatusForbidden, CodeForbidden, "could not verify the passkey")
	default:
		WriteError(w, http.StatusInternalServerError, CodeInternal, "something went wrong")
	}
}
