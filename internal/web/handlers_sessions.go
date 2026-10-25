package web

import (
	"encoding/base64"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"burrow/internal/audit"
	"burrow/internal/config"
	"burrow/internal/identity"
	"burrow/internal/sessions"
)

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, s.deps.Sessions.List())
}

type createSessionRequest struct {
	ProjectID      string `json:"projectId"`
	Name           string `json:"name"`
	Mode           string `json:"mode"`
	PermissionMode string `json:"permissionMode"`
	Spawn          string `json:"spawn"`
	Launcher       string `json:"launcher"`
}

// handleListLaunchers returns the one-tap buttons defined in burrow.yaml.
// Config directories are not sent: the phone only needs a name to show.
func (s *Server) handleListLaunchers(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Config()
	type launcher struct {
		Name           string `json:"name"`
		ProjectID      string `json:"projectId,omitempty"`
		PermissionMode string `json:"permissionMode,omitempty"`
		// OneTap means the button has a folder of its own, so the app starts
		// the session straight away instead of asking for a project.
		OneTap bool `json:"oneTap"`
	}
	out := make([]launcher, 0, len(cfg.Launchers))
	for _, l := range cfg.Launchers {
		item := launcher{Name: l.Name, PermissionMode: l.PermissionMode, OneTap: l.Path != ""}
		for _, p := range cfg.Projects {
			if strings.EqualFold(p.Name, l.Project) {
				item.ProjectID = sessions.ProjectID(p)
				item.OneTap = true
			}
		}
		out = append(out, item)
	}
	WriteJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req createSessionRequest
	if !DecodeJSON(w, r, &req) {
		return
	}
	// A session with no permission prompts can do anything the owner's
	// account can, so starting one needs Face ID every time, like shutdown.
	// A launcher's own default counts, not just what the request says.
	effectiveMode := req.PermissionMode
	if effectiveMode == "" && req.Launcher != "" {
		if l, ok := s.deps.Config().Launcher(req.Launcher); ok {
			effectiveMode = l.PermissionMode
		}
	}
	if effectiveMode == "bypassPermissions" {
		cur, ok := SessionFrom(r.Context())
		if !ok || !cur.StepupFresh(s.deps.Auth.Now(), s.deps.Auth.StepupWindow()) {
			s.log(audit.Entry{
				Actor:  Actor(r),
				Action: audit.ActionDenyStepup,
				Target: r.URL.Path,
				Result: audit.ResultDenied,
				IP:     identity.HostOnly(r.RemoteAddr),
				Meta:   map[string]any{"permissionMode": req.PermissionMode},
			})
			WriteError(w, http.StatusForbidden, CodeStepupRequired, "confirm with Face ID to continue")
			return
		}
	}
	sess, err := s.deps.Sessions.Start(sessions.StartOptions{
		ProjectID:      req.ProjectID,
		Name:           req.Name,
		Mode:           sessions.Mode(req.Mode),
		PermissionMode: req.PermissionMode,
		Spawn:          sessions.Spawn(req.Spawn),
		Actor:          Actor(r),
		Launcher:       req.Launcher,
	})
	if err != nil {
		// Recorded with the cause, which the response deliberately omits:
		// without it a failed start left no trace anywhere (the binary not
		// being found looked exactly like a bad request).
		s.log(audit.Entry{
			Actor:  Actor(r),
			Action: audit.ActionSessionStart,
			Target: req.ProjectID,
			Result: audit.ResultError,
			IP:     identity.HostOnly(r.RemoteAddr),
			Meta:   map[string]any{"error": err.Error()},
		})
		s.writeSessionError(w, err)
		return
	}
	WriteJSON(w, http.StatusCreated, sess)
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	sess, err := s.deps.Sessions.Get(r.PathValue("id"))
	if err != nil {
		s.writeSessionError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, sess)
}

type sessionInputRequest struct {
	Text string `json:"text"`
	Key  string `json:"key"`
}

func (s *Server) handleSessionInput(w http.ResponseWriter, r *http.Request) {
	var req sessionInputRequest
	if !DecodeJSON(w, r, &req) {
		return
	}
	id := r.PathValue("id")

	var err error
	switch {
	case req.Key != "":
		err = s.deps.Sessions.SendKey(id, req.Key, Actor(r))
	case req.Text != "":
		err = s.deps.Sessions.SendText(id, req.Text, Actor(r))
	default:
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "send either text or key")
		return
	}
	if err != nil {
		s.writeSessionError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleStopSession(w http.ResponseWriter, r *http.Request) {
	if err := s.deps.Sessions.Stop(r.Context(), r.PathValue("id"), Actor(r)); err != nil {
		s.writeSessionError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleResumeSession(w http.ResponseWriter, r *http.Request) {
	sess, err := s.deps.Sessions.Resume(r.PathValue("id"), Actor(r))
	if err != nil {
		s.writeSessionError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, sess)
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	if err := s.deps.Sessions.Delete(r.PathValue("id")); err != nil {
		s.writeSessionError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleKillAll(w http.ResponseWriter, r *http.Request) {
	killed := s.deps.Sessions.KillAll(Actor(r))
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "killed": killed})
}

func (s *Server) handleScrollback(w http.ResponseWriter, r *http.Request) {
	b, err := s.deps.Sessions.Scrollback(r.PathValue("id"))
	if err != nil {
		s.writeSessionError(w, err)
		return
	}
	// Base64 rather than raw: the scrollback is arbitrary bytes including
	// escape sequences and partial UTF-8, which JSON cannot carry intact.
	WriteJSON(w, http.StatusOK, map[string]any{
		"bytes": base64.StdEncoding.EncodeToString(b),
		"len":   len(b),
	})
}

func (s *Server) writeSessionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sessions.ErrNotFound):
		WriteError(w, http.StatusNotFound, CodeNotFound, "no such session")
	case errors.Is(err, sessions.ErrMaxSessions):
		WriteError(w, http.StatusConflict, CodeMaxSessions, "too many sessions are running; stop one first")
	case errors.Is(err, sessions.ErrNotRunning):
		WriteError(w, http.StatusConflict, CodeConflict, "that session is not running")
	case errors.Is(err, sessions.ErrStillRuns):
		WriteError(w, http.StatusConflict, CodeConflict, "that session is still running")
	case errors.Is(err, config.ErrBypassDisabled):
		WriteError(w, http.StatusBadRequest, CodeBadRequest,
			"bypass permissions is not switched on for this PC (allow_bypass_permissions in burrow.yaml)")
	case errors.Is(err, sessions.ErrBadLauncher):
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "unknown launcher")
	case errors.Is(err, sessions.ErrBadProject):
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "unknown project")
	default:
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "could not complete that request")
	}
}

// isGitRepo reports whether a directory is a git working tree, so the PWA can
// offer the worktree option only where it would work.
func isGitRepo(dir string) bool {
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}
