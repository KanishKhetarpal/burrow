package web

import (
	"context"
	"io/fs"
	"net/http"
	"time"

	"burrow/internal/apps"
	"burrow/internal/audit"
	"burrow/internal/auth"
	"burrow/internal/config"
	"burrow/internal/files"
	"burrow/internal/identity"
	"burrow/internal/power"
	"burrow/internal/sessions"
	"burrow/internal/system"
)

// Version is stamped at build time by scripts/build.ps1.
var Version = "dev"

// Deps is everything the HTTP layer needs. Config and Origin are functions so
// the server picks up changes (a newly paired device, the FQDN arriving once
// tsnet is up) without a restart.
type Deps struct {
	Config   func() *config.Config
	Origin   func() string
	Resolver identity.Resolver
	Auth     *auth.Manager
	Sessions *sessions.Manager
	Files    *files.Service
	Power    *power.Manager
	System   system.Collector
	Audit    *audit.Logger
	// Static is the built PWA, embedded in the binary.
	Static fs.FS
	// SSHKeyAdder appends a public key to the right authorized_keys file.
	SSHKeyAdder func(ctx context.Context, publicKey string) (fingerprint string, err error)
	// Apps opens the "Open on PC" programs on the owner's desktop.
	Apps apps.Opener
	// Lockdown enters lockdown: kills sessions and stops sshd and SMB where it
	// can. Returns the parts it could not stop, which the response reports.
	Lockdown func(ctx context.Context, actor string) (couldNotStop []string, err error)
}

// Server holds the router and its dependencies.
type Server struct {
	deps Deps
	log  func(audit.Entry)
}

// NewServer builds the HTTP server.
func NewServer(deps Deps) *Server {
	s := &Server{deps: deps}
	s.log = func(e audit.Entry) {
		if deps.Audit == nil {
			return
		}
		// A failure to write the audit log must not fail the request that
		// caused it; the agent's own log carries the problem.
		_ = deps.Audit.Log(e)
	}
	return s
}

// Handler builds the full middleware stack and routes.
//
// The order is the control layers of docs/03-SECURITY.md §3, outermost first:
// security headers on every response, then the identity gate, then rate limits,
// then CSRF, then per-route session and step-up checks.
func (s *Server) Handler() http.Handler {
	gate := NewGate(GateDeps{
		Resolver:    s.deps.Resolver,
		Config:      s.deps.Config,
		PairingOpen: s.deps.Auth.PairingOpen,
		Lockdown:    s.deps.Auth.Lockdown,
		Audit:       s.log,
	})

	mux := http.NewServeMux()
	s.routes(mux)

	var h http.Handler = mux
	h = CSRF(s.deps.Origin, s.log)(h)
	h = exceptHealth(RateLimit(NewLimiter(APIRatePerMin, time.Minute), s.log)(h), h)
	h = gate.Middleware(h)
	h = SecurityHeaders(s.wakerOrigin())(h)
	return h
}

// exceptHealth sends /healthz past the API rate limit. The PWA polls it to
// decide whether the PC is on, so a 429 there read as "the PC is off" after an
// ordinary burst of use. It is still behind the identity gate, takes no input
// and returns two fields, so there is nothing to brute-force.
func exceptHealth(limited, unlimited http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			unlimited.ServeHTTP(w, r)
			return
		}
		limited.ServeHTTP(w, r)
	})
}

// wakerOrigin returns the waker's origin for the CSP, or "" when no waker is
// configured — which is the v1 case, and keeps connect-src at 'self'.
func (s *Server) wakerOrigin() string {
	cfg := s.deps.Config()
	if cfg == nil || cfg.Wake.Method != config.WakeWaker {
		return ""
	}
	return cfg.Wake.WakerURL
}

