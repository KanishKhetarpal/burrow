package auth

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"burrow/internal/audit"
	"burrow/internal/config"
	"burrow/internal/store"
)

// fixture builds a manager over an in-memory store with a controllable clock.
// WebAuthn ceremonies need a real authenticator, so these tests cover
// everything around them: the pairing window, lockout, session lifetime, the
// node binding, step-up freshness and lockdown.
type fixture struct {
	m   *Manager
	st  *store.Store
	cfg *config.Config
	now time.Time
	log []audit.Entry
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	f := &fixture{
		st:  st,
		now: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC),
		cfg: &config.Config{
			Owner:  config.Owner{TailscaleLogin: "owner@example.com"},
			Device: config.Device{MaxDevices: 3},
			Agent: config.Agent{Auth: config.AuthCfg{
				SessionIdleMinutes:   30,
				SessionAbsoluteHours: 12,
				StepupSeconds:        300,
				LoginMaxFailures:     5,
				LockoutMinutes:       15,
				PairingWindowMinutes: 10,
			}},
		},
	}
	f.m = NewManager(st,
		func() *config.Config { return f.cfg },
		func(e audit.Entry) { f.log = append(f.log, e) },
		func() time.Time { return f.now },
	)
	if err := f.m.SetRelyingParty("n-ab12cd34.tail1234.ts.net"); err != nil {
		t.Fatalf("SetRelyingParty: %v", err)
	}
	return f
}

// addDevice inserts a paired device with one credential, standing in for a
// completed pairing ceremony.
func (f *fixture) addDevice(t *testing.T, id, name, kind, nodeID string) store.Device {
	t.Helper()
	dev := store.Device{
		ID: id, Name: name, Kind: kind, NodeID: nodeID,
		Login: f.cfg.Owner.TailscaleLogin, PairedAt: f.now,
	}
	if err := f.st.AddDevice(context.Background(), dev); err != nil {
		t.Fatalf("AddDevice: %v", err)
	}
	if err := f.st.AddCredential(context.Background(), store.Credential{
		ID: "cred-" + id, DeviceID: id, PublicKey: []byte("pk"), CreatedAt: f.now,
	}); err != nil {
		t.Fatalf("AddCredential: %v", err)
	}
	return dev
}

// session creates a logged-in session for a device, with step-up fresh.
func (f *fixture) session(t *testing.T, dev store.Device) store.AuthSession {
	t.Helper()
	at := f.now
	s := store.AuthSession{
		ID: "sess-" + dev.ID, DeviceID: dev.ID, NodeID: dev.NodeID,
		CreatedAt: at, LastSeenAt: at, StepupAt: &at,
	}
	if err := f.st.CreateAuthSession(context.Background(), s); err != nil {
		t.Fatalf("CreateAuthSession: %v", err)
	}
	return s
}

// ---------- pairing window ----------

func TestPairingWindowOpensAndCloses(t *testing.T) {
	f := newFixture(t)
	if f.m.PairingOpen() {
		t.Fatal("a pairing window is open before `burrow pair` ran")
	}
	code, expires, err := f.m.OpenPairing()
	if err != nil {
		t.Fatalf("OpenPairing: %v", err)
	}
	if len(code) != 6 {
		t.Errorf("code = %q, want 6 digits", code)
	}
	if !expires.Equal(f.now.Add(10 * time.Minute)) {
		t.Errorf("expires = %v, want 10 minutes out", expires)
	}
	if !f.m.PairingOpen() {
		t.Fatal("PairingOpen = false right after opening")
	}

	f.m.ClosePairing()
	if f.m.PairingOpen() {
		t.Fatal("PairingOpen = true after ClosePairing")
	}
}

func TestPairingWindowExpires(t *testing.T) {
	f := newFixture(t)
	if _, _, err := f.m.OpenPairing(); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(10*time.Minute + time.Second)
	if f.m.PairingOpen() {
		t.Fatal("the window is still open past its expiry")
	}
}

func TestPairingCodeAttemptsCloseTheWindow(t *testing.T) {
	f := newFixture(t)
	code, _, err := f.m.OpenPairing()
	if err != nil {
		t.Fatal(err)
	}

	for i := 1; i < PairingMaxAttempts; i++ {
		if err := f.m.checkPairingCode("000000"); !errors.Is(err, ErrPairingCode) {
			t.Fatalf("attempt %d = %v, want ErrPairingCode", i, err)
		}
		if !f.m.PairingOpen() {
			t.Fatalf("the window closed after %d wrong codes, want it open until %d", i, PairingMaxAttempts)
		}
	}
	// The last allowed attempt closes the window.
	if err := f.m.checkPairingCode("000000"); !errors.Is(err, ErrPairingCode) {
		t.Fatalf("final attempt = %v, want ErrPairingCode", err)
	}
	if f.m.PairingOpen() {
		t.Fatalf("the window is still open after %d wrong codes", PairingMaxAttempts)
	}
	// Even the right code no longer works: pairing has to be restarted at the PC.
	if err := f.m.checkPairingCode(code); !errors.Is(err, ErrPairingClosed) {
		t.Fatalf("correct code after lockout = %v, want ErrPairingClosed", err)
	}
}

