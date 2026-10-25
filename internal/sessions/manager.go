package sessions

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"burrow/internal/audit"
	"burrow/internal/config"
	"burrow/internal/pty"
)

// Errors the HTTP layer maps to API codes.
var (
	ErrMaxSessions = errors.New("sessions: session limit reached")
	ErrNotFound    = errors.New("sessions: no such session")
	ErrNotRunning  = errors.New("sessions: session is not running")
	ErrStillRuns   = errors.New("sessions: session is still running")
	ErrBadProject  = errors.New("sessions: unknown project")
	ErrBadLauncher = errors.New("sessions: unknown launcher")
)

// Starter starts a process on a pseudo-console. Swapped for a fake in tests, so
// the manager's lifecycle can be exercised without launching Claude Code.
type Starter func(pty.Config) (pty.Process, error)

// Manager owns every session.
type Manager struct {
	cfg   func() *config.Config
	log   func(audit.Entry)
	start Starter
	now   func() time.Time

	mu       sync.RWMutex
	sessions map[string]*liveSession
	// keepAwake is called with the number of running sessions whenever it
	// changes, so the power module can hold the machine awake while Claude
	// works (docs/01-PRD.md US-7).
	keepAwake func(running int)

	wg sync.WaitGroup
}

// NewManager builds a session manager. start may be nil, in which case the real
// ConPTY starter is used.
func NewManager(cfg func() *config.Config, log func(audit.Entry), start Starter) *Manager {
	if log == nil {
		log = func(audit.Entry) {}
	}
	if start == nil {
		start = pty.Start
	}
	return &Manager{
		cfg:       cfg,
		log:       log,
		start:     start,
		now:       time.Now,
		sessions:  map[string]*liveSession{},
		keepAwake: func(int) {},
	}
}

// OnRunningCountChange registers the keep-awake callback.
func (m *Manager) OnRunningCountChange(fn func(running int)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if fn != nil {
		m.keepAwake = fn
	}
}

// StartOptions is a request to start a session.
type StartOptions struct {
	ProjectID      string
	Name           string
	Mode           Mode
	PermissionMode string
	Spawn          Spawn
	Actor          string
	// Launcher names a launcher from the config; it supplies the Claude login
	// (CLAUDE_CONFIG_DIR), and the project and permission mode when the
	// request leaves them out.
	Launcher string
}

// Start launches a new Claude session.
func (m *Manager) Start(opts StartOptions) (Session, error) {
	cfg := m.cfg()
	if cfg == nil {
		return Session{}, errors.New("sessions: no config")
	}

	var launcher config.Launcher
	if opts.Launcher != "" {
		l, ok := cfg.Launcher(opts.Launcher)
		if !ok {
			return Session{}, ErrBadLauncher
		}
		launcher = l
		if opts.ProjectID == "" && l.Project != "" {
			for _, p := range cfg.Projects {
				if strings.EqualFold(p.Name, l.Project) {
					opts.ProjectID = ProjectID(p)
				}
			}
		}
		if opts.PermissionMode == "" {
			opts.PermissionMode = l.PermissionMode
		}
	}

	// A launcher with its own folder starts there when the request names no
	// project: the one-tap "open Claude here" case.
	var project config.Project
	if opts.ProjectID == "" && launcher.Path != "" {
		project = config.Project{Name: launcher.Name, Path: launcher.Path}
	} else {
		p, ok := findProject(cfg, opts.ProjectID)
		if !ok {
			return Session{}, ErrBadProject
		}
		project = p
	}

	if n := m.RunningCount(); n >= cfg.Claude.MaxSessions {
		return Session{}, fmt.Errorf("%w: %d of %d running", ErrMaxSessions, n, cfg.Claude.MaxSessions)
	}

	mode := opts.Mode
	if mode == "" {
		mode = Mode(cfg.Claude.DefaultMode)
	}
	if mode != ModeChat && mode != ModeServer {
		return Session{}, fmt.Errorf("sessions: mode must be chat or server, got %q", mode)
	}

	permMode := opts.PermissionMode
	if permMode == "" {
		permMode = cfg.Claude.DefaultPermissionMode
	}
	// bypassPermissions is refused here unless the owner enabled it in
	// burrow.yaml, because this is the path a client request takes
	// (docs/03-SECURITY.md T7). The web layer also demands a fresh step-up.
	if err := config.ValidateSessionPermissionMode(permMode, cfg.Claude.AllowBypassPermissions); err != nil {
		return Session{}, fmt.Errorf("sessions: %w", err)
	}

	spawn := opts.Spawn
	if mode == ModeServer && spawn == "" {
		spawn = Spawn(cfg.Claude.ServerSpawn)
	}
	if mode == ModeChat {
		spawn = ""
	}

	name := strings.TrimSpace(opts.Name)
	if name == "" {
		name = defaultSessionName(project.Name, m.now())
	}

	id := "s_" + ulid.Make().String()
	live := &liveSession{
		scroll: newRing(cfg.Claude.ScrollbackBytes),
		window: newRing(8 << 10), // the 8 KiB detection window from §5.4
		done:   make(chan struct{}),
		meta: Session{
			ID:             id,
			Name:           name,
			ProjectID:      opts.ProjectID,
			CWD:            project.Path,
			Mode:           mode,
			PermissionMode: permMode,
			Launcher:       launcher.Name,
			Spawn:          spawn,
			Status:         StatusStarting,
			StartedAt:      m.now(),
		},
	}

	if err := m.spawn(cfg, live); err != nil {
		return Session{}, err
	}

	m.mu.Lock()
	m.sessions[id] = live
	m.mu.Unlock()
	m.notifyRunning()

	m.log(audit.Entry{
		Actor:  opts.Actor,
		Action: audit.ActionSessionStart,
		Target: name,
		Result: audit.ResultOK,
		Meta: map[string]any{
			"mode": string(mode), "project": project.Name, "permissionMode": permMode,
		},
	})
	return live.snapshot(), nil
}

