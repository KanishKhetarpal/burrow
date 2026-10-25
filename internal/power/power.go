// Package power performs power actions and keeps the machine awake while
// Claude is working.
//
// Sleep deserves a note: with the smart-plug wake method a sleeping PC cannot
// be woken remotely, because turning the plug on does nothing to a machine that
// is already powered. Setup therefore sets AC sleep to Never, and the PWA
// labels Sleep accordingly. Burrow still offers it, because the owner may be
// standing in front of the machine (docs/01-PRD.md §7, US-14).
package power

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"burrow/internal/audit"
)

// Action is a power action.
type Action string

const (
	Sleep     Action = "sleep"
	Hibernate Action = "hibernate"
	Shutdown  Action = "shutdown"
	Restart   Action = "restart"
	Lock      Action = "lock"
)

// Errors the HTTP layer maps to API codes.
var (
	ErrUnknownAction   = errors.New("power: unknown action")
	ErrBadDelay        = errors.New("power: delay must be 0..600 seconds")
	ErrLockUnavailable = errors.New("power: locking the desktop is not available from the service session")
)

// ParseAction validates an action name.
func ParseAction(s string) (Action, error) {
	a := Action(strings.ToLower(strings.TrimSpace(s)))
	switch a {
	case Sleep, Hibernate, Shutdown, Restart, Lock:
		return a, nil
	}
	return "", fmt.Errorf("%w: %q", ErrUnknownAction, s)
}

// PlugOffSafe reports whether the PWA may offer to cut plug power after this
// action. Only a real shutdown leaves the machine off; after sleep or a
// restart, cutting power would be a hard power-off
// (docs/02-ARCHITECTURE.md §3.1).
func PlugOffSafe(a Action) bool { return a == Shutdown }

// WakeableAfter reports whether the smart plug can bring the PC back from the
// state this action leaves it in.
func WakeableAfter(a Action) bool {
	switch a {
	case Shutdown:
		return true // plug on -> BIOS AC restore boots it
	case Sleep, Hibernate:
		return false // the PC still has power, so there is no AC-restore event
	default:
		return true // restart and lock leave it running
	}
}

// Backend performs the OS-level operations. Replaced in tests.
type Backend interface {
	Suspend(hibernate bool) error
	Shutdown(restart bool, delay time.Duration) error
	CancelShutdown() error
	LockWorkstation() error
	// KeepAwake holds off system sleep while held. Calling with false releases.
	KeepAwake(on bool) error
	// InteractiveSession reports whether the process can reach the user's
	// desktop. A service runs in session 0 and cannot.
	InteractiveSession() bool
}

// Manager serialises power actions and owns the keep-awake state.
type Manager struct {
	be  Backend
	log func(audit.Entry)
	now func() time.Time

	mu sync.Mutex
	// keepAwakeReasons tracks why the machine is being held awake, so sessions
	// finishing does not cancel a manual hold and vice versa.
	keepAwakeReasons map[string]time.Time
	keepAwakeOn      bool
	pending          *Pending
}

// Pending describes a scheduled power action.
type Pending struct {
	Action      Action    `json:"action"`
	ScheduledAt time.Time `json:"scheduledAt"`
	PlugOffSafe bool      `json:"plugOffSafe"`
}

// KeepAwakeState is reported by GET /api/system.
type KeepAwakeState struct {
	Active bool       `json:"active"`
	Reason string     `json:"reason,omitempty"`
	Until  *time.Time `json:"until"`
}

// NewManager builds the power manager. be may be nil, in which case the real
// Windows backend is used.
func NewManager(be Backend, log func(audit.Entry)) *Manager {
	if be == nil {
		be = defaultBackend()
	}
	if log == nil {
		log = func(audit.Entry) {}
	}
	return &Manager{
		be:               be,
		log:              log,
		now:              time.Now,
		keepAwakeReasons: map[string]time.Time{},
	}
}

// Do performs a power action after an optional delay.
func (m *Manager) Do(a Action, delay time.Duration, actor string) (Pending, error) {
	if delay < 0 || delay > 10*time.Minute {
		return Pending{}, ErrBadDelay
	}

	p := Pending{
		Action:      a,
		ScheduledAt: m.now().Add(delay),
		PlugOffSafe: PlugOffSafe(a),
	}

	var err error
	switch a {
	case Shutdown, Restart:
		// shutdown.exe owns the delay, so the OS shows its own warning and the
		// action survives the agent being stopped in the meantime.
		err = m.be.Shutdown(a == Restart, delay)
	case Sleep, Hibernate:
		// Suspend has no delay parameter; a scheduled suspend is implemented
		// by waiting here, which is fine because the call returns immediately
		// once the machine comes back.
		if delay > 0 {
			go func() {
				time.Sleep(delay)
				// A release of keep-awake is needed or the machine refuses.
				_ = m.be.Suspend(a == Hibernate)
			}()
		} else {
			err = m.be.Suspend(a == Hibernate)
		}
	case Lock:
		if !m.be.InteractiveSession() {
			err = ErrLockUnavailable
		} else {
			err = m.be.LockWorkstation()
		}
	default:
		return Pending{}, fmt.Errorf("%w: %q", ErrUnknownAction, a)
	}

	result := audit.ResultOK
	meta := map[string]any{"delaySec": int(delay.Seconds())}
	if err != nil {
		result = audit.ResultError
		meta["error"] = err.Error()
	}
	m.log(audit.Entry{
		Actor:  actor,
		Action: auditAction(a),
		Result: result,
		Meta:   meta,
	})
	if err != nil {
		return Pending{}, err
	}

	m.mu.Lock()
	if a == Shutdown || a == Restart {
		m.pending = &p
	}
	m.mu.Unlock()
	return p, nil
}

