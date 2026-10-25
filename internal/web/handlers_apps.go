package web

import (
	"errors"
	"net/http"

	"burrow/internal/apps"
	"burrow/internal/audit"
	"burrow/internal/identity"
)

// handleListApps returns the "Open on PC" buttons from burrow.yaml. Only
// names go to the phone; program paths and arguments stay on the PC.
func (s *Server) handleListApps(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Config()
	out := make([]map[string]string, 0, len(cfg.Apps))
	for _, a := range cfg.Apps {
		out = append(out, map[string]string{"name": a.Name})
	}
	WriteJSON(w, http.StatusOK, out)
}

// handleOpenApp starts one listed app on the owner's desktop. The name must
// be in burrow.yaml; nothing the phone sends becomes part of a command line.
func (s *Server) handleOpenApp(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.deps.Config().App(name); !ok || s.deps.Apps == nil {
		WriteError(w, http.StatusNotFound, CodeNotFound, "no such app")
		return
	}
	err := s.deps.Apps.Open(name)
	result := audit.ResultOK
	if err != nil {
		result = audit.ResultError
	}
	s.log(audit.Entry{
		Actor:  Actor(r),
		Action: audit.ActionAppOpen,
		Target: name,
		Result: result,
		IP:     identity.HostOnly(r.RemoteAddr),
	})
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, apps.ErrNoDesktop):
		WriteError(w, http.StatusConflict, CodeConflict,
			"nobody is logged into Windows on the PC, so there is no desktop to open it on")
	case errors.Is(err, apps.ErrUnknown):
		WriteError(w, http.StatusNotFound, CodeNotFound, "no such app")
	default:
		WriteError(w, http.StatusInternalServerError, CodeInternal, "could not open it")
	}
}