// spawn starts the child process for a session and begins reading it.
func (m *Manager) spawn(cfg *config.Config, live *liveSession) error {
	meta := live.snapshot()
	args := claudeArgs(cfg, meta, false)

	env := sessionEnv(cfg, meta)

	proc, err := m.start(pty.Config{
		Path:       cfg.Claude.Binary,
		Args:       args,
		Dir:        meta.CWD,
		Env:        env,
		Size:       pty.DefaultSize,
		Restricted: cfg.Claude.RestrictedToken,
	})
	if err != nil {
		live.mu.Lock()
		live.meta.Status = StatusFailed
		live.mu.Unlock()
		return fmt.Errorf("sessions: start claude: %w", err)
	}

	live.mu.Lock()
	live.proc = proc
	live.meta.PID = proc.PID()
	live.meta.Status = StatusStarting
	live.stopping = false
	live.mu.Unlock()

	m.wg.Add(1)
	go m.pump(live, proc)
	return nil
}

// sessionEnv is the extra environment for a session's Claude process. Used
// for both start and resume, so a resumed session keeps its login.
func sessionEnv(cfg *config.Config, s Session) map[string]string {
	env := map[string]string{}
	if cfg.Claude.GitBashPath != "" {
		// Claude Code needs to be told where bash is when it is not on PATH.
		env["CLAUDE_CODE_GIT_BASH_PATH"] = cfg.Claude.GitBashPath
	}
	if s.Launcher != "" {
		if l, ok := cfg.Launcher(s.Launcher); ok && l.ClaudeConfigDir != "" {
			env["CLAUDE_CONFIG_DIR"] = l.ClaudeConfigDir
		}
	}
	return env
}

// claudeArgs builds the command line (docs/02-ARCHITECTURE.md §3.2).
func claudeArgs(cfg *config.Config, s Session, resume bool) []string {
	switch s.Mode {
	case ModeServer:
		args := []string{"remote-control", "--name", s.Name}
		if resume {
			args = append(args, "--continue")
		} else {
			args = append(args, "--spawn", string(s.Spawn),
				"--capacity", fmt.Sprint(cfg.Claude.ServerCapacity))
		}
		return args
	default:
		args := []string{}
		if resume {
			args = append(args, "--continue")
		}
		args = append(args, "--remote-control", s.Name)
		// Only passed when it differs from Claude's own default, so a future
		// change to that default is inherited rather than overridden.
		if s.PermissionMode != "" && s.PermissionMode != "default" {
			args = append(args, "--permission-mode", s.PermissionMode)
		}
		return args
	}
}

