package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"burrow/internal/audit"
	"burrow/internal/config"
	"burrow/internal/identity"
)

const (
	phoneIP  = "100.64.0.2"
	laptopIP = "100.64.0.3"
	wakerIP  = "100.64.0.4"
	otherIP  = "100.64.0.9"
	ownerLog = "owner@example.com"
)

// gateFixture builds a gate over a fake tailnet with the phone and laptop
// paired, a waker node, and an unpaired node owned by the owner.
func gateFixture(t *testing.T) (*Gate, *identity.Fake, *[]audit.Entry, *bool, *bool) {
	t.Helper()
	res := identity.NewFake().
		Add(phoneIP, identity.Node{StableID: "nPHONE", Login: ownerLog, Hostname: "iphone"}).
		Add(laptopIP, identity.Node{StableID: "nLAPTOP", Login: ownerLog, Hostname: "laptop"}).
		Add(wakerIP, identity.Node{StableID: "nWAKER", Tags: []string{DefaultWakerTag}, Hostname: "waker"}).
		Add(otherIP, identity.Node{StableID: "nOTHER", Login: ownerLog, Hostname: "ipad"}).
		Add("100.64.0.10", identity.Node{StableID: "nSTRANGER", Login: "someone@else.com", Hostname: "theirs"}).
		Add("100.64.0.11", identity.Node{StableID: "nTAGGED", Tags: []string{"tag:pc"}, Hostname: "pc"})

	cfg := &config.Config{
		Owner: config.Owner{TailscaleLogin: ownerLog},
		Device: config.Device{MaxDevices: 3, Allowed: []config.AllowedDevice{
			{Name: "iPhone", Kind: config.KindPhone, NodeID: "nPHONE"},
			{Name: "Laptop", Kind: config.KindLaptop, NodeID: "nLAPTOP"},
		}},
	}

	var entries []audit.Entry
	pairingOpen := false
	lockdown := false

	g := NewGate(GateDeps{
		Resolver:    res,
		Config:      func() *config.Config { return cfg },
		PairingOpen: func() bool { return pairingOpen },
		Lockdown:    func() bool { return lockdown },
		Audit:       func(e audit.Entry) { entries = append(entries, e) },
	})
	return g, res, &entries, &pairingOpen, &lockdown
}

// okHandler reports that it ran, so a test can tell "passed the gate" from
// "refused".
func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("reached"))
	})
}

func get(t *testing.T, h http.Handler, ip, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.RemoteAddr = ip + ":51234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestGateAllowsPairedDevices(t *testing.T) {
	g, _, _, _, _ := gateFixture(t)
	h := g.Middleware(okHandler())

	for _, ip := range []string{phoneIP, laptopIP} {
		w := get(t, h, ip, "/api/system")
		if w.Code != http.StatusOK {
			t.Errorf("paired device %s got %d, want 200", ip, w.Code)
		}
	}
}

func TestGateRefusesEverythingElseWith404(t *testing.T) {
	g, _, entries, _, _ := gateFixture(t)
	h := g.Middleware(okHandler())

	cases := []struct {
		name string
		ip   string
	}{
		{"owner's unpaired device", otherIP},
		{"another tailnet user", "100.64.0.10"},
		{"a tagged machine node", "100.64.0.11"},
		{"an address not on the tailnet", "100.64.0.99"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := get(t, h, tc.ip, "/api/system")
			if w.Code != http.StatusNotFound {
				t.Fatalf("got %d, want 404", w.Code)
			}
			if body := w.Body.String(); strings.Contains(body, "reached") || body != "" {
				t.Fatalf("body = %q, want empty (the service must not reveal itself)", body)
			}
		})
	}
	if len(*entries) != 4 {
		t.Errorf("audited %d denials, want 4", len(*entries))
	}
	for _, e := range *entries {
		if e.Action != audit.ActionDenyIdentity || e.Result != audit.ResultDenied {
			t.Errorf("audit entry = %+v, want a denied identity entry", e)
		}
	}
}