func TestPairingCodeAcceptsTheRightCode(t *testing.T) {
	f := newFixture(t)
	code, _, err := f.m.OpenPairing()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.m.checkPairingCode(code); err != nil {
		t.Fatalf("correct code = %v, want nil", err)
	}
}

func TestPairingRefusedDuringLockdown(t *testing.T) {
	f := newFixture(t)
	if err := f.m.EnterLockdown(context.Background(), "console"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.m.OpenPairing(); !errors.Is(err, ErrLockdown) {
		t.Fatalf("OpenPairing during lockdown = %v, want ErrLockdown", err)
	}
}

func TestPairBeginRefusesWhenAtDeviceLimit(t *testing.T) {
	f := newFixture(t)
	f.cfg.Device.MaxDevices = 2
	f.addDevice(t, "d1", "iPhone", "phone", "nPHONE")
	f.addDevice(t, "d2", "Laptop", "laptop", "nLAPTOP")

	code, _, err := f.m.OpenPairing()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = f.m.PairBegin(context.Background(), "nTHIRD", f.cfg.Owner.TailscaleLogin, code, "iPad", "phone")
	if !errors.Is(err, ErrTooManyDevices) {
		t.Fatalf("PairBegin at the limit = %v, want ErrTooManyDevices", err)
	}
}

func TestPairBeginRefusesWrongCodeAndAudits(t *testing.T) {
	f := newFixture(t)
	if _, _, err := f.m.OpenPairing(); err != nil {
		t.Fatal(err)
	}
	_, _, err := f.m.PairBegin(context.Background(), "nNEW", f.cfg.Owner.TailscaleLogin, "999999", "iPad", "phone")
	if !errors.Is(err, ErrPairingCode) {
		t.Fatalf("PairBegin with a wrong code = %v, want ErrPairingCode", err)
	}
	if len(f.log) != 1 || f.log[0].Action != audit.ActionPair || f.log[0].Result != audit.ResultDenied {
		t.Fatalf("audit = %+v, want one denied auth.pair entry", f.log)
	}
}

func TestPairBeginRefusesBadDeviceKind(t *testing.T) {
	f := newFixture(t)
	code, _, err := f.m.OpenPairing()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.m.PairBegin(context.Background(), "nNEW", f.cfg.Owner.TailscaleLogin, code, "Watch", "watch"); err == nil {
		t.Fatal("PairBegin accepted an unknown device kind")
	}
}

// ---------- sessions ----------

func TestValidateAcceptsAFreshSession(t *testing.T) {
	f := newFixture(t)
	dev := f.addDevice(t, "d1", "iPhone", "phone", "nPHONE")
	s := f.session(t, dev)

	got, err := f.m.Validate(context.Background(), s.ID, "nPHONE")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got.Device.Name != "iPhone" {
		t.Errorf("device = %q, want iPhone", got.Device.Name)
	}
	if !got.StepupFresh(f.now, f.m.StepupWindow()) {
		t.Error("a new session is not step-up fresh, want it fresh")
	}
}

func TestValidateRejectsUnknownOrEmptySession(t *testing.T) {
	f := newFixture(t)
	f.addDevice(t, "d1", "iPhone", "phone", "nPHONE")
	for _, id := range []string{"", "nope"} {
		if _, err := f.m.Validate(context.Background(), id, "nPHONE"); !errors.Is(err, ErrNoSession) {
			t.Errorf("Validate(%q) = %v, want ErrNoSession", id, err)
		}
	}
}

func TestValidateRejectsSessionFromAnotherNode(t *testing.T) {
	f := newFixture(t)
	phone := f.addDevice(t, "d1", "iPhone", "phone", "nPHONE")
	f.addDevice(t, "d2", "Laptop", "laptop", "nLAPTOP")
	s := f.session(t, phone)

	// The same cookie presented from the laptop's node must not work.
	if _, err := f.m.Validate(context.Background(), s.ID, "nLAPTOP"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("Validate from another node = %v, want ErrNoSession", err)
	}
}

func TestValidateEnforcesIdleTimeout(t *testing.T) {
	f := newFixture(t)
	dev := f.addDevice(t, "d1", "iPhone", "phone", "nPHONE")
	s := f.session(t, dev)

	// Just inside the idle window: still valid, and the window slides.
	f.now = f.now.Add(29 * time.Minute)
	if _, err := f.m.Validate(context.Background(), s.ID, "nPHONE"); err != nil {
		t.Fatalf("Validate at 29 min = %v, want nil", err)
	}
	// Another 29 minutes from the touch, so 58 overall: still valid because
	// activity slid the window.
	f.now = f.now.Add(29 * time.Minute)
	if _, err := f.m.Validate(context.Background(), s.ID, "nPHONE"); err != nil {
		t.Fatalf("Validate after a sliding touch = %v, want nil", err)
	}
	// Now go quiet past the idle limit.
	f.now = f.now.Add(31 * time.Minute)
	if _, err := f.m.Validate(context.Background(), s.ID, "nPHONE"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("Validate after idling out = %v, want ErrNoSession", err)
	}
}

func TestValidateEnforcesAbsoluteTimeout(t *testing.T) {
	f := newFixture(t)
	dev := f.addDevice(t, "d1", "iPhone", "phone", "nPHONE")
	s := f.session(t, dev)

	// Stay active every 25 minutes, so the idle window never lapses. The
	// absolute limit must still end the session.
	const step = 25 * time.Minute
	deadline := s.CreatedAt.Add(f.m.AbsoluteLifetime())
	for f.now.Before(deadline.Add(-step)) {
		f.now = f.now.Add(step)
		if _, err := f.m.Validate(context.Background(), s.ID, "nPHONE"); err != nil {
			t.Fatalf("session died after %v of continuous activity, before the %v limit",
				f.now.Sub(s.CreatedAt), f.m.AbsoluteLifetime())
		}
	}
	// Step past the absolute limit, still well inside the idle window.
	f.now = deadline.Add(time.Second)
	if _, err := f.m.Validate(context.Background(), s.ID, "nPHONE"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("Validate past the %v absolute limit = %v, want ErrNoSession",
			f.m.AbsoluteLifetime(), err)
	}
}

func TestLogoutRevokesImmediately(t *testing.T) {
	f := newFixture(t)
	dev := f.addDevice(t, "d1", "iPhone", "phone", "nPHONE")
	s := f.session(t, dev)

	if err := f.m.Logout(context.Background(), s.ID); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := f.m.Validate(context.Background(), s.ID, "nPHONE"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("Validate after logout = %v, want ErrNoSession", err)
	}
}

func TestRevokeDeviceKillsItsSessionsButNotTheOthers(t *testing.T) {
	f := newFixture(t)
	phone := f.addDevice(t, "d1", "iPhone", "phone", "nPHONE")
	laptop := f.addDevice(t, "d2", "Laptop", "laptop", "nLAPTOP")
	ps := f.session(t, phone)
	ls := f.session(t, laptop)

	if err := f.m.RevokeDevice(context.Background(), phone.ID); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	if _, err := f.m.Validate(context.Background(), ps.ID, "nPHONE"); !errors.Is(err, ErrNoSession) {
		t.Errorf("the revoked device's session = %v, want ErrNoSession", err)
	}
	if _, err := f.m.Validate(context.Background(), ls.ID, "nLAPTOP"); err != nil {
		t.Errorf("the other device's session = %v, want it unaffected", err)
	}
	// Its passkey must stop working too.
	if _, err := f.st.CredentialByID(context.Background(), "cred-"+phone.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the revoked device's credential is still usable: %v", err)
	}
}

// ---------- step-up ----------

func TestStepupFreshnessWindow(t *testing.T) {
	f := newFixture(t)
	dev := f.addDevice(t, "d1", "iPhone", "phone", "nPHONE")
	s := f.session(t, dev)

	sess, err := f.m.Validate(context.Background(), s.ID, "nPHONE")
	if err != nil {
		t.Fatal(err)
	}
	window := f.m.StepupWindow()

	if !sess.StepupFresh(f.now.Add(299*time.Second), window) {
		t.Error("step-up at 299s is stale, want fresh inside the 300s window")
	}
	if sess.StepupFresh(f.now.Add(301*time.Second), window) {
		t.Error("step-up at 301s is fresh, want it stale")
	}

	// A session that never stepped up is never fresh.
	noStepup := Session{AuthSession: store.AuthSession{}}
	if noStepup.StepupFresh(f.now, window) {
		t.Error("a session with no assertion reports step-up fresh")
	}
}

func TestMarkStepupRefreshesTheWindow(t *testing.T) {
	f := newFixture(t)
	dev := f.addDevice(t, "d1", "iPhone", "phone", "nPHONE")
	s := f.session(t, dev)

	f.now = f.now.Add(10 * time.Minute) // step-up now stale
	sess, err := f.m.Validate(context.Background(), s.ID, "nPHONE")
	if err != nil {
		t.Fatal(err)
	}
	if sess.StepupFresh(f.now, f.m.StepupWindow()) {
		t.Fatal("step-up is still fresh after 10 minutes")
	}

	if err := f.st.MarkStepup(context.Background(), s.ID, f.now); err != nil {
		t.Fatal(err)
	}
	sess, err = f.m.Validate(context.Background(), s.ID, "nPHONE")
	if err != nil {
		t.Fatal(err)
	}
	if !sess.StepupFresh(f.now, f.m.StepupWindow()) {
		t.Fatal("step-up is stale right after a fresh assertion")
	}
}

// ---------- lockout ----------

func TestLoginLockoutAfterMaxFailures(t *testing.T) {
	f := newFixture(t)
	f.addDevice(t, "d1", "iPhone", "phone", "nPHONE")
	ctx := context.Background()

	for i := 0; i < f.cfg.Agent.Auth.LoginMaxFailures; i++ {
		f.m.recordFailure(ctx, "nPHONE", nil)
	}
	if _, _, err := f.m.LoginBegin(ctx, "nPHONE"); !errors.Is(err, ErrLockedOut) {
		t.Fatalf("LoginBegin after %d failures = %v, want ErrLockedOut",
			f.cfg.Agent.Auth.LoginMaxFailures, err)
	}

	// The lock lifts after the configured window.
	f.now = f.now.Add(f.cfg.Agent.Auth.Lockout() + time.Second)
	if _, _, err := f.m.LoginBegin(ctx, "nPHONE"); errors.Is(err, ErrLockedOut) {
		t.Fatal("still locked out after the lockout window passed")
	}
}

func TestLockoutIsPerNode(t *testing.T) {
	f := newFixture(t)
	f.addDevice(t, "d1", "iPhone", "phone", "nPHONE")
	f.addDevice(t, "d2", "Laptop", "laptop", "nLAPTOP")
	ctx := context.Background()

	for i := 0; i < f.cfg.Agent.Auth.LoginMaxFailures; i++ {
		f.m.recordFailure(ctx, "nPHONE", nil)
	}
	if _, _, err := f.m.LoginBegin(ctx, "nLAPTOP"); errors.Is(err, ErrLockedOut) {
		t.Fatal("locking out the phone also locked out the laptop")
	}
}

func TestClearLoginFailuresResetsTheCounter(t *testing.T) {
	f := newFixture(t)
	f.addDevice(t, "d1", "iPhone", "phone", "nPHONE")
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		f.m.recordFailure(ctx, "nPHONE", nil)
	}
	if err := f.st.ClearLoginFailures(ctx, "nPHONE"); err != nil {
		t.Fatal(err)
	}
	// A fresh run of failures must take the full count again, not one more.
	for i := 0; i < 4; i++ {
		f.m.recordFailure(ctx, "nPHONE", nil)
	}
	if _, _, err := f.m.LoginBegin(ctx, "nPHONE"); errors.Is(err, ErrLockedOut) {
		t.Fatal("locked out after 4 failures following a reset, want the counter restarted")
	}
}

