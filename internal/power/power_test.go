package power

import (
	"errors"
	"sync"
	"testing"
	"time"

	"burrow/internal/audit"
)

// fakeBackend records calls instead of touching the machine.
type fakeBackend struct {
	mu          sync.Mutex
	suspends    []bool // true = hibernate
	shutdowns   []shutdownCall
	cancels     int
	locks       int
	keepAwake   bool
	keepCalls   []bool
	interactive bool
	err         error
}

type shutdownCall struct {
	restart bool
	delay   time.Duration
}

func (f *fakeBackend) Suspend(h bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.suspends = append(f.suspends, h)
	return nil
}

func (f *fakeBackend) Shutdown(restart bool, d time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.shutdowns = append(f.shutdowns, shutdownCall{restart, d})
	return nil
}

func (f *fakeBackend) CancelShutdown() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels++
	return f.err
}

func (f *fakeBackend) LockWorkstation() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.locks++
	return nil
}

func (f *fakeBackend) KeepAwake(on bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.keepAwake = on
	f.keepCalls = append(f.keepCalls, on)
	return nil
}

func (f *fakeBackend) InteractiveSession() bool { return f.interactive }

func (f *fakeBackend) held() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.keepAwake
}

func (f *fakeBackend) calls() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]bool, len(f.keepCalls))
	copy(out, f.keepCalls)
	return out
}

func newPowerFixture(t *testing.T) (*Manager, *fakeBackend, *[]audit.Entry, *time.Time) {
	t.Helper()
	be := &fakeBackend{interactive: true}
	var entries []audit.Entry
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	m := NewManager(be, func(e audit.Entry) { entries = append(entries, e) })
	m.now = func() time.Time { return now }
	return m, be, &entries, &now
}

func TestParseAction(t *testing.T) {
	for _, in := range []string{"sleep", "SLEEP", " restart ", "hibernate", "shutdown", "lock"} {
		if _, err := ParseAction(in); err != nil {
			t.Errorf("ParseAction(%q) = %v, want it accepted", in, err)
		}
	}
	for _, bad := range []string{"", "poweroff", "halt", "reboot"} {
		if _, err := ParseAction(bad); !errors.Is(err, ErrUnknownAction) {
			t.Errorf("ParseAction(%q) = %v, want ErrUnknownAction", bad, err)
		}
	}
}

// TestPlugOffSafe is the rule that keeps the PWA from cutting power to a
// machine that is still running (docs/02-ARCHITECTURE.md §3.1).
func TestPlugOffSafe(t *testing.T) {
	if !PlugOffSafe(Shutdown) {
		t.Error("PlugOffSafe(shutdown) = false, want true")
	}
	for _, a := range []Action{Sleep, Hibernate, Restart, Lock} {
		if PlugOffSafe(a) {
			t.Errorf("PlugOffSafe(%q) = true; cutting power after this would be a hard power-off", a)
		}
	}
}

// TestWakeableAfter covers the smart-plug consequence the PWA labels: a
// sleeping PC cannot be woken by turning the plug on.
func TestWakeableAfter(t *testing.T) {
	if !WakeableAfter(Shutdown) {
		t.Error("WakeableAfter(shutdown) = false, want true (BIOS AC restore boots it)")
	}
	for _, a := range []Action{Sleep, Hibernate} {
		if WakeableAfter(a) {
			t.Errorf("WakeableAfter(%q) = true, want false", a)
		}
	}
}

func TestDoShutdownAndRestart(t *testing.T) {
	m, be, entries, now := newPowerFixture(t)

	p, err := m.Do(Shutdown, 30*time.Second, "device:iPhone")
	if err != nil {
		t.Fatalf("Do(shutdown): %v", err)
	}
	if !p.PlugOffSafe {
		t.Error("the shutdown response does not report plugOffSafe")
	}
	if !p.ScheduledAt.Equal(now.Add(30 * time.Second)) {
		t.Errorf("ScheduledAt = %v, want 30s out", p.ScheduledAt)
	}
	if len(be.shutdowns) != 1 || be.shutdowns[0].restart || be.shutdowns[0].delay != 30*time.Second {
		t.Errorf("backend calls = %+v, want one shutdown with a 30s delay", be.shutdowns)
	}
	if m.Pending() == nil || m.Pending().Action != Shutdown {
		t.Error("Pending does not report the scheduled shutdown")
	}

	if _, err := m.Do(Restart, 0, "device:iPhone"); err != nil {
		t.Fatalf("Do(restart): %v", err)
	}
	if len(be.shutdowns) != 2 || !be.shutdowns[1].restart {
		t.Errorf("backend calls = %+v, want a restart second", be.shutdowns)
	}

	var sawShutdown, sawRestart bool
	for _, e := range *entries {
		switch e.Action {
		case audit.ActionPowerShutdown:
			sawShutdown = true
		case audit.ActionPowerRestart:
			sawRestart = true
		}
	}
	if !sawShutdown || !sawRestart {
		t.Errorf("audit = %+v, want power.shutdown and power.restart", *entries)
	}
}

func TestDoRejectsBadDelay(t *testing.T) {
	m, _, _, _ := newPowerFixture(t)
	for _, d := range []time.Duration{-time.Second, 11 * time.Minute} {
		if _, err := m.Do(Shutdown, d, "device:iPhone"); !errors.Is(err, ErrBadDelay) {
			t.Errorf("Do with delay %v = %v, want ErrBadDelay", d, err)
		}
	}
}

