package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"burrow/internal/audit"
)

const testOrigin = "https://n-ab12cd34.tail1234.ts.net"

func csrfHandler(t *testing.T) http.Handler {
	t.Helper()
	return CSRF(func() string { return testOrigin }, nil)(okHandler())
}

func TestCSRFAllowsSafeMethods(t *testing.T) {
	h := csrfHandler(t)
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		r := httptest.NewRequest(m, "/api/system", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("%s got %d, want 200 (no CSRF check on a safe method)", m, w.Code)
		}
	}
}

func TestCSRFRejects(t *testing.T) {
	cases := []struct {
		name    string
		header  string
		origin  string
		referer string
	}{
		{name: "missing X-Burrow header", origin: testOrigin},
		{name: "wrong X-Burrow value", header: "0", origin: testOrigin},
		{name: "foreign origin", header: CSRFHeaderValue, origin: "https://evil.example"},
		{name: "origin that merely starts with ours", header: CSRFHeaderValue, origin: testOrigin + ".evil.example"},
		{name: "no origin and no referer", header: CSRFHeaderValue},
		{name: "foreign referer", header: CSRFHeaderValue, referer: "https://evil.example/x"},
	}
	h := csrfHandler(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/sessions", nil)
			if tc.header != "" {
				r.Header.Set(CSRFHeader, tc.header)
			}
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.referer != "" {
				r.Header.Set("Referer", tc.referer)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatalf("got %d, want 403", w.Code)
			}
			if !strings.Contains(w.Body.String(), `"csrf"`) {
				t.Fatalf("body = %q, want the csrf error code", w.Body.String())
			}
		})
	}
}

func TestCSRFAccepts(t *testing.T) {
	h := csrfHandler(t)

	r := httptest.NewRequest(http.MethodPost, "/api/sessions", nil)
	r.Header.Set(CSRFHeader, CSRFHeaderValue)
	r.Header.Set("Origin", testOrigin)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("same-origin POST got %d, want 200", w.Code)
	}

	// Referer is the fallback when Origin is absent.
	r = httptest.NewRequest(http.MethodPost, "/api/sessions", nil)
	r.Header.Set(CSRFHeader, CSRFHeaderValue)
	r.Header.Set("Referer", testOrigin+"/sessions")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("POST with a matching Referer got %d, want 200", w.Code)
	}
}

func TestCSRFAuditsDenials(t *testing.T) {
	var entries []audit.Entry
	h := CSRF(func() string { return testOrigin }, func(e audit.Entry) { entries = append(entries, e) })(okHandler())

	r := httptest.NewRequest(http.MethodPost, "/api/power", nil)
	h.ServeHTTP(httptest.NewRecorder(), r)

	if len(entries) != 1 || entries[0].Action != audit.ActionDenyCSRF {
		t.Fatalf("audit = %+v, want one deny.csrf entry", entries)
	}
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	// Deliberately an erroring handler: the headers must be set regardless of
	// what the handler does.
	h := SecurityHeaders("")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "boom")
	}))
	r := httptest.NewRequest(http.MethodGet, "/api/system", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	want := map[string]string{
		"Strict-Transport-Security":    "max-age=31536000",
		"X-Content-Type-Options":       "nosniff",
		"Referrer-Policy":              "no-referrer",
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
		"X-Frame-Options":              "DENY",
		"Cache-Control":                "no-store",
	}
	for k, v := range want {
		if got := w.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	csp := w.Header().Get("Content-Security-Policy")
	for _, must := range []string{
		"default-src 'self'", "script-src 'self'", "frame-ancestors 'none'",
		"base-uri 'none'", "connect-src 'self'", "object-src 'none'",
	} {
		if !strings.Contains(csp, must) {
			t.Errorf("CSP %q is missing %q", csp, must)
		}
	}
	// No inline or eval escape hatches.
	for _, forbidden := range []string{"unsafe-inline", "unsafe-eval", "*"} {
		if strings.Contains(csp, forbidden) {
			t.Errorf("CSP %q contains %q", csp, forbidden)
		}
	}
	if w.Header().Get("Permissions-Policy") == "" {
		t.Error("Permissions-Policy is missing")
	}
}

func TestCSPAddsWakerOriginOnlyWhenConfigured(t *testing.T) {
	plain := CSPFor("")
	if strings.Contains(plain, "https://") {
		t.Errorf("CSP without a waker mentions an external origin: %q", plain)
	}
	withWaker := CSPFor("https://waker.tail1234.ts.net")
	if !strings.Contains(withWaker, "connect-src 'self' https://waker.tail1234.ts.net") {
		t.Errorf("CSP with a waker = %q, want the waker in connect-src", withWaker)
	}
	// The waker belongs in connect-src only, never script-src.
	if strings.Contains(withWaker, "script-src 'self' https://") {
		t.Errorf("CSP with a waker widened script-src: %q", withWaker)
	}
}

func TestInlineAllowedRefusesActiveContent(t *testing.T) {
	for _, ok := range []string{"image/jpeg", "image/png", "image/gif", "image/webp",
		"application/pdf", "text/plain; charset=utf-8"} {
		if !InlineAllowed(ok) {
			t.Errorf("InlineAllowed(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"text/html", "image/svg+xml", "application/xhtml+xml",
		"text/html; charset=utf-8", "application/javascript", "text/plain"} {
		if InlineAllowed(bad) {
			t.Errorf("InlineAllowed(%q) = true, want false", bad)
		}
	}
}

func TestLimiter(t *testing.T) {
	now := time.Now()
	l := NewLimiter(3, time.Minute)
	l.now = func() time.Time { return now }

	for i := 1; i <= 3; i++ {
		if ok, _ := l.Allow("node:a"); !ok {
			t.Fatalf("request %d was refused, want it allowed", i)
		}
	}
	if ok, retry := l.Allow("node:a"); ok {
		t.Fatal("the 4th request was allowed, want it refused")
	} else if retry <= 0 {
		t.Errorf("retry-after = %v, want it positive", retry)
	}

	// A different key has its own budget.
	if ok, _ := l.Allow("node:b"); !ok {
		t.Error("a different node was refused, want its own budget")
	}

	// The window resets.
	now = now.Add(time.Minute + time.Second)
	if ok, _ := l.Allow("node:a"); !ok {
		t.Error("request after the window reset was refused")
	}
}

func TestRateLimitResponse(t *testing.T) {
	var entries []audit.Entry
	l := NewLimiter(1, time.Minute)
	h := RateLimit(l, func(e audit.Entry) { entries = append(entries, e) })(okHandler())

	r := httptest.NewRequest(http.MethodGet, "/api/system", nil)
	r.RemoteAddr = phoneIP + ":1234"
	h.ServeHTTP(httptest.NewRecorder(), r)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("Retry-After is missing")
	}
	if len(entries) != 1 || entries[0].Action != audit.ActionDenyRateLimit {
		t.Errorf("audit = %+v, want one deny.ratelimit entry", entries)
	}
}

func TestDecodeJSONRejectsUnknownFields(t *testing.T) {
	var body struct {
		Name string `json:"name"`
	}
	r := httptest.NewRequest(http.MethodPost, "/api/sessions",
		strings.NewReader(`{"name":"x","permissionMode":"bypassPermissions"}`))
	w := httptest.NewRecorder()
	if DecodeJSON(w, r, &body) {
		t.Fatal("DecodeJSON accepted an unknown field; a smuggled field must be refused")
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400", w.Code)
	}
}