// pump reads PTY output, tees it into the scrollback and detection window, and
// runs the detectors.
func (m *Manager) pump(live *liveSession, proc pty.Process) {
	defer m.wg.Done()

	buf := make([]byte, 16<<10)
	for {
		n, err := proc.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			live.scroll.Write(chunk)
			live.window.Write(chunk)
			live.fanout(chunk)
			m.detect(live)
		}
		if err != nil {
			break
		}
	}

	code, waitErr := proc.Wait()
	ended := m.now()

	live.mu.Lock()
	wasStopping := live.stopping
	live.meta.EndedAt = &ended
	live.meta.ExitCode = &code
	switch {
	case wasStopping:
		live.meta.Status = StatusStopped
	case waitErr != nil || code != 0:
		live.meta.Status = StatusFailed
	default:
		live.meta.Status = StatusExited
	}
	status := live.meta.Status
	mode := live.meta.Mode
	restarts := live.meta.Restarts
	live.mu.Unlock()

	live.emit(Event{T: "exit", Status: status, ExitCode: &code})
	proc.Close()
	close(live.done)
	m.notifyRunning()

	// Server mode exits on its own after about ten minutes offline, so it is
	// restarted with backoff. Chat mode reconnects internally and is left
	// alone; restarting it would lose the conversation's terminal state.
	if mode == ModeServer && !wasStopping && status != StatusStopped {
		m.restartWithBackoff(live, restarts)
	}
}

// maxServerRestarts bounds the watchdog so a project that cannot start does not
// spin forever.
const maxServerRestarts = 8

func (m *Manager) restartWithBackoff(live *liveSession, restarts int) {
	if restarts >= maxServerRestarts {
		return
	}
	// 2s, 4s, 8s … capped at 2 minutes, with jitter so several sessions
	// restarting together do not line up.
	backoff := time.Duration(1<<uint(restarts+1)) * time.Second
	if backoff > 2*time.Minute {
		backoff = 2 * time.Minute
	}
	backoff += time.Duration(rand.Int63n(int64(time.Second)))

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		timer := time.NewTimer(backoff)
		defer timer.Stop()
		<-timer.C

		cfg := m.cfg()
		if cfg == nil {
			return
		}
		live.mu.Lock()
		if live.stopping {
			live.mu.Unlock()
			return
		}
		live.meta.Restarts++
		live.meta.EndedAt = nil
		live.meta.ExitCode = nil
		live.meta.RemoteURL = ""
		live.done = make(chan struct{})
		live.mu.Unlock()

		if err := m.spawn(cfg, live); err != nil {
			live.setStatus(StatusFailed)
			return
		}
		m.notifyRunning()
		m.log(audit.Entry{
			Actor:  "watchdog",
			Action: audit.ActionSessionResume,
			Target: live.snapshot().Name,
			Result: audit.ResultOK,
			Meta:   map[string]any{"reason": "server mode exited", "restarts": live.snapshot().Restarts},
		})
	}()
}

// detect runs the URL and prompt detectors over the rolling window.
func (m *Manager) detect(live *liveSession) {
	window := string(live.window.Bytes())

	if url := FindRemoteURL(window); url != "" {
		live.setRemoteURL(url)
	}
	if p, ok := DetectPrompt(window); ok {
		live.setPrompt(p)
		return
	}
	// No prompt in view: if the session was waiting, it is working again.
	snap := live.snapshot()
	if snap.Status == StatusNeedsInput {
		live.clearPrompt()
	} else if snap.Status == StatusStarting && snap.RemoteURL == "" {
		// Output is flowing but no URL yet; "running" is more honest than
		// leaving it on "starting" forever.
		if live.scroll.Len() > 2048 {
			live.setStatus(StatusRunning)
		}
	}
}

// ---------- queries ----------

// List returns every session, running first then newest first.
func (m *Manager) List() []Session {
	m.mu.RLock()
	out := make([]Session, 0, len(m.sessions))
	for _, l := range m.sessions {
		out = append(out, l.snapshot())
	}
	m.mu.RUnlock()

	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := out[i].Status.Running(), out[j].Status.Running()
		if ri != rj {
			return ri
		}
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	return out
}

// Get returns one session.
func (m *Manager) Get(id string) (Session, error) {
	l, err := m.live(id)
	if err != nil {
		return Session{}, err
	}
	return l.snapshot(), nil
}

