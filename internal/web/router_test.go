package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"burrow/internal/apps"
	"burrow/internal/audit"
	"burrow/internal/auth"
	"burrow/internal/config"
	"burrow/internal/files"
	"burrow/internal/identity"
	"burrow/internal/power"
	"burrow/internal/pty"
	"burrow/internal/sessions"
	"burrow/internal/store"
	"burrow/internal/system"
)

// routerFixture wires a real Server over real packages with a fake tailnet, so
// the middleware stack is exercised end to end rather than per handler.
type routerFixture struct {
	h       http.Handler
	cfg     *config.Config
	auth    *auth.Manager
	store   *store.Store
	now     time.Time
	root    string
	auditLg *audit.Logger
	apps    *fakeOpener
}

const (
	testPhoneIP = "100.64.0.2"
	testOwner   = "owner@example.com"
)

func newRouterFixture(t *testing.T) *routerFixture {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "Docs")
	project := filepath.Join(dir, "code", "demo")
	for _, d := range []string{root, project} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Owner: config.Owner{TailscaleLogin: testOwner},
		Device: config.Device{MaxDevices: 3, Allowed: []config.AllowedDevice{
			{Name: "iPhone", Kind: config.KindPhone, NodeID: "nPHONE"},
			{Name: "Laptop", Kind: config.KindLaptop, NodeID: "nLAPTOP"},
		}},
		Agent: config.Agent{Hostname: "n-ab12cd34", StateDir: dir,
			Auth: config.AuthCfg{
				SessionIdleMinutes: 30, SessionAbsoluteHours: 12, StepupSeconds: 300,
				LoginMaxFailures: 5, LockoutMinutes: 15, PairingWindowMinutes: 10,
			}},
		Claude: config.Claude{
			Binary: "claude", DefaultMode: "chat", DefaultPermissionMode: "default",
			ServerSpawn: "same-dir", ServerCapacity: 8, MaxSessions: 3,
			RestrictedToken: true, ScrollbackBytes: 64 << 10, ReadyTimeoutSeconds: 30,
		},
		Projects: []config.Project{{Name: "demo", Path: project}},
		Files: config.Files{
			Roots:             []config.Root{{Name: "Docs", Path: root, Writable: true}},
			UploadDefaultRoot: "Docs", MaxUploadMB: 8,
		},
		Wake: config.Wake{
			Method:             config.WakeSmartPlug,
			Shortcuts:          config.Shortcuts{On: "PC On", Off: "PC Off", PowerCycle: "PC Power Cycle"},
			WakeTimeoutSeconds: 180, PowerCycleAfterSeconds: 90, OffConfirmSeconds: 30,
		},
		SSH:     config.SSH{User: "pcuser", Host: "pc.tail1234.ts.net"},
		Audit:   config.AuditCfg{Path: filepath.Join(dir, "audit.jsonl")},
		Logging: config.LoggingCfg{Dir: dir, Level: "info", MaxFiles: 10, MaxSizeMB: 10},
	}
	cfg.SetPath(filepath.Join(dir, "burrow.yaml"))

	st, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	f := &routerFixture{cfg: cfg, store: st, now: time.Now().UTC(), root: root, apps: &fakeOpener{}}
	f.auth = auth.NewManager(st, func() *config.Config { return cfg }, nil,
		func() time.Time { return f.now })
	if err := f.auth.SetRelyingParty("n-ab12cd34.tail1234.ts.net"); err != nil {
		t.Fatal(err)
	}

	lg, err := audit.Open(cfg.Audit.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lg.Close() })
	f.auditLg = lg

	resolver := identity.NewFake().
		Add(testPhoneIP, identity.Node{StableID: "nPHONE", Login: testOwner}).
		Add("100.64.0.3", identity.Node{StableID: "nLAPTOP", Login: testOwner}).
		Add("100.64.0.9", identity.Node{StableID: "nSTRANGER", Login: "other@example.com"})

	fileRes, err := files.NewResolver([]files.RootSpec{
		{Name: "Docs", Path: root, Writable: true},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Sessions start a fake process, so no Claude binary is needed.
	sessMgr := sessions.NewManager(func() *config.Config { return cfg }, nil,
		func(pty.Config) (pty.Process, error) { return newStubProc(), nil })
	t.Cleanup(sessMgr.Shutdown)

	srv := NewServer(Deps{
		Config:   func() *config.Config { return cfg },
		Origin:   func() string { return testOrigin },
		Resolver: resolver,
		Auth:     f.auth,
		Sessions: sessMgr,
		Files:    files.NewService(fileRes, cfg.Files.MaxUploadMB),
		Power:    power.NewManager(&stubPower{}, nil),
		System:   system.Static{Info: system.Info{Hostname: "pc", OSVersion: "Windows 11"}},
		Audit:    lg,
		SSHKeyAdder: func(_ context.Context, key string) (string, error) {
			return "SHA256:test", nil
		},
		Lockdown: func(_ context.Context, actor string) ([]string, error) {
			return nil, nil
		},
		Apps: f.apps,
	})
	f.h = srv.Handler()
	return f
}

// newSession inserts a logged-in session and returns its cookie.
func (f *routerFixture) newSession(t *testing.T, stepupFresh bool) *http.Cookie {
	t.Helper()
	dev := store.Device{ID: "d1", Name: "iPhone", Kind: "phone", NodeID: "nPHONE",
		Login: testOwner, PairedAt: f.now}
	if err := f.store.AddDevice(t.Context(), dev); err != nil {
		t.Fatal(err)
	}
	stepupAt := f.now
	if !stepupFresh {
		stepupAt = f.now.Add(-time.Hour)
	}
	sess := store.AuthSession{ID: "sess-1", DeviceID: dev.ID, NodeID: "nPHONE",
		CreatedAt: f.now, LastSeenAt: f.now, StepupAt: &stepupAt}
	if err := f.store.CreateAuthSession(t.Context(), sess); err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: auth.CookieName, Value: sess.ID}
}

