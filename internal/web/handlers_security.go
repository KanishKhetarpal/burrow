package web

import (
	"net/http"
	"strconv"

	"burrow/internal/audit"
	"burrow/internal/auth"
	"burrow/internal/config"
)

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Config()
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)

	entries, err := audit.Read(cfg.Audit.Path, limit, before)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "could not read the audit log")
		return
	}

	// The hash chain is deliberately left out of the response: it is a wall of
	// hex a phone can do nothing with, and `burrow audit verify` on the PC is
	// what actually checks it.
	type row struct {
		Seq    int64          `json:"seq"`
		TS     string         `json:"ts"`
		Actor  string         `json:"actor"`
		Action audit.Action   `json:"action"`
		Target string         `json:"target,omitempty"`
		Result audit.Result   `json:"result"`
		Meta   map[string]any `json:"meta,omitempty"`
	}
	out := make([]row, 0, len(entries))
	for _, e := range entries {
		out = append(out, row{
			Seq: e.Seq, TS: e.TS.Format("2006-01-02T15:04:05Z07:00"),
			Actor: e.Actor, Action: e.Action, Target: e.Target,
			Result: e.Result, Meta: e.Meta,
		})
	}
	WriteJSON(w, http.StatusOK, map[string]any{"entries": out})
}

func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Config()
	current, _ := DeviceFrom(r.Context())

	type device struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Kind     string `json:"kind"`
		NodeTail string `json:"nodeTail"`
		Current  bool   `json:"current"`
	}
	out := make([]device, 0, len(cfg.Device.Allowed))
	for _, d := range cfg.Device.Allowed {
		out = append(out, device{
			ID:   d.NodeID,
			Name: d.Name,
			Kind: string(d.Kind),
			// Only the tail of the node id: enough to tell two devices apart,
			// without putting a full identifier on screen.
			NodeTail: tailOf(d.NodeID, 6),
			Current:  d.NodeID == current.NodeID,
		})
	}
	WriteJSON(w, http.StatusOK, out)
}

func (s *Server) handleRevokeDevice(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("id")
	cfg := s.deps.Config()

	remaining := make([]config.AllowedDevice, 0, len(cfg.Device.Allowed))
	found := false
	for _, d := range cfg.Device.Allowed {
		if d.NodeID == nodeID {
			found = true
			continue
		}
		remaining = append(remaining, d)
	}
	if !found {
		WriteError(w, http.StatusNotFound, CodeNotFound, "no such device")
		return
	}
	// Revoking the last device would lock the owner out of the app entirely,
	// with pairing reachable only from the PC console. Refuse rather than let
	// one tap do that.
	if len(remaining) == 0 {
		WriteError(w, http.StatusConflict, CodeConflict,
			"that is your only paired device; pair another one first")
		return
	}

	// The store holds the passkey and the sessions, so they are revoked too.
	// Done before the config is written, so a failure here leaves the device
	// still allowed rather than allowed-but-unusable.
	if dev, err := s.deps.Auth.DeviceByNode(r.Context(), nodeID); err == nil {
		if err := s.deps.Auth.RevokeDevice(r.Context(), dev.ID); err != nil {
			WriteError(w, http.StatusInternalServerError, CodeInternal, "could not revoke the passkey")
			return
		}
	}

	previous := cfg.Device.Allowed
	cfg.Device.Allowed = remaining
	if err := config.SaveDevices(cfg); err != nil {
		cfg.Device.Allowed = previous
		WriteError(w, http.StatusInternalServerError, CodeInternal, "could not save the change")
		return
	}

	s.log(audit.Entry{
		Actor: Actor(r), Action: audit.ActionDeviceRevoke,
		Target: tailOf(nodeID, 6), Result: audit.ResultOK,
	})

	// Revoking the device in use ends this session immediately.
	if current, ok := DeviceFrom(r.Context()); ok && current.NodeID == nodeID {
		if sess, ok := SessionFrom(r.Context()); ok {
			_ = s.deps.Auth.Logout(r.Context(), sess.ID)
		}
		auth.ClearSessionCookie(w)
	}
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// tailOf returns the last n characters of s, marked as a fragment.
func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