func TestGateRefusesAllowedNodeWithWrongLogin(t *testing.T) {
	g, res, _, _, _ := gateFixture(t)
	// The node id is allowlisted, but the tailnet says a different user owns
	// it now — a node id reused after a transfer must not inherit access.
	res.Add(phoneIP, identity.Node{StableID: "nPHONE", Login: "someone@else.com"})
	h := g.Middleware(okHandler())

	if w := get(t, h, phoneIP, "/api/system"); w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", w.Code)
	}
}

func TestGateWakerNodeHealthOnly(t *testing.T) {
	g, _, _, _, _ := gateFixture(t)
	h := g.Middleware(okHandler())

	if w := get(t, h, wakerIP, "/healthz"); w.Code != http.StatusOK {
		t.Errorf("waker on /healthz got %d, want 200", w.Code)
	}
	for _, path := range []string{"/api/system", "/api/sessions", "/", "/api/files/list"} {
		if w := get(t, h, wakerIP, path); w.Code != http.StatusNotFound {
			t.Errorf("waker on %s got %d, want 404", path, w.Code)
		}
	}
}

func TestGateWhoIsErrorIs404(t *testing.T) {
	g, res, _, _, _ := gateFixture(t)
	res.Err = errors.New("tailscaled is not answering")
	h := g.Middleware(okHandler())

	// A lookup failure must not become a 500, which would confirm something is
	// listening.
	if w := get(t, h, phoneIP, "/api/system"); w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", w.Code)
	}
}

func TestGatePairingWindowScope(t *testing.T) {
	g, _, _, pairingOpen, _ := gateFixture(t)
	h := g.Middleware(okHandler())

	// Closed window: the unpaired node reaches nothing.
	for _, path := range []string{"/pair", "/api/auth/pair/begin", "/healthz", "/api/auth/state"} {
		if w := get(t, h, otherIP, path); w.Code != http.StatusNotFound {
			t.Errorf("closed window, %s got %d, want 404", path, w.Code)
		}
	}

	*pairingOpen = true

	allowed := []string{"/pair", "/api/auth/pair/begin", "/api/auth/pair/finish",
		"/assets/app.js", "/manifest.webmanifest", "/sw.js",
		// The PWA asks both before rendering /pair; refused, it shows "PC off".
		"/healthz", "/api/auth/state"}
	for _, path := range allowed {
		if w := get(t, h, otherIP, path); w.Code != http.StatusOK {
			t.Errorf("open window, %s got %d, want 200", path, w.Code)
		}
	}

	// Everything else stays closed even while pairing is open.
	denied := []string{"/api/system", "/api/sessions", "/api/files/list", "/api/auth/login/begin",
		"/api/audit", "/api/wake-config", "/api/auth/me", "/"}
	for _, path := range denied {
		if w := get(t, h, otherIP, path); w.Code != http.StatusNotFound {
			t.Errorf("open window, %s got %d, want 404", path, w.Code)
		}
	}

	// A stranger gets nothing even while the window is open.
	if w := get(t, h, "100.64.0.10", "/pair"); w.Code != http.StatusNotFound {
		t.Errorf("stranger during pairing got %d, want 404", w.Code)
	}
}

func TestGateLockdownLeavesOnlyHealth(t *testing.T) {
	g, _, _, _, lockdown := gateFixture(t)
	h := g.Middleware(okHandler())
	*lockdown = true

	if w := get(t, h, phoneIP, "/healthz"); w.Code != http.StatusOK {
		t.Errorf("/healthz during lockdown got %d, want 200", w.Code)
	}
	for _, path := range []string{"/", "/api/system", "/api/sessions", "/pair", "/api/auth/login/begin"} {
		if w := get(t, h, phoneIP, path); w.Code != http.StatusNotFound {
			t.Errorf("%s during lockdown got %d, want 404", path, w.Code)
		}
	}
}

func TestGatePutsDeviceInContext(t *testing.T) {
	g, _, _, _, _ := gateFixture(t)
	var gotKind config.DeviceKind
	h := g.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d, ok := DeviceFrom(r.Context())
		if !ok {
			t.Error("no device in context behind the gate")
		}
		gotKind = d.Kind
		if IsPairingCandidate(r.Context()) {
			t.Error("a paired device was marked a pairing candidate")
		}
	}))

	get(t, h, laptopIP, "/api/system")
	if gotKind != config.KindLaptop {
		t.Errorf("device kind = %q, want laptop", gotKind)
	}
}