// do sends a request as the phone.
func (f *routerFixture) do(t *testing.T, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.RemoteAddr = testPhoneIP + ":51234"
	if method != http.MethodGet && method != http.MethodHead {
		r.Header.Set(CSRFHeader, CSRFHeaderValue)
		r.Header.Set("Origin", testOrigin)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var e APIError
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		return ""
	}
	return e.Error.Code
}

// ---------- tests ----------

func TestHealthzNeedsNoSession(t *testing.T) {
	f := newRouterFixture(t)
	w := f.do(t, http.MethodGet, "/healthz", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["ok"] != true {
		t.Errorf("body = %v, want ok:true", body)
	}
	// It must not leak anything beyond liveness and version.
	for _, k := range []string{"hostname", "sessions", "disks", "login"} {
		if _, ok := body[k]; ok {
			t.Errorf("/healthz leaked %q", k)
		}
	}
}

func TestEndpointsNeedASession(t *testing.T) {
	f := newRouterFixture(t)
	for _, path := range []string{
		"/api/system", "/api/sessions", "/api/files/roots", "/api/audit",
		"/api/devices", "/api/projects", "/api/wake-config",
	} {
		w := f.do(t, http.MethodGet, path, "", nil)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s without a session = %d, want 401", path, w.Code)
		}
		if got := errorCode(t, w); got != CodeAuthRequired {
			t.Errorf("%s error code = %q, want %q", path, got, CodeAuthRequired)
		}
	}
}