// ---------- lockdown ----------

func TestLockdownKillsSessionsAndPersists(t *testing.T) {
	f := newFixture(t)
	dev := f.addDevice(t, "d1", "iPhone", "phone", "nPHONE")
	s := f.session(t, dev)
	ctx := context.Background()

	if err := f.m.EnterLockdown(ctx, "device:iPhone"); err != nil {
		t.Fatalf("EnterLockdown: %v", err)
	}
	if !f.m.Lockdown() {
		t.Fatal("Lockdown() = false right after entering")
	}
	if _, err := f.m.Validate(ctx, s.ID, "nPHONE"); !errors.Is(err, ErrLockdown) {
		t.Errorf("Validate during lockdown = %v, want ErrLockdown", err)
	}

	// A restart must come back locked down: the flag lives in the store.
	fresh := NewManager(f.st, func() *config.Config { return f.cfg }, nil, func() time.Time { return f.now })
	if err := fresh.LoadLockdown(ctx); err != nil {
		t.Fatalf("LoadLockdown: %v", err)
	}
	if !fresh.Lockdown() {
		t.Fatal("a restarted manager is not locked down; the flag did not persist")
	}

	// And the sessions stay dead after unlock.
	if err := fresh.ExitLockdown(ctx, "console"); err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.Validate(ctx, s.ID, "nPHONE"); !errors.Is(err, ErrNoSession) {
		t.Errorf("session after unlock = %v, want ErrNoSession (it was revoked)", err)
	}
}

