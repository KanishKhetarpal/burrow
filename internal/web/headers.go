package web

import (
	"net/http"
	"strings"
)

// CSPFor builds the Content-Security-Policy. Everything is served by the agent
// itself, so 'self' is the whole allowlist; a waker origin is added to
// connect-src only when a waker is actually configured
// (docs/03-SECURITY.md §4).
func CSPFor(wakerOrigin string) string {
	connect := "'self'"
	if wakerOrigin != "" {
		connect += " " + wakerOrigin
	}
	return strings.Join([]string{
		"default-src 'self'",
		"script-src 'self'",
		"style-src 'self'",
		"img-src 'self' data: blob:",
		"connect-src " + connect,
		"frame-ancestors 'none'",
		"base-uri 'none'",
		"form-action 'self'",
		"object-src 'none'",
	}, "; ")
}

// SecurityHeaders sets the headers required on every response, including error
// responses — which is why it wraps the whole router rather than individual
// handlers.
func SecurityHeaders(wakerOrigin string) func(http.Handler) http.Handler {
	csp := CSPFor(wakerOrigin)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Content-Security-Policy", csp)
			h.Set("Strict-Transport-Security", "max-age=31536000")
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), usb=(), serial=(), payment=()")
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
			h.Set("Cross-Origin-Resource-Policy", "same-origin")
			h.Set("X-Frame-Options", "DENY")
			// API responses must never be cached by the browser or a service
			// worker; the PWA's own worker is told the same thing, but the
			// header is the part an attacker cannot change.
			if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/wake-config" {
				h.Set("Cache-Control", "no-store")
			}
			next.ServeHTTP(w, r)
		})
	}
}

// safeInlineTypes are the only content types served without an attachment
// disposition. HTML and SVG are deliberately absent: both can carry script, and
// serving them inline from the agent's own origin would hand an uploaded file
// the app's cookies (docs/03-SECURITY.md §4).
var safeInlineTypes = map[string]bool{
	"image/jpeg":                true,
	"image/png":                 true,
	"image/gif":                 true,
	"image/webp":                true,
	"application/pdf":           true,
	"text/plain; charset=utf-8": true,
}

// InlineAllowed reports whether a content type may be shown in the browser.
func InlineAllowed(contentType string) bool {
	return safeInlineTypes[strings.ToLower(strings.TrimSpace(contentType))]
}