func TestSessionGivesAccess(t *testing.T) {
	f := newRouterFixture(t)
	c := f.newSession(t, true)

	w := f.do(t, http.MethodGet, "/api/system", "", c)
	if w.Code != http.StatusOK {
		t.Fatalf("/api/system = %d (%s), want 200", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["hostname"] != "pc" {
		t.Errorf("hostname = %v, want pc", body["hostname"])
	}
	if _, ok := body["keepAwake"]; !ok {
		t.Error("the response has no keepAwake block")
	}
}

// TestStepupRoutes is the table the test plan asks for: each dangerous endpoint
// must refuse without a fresh assertion and work with one.
func TestStepupRoutes(t *testing.T) {
	cases := []struct {
		method, path, body string
	}{
		{http.MethodPost, "/api/files/delete", `{"root":"Docs","path":"notes.txt"}`},
		{http.MethodPost, "/api/ssh-keys", `{"publicKey":"ssh-ed25519 AAAAC3Nz test"}`},
		{http.MethodPost, "/api/lockdown", `{}`},
		{http.MethodDelete, "/api/devices/nLAPTOP", ""},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			// Stale step-up: refused.
			stale := newRouterFixture(t)
			c := stale.newSession(t, false)
			w := stale.do(t, tc.method, tc.path, tc.body, c)
			if w.Code != http.StatusForbidden {
				t.Fatalf("with a stale step-up = %d (%s), want 403", w.Code, w.Body.String())
			}
			if got := errorCode(t, w); got != CodeStepupRequired {
				t.Fatalf("error code = %q, want %q", got, CodeStepupRequired)
			}

			// Fresh step-up: allowed through to the handler.
			fresh := newRouterFixture(t)
			c2 := fresh.newSession(t, true)
			w2 := fresh.do(t, tc.method, tc.path, tc.body, c2)
			if w2.Code == http.StatusForbidden && errorCode(t, w2) == CodeStepupRequired {
				t.Fatalf("with a fresh step-up still demanded one: %s", w2.Body.String())
			}
		})
	}
}

func TestPowerShutdownNeedsStepupButSleepDoesNot(t *testing.T) {
	f := newRouterFixture(t)
	c := f.newSession(t, false) // stale

	w := f.do(t, http.MethodPost, "/api/power", `{"action":"shutdown"}`, c)
	if w.Code != http.StatusForbidden || errorCode(t, w) != CodeStepupRequired {
		t.Fatalf("shutdown with a stale step-up = %d %s, want a step-up demand", w.Code, w.Body.String())
	}

	// Sleep is not destructive in the same way and needs no step-up.
	w = f.do(t, http.MethodPost, "/api/power", `{"action":"sleep"}`, c)
	if w.Code == http.StatusForbidden && errorCode(t, w) == CodeStepupRequired {
		t.Fatalf("sleep demanded a step-up: %s", w.Body.String())
	}
}

func TestPowerReportsPlugOffSafe(t *testing.T) {
	f := newRouterFixture(t)
	c := f.newSession(t, true)

	w := f.do(t, http.MethodPost, "/api/power", `{"action":"shutdown"}`, c)
	if w.Code != http.StatusOK {
		t.Fatalf("shutdown = %d (%s), want 200", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["plugOffSafe"] != true {
		t.Errorf("plugOffSafe = %v after a shutdown, want true", body["plugOffSafe"])
	}

	// Sleep must report the opposite, since the PWA uses this to decide
	// whether offering "cut plug power" is safe.
	w = f.do(t, http.MethodPost, "/api/power", `{"action":"sleep"}`, c)
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["plugOffSafe"] != false {
		t.Errorf("plugOffSafe = %v after sleep, want false", body["plugOffSafe"])
	}
	if body["wakeable"] != false {
		t.Errorf("wakeable = %v after sleep, want false", body["wakeable"])
	}
}

func TestCreateSessionRefusesBypassPermissions(t *testing.T) {
	f := newRouterFixture(t)
	c := f.newSession(t, true)

	w := f.do(t, http.MethodPost, "/api/sessions",
		`{"projectId":"p_demo","permissionMode":"bypassPermissions"}`, c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("= %d (%s), want 400", w.Code, w.Body.String())
	}
}

func TestCreateAndListSession(t *testing.T) {
	f := newRouterFixture(t)
	c := f.newSession(t, true)

	w := f.do(t, http.MethodPost, "/api/sessions", `{"projectId":"p_demo","name":"demo-1"}`, c)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s), want 201", w.Code, w.Body.String())
	}
	var created sessions.Session
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Name != "demo-1" {
		t.Errorf("name = %q, want demo-1", created.Name)
	}

	w = f.do(t, http.MethodGet, "/api/sessions", "", c)
	var list []sessions.Session
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("list = %+v, want the created session", list)
	}

	// A literal path segment must not be swallowed by the {id} route.
	w = f.do(t, http.MethodPost, "/api/sessions/kill-all", `{}`, c)
	if w.Code != http.StatusOK {
		t.Fatalf("kill-all = %d (%s), want 200; the {id} route may be shadowing it",
			w.Code, w.Body.String())
	}
}

