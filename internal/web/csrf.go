package web

import (
	"net/http"
	"strings"

	"burrow/internal/audit"
	"burrow/internal/identity"
)

// CSRFHeader is required on every state-changing request. A cross-site form
// post cannot set a custom header, and a cross-origin fetch that tries is
// stopped by the preflight, which Burrow never answers (docs/04-API.md §0).
const CSRFHeader = "X-Burrow"

// CSRFHeaderValue is the only accepted value.
const CSRFHeaderValue = "1"

// CSRF rejects state-changing requests that lack the custom header or carry a
// foreign Origin. allowedOrigin is the agent's own HTTPS origin.
func CSRF(allowedOrigin func() string, auditFn func(audit.Entry)) func(http.Handler) http.Handler {
	if auditFn == nil {
		auditFn = func(audit.Entry) {}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isSafeMethod(r.Method) {
				next.ServeHTTP(w, r)
				return
			}

			deny := func(reason string) {
				actor := "node:unknown"
				if n, ok := NodeFrom(r.Context()); ok {
					actor = "node:" + n.StableID
				}
				auditFn(audit.Entry{
					Actor:  actor,
					Action: audit.ActionDenyCSRF,
					Target: r.URL.Path,
					Result: audit.ResultDenied,
					IP:     identity.HostOnly(r.RemoteAddr),
					Meta:   map[string]any{"reason": reason, "method": r.Method},
				})
				WriteError(w, http.StatusForbidden, "csrf", "request rejected")
			}

			if r.Header.Get(CSRFHeader) != CSRFHeaderValue {
				deny("missing or wrong " + CSRFHeader + " header")
				return
			}

			// Origin is absent on some same-origin requests from older
			// browsers, so Referer is accepted as a fallback. Both are checked
			// against an exact match; a prefix check would accept
			// https://agent.ts.net.evil.com.
			want := allowedOrigin()
			origin := r.Header.Get("Origin")
			switch {
			case origin != "":
				if want == "" || origin != want {
					deny("origin " + origin + " is not this agent")
					return
				}
			case r.Header.Get("Referer") != "":
				ref := r.Header.Get("Referer")
				if want == "" || !strings.HasPrefix(ref, want+"/") && ref != want {
					deny("referer is not this agent")
					return
				}
			default:
				deny("no Origin or Referer")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func isSafeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}