// routes registers every endpoint.
//
// Go's ServeMux resolves by pattern specificity, so a literal segment such as
// "/api/sessions/kill-all" wins over "/api/sessions/{id}" regardless of
// registration order. That removes the route-ordering trap the spec warns about
// in other frameworks, where "/api/sessions/{id}" would swallow it.
func (s *Server) routes(mux *http.ServeMux) {
	needSession := RequireSession(s.deps.Auth)
	needStepup := func(h http.Handler) http.Handler {
		return needSession(RequireStepup(s.deps.Auth, s.log)(h))
	}
	// authLimiter is separate and much tighter than the API limiter, because
	// these are the endpoints worth brute-forcing.
	authLimit := RateLimit(NewLimiter(AuthRatePerMin, time.Minute), s.log)

	// --- health and bootstrap ---
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.Handle("GET /api/wake-config", needSession(http.HandlerFunc(s.handleWakeConfig)))

	// --- auth ---
	mux.HandleFunc("GET /api/auth/state", s.handleAuthState)
	mux.Handle("POST /api/auth/pair/begin", authLimit(http.HandlerFunc(s.handlePairBegin)))
	mux.Handle("POST /api/auth/pair/finish", authLimit(http.HandlerFunc(s.handlePairFinish)))
	mux.Handle("POST /api/auth/login/begin", authLimit(http.HandlerFunc(s.handleLoginBegin)))
	mux.Handle("POST /api/auth/login/finish", authLimit(http.HandlerFunc(s.handleLoginFinish)))
	mux.Handle("POST /api/auth/stepup/begin", authLimit(needSession(http.HandlerFunc(s.handleStepupBegin))))
	mux.Handle("POST /api/auth/stepup/finish", authLimit(needSession(http.HandlerFunc(s.handleStepupFinish))))
	mux.Handle("POST /api/auth/logout", needSession(http.HandlerFunc(s.handleLogout)))
	mux.Handle("GET /api/auth/me", needSession(http.HandlerFunc(s.handleMe)))
	mux.Handle("POST /api/ssh-keys", needStepup(http.HandlerFunc(s.handleAddSSHKey)))

	// --- system and power ---
	mux.Handle("GET /api/system", needSession(http.HandlerFunc(s.handleSystem)))
	mux.Handle("POST /api/power", needSession(http.HandlerFunc(s.handlePower)))
	mux.Handle("POST /api/power/keep-awake", needSession(http.HandlerFunc(s.handleKeepAwake)))
	mux.Handle("POST /api/lockdown", needStepup(http.HandlerFunc(s.handleLockdown)))

	// --- projects ---
	mux.Handle("GET /api/projects", needSession(http.HandlerFunc(s.handleListProjects)))
	mux.Handle("GET /api/launchers", needSession(http.HandlerFunc(s.handleListLaunchers)))
	mux.Handle("GET /api/apps", needSession(http.HandlerFunc(s.handleListApps)))
	mux.Handle("POST /api/apps/{name}/open", needSession(http.HandlerFunc(s.handleOpenApp)))
	mux.Handle("GET /api/projects/candidates", needStepup(http.HandlerFunc(s.handleProjectCandidates)))
	mux.Handle("POST /api/projects", needStepup(http.HandlerFunc(s.handleAddProject)))
	mux.Handle("DELETE /api/projects/{id}", needStepup(http.HandlerFunc(s.handleRemoveProject)))

	// --- sessions ---
	mux.Handle("GET /api/sessions", needSession(http.HandlerFunc(s.handleListSessions)))
	mux.Handle("POST /api/sessions", needSession(http.HandlerFunc(s.handleCreateSession)))
	mux.Handle("POST /api/sessions/kill-all", needSession(http.HandlerFunc(s.handleKillAll)))
	mux.Handle("GET /api/sessions/{id}", needSession(http.HandlerFunc(s.handleGetSession)))
	mux.Handle("DELETE /api/sessions/{id}", needSession(http.HandlerFunc(s.handleDeleteSession)))
	mux.Handle("POST /api/sessions/{id}/input", needSession(http.HandlerFunc(s.handleSessionInput)))
	mux.Handle("POST /api/sessions/{id}/stop", needSession(http.HandlerFunc(s.handleStopSession)))
	mux.Handle("POST /api/sessions/{id}/resume", needSession(http.HandlerFunc(s.handleResumeSession)))
	mux.Handle("GET /api/sessions/{id}/scrollback", needSession(http.HandlerFunc(s.handleScrollback)))
	mux.Handle("GET /api/sessions/{id}/term", needSession(http.HandlerFunc(s.handleTerminal)))

	// --- files ---
	mux.Handle("GET /api/files/roots", needSession(http.HandlerFunc(s.handleFileRoots)))
	mux.Handle("GET /api/files/list", needSession(http.HandlerFunc(s.handleFileList)))
	mux.Handle("GET /api/files/download", needSession(http.HandlerFunc(s.handleFileDownload)))
	mux.Handle("GET /api/files/preview", needSession(http.HandlerFunc(s.handleFilePreview)))
	mux.Handle("GET /api/files/search", needSession(http.HandlerFunc(s.handleFileSearch)))
	mux.Handle("POST /api/files/upload", needSession(http.HandlerFunc(s.handleFileUpload)))
	mux.Handle("POST /api/files/mkdir", needSession(http.HandlerFunc(s.handleFileMkdir)))
	mux.Handle("POST /api/files/rename", needSession(http.HandlerFunc(s.handleFileRename)))
	mux.Handle("POST /api/files/delete", needStepup(http.HandlerFunc(s.handleFileDelete)))

	// --- security ---
	mux.Handle("GET /api/audit", needSession(http.HandlerFunc(s.handleAudit)))
	mux.Handle("GET /api/devices", needSession(http.HandlerFunc(s.handleListDevices)))
	mux.Handle("DELETE /api/devices/{id}", needStepup(http.HandlerFunc(s.handleRevokeDevice)))

	// --- the PWA ---
	// Registered last and on the root pattern, so it only sees what no API
	// route claimed. An unknown /api/ path must not fall through to the app
	// shell, or a typo in a fetch would return HTML and be reported as a
	// parse error rather than a 404.
	mux.Handle("GET /", s.staticHandler())
}