// RunningCount counts sessions with a live process.
func (m *Manager) RunningCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := 0
	for _, l := range m.sessions {
		if l.snapshot().Status.Running() {
			n++
		}
	}
	return n
}

func (m *Manager) live(id string) (*liveSession, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	l, ok := m.sessions[id]
	if !ok {
		return nil, ErrNotFound
	}
	return l, nil
}

func (m *Manager) notifyRunning() {
	m.mu.RLock()
	fn := m.keepAwake
	m.mu.RUnlock()
	fn(m.RunningCount())
}

// ---------- input ----------

// SendText writes raw text to a session's PTY.
func (m *Manager) SendText(id, text, actor string) error {
	l, err := m.live(id)
	if err != nil {
		return err
	}
	l.mu.RLock()
	proc, running := l.proc, l.meta.Status.Running()
	l.mu.RUnlock()
	if proc == nil || !running {
		return ErrNotRunning
	}
	if _, err := proc.Write([]byte(text)); err != nil {
		return fmt.Errorf("sessions: write input: %w", err)
	}
	l.clearPrompt()
	// The text itself is deliberately not logged: it is whatever the user
	// typed at Claude, which belongs in the session, not the audit trail.
	m.log(audit.Entry{
		Actor:  actor,
		Action: audit.ActionSessionInput,
		Target: l.snapshot().Name,
		Result: audit.ResultOK,
		Meta:   map[string]any{"bytes": len(text)},
	})
	return nil
}

// SendKey writes a named key to a session's PTY.
func (m *Manager) SendKey(id, key, actor string) error {
	b, ok := KeyBytes(key)
	if !ok {
		return fmt.Errorf("sessions: unknown key %q", key)
	}
	return m.SendText(id, string(b), actor)
}

// Resize sets a session's terminal size.
func (m *Manager) Resize(id string, cols, rows uint16) error {
	l, err := m.live(id)
	if err != nil {
		return err
	}
	l.mu.RLock()
	proc := l.proc
	l.mu.RUnlock()
	if proc == nil {
		return ErrNotRunning
	}
	return proc.Resize(pty.Size{Cols: cols, Rows: rows})
}

// Scrollback returns the session's buffered PTY output.
func (m *Manager) Scrollback(id string) ([]byte, error) {
	l, err := m.live(id)
	if err != nil {
		return nil, err
	}
	return l.scroll.Bytes(), nil
}

// Subscribe attaches to a session's live output and events.
func (m *Manager) Subscribe(id string) (<-chan []byte, <-chan Event, func(), error) {
	l, err := m.live(id)
	if err != nil {
		return nil, nil, nil, err
	}
	out, ev, cancel := l.subscribe()
	return out, ev, cancel, nil
}

// ---------- lifecycle ----------

// stopGrace is the escalation schedule from docs/04-API.md §5.2: ask Claude to
// exit, then interrupt, then kill the job.
var (
	stopExitDelay = 5 * time.Second
	stopKillDelay = 10 * time.Second
)

// Stop ends a session gracefully, escalating if it does not go.
func (m *Manager) Stop(ctx context.Context, id, actor string) error {
	l, err := m.live(id)
	if err != nil {
		return err
	}
	l.mu.Lock()
	if !l.meta.Status.Running() {
		l.mu.Unlock()
		return ErrNotRunning
	}
	l.stopping = true
	proc := l.proc
	name := l.meta.Name
	done := l.done
	l.mu.Unlock()

	if proc == nil {
		return ErrNotRunning
	}

	// Ask nicely: /exit is how Claude Code is told to quit.
	_, _ = proc.Write([]byte("/exit\r"))

	select {
	case <-done:
		m.logStop(actor, name, "exit")
		return nil
	case <-time.After(stopExitDelay):
	case <-ctx.Done():
	}

	// Interrupt twice, which is how a TUI is told to give up.
	_, _ = proc.Write([]byte{0x03})
	_, _ = proc.Write([]byte{0x03})

	select {
	case <-done:
		m.logStop(actor, name, "interrupt")
		return nil
	case <-time.After(stopKillDelay - stopExitDelay):
	case <-ctx.Done():
	}

	if err := proc.Kill(); err != nil {
		return fmt.Errorf("sessions: kill: %w", err)
	}
	m.logStop(actor, name, "kill")
	return nil
}