func TestFileListAndDownload(t *testing.T) {
	f := newRouterFixture(t)
	c := f.newSession(t, true)

	w := f.do(t, http.MethodGet, "/api/files/list?root=Docs&path=", "", c)
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d (%s), want 200", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "notes.txt") {
		t.Errorf("list body = %s, want notes.txt", w.Body.String())
	}

	w = f.do(t, http.MethodGet, "/api/files/download?root=Docs&path=notes.txt", "", c)
	if w.Code != http.StatusOK {
		t.Fatalf("download = %d, want 200", w.Code)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Errorf("Content-Disposition = %q, want an attachment", cd)
	}
	if w.Body.String() != "hello" {
		t.Errorf("body = %q, want \"hello\"", w.Body.String())
	}
}

func TestFileTraversalIsRefusedThroughTheAPI(t *testing.T) {
	f := newRouterFixture(t)
	c := f.newSession(t, true)

	for _, p := range []string{
		"/api/files/list?root=Docs&path=../",
		"/api/files/download?root=Docs&path=../../Windows/System32/config/SAM",
		"/api/files/download?root=Docs&path=" + "C:%5CWindows%5Cwin.ini",
		"/api/files/list?root=Docs&path=.ssh",
	} {
		w := f.do(t, http.MethodGet, p, "", c)
		if w.Code == http.StatusOK {
			t.Errorf("%s returned 200; it must be refused", p)
		}
		if got := errorCode(t, w); got != CodeBadRequest && got != CodeNotFound {
			t.Errorf("%s error code = %q, want a generic refusal", p, got)
		}
	}
}