// handleHealthz is the only endpoint a waker node may reach, and the only one
// that answers during lockdown. It deliberately reveals nothing beyond
// liveness and version (docs/04-API.md §1).
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "version": Version})
}

// handleWakeConfig is cached by the service worker, so the PWA can show the
// Wake button while the PC — and therefore this endpoint — is unreachable.
func (s *Server) handleWakeConfig(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Config()
	if cfg == nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "no configuration")
		return
	}
	var wakerURL *string
	if cfg.Wake.Method == config.WakeWaker && cfg.Wake.WakerURL != "" {
		u := cfg.Wake.WakerURL
		wakerURL = &u
	}
	// This one response may be cached, since it is what makes the offline Wake
	// flow possible; every other /api/ response is no-store.
	w.Header().Set("Cache-Control", "public, max-age=300")
	WriteJSON(w, http.StatusOK, map[string]any{
		"method": cfg.Wake.Method,
		"shortcuts": map[string]string{
			"on":         cfg.Wake.Shortcuts.On,
			"off":        cfg.Wake.Shortcuts.Off,
			"powerCycle": cfg.Wake.Shortcuts.PowerCycle,
		},
		"wakeTimeoutSec":     cfg.Wake.WakeTimeoutSeconds,
		"powerCycleAfterSec": cfg.Wake.PowerCycleAfterSeconds,
		"offConfirmSec":      cfg.Wake.OffConfirmSeconds,
		"wakerUrl":           wakerURL,
		"routerHint":         cfg.Wake.RouterHint,
		"ssh": map[string]string{
			"user": cfg.SSH.User,
			"host": cfg.SSH.Host,
		},
	})
}