func TestLockdownAuditsBothTransitions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.m.EnterLockdown(ctx, "device:iPhone"); err != nil {
		t.Fatal(err)
	}
	if err := f.m.ExitLockdown(ctx, "console"); err != nil {
		t.Fatal(err)
	}
	var enter, exit bool
	for _, e := range f.log {
		switch e.Action {
		case audit.ActionLockdownEnter:
			enter = true
		case audit.ActionLockdownExit:
			exit = true
		}
	}
	if !enter || !exit {
		t.Fatalf("audit = %+v, want both lockdown.enter and lockdown.exit", f.log)
	}
}

// ---------- ceremonies ----------

func TestCeremonyIsSingleUseAndNodeBound(t *testing.T) {
	f := newFixture(t)
	id, err := f.m.putCeremony(&ceremony{kind: ceremonyLogin, nodeID: "nPHONE"})
	if err != nil {
		t.Fatal(err)
	}

	// Wrong node.
	if _, err := f.m.takeCeremony(id, ceremonyLogin, "nLAPTOP"); !errors.Is(err, ErrCeremony) {
		t.Errorf("takeCeremony from another node = %v, want ErrCeremony", err)
	}
	// Consumed by the failed attempt, so even the right node now fails: a
	// challenge is single-use.
	if _, err := f.m.takeCeremony(id, ceremonyLogin, "nPHONE"); !errors.Is(err, ErrCeremony) {
		t.Errorf("second takeCeremony = %v, want ErrCeremony", err)
	}
}