func TestDoSleepReleasesNothingItDoesNotHold(t *testing.T) {
	m, be, _, _ := newPowerFixture(t)
	if _, err := m.Do(Sleep, 0, "device:iPhone"); err != nil {
		t.Fatalf("Do(sleep): %v", err)
	}
	if len(be.suspends) != 1 || be.suspends[0] {
		t.Errorf("suspends = %v, want one sleep (not hibernate)", be.suspends)
	}
	if _, err := m.Do(Hibernate, 0, "device:iPhone"); err != nil {
		t.Fatal(err)
	}
	if len(be.suspends) != 2 || !be.suspends[1] {
		t.Errorf("suspends = %v, want a hibernate second", be.suspends)
	}
}

func TestLockRefusedFromTheServiceSession(t *testing.T) {
	m, be, _, _ := newPowerFixture(t)
	be.interactive = false

	if _, err := m.Do(Lock, 0, "device:iPhone"); !errors.Is(err, ErrLockUnavailable) {
		t.Fatalf("Do(lock) from session 0 = %v, want ErrLockUnavailable", err)
	}
	if be.locks != 0 {
		t.Error("LockWorkstation was called although it could not work")
	}

	// In an interactive session it goes through.
	be.interactive = true
	if _, err := m.Do(Lock, 0, "device:iPhone"); err != nil {
		t.Fatalf("Do(lock) interactive: %v", err)
	}
	if be.locks != 1 {
		t.Errorf("locks = %d, want 1", be.locks)
	}
}

func TestDoRecordsBackendFailures(t *testing.T) {
	m, be, entries, _ := newPowerFixture(t)
	be.err = errors.New("the OS said no")

	if _, err := m.Do(Shutdown, 0, "device:iPhone"); err == nil {
		t.Fatal("Do reported success although the backend failed")
	}
	if m.Pending() != nil {
		t.Error("a failed shutdown was recorded as pending")
	}
	if len(*entries) != 1 || (*entries)[0].Result != audit.ResultError {
		t.Fatalf("audit = %+v, want one error entry", *entries)
	}
}

func TestCancel(t *testing.T) {
	m, be, _, _ := newPowerFixture(t)
	if _, err := m.Do(Shutdown, 60*time.Second, "device:iPhone"); err != nil {
		t.Fatal(err)
	}
	if err := m.Cancel("device:iPhone"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if be.cancels != 1 {
		t.Errorf("cancels = %d, want 1", be.cancels)
	}
	if m.Pending() != nil {
		t.Error("Pending still reports a shutdown after Cancel")
	}
}

// TestKeepAwakeReasonsAreIndependent is the behaviour that matters in practice:
// a session finishing must not cancel a manual hold, and vice versa.
func TestKeepAwakeReasonsAreIndependent(t *testing.T) {
	m, be, _, _ := newPowerFixture(t)

	m.SetSessionsRunning(2)
	if !be.held() {
		t.Fatal("running sessions did not hold the machine awake")
	}
	if err := m.SetManual(true, 0, "device:iPhone"); err != nil {
		t.Fatal(err)
	}

	// Sessions finish; the manual hold must keep it awake.
	m.SetSessionsRunning(0)
	if !be.held() {
		t.Error("the manual hold was released when sessions finished")
	}
	st := m.KeepAwake()
	if !st.Active || st.Reason != ReasonManual {
		t.Errorf("KeepAwake = %+v, want active for the manual reason", st)
	}

	// Manual off, and nothing else holding: released.
	if err := m.SetManual(false, 0, "device:iPhone"); err != nil {
		t.Fatal(err)
	}
	if be.held() {
		t.Error("the machine is still held awake with no reason left")
	}
	if m.KeepAwake().Active {
		t.Error("KeepAwake reports active with no reason left")
	}
}

func TestKeepAwakeSessionsReasonIsReportedFirst(t *testing.T) {
	m, _, _, _ := newPowerFixture(t)
	if err := m.SetManual(true, 30, "device:iPhone"); err != nil {
		t.Fatal(err)
	}
	m.SetSessionsRunning(1)

	st := m.KeepAwake()
	if st.Reason != ReasonSessions {
		t.Errorf("Reason = %q, want %q (the one the user did not ask for)", st.Reason, ReasonSessions)
	}
}

func TestKeepAwakeTimedHoldExpires(t *testing.T) {
	m, be, _, now := newPowerFixture(t)
	if err := m.SetManual(true, 30, "device:iPhone"); err != nil {
		t.Fatal(err)
	}
	if !be.held() {
		t.Fatal("the timed hold did not take effect")
	}
	if st := m.KeepAwake(); st.Until == nil {
		t.Error("KeepAwake does not report an expiry for a timed hold")
	}

	*now = now.Add(31 * time.Minute)
	m.Tick()
	if be.held() {
		t.Error("the timed hold is still in force past its expiry")
	}
	if m.KeepAwake().Active {
		t.Error("KeepAwake reports active past the expiry")
	}
}

func TestKeepAwakeDoesNotRepeatTheOSCall(t *testing.T) {
	m, be, _, _ := newPowerFixture(t)
	m.SetSessionsRunning(1)
	m.SetSessionsRunning(2)
	m.SetSessionsRunning(3)

	// Three reports of "sessions are running" are one hold.
	if got := be.calls(); len(got) != 1 || !got[0] {
		t.Errorf("backend KeepAwake calls = %v, want exactly one true", got)
	}
}

func TestSetManualRejectsOutOfRangeMinutes(t *testing.T) {
	m, _, _, _ := newPowerFixture(t)
	for _, mins := range []int{-1, 721} {
		if err := m.SetManual(true, mins, "device:iPhone"); err == nil {
			t.Errorf("SetManual(%d minutes) was accepted", mins)
		}
	}
}