// Cancel aborts a scheduled shutdown or restart.
func (m *Manager) Cancel(actor string) error {
	if err := m.be.CancelShutdown(); err != nil {
		return err
	}
	m.mu.Lock()
	m.pending = nil
	m.mu.Unlock()
	m.log(audit.Entry{Actor: actor, Action: audit.ActionPowerShutdown,
		Result: audit.ResultOK, Meta: map[string]any{"cancelled": true}})
	return nil
}

// Pending reports a scheduled action, if any.
func (m *Manager) Pending() *Pending {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending == nil {
		return nil
	}
	p := *m.pending
	return &p
}

// ---------- keep-awake ----------

// Reasons for holding the machine awake.
const (
	ReasonSessions = "sessions"
	ReasonManual   = "manual"
)

// SetSessionsRunning is wired to the session manager's running count, so the
// machine stays awake exactly while Claude has work in flight.
func (m *Manager) SetSessionsRunning(n int) {
	if n > 0 {
		m.hold(ReasonSessions, nil)
	} else {
		m.release(ReasonSessions)
	}
}

// SetManual turns the manual keep-awake toggle on or off. minutes of 0 means
// "until switched off".
func (m *Manager) SetManual(on bool, minutes int, actor string) error {
	if !on {
		m.release(ReasonManual)
		m.log(audit.Entry{Actor: actor, Action: audit.ActionPowerKeepAwake,
			Result: audit.ResultOK, Meta: map[string]any{"enabled": false}})
		return nil
	}
	if minutes < 0 || minutes > 720 {
		return fmt.Errorf("power: keep-awake minutes must be 0..720")
	}
	var until *time.Time
	if minutes > 0 {
		t := m.now().Add(time.Duration(minutes) * time.Minute)
		until = &t
	}
	m.hold(ReasonManual, until)
	m.log(audit.Entry{Actor: actor, Action: audit.ActionPowerKeepAwake,
		Result: audit.ResultOK, Meta: map[string]any{"enabled": true, "minutes": minutes}})
	return nil
}

func (m *Manager) hold(reason string, until *time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if until != nil {
		m.keepAwakeReasons[reason] = *until
	} else {
		// The zero time means "no expiry".
		m.keepAwakeReasons[reason] = time.Time{}
	}
	m.applyLocked()
}

func (m *Manager) release(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.keepAwakeReasons, reason)
	m.applyLocked()
}

// applyLocked turns the OS-level hold on or off to match the reasons held.
func (m *Manager) applyLocked() {
	m.expireLocked()
	want := len(m.keepAwakeReasons) > 0
	if want == m.keepAwakeOn {
		return
	}
	if err := m.be.KeepAwake(want); err != nil {
		// Nothing useful to do: the action the user asked for has already
		// happened, and the next change will try again.
		return
	}
	m.keepAwakeOn = want
}

func (m *Manager) expireLocked() {
	now := m.now()
	for r, until := range m.keepAwakeReasons {
		if !until.IsZero() && now.After(until) {
			delete(m.keepAwakeReasons, r)
		}
	}
}

// KeepAwake reports the current state for GET /api/system.
func (m *Manager) KeepAwake() KeepAwakeState {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked()
	if len(m.keepAwakeReasons) == 0 {
		return KeepAwakeState{}
	}
	// Sessions take precedence in the reported reason: it is the one the user
	// did not ask for and so the one worth explaining.
	reason := ReasonManual
	if _, ok := m.keepAwakeReasons[ReasonSessions]; ok {
		reason = ReasonSessions
	}
	st := KeepAwakeState{Active: true, Reason: reason}
	if until, ok := m.keepAwakeReasons[reason]; ok && !until.IsZero() {
		u := until
		st.Until = &u
	}
	return st
}

// Tick expires timed holds. Called periodically by the agent, since a hold with
// an expiry must end even if nothing else happens.
func (m *Manager) Tick() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.applyLocked()
}

func auditAction(a Action) audit.Action {
	switch a {
	case Sleep:
		return audit.ActionPowerSleep
	case Hibernate:
		return audit.ActionPowerHibernate
	case Shutdown:
		return audit.ActionPowerShutdown
	case Restart:
		return audit.ActionPowerRestart
	case Lock:
		return audit.ActionPowerLock
	default:
		return audit.Action("power." + string(a))
	}
}