func TestCeremonyKindCannotBeCrossed(t *testing.T) {
	f := newFixture(t)
	id, err := f.m.putCeremony(&ceremony{kind: ceremonyStepup, nodeID: "nPHONE"})
	if err != nil {
		t.Fatal(err)
	}
	// A step-up challenge must not be redeemable as a login, which would turn a
	// step-up prompt into a new 12-hour session.
	if _, err := f.m.takeCeremony(id, ceremonyLogin, "nPHONE"); !errors.Is(err, ErrCeremony) {
		t.Fatalf("step-up challenge used as a login = %v, want ErrCeremony", err)
	}
}

func TestCeremonyExpires(t *testing.T) {
	f := newFixture(t)
	id, err := f.m.putCeremony(&ceremony{kind: ceremonyLogin, nodeID: "nPHONE"})
	if err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(ceremonyTTL + time.Second)
	if _, err := f.m.takeCeremony(id, ceremonyLogin, "nPHONE"); !errors.Is(err, ErrCeremony) {
		t.Fatalf("expired ceremony = %v, want ErrCeremony", err)
	}
}

func TestStepupFinishRejectsAnotherDevicesSession(t *testing.T) {
	f := newFixture(t)
	f.addDevice(t, "d1", "iPhone", "phone", "nPHONE")
	ctx := context.Background()

	// No such session: step-up must not create one.
	r := httptest.NewRequest("POST", "/api/auth/stepup/finish", nil)
	if err := f.m.StepupFinish(ctx, "whatever", "nPHONE", "no-such-session", r); !errors.Is(err, ErrNoSession) {
		t.Fatalf("StepupFinish without a session = %v, want ErrNoSession", err)
	}
}

func TestSetRelyingPartyRequiresAnFQDN(t *testing.T) {
	f := newFixture(t)
	if err := f.m.SetRelyingParty(""); err == nil {
		t.Fatal("SetRelyingParty accepted an empty FQDN")
	}
	// The valid one from the fixture must still be in place.
	if f.m.RelyingPartyID() == "" {
		t.Fatal("the relying party was cleared by a failed call")
	}
}

func TestSixDigitCodeIsSixDigits(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		c, err := sixDigitCode()
		if err != nil {
			t.Fatal(err)
		}
		if len(c) != 6 {
			t.Fatalf("code %q is not 6 characters", c)
		}
		for _, r := range c {
			if r < '0' || r > '9' {
				t.Fatalf("code %q has a non-digit", c)
			}
		}
		seen[c] = true
	}
	// Not a statistical test, just a guard against a constant.
	if len(seen) < 100 {
		t.Errorf("only %d distinct codes in 200 draws; the generator looks weak", len(seen))
	}
}