func TestPreviewRefusesActiveContent(t *testing.T) {
	f := newRouterFixture(t)
	c := f.newSession(t, true)

	// An HTML file in a root must not be previewable, or it would run with the
	// app's origin and cookies.
	if err := os.WriteFile(filepath.Join(f.root, "page.html"),
		[]byte("<script>alert(1)</script>"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := f.do(t, http.MethodGet, "/api/files/preview?root=Docs&path=page.html", "", c)
	if w.Code == http.StatusOK {
		t.Fatalf("an HTML preview was served with content type %q", w.Header().Get("Content-Type"))
	}

	// Text previews are served as plain text.
	w = f.do(t, http.MethodGet, "/api/files/preview?root=Docs&path=notes.txt", "", c)
	if w.Code != http.StatusOK {
		t.Fatalf("text preview = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
}

func TestUnknownAPIPathIs404NotTheAppShell(t *testing.T) {
	f := newRouterFixture(t)
	c := f.newSession(t, true)

	w := f.do(t, http.MethodGet, "/api/nope", "", c)
	if w.Code != http.StatusNotFound {
		t.Fatalf("= %d, want 404", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Errorf("Content-Type = %q, want JSON (not the HTML shell)", ct)
	}
}

func TestSecurityHeadersOnAPIAndErrors(t *testing.T) {
	f := newRouterFixture(t)
	c := f.newSession(t, true)

	for _, path := range []string{"/healthz", "/api/system", "/api/nope"} {
		w := f.do(t, http.MethodGet, path, "", c)
		for _, h := range []string{
			"Content-Security-Policy", "Strict-Transport-Security",
			"X-Content-Type-Options", "Referrer-Policy",
			"Cross-Origin-Opener-Policy", "Cross-Origin-Resource-Policy",
		} {
			if w.Header().Get(h) == "" {
				t.Errorf("%s is missing %s", path, h)
			}
		}
	}
}

func TestCSRFBlocksStateChangesWithoutTheHeader(t *testing.T) {
	f := newRouterFixture(t)
	c := f.newSession(t, true)

	r := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(`{"projectId":"p_demo"}`))
	r.RemoteAddr = testPhoneIP + ":51234"
	r.Header.Set("Origin", testOrigin)
	r.AddCookie(c) // header deliberately absent
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden || errorCode(t, w) != CodeCSRF {
		t.Fatalf("= %d %s, want a csrf refusal", w.Code, w.Body.String())
	}
}

func TestStrangerSeesNothing(t *testing.T) {
	f := newRouterFixture(t)
	c := f.newSession(t, true)

	// Same cookie, different node: the gate refuses before the session is even
	// looked at.
	r := httptest.NewRequest(http.MethodGet, "/api/system", nil)
	r.RemoteAddr = "100.64.0.9:51234"
	r.AddCookie(c)
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("= %d, want 404", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", w.Body.String())
	}
}

func TestSessionCookieIsRejectedFromAnotherPairedDevice(t *testing.T) {
	f := newRouterFixture(t)
	c := f.newSession(t, true) // bound to nPHONE

	r := httptest.NewRequest(http.MethodGet, "/api/system", nil)
	r.RemoteAddr = "100.64.0.3:51234" // the laptop, also paired
	r.AddCookie(c)
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("= %d (%s), want 401; a session must be bound to its node", w.Code, w.Body.String())
	}
}

func TestLockdownClosesEverythingButHealth(t *testing.T) {
	f := newRouterFixture(t)
	c := f.newSession(t, true)
	if err := f.auth.EnterLockdown(t.Context(), "test"); err != nil {
		t.Fatal(err)
	}

	if w := f.do(t, http.MethodGet, "/healthz", "", c); w.Code != http.StatusOK {
		t.Errorf("/healthz during lockdown = %d, want 200", w.Code)
	}
	for _, path := range []string{"/api/system", "/api/sessions", "/api/files/roots", "/", "/api/auth/state"} {
		w := f.do(t, http.MethodGet, path, "", c)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s during lockdown = %d, want 404", path, w.Code)
		}
	}
}

func TestWakeConfigIsCacheableAndComplete(t *testing.T) {
	f := newRouterFixture(t)
	c := f.newSession(t, true)

	w := f.do(t, http.MethodGet, "/api/wake-config", "", c)
	if w.Code != http.StatusOK {
		t.Fatalf("= %d, want 200", w.Code)
	}
	// This is the one API response the service worker may cache; without it
	// the offline Wake screen cannot work.
	if cc := w.Header().Get("Cache-Control"); strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q, want it cacheable", cc)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["method"] != "smartplug" {
		t.Errorf("method = %v, want smartplug", body["method"])
	}
	shortcuts, ok := body["shortcuts"].(map[string]any)
	if !ok {
		t.Fatalf("shortcuts = %v, want an object", body["shortcuts"])
	}
	for _, k := range []string{"on", "off", "powerCycle"} {
		if shortcuts[k] == "" || shortcuts[k] == nil {
			t.Errorf("shortcuts.%s is empty", k)
		}
	}
	if body["wakerUrl"] != nil {
		t.Errorf("wakerUrl = %v, want null for the smartplug method", body["wakerUrl"])
	}
}

func TestPathsHiddenWithoutStepup(t *testing.T) {
	f := newRouterFixture(t)
	stale := f.newSession(t, false)

	w := f.do(t, http.MethodGet, "/api/files/roots", "", stale)
	if strings.Contains(w.Body.String(), f.root) {
		t.Errorf("roots exposed an absolute path without a step-up: %s", w.Body.String())
	}
	w = f.do(t, http.MethodGet, "/api/projects", "", stale)
	if strings.Contains(w.Body.String(), "code") && strings.Contains(w.Body.String(), ":\\\\") {
		t.Errorf("projects exposed an absolute path without a step-up: %s", w.Body.String())
	}
}

func TestAddSSHKeyValidation(t *testing.T) {
	f := newRouterFixture(t)
	c := f.newSession(t, true)

	bad := []string{
		`{"publicKey":""}`,
		`{"publicKey":"ssh-rsa AAAAB3Nza test"}`,
		`{"publicKey":"not-a-key"}`,
		// A newline would let one request append several authorized_keys lines.
		`{"publicKey":"ssh-ed25519 AAAAC3Nz a\ncommand=\"rm -rf /\" ssh-ed25519 AAAAC3Nz b"}`,
	}
	for _, body := range bad {
		w := f.do(t, http.MethodPost, "/api/ssh-keys", body, c)
		if w.Code != http.StatusBadRequest {
			t.Errorf("POST /api/ssh-keys %s = %d, want 400", body, w.Code)
		}
	}
	w := f.do(t, http.MethodPost, "/api/ssh-keys", `{"publicKey":"ssh-ed25519 AAAAC3NzaC1 termius"}`, c)
	if w.Code != http.StatusOK {
		t.Errorf("a valid ed25519 key = %d (%s), want 200", w.Code, w.Body.String())
	}
}

func TestRevokingTheOnlyDeviceIsRefused(t *testing.T) {
	f := newRouterFixture(t)
	f.cfg.Device.Allowed = f.cfg.Device.Allowed[:1] // only the phone
	c := f.newSession(t, true)

	w := f.do(t, http.MethodDelete, "/api/devices/nPHONE", "", c)
	if w.Code != http.StatusConflict {
		t.Fatalf("= %d (%s), want 409; revoking the last device would lock the owner out",
			w.Code, w.Body.String())
	}
}

func TestAuditLogIsReadableAndChainsCorrectly(t *testing.T) {
	f := newRouterFixture(t)
	c := f.newSession(t, true)

	// Produce a few audited actions.
	f.do(t, http.MethodGet, "/api/files/download?root=Docs&path=notes.txt", "", c)
	f.do(t, http.MethodPost, "/api/files/mkdir", `{"root":"Docs","path":"made"}`, c)

	w := f.do(t, http.MethodGet, "/api/audit?limit=50", "", c)
	if w.Code != http.StatusOK {
		t.Fatalf("= %d, want 200", w.Code)
	}
	var body struct {
		Entries []struct {
			Action string `json:"action"`
			Hash   string `json:"hash"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Entries) < 2 {
		t.Fatalf("audit returned %d entries, want at least 2", len(body.Entries))
	}
	// The chain is not exposed to clients.
	for _, e := range body.Entries {
		if e.Hash != "" {
			t.Errorf("the audit API exposed a chain hash: %+v", e)
		}
	}
	// And the on-disk chain still verifies.
	res, err := audit.Verify(f.cfg.Audit.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("the audit chain is broken: %+v", res.Problems)
	}
}

// ---------- stubs ----------

// stubPower accepts everything without touching the machine.
type stubPower struct{}

func (stubPower) Suspend(bool) error                 { return nil }
func (stubPower) Shutdown(bool, time.Duration) error { return nil }
func (stubPower) CancelShutdown() error              { return nil }
func (stubPower) LockWorkstation() error             { return nil }
func (stubPower) KeepAwake(bool) error               { return nil }
func (stubPower) InteractiveSession() bool           { return true }

// stubProc is a PTY process that produces nothing and exits when killed.
type stubProc struct{ done chan struct{} }

func newStubProc() *stubProc { return &stubProc{done: make(chan struct{})} }

func (p *stubProc) Read(b []byte) (int, error) {
	<-p.done
	return 0, os.ErrClosed
}
func (p *stubProc) Write(b []byte) (int, error) { return len(b), nil }
func (p *stubProc) Resize(pty.Size) error       { return nil }
func (p *stubProc) PID() int                    { return 4242 }
func (p *stubProc) Wait() (int, error)          { <-p.done; return 0, nil }
func (p *stubProc) Kill() error                 { p.closeOnce(); return nil }
func (p *stubProc) Close() error                { p.closeOnce(); return nil }
func (p *stubProc) closeOnce() {
	select {
	case <-p.done:
	default:
		close(p.done)
	}
}

func TestProjectsCanBeAddedOnlyFromInsideAParent(t *testing.T) {
	f := newRouterFixture(t)
	parent := filepath.Dir(f.cfg.Projects[0].Path) // .../code
	inside := filepath.Join(parent, "crm")
	outside := t.TempDir()
	for _, d := range []string{inside, filepath.Join(parent, ".secret")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.cfg.ProjectsParents = []string{parent}

	stale := f.newSession(t, false)
	if w := f.do(t, "POST", "/api/projects", `{"path":`+jsonString(inside)+`}`, stale); errorCode(t, w) != CodeStepupRequired {
		t.Fatalf("adding a project without step-up got %d %s", w.Code, w.Body)
	}

	f.store.RevokeAllAuthSessions(t.Context(), f.now)
	fresh := &http.Cookie{Name: auth.CookieName, Value: "sess-2"}
	stepup := f.now
	if err := f.store.CreateAuthSession(t.Context(), store.AuthSession{ID: "sess-2", DeviceID: "d1", NodeID: "nPHONE",
		CreatedAt: f.now, LastSeenAt: f.now, StepupAt: &stepup}); err != nil {
		t.Fatal(err)
	}

	w := f.do(t, "GET", "/api/projects/candidates", "", fresh)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"crm"`) || strings.Contains(w.Body.String(), ".secret") {
		t.Fatalf("candidates = %d %s", w.Code, w.Body)
	}

	if w := f.do(t, "POST", "/api/projects", `{"path":`+jsonString(outside)+`}`, fresh); w.Code != http.StatusBadRequest {
		t.Fatalf("a folder outside every parent got %d, want 400", w.Code)
	}
	if w := f.do(t, "POST", "/api/projects", `{"path":`+jsonString(filepath.Join(parent, ".secret"))+`}`, fresh); w.Code != http.StatusBadRequest {
		t.Fatalf("a hidden folder got %d, want 400", w.Code)
	}
	if w := f.do(t, "POST", "/api/projects", `{"path":`+jsonString(inside)+`}`, fresh); w.Code != http.StatusCreated {
		t.Fatalf("adding crm got %d %s", w.Code, w.Body)
	}
	if w := f.do(t, "POST", "/api/projects", `{"path":`+jsonString(inside)+`}`, fresh); w.Code != http.StatusConflict {
		t.Fatalf("adding crm twice got %d, want 409", w.Code)
	}
	if len(f.cfg.Projects) != 2 || f.cfg.Projects[1].Name != "crm" {
		t.Fatalf("projects = %+v", f.cfg.Projects)
	}
	saved, err := os.ReadFile(filepath.Join(f.cfg.Agent.StateDir, "burrow.yaml"))
	if err != nil || !strings.Contains(string(saved), "crm") {
		t.Fatalf("the new project was not saved to burrow.yaml: %v", err)
	}

	id := sessions.ProjectID(f.cfg.Projects[1])
	if w := f.do(t, "DELETE", "/api/projects/"+id, "", fresh); w.Code != http.StatusNoContent {
		t.Fatalf("removing crm got %d %s", w.Code, w.Body)
	}
	if len(f.cfg.Projects) != 1 {
		t.Fatalf("projects after remove = %+v", f.cfg.Projects)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestBypassPermissionsNeedsTheFlagAndAFreshStepup(t *testing.T) {
	f := newRouterFixture(t)
	f.cfg.Claude.AllowBypassPermissions = true
	body := `{"projectId":"p_demo","permissionMode":"bypassPermissions"}`

	stale := f.newSession(t, false)
	if w := f.do(t, http.MethodPost, "/api/sessions", body, stale); errorCode(t, w) != CodeStepupRequired {
		t.Fatalf("bypass without step-up = %d %s, want stepup_required", w.Code, w.Body)
	}

	stepup := f.now
	if err := f.store.CreateAuthSession(t.Context(), store.AuthSession{ID: "sess-fresh", DeviceID: "d1",
		NodeID: "nPHONE", CreatedAt: f.now, LastSeenAt: f.now, StepupAt: &stepup}); err != nil {
		t.Fatal(err)
	}
	fresh := &http.Cookie{Name: auth.CookieName, Value: "sess-fresh"}
	w := f.do(t, http.MethodPost, "/api/sessions", body, fresh)
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), "bypassPermissions") {
		t.Fatalf("bypass with the flag and a fresh step-up = %d %s, want 201", w.Code, w.Body)
	}
}

func TestLaunchersStartWithTheirOwnClaudeLogin(t *testing.T) {
	f := newRouterFixture(t)
	confDir := t.TempDir()
	f.cfg.Launchers = []config.Launcher{{Name: "Personal work", Project: "demo", ClaudeConfigDir: confDir}}
	c := f.newSession(t, true)

	w := f.do(t, http.MethodGet, "/api/launchers", "", c)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Personal work") ||
		strings.Contains(w.Body.String(), jsonString(confDir)[1:len(jsonString(confDir))-1]) {
		t.Fatalf("launchers = %d %s (must list the name, not the config dir)", w.Code, w.Body)
	}

	w = f.do(t, http.MethodPost, "/api/sessions", `{"launcher":"Personal work"}`, c)
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), `"launcher":"Personal work"`) {
		t.Fatalf("start from launcher = %d %s", w.Code, w.Body)
	}
	if w := f.do(t, http.MethodPost, "/api/sessions", `{"launcher":"nope"}`, c); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown launcher = %d, want 400", w.Code)
	}
}

