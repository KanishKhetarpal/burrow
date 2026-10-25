package web

import (
	"context"
	"errors"
	"net/http"

	"burrow/internal/audit"
	"burrow/internal/auth"
	"burrow/internal/identity"
)

const ctxSession ctxKey = 100

// SessionFrom returns the validated session behind a request. Only handlers
// mounted behind RequireSession may call it.
func SessionFrom(ctx context.Context) (auth.Session, bool) {
	s, ok := ctx.Value(ctxSession).(auth.Session)
	return s, ok
}

// RequireSession rejects requests without a valid session cookie.
func RequireSession(m *auth.Manager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			node, ok := NodeFrom(r.Context())
			if !ok {
				// Not reachable in production: the gate always sets the node.
				// Failing closed rather than trusting the cookie alone.
				notFound(w)
				return
			}
			c, err := r.Cookie(auth.CookieName)
			if err != nil {
				WriteError(w, http.StatusUnauthorized, CodeAuthRequired, "sign in required")
				return
			}
			sess, err := m.Validate(r.Context(), c.Value, node.StableID)
			if err != nil {
				if errors.Is(err, auth.ErrLockdown) {
					notFound(w)
					return
				}
				// Clear the cookie so the PWA shows the unlock screen rather
				// than retrying with a session that will never work again.
				auth.ClearSessionCookie(w)
				WriteError(w, http.StatusUnauthorized, CodeAuthRequired, "sign in required")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxSession, sess)))
		})
	}
}

// RequireStepup guards the dangerous endpoints: shutdown, restart, delete,
// adding an SSH key, editing projects or roots, lockdown, revoking a device.
//
// It is a separate middleware rather than a check inside each handler so a new
// dangerous endpoint cannot be added without choosing, at the route, whether it
// needs step-up (docs/03-SECURITY.md §3.4).
func RequireStepup(m *auth.Manager, auditFn func(audit.Entry)) func(http.Handler) http.Handler {
	if auditFn == nil {
		auditFn = func(audit.Entry) {}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sess, ok := SessionFrom(r.Context())
			if !ok {
				WriteError(w, http.StatusUnauthorized, CodeAuthRequired, "sign in required")
				return
			}
			if !sess.StepupFresh(m.Now(), m.StepupWindow()) {
				auditFn(audit.Entry{
					Actor:  "device:" + sess.Device.Name,
					Action: audit.ActionDenyStepup,
					Target: r.URL.Path,
					Result: audit.ResultDenied,
					IP:     identity.HostOnly(r.RemoteAddr),
				})
				WriteError(w, http.StatusForbidden, CodeStepupRequired, "confirm with Face ID to continue")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Actor names the session's device for the audit log.
func Actor(r *http.Request) string {
	if s, ok := SessionFrom(r.Context()); ok {
		return "device:" + s.Device.Name
	}
	if n, ok := NodeFrom(r.Context()); ok {
		return "node:" + n.StableID
	}
	return "unknown"
}
