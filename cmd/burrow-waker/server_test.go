package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "waker.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const goodConfig = `
hostname: "n-wk12ab34"
state_dir: "/tmp/burrow-waker"
owner_login: "owner@example.com"
allowed_node_ids:
  - "nPHONE"
agent_origin: "https://n-ab12cd34.tail1234.ts.net"
pc:
  mac: "AA:BB:CC:DD:EE:FF"
  lan_prefix: "192.168.1.0/24"
  probe_addr: "192.168.1.50:135"
`

func TestLoadConfig(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, goodConfig))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Hostname != "n-wk12ab34" {
		t.Errorf("hostname = %q", cfg.Hostname)
	}
	if len(cfg.AllowedNodeIDs) != 1 || cfg.AllowedNodeIDs[0] != "nPHONE" {
		t.Errorf("allowed_node_ids = %v", cfg.AllowedNodeIDs)
	}
}

func TestLoadConfigRejects(t *testing.T) {
	cases := map[string]string{
		"no allowed nodes": strings.Replace(goodConfig,
			"allowed_node_ids:\n  - \"nPHONE\"", "allowed_node_ids: []", 1),
		"http agent origin": strings.Replace(goodConfig,
			`"https://n-ab12cd34.tail1234.ts.net"`, `"http://pc.local"`, 1),
		"bad mac":     strings.Replace(goodConfig, `"AA:BB:CC:DD:EE:FF"`, `"nope"`, 1),
		"bad prefix":  strings.Replace(goodConfig, `"192.168.1.0/24"`, `"192.168.1.0/99"`, 1),
		"unknown key": goodConfig + "\nbogus: 1\n",
		"no hostname": strings.Replace(goodConfig, `hostname: "n-wk12ab34"`, `hostname: ""`, 1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := loadConfig(writeConfig(t, body)); err == nil {
				t.Fatal("loadConfig accepted an invalid config")
			}
		})
	}
}

// testServer builds a server without a tailnet, for the parts that do not need
// one: rate limiting and CORS.
func testServer(t *testing.T) *server {
	t.Helper()
	cfg, err := loadConfig(writeConfig(t, goodConfig))
	if err != nil {
		t.Fatal(err)
	}
	return &server{cfg: cfg, logf: func(string, ...any) {}, dayStart: time.Now()}
}

func TestWakeRateLimitPerInterval(t *testing.T) {
	s := testServer(t)

	if _, ok := s.allowWake(); !ok {
		t.Fatal("the first wake was refused")
	}
	retry, ok := s.allowWake()
	if ok {
		t.Fatal("a second wake was allowed immediately; the interval limit is not applied")
	}
	if retry <= 0 || retry > wakeMinInterval {
		t.Errorf("retry = %v, want up to %v", retry, wakeMinInterval)
	}

	// Once the interval has passed it is allowed again.
	s.mu.Lock()
	s.lastWake = time.Now().Add(-wakeMinInterval - time.Second)
	s.mu.Unlock()
	if _, ok := s.allowWake(); !ok {
		t.Error("a wake after the interval was refused")
	}
}

func TestWakeRateLimitPerDay(t *testing.T) {
	s := testServer(t)
	for i := 0; i < wakeMaxPerDay; i++ {
		s.mu.Lock()
		s.lastWake = time.Time{} // ignore the interval limit for this test
		s.mu.Unlock()
		if _, ok := s.allowWake(); !ok {
			t.Fatalf("wake %d of %d was refused", i+1, wakeMaxPerDay)
		}
	}
	s.mu.Lock()
	s.lastWake = time.Time{}
	s.mu.Unlock()
	if _, ok := s.allowWake(); ok {
		t.Fatalf("wake %d was allowed; the daily limit of %d is not applied",
			wakeMaxPerDay+1, wakeMaxPerDay)
	}

	// The counter resets after a day.
	s.mu.Lock()
	s.dayStart = time.Now().Add(-25 * time.Hour)
	s.lastWake = time.Time{}
	s.mu.Unlock()
	if _, ok := s.allowWake(); !ok {
		t.Error("the daily counter did not reset")
	}
}

func TestCORSAllowsExactlyTheAgentOrigin(t *testing.T) {
	s := testServer(t)
	w := httptest.NewRecorder()
	s.setCORS(w)

	h := w.Header()
	if got := h.Get("Access-Control-Allow-Origin"); got != s.cfg.AgentOrigin {
		t.Errorf("Allow-Origin = %q, want exactly %q", got, s.cfg.AgentOrigin)
	}
	if got := h.Get("Access-Control-Allow-Origin"); got == "*" {
		t.Error("Allow-Origin is a wildcard")
	}
	if got := h.Get("Access-Control-Allow-Methods"); got != "GET, POST" {
		t.Errorf("Allow-Methods = %q, want \"GET, POST\"", got)
	}
	// Credentials must not be allowed: the waker has no session.
	if h.Get("Access-Control-Allow-Credentials") != "" {
		t.Error("Allow-Credentials is set; the waker needs no credentials")
	}
	if h.Get("Access-Control-Max-Age") != "600" {
		t.Errorf("Max-Age = %q, want 600", h.Get("Access-Control-Max-Age"))
	}
}

func TestFallbackPageHasNoInlineScriptOrExternalSources(t *testing.T) {
	s := testServer(t)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	s.handleIndex(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("= %d, want 200", w.Code)
	}
	body := w.Body.String()
	// The script lives in its own file, so the CSP can stay at
	// script-src 'self'.
	if strings.Contains(body, "<script>") {
		t.Error("the page carries an inline script")
	}
	if strings.Contains(body, "http://") || strings.Contains(body, "https://") {
		t.Error("the page references an external source")
	}
	csp := w.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "script-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q is missing %q", csp, want)
		}
	}
	if strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
		t.Error("the CSP allows inline script")
	}
}

func TestAppJSIsServedSeparately(t *testing.T) {
	s := testServer(t)
	r := httptest.NewRequest(http.MethodGet, "/app.js", nil)
	w := httptest.NewRecorder()
	s.handleIndex(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("= %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type = %q", ct)
	}
	if !strings.Contains(w.Body.String(), "/api/wake") {
		t.Error("the script does not call the wake endpoint")
	}
}

func TestUnknownPathIs404(t *testing.T) {
	s := testServer(t)
	r := httptest.NewRequest(http.MethodGet, "/secret", nil)
	w := httptest.NewRecorder()
	s.handleIndex(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("= %d, want 404", w.Code)
	}
}

func TestStatusShapeMatchesTheContract(t *testing.T) {
	s := testServer(t)
	// No tailnet and no LAN probe here, so the state is offline — the right
	// answer when nothing can be reached, and it must not panic.
	s.cfg.PC.ProbeAddr = ""
	r := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w := httptest.NewRecorder()
	s.handleStatus(w, r)

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"pc", "checkedAt", "lastWakeAt"} {
		if _, ok := body[k]; !ok {
			t.Errorf("the status response is missing %q", k)
		}
	}
	if body["lastWakeAt"] != nil {
		t.Errorf("lastWakeAt = %v before any wake, want null", body["lastWakeAt"])
	}
	if body["pc"] != string(stateOffline) {
		t.Errorf("pc = %v with nothing reachable, want %q", body["pc"], stateOffline)
	}
}