// fakeOpener records which apps were opened.
type fakeOpener struct {
	opened    []string
	noDesktop bool
}

func (o *fakeOpener) Sync([]config.App) error { return nil }
func (o *fakeOpener) Open(name string) error {
	if o.noDesktop {
		return apps.ErrNoDesktop
	}
	o.opened = append(o.opened, name)
	return nil
}

func TestOpenOnPCOnlyOpensListedApps(t *testing.T) {
	f := newRouterFixture(t)
	f.cfg.Apps = []config.App{{Name: "Chrome · Work", Path: `C:\chrome.exe`, Args: []string{"--profile-directory=Profile 2"}}}
	c := f.newSession(t, false)

	w := f.do(t, http.MethodGet, "/api/apps", "", c)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Chrome · Work") || strings.Contains(w.Body.String(), "chrome.exe") {
		t.Fatalf("apps = %d %s (names only, no paths)", w.Code, w.Body)
	}
	if w := f.do(t, http.MethodPost, "/api/apps/"+url.PathEscape("Chrome · Work")+"/open", "", c); w.Code != http.StatusNoContent {
		t.Fatalf("open = %d %s", w.Code, w.Body)
	}
	if len(f.apps.opened) != 1 {
		t.Fatalf("opened = %v", f.apps.opened)
	}
	if w := f.do(t, http.MethodPost, "/api/apps/cmd.exe/open", "", c); w.Code != http.StatusNotFound {
		t.Fatalf("an unlisted app = %d, want 404", w.Code)
	}
	f.apps.noDesktop = true
	if w := f.do(t, http.MethodPost, "/api/apps/"+url.PathEscape("Chrome · Work")+"/open", "", c); w.Code != http.StatusConflict {
		t.Fatalf("no desktop = %d, want 409", w.Code)
	}
}
