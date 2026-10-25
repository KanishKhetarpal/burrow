package web

import (
	"errors"
	"net/http"
	"time"

	"burrow/internal/audit"
	"burrow/internal/power"
	"burrow/internal/sessions"
)

func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	info, err := s.deps.System.Collect()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "could not read system stats")
		return
	}
	running := s.deps.Sessions.RunningCount()

	WriteJSON(w, http.StatusOK, map[string]any{
		"hostname":     info.Hostname,
		"osVersion":    info.OSVersion,
		"uptimeSec":    info.UptimeSec,
		"cpuPct":       info.CPUPct,
		"ramUsedMB":    info.RAMUsedMB,
		"ramTotalMB":   info.RAMTotalMB,
		"disks":        info.Disks,
		"keepAwake":    s.deps.Power.KeepAwake(),
		"sessions":     map[string]int{"running": running},
		"tailscale":    map[string]bool{"online": true}, // this request arrived over the tailnet
		"version":      Version,
		"pendingPower": s.deps.Power.Pending(),
	})
}

type powerRequest struct {
	Action   string `json:"action"`
	DelaySec int    `json:"delaySec"`
	Force    bool   `json:"force"`
}

func (s *Server) handlePower(w http.ResponseWriter, r *http.Request) {
	var req powerRequest
	if !DecodeJSON(w, r, &req) {
		return
	}
	action, err := power.ParseAction(req.Action)
	if err != nil {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "unknown power action")
		return
	}

	// Shutdown and restart need step-up. The check is here rather than at the
	// route because the route also serves sleep and lock, which do not.
	if action == power.Shutdown || action == power.Restart {
		sess, ok := SessionFrom(r.Context())
		if !ok || !sess.StepupFresh(s.deps.Auth.Now(), s.deps.Auth.StepupWindow()) {
			s.log(audit.Entry{
				Actor: Actor(r), Action: audit.ActionDenyStepup,
				Target: r.URL.Path, Result: audit.ResultDenied,
				Meta: map[string]any{"action": string(action)},
			})
			WriteError(w, http.StatusForbidden, CodeStepupRequired, "confirm with Face ID to continue")
			return
		}
	}

	// Losing a running Claude session to an accidental shutdown is worth one
	// extra confirmation, so it has to be asked for explicitly.
	if !req.Force {
		if n := s.deps.Sessions.RunningCount(); n > 0 && (action == power.Shutdown ||
			action == power.Restart || action == power.Sleep || action == power.Hibernate) {
			WriteError(w, http.StatusConflict, CodeSessionsRun,
				"Claude sessions are running; confirm to continue")
			return
		}
	}

	pending, err := s.deps.Power.Do(action, time.Duration(req.DelaySec)*time.Second, Actor(r))
	if err != nil {
		switch {
		case errors.Is(err, power.ErrBadDelay):
			WriteError(w, http.StatusBadRequest, CodeBadRequest, "delay must be 0 to 600 seconds")
		case errors.Is(err, power.ErrLockUnavailable):
			// Honest rather than a silent success: the service runs in session
			// 0 and cannot reach the desktop.
			WriteError(w, http.StatusConflict, CodeConflict,
				"Burrow cannot lock the screen from the background service")
		default:
			WriteError(w, http.StatusInternalServerError, CodeInternal, "the power action failed")
		}
		return
	}

	WriteJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"scheduledAt": pending.ScheduledAt,
		"plugOffSafe": pending.PlugOffSafe,
		"wakeable":    power.WakeableAfter(action),
	})
}

type keepAwakeRequest struct {
	Enabled bool `json:"enabled"`
	Minutes int  `json:"minutes"`
}

func (s *Server) handleKeepAwake(w http.ResponseWriter, r *http.Request) {
	var req keepAwakeRequest
	if !DecodeJSON(w, r, &req) {
		return
	}
	if err := s.deps.Power.SetManual(req.Enabled, req.Minutes, Actor(r)); err != nil {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "minutes must be 0 to 720")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "keepAwake": s.deps.Power.KeepAwake()})
}

func (s *Server) handleLockdown(w http.ResponseWriter, r *http.Request) {
	actor := Actor(r)
	if s.deps.Lockdown == nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "lockdown is not configured")
		return
	}
	couldNotStop, err := s.deps.Lockdown(r.Context(), actor)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "could not enter lockdown")
		return
	}
	// Reported rather than hidden: the owner needs to know if sshd or the SMB
	// share is still up, because that is the difference between locked down and
	// mostly locked down (docs/04-API.md §3).
	WriteJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"couldNotStop": couldNotStop,
	})
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Config()
	type project struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Path  string `json:"path,omitempty"`
		IsGit bool   `json:"isGit"`
	}
	out := make([]project, 0, len(cfg.Projects))

	// Absolute paths are only shown with a fresh step-up: the path layout of
	// the machine is itself information worth withholding
	// (docs/03-SECURITY.md §3.4).
	showPaths := false
	if sess, ok := SessionFrom(r.Context()); ok {
		showPaths = sess.StepupFresh(s.deps.Auth.Now(), s.deps.Auth.StepupWindow())
	}

	for _, p := range cfg.Projects {
		item := project{
			ID:    sessions.ProjectID(p),
			Name:  p.Name,
			IsGit: isGitRepo(p.Path),
		}
		if showPaths {
			item.Path = p.Path
		}
		out = append(out, item)
	}
	WriteJSON(w, http.StatusOK, out)
}