func (m *Manager) logStop(actor, name, how string) {
	m.log(audit.Entry{
		Actor:  actor,
		Action: audit.ActionSessionStop,
		Target: name,
		Result: audit.ResultOK,
		Meta:   map[string]any{"method": how},
	})
}

// Resume restarts an exited session in the same directory, continuing the
// conversation.
func (m *Manager) Resume(id, actor string) (Session, error) {
	cfg := m.cfg()
	if cfg == nil {
		return Session{}, errors.New("sessions: no config")
	}
	l, err := m.live(id)
	if err != nil {
		return Session{}, err
	}
	// Both checks run before the session lock is taken: RunningCount walks
	// every session and reads its metadata, so calling it while holding this
	// session's lock deadlocks against itself.
	if l.snapshot().Status.Running() {
		return Session{}, ErrStillRuns
	}
	if n := m.RunningCount(); n >= cfg.Claude.MaxSessions {
		return Session{}, ErrMaxSessions
	}

	l.mu.Lock()
	// Re-check under the lock in case two resumes raced.
	if l.meta.Status.Running() {
		l.mu.Unlock()
		return Session{}, ErrStillRuns
	}
	l.meta.Status = StatusStarting
	l.meta.EndedAt = nil
	l.meta.ExitCode = nil
	l.meta.RemoteURL = ""
	l.meta.Prompt = nil
	l.stopping = false
	l.done = make(chan struct{})
	meta := l.meta
	l.mu.Unlock()

	// Resume uses --continue, so the conversation carries on rather than
	// starting fresh (docs/04-API.md §5.2).
	args := claudeArgs(cfg, meta, true)
	env := sessionEnv(cfg, meta)
	proc, startErr := m.start(pty.Config{
		Path:       cfg.Claude.Binary,
		Args:       args,
		Dir:        meta.CWD,
		Env:        env,
		Size:       pty.DefaultSize,
		Restricted: cfg.Claude.RestrictedToken,
	})
	if startErr != nil {
		l.setStatus(StatusFailed)
		return Session{}, fmt.Errorf("sessions: resume: %w", startErr)
	}

	l.mu.Lock()
	l.proc = proc
	l.meta.PID = proc.PID()
	l.mu.Unlock()

	m.wg.Add(1)
	go m.pump(l, proc)
	m.notifyRunning()

	m.log(audit.Entry{
		Actor: actor, Action: audit.ActionSessionResume,
		Target: meta.Name, Result: audit.ResultOK,
	})
	return l.snapshot(), nil
}

// Delete removes a stopped session's record.
func (m *Manager) Delete(id string) error {
	l, err := m.live(id)
	if err != nil {
		return err
	}
	if l.snapshot().Status.Running() {
		return ErrStillRuns
	}
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
	return nil
}

// KillAll terminates every session at once, for the panic button and lockdown.
func (m *Manager) KillAll(actor string) int {
	m.mu.RLock()
	live := make([]*liveSession, 0, len(m.sessions))
	for _, l := range m.sessions {
		live = append(live, l)
	}
	m.mu.RUnlock()

	killed := 0
	for _, l := range live {
		l.mu.Lock()
		proc, running := l.proc, l.meta.Status.Running()
		l.stopping = true
		l.mu.Unlock()
		if proc != nil && running {
			_ = proc.Kill()
			killed++
		}
	}
	if killed > 0 {
		m.log(audit.Entry{
			Actor: actor, Action: audit.ActionSessionKillAl,
			Result: audit.ResultOK, Meta: map[string]any{"killed": killed},
		})
	}
	m.notifyRunning()
	return killed
}

// Shutdown kills everything and waits for the readers to finish.
func (m *Manager) Shutdown() {
	m.KillAll("service")
	m.wg.Wait()
}

// ---------- helpers ----------

func findProject(cfg *config.Config, id string) (config.Project, bool) {
	for _, p := range cfg.Projects {
		if ProjectID(p) == id {
			return p, true
		}
	}
	return config.Project{}, false
}

// ProjectID is a stable id for a configured project. The name is used rather
// than an index so an id stays valid when the list is reordered.
func ProjectID(p config.Project) string {
	return "p_" + strings.ToLower(p.Name)
}

// defaultSessionName is "<project>-<HHmm>", the default offered by the PWA.
func defaultSessionName(project string, now time.Time) string {
	return fmt.Sprintf("%s-%s", project, now.Format("1504"))
}
