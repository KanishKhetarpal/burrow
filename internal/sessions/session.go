// Package sessions runs and tracks Claude Code sessions.
//
// Burrow does not reimplement a Claude UI. It starts `claude` with Remote
// Control on and hands the phone the URL that opens the Claude app; the raw
// terminal stays available as a fallback (docs/02-ARCHITECTURE.md D3).
package sessions

import (
	"encoding/json"
	"sync"
	"time"

	"burrow/internal/pty"
)

// Status is a session's lifecycle state (docs/04-API.md §5.1).
type Status string

const (
	StatusStarting   Status = "starting"
	StatusReady      Status = "ready"
	StatusNeedsInput Status = "needs_input"
	StatusRunning    Status = "running"
	StatusExited     Status = "exited"
	StatusFailed     Status = "failed"
	StatusStopped    Status = "stopped"
)

// Running reports whether a session still has a live process.
func (s Status) Running() bool {
	switch s {
	case StatusStarting, StatusReady, StatusNeedsInput, StatusRunning:
		return true
	}
	return false
}

// Mode is how Claude Code is launched.
type Mode string

const (
	// ModeChat is one interactive session, the default. Interactive mode
	// reconnects by itself after a network outage.
	ModeChat Mode = "chat"
	// ModeServer is a project server, so further chats can be started from
	// inside the Claude app. It exits after roughly 10 minutes offline, which
	// is why it gets a watchdog.
	ModeServer Mode = "server"
)

// Spawn is the server-mode working-directory strategy.
type Spawn string

const (
	SpawnSameDir  Spawn = "same-dir"
	SpawnWorktree Spawn = "worktree"
)

// Session is the API view of a session. It is JSON-encoded straight to the
// client, so the tags are part of the contract in docs/04-API.md §5.1.
type Session struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	ProjectID      string     `json:"projectId"`
	CWD            string     `json:"cwd"`
	Mode           Mode       `json:"mode"`
	PermissionMode string     `json:"permissionMode"`
	Launcher       string     `json:"launcher,omitempty"`
	Spawn          Spawn      `json:"spawn,omitempty"`
	Status         Status     `json:"status"`
	RemoteURL      string     `json:"remoteUrl,omitempty"`
	Prompt         *Prompt    `json:"prompt,omitempty"`
	PID            int        `json:"pid,omitempty"`
	StartedAt      time.Time  `json:"startedAt"`
	EndedAt        *time.Time `json:"endedAt"`
	ExitCode       *int       `json:"exitCode"`
	Restarts       int        `json:"restarts"`
}

// liveSession is the manager's internal state for one session.
type liveSession struct {
	mu   sync.RWMutex
	meta Session

	proc      pty.Process
	scroll    *ring
	window    *ring // smaller rolling window for prompt detection
	subs      map[int]chan []byte
	nextSub   int
	events    map[int]chan Event
	nextEvent int

	// stopping marks a deliberate stop, so the watchdog does not restart it
	// and the exit is reported as "stopped" rather than "failed".
	stopping bool
	// done closes when the process has exited and been reaped.
	done chan struct{}
}

// Event is a control message sent to terminal subscribers.
type Event struct {
	T        string  `json:"t"`
	Status   Status  `json:"status,omitempty"`
	Prompt   *Prompt `json:"prompt,omitempty"`
	ExitCode *int    `json:"code,omitempty"`
	URL      string  `json:"url,omitempty"`
}

// MarshalJSON is defined so an Event always carries its type tag.
func (e Event) MarshalJSON() ([]byte, error) {
	type alias Event
	if e.T == "" {
		e.T = "status"
	}
	return json.Marshal(alias(e))
}

// snapshot returns a copy of the session metadata, safe to hand to a handler.
func (l *liveSession) snapshot() Session {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := l.meta
	if l.meta.Prompt != nil {
		p := *l.meta.Prompt
		out.Prompt = &p
	}
	return out
}

// setStatus updates the status and notifies subscribers.
func (l *liveSession) setStatus(s Status) {
	l.mu.Lock()
	if l.meta.Status == s {
		l.mu.Unlock()
		return
	}
	l.meta.Status = s
	l.mu.Unlock()
	l.emit(Event{T: "status", Status: s})
}

// setPrompt records a detected prompt and moves the session to needs_input.
func (l *liveSession) setPrompt(p Prompt) {
	l.mu.Lock()
	if l.meta.Prompt != nil && l.meta.Prompt.Text == p.Text && l.meta.Status == StatusNeedsInput {
		l.mu.Unlock()
		return // already surfaced
	}
	cp := p
	l.meta.Prompt = &cp
	l.meta.Status = StatusNeedsInput
	l.mu.Unlock()
	l.emit(Event{T: "prompt", Status: StatusNeedsInput, Prompt: &p})
}

// clearPrompt drops a prompt once input has been sent.
func (l *liveSession) clearPrompt() {
	l.mu.Lock()
	had := l.meta.Prompt != nil
	l.meta.Prompt = nil
	if l.meta.Status == StatusNeedsInput {
		if l.meta.RemoteURL != "" {
			l.meta.Status = StatusReady
		} else {
			l.meta.Status = StatusRunning
		}
	}
	status := l.meta.Status
	l.mu.Unlock()
	if had {
		l.emit(Event{T: "status", Status: status})
	}
}

// setRemoteURL records the Remote Control link and marks the session ready.
func (l *liveSession) setRemoteURL(url string) {
	l.mu.Lock()
	if l.meta.RemoteURL != "" {
		l.mu.Unlock()
		return
	}
	l.meta.RemoteURL = url
	if l.meta.Status == StatusStarting {
		l.meta.Status = StatusReady
	}
	status := l.meta.Status
	l.mu.Unlock()
	l.emit(Event{T: "status", Status: status, URL: url})
}

// ---------- output and event fan-out ----------

// subscribe registers a byte stream for a terminal WebSocket. The returned
// cancel removes it.
func (l *liveSession) subscribe() (<-chan []byte, <-chan Event, func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.subs == nil {
		l.subs = map[int]chan []byte{}
		l.events = map[int]chan Event{}
	}
	id := l.nextSub
	l.nextSub++
	// Buffered so a slow client does not stall the PTY reader; if it overflows
	// the client is dropped rather than blocking the session.
	out := make(chan []byte, 256)
	ev := make(chan Event, 32)
	l.subs[id] = out
	l.events[id] = ev

	return out, ev, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if c, ok := l.subs[id]; ok {
			delete(l.subs, id)
			close(c)
		}
		if c, ok := l.events[id]; ok {
			delete(l.events, id)
			close(c)
		}
	}
}

// fanout sends PTY bytes to every subscriber.
func (l *liveSession) fanout(b []byte) {
	l.mu.RLock()
	subs := make([]chan []byte, 0, len(l.subs))
	for _, c := range l.subs {
		subs = append(subs, c)
	}
	l.mu.RUnlock()

	for _, c := range subs {
		cp := make([]byte, len(b))
		copy(cp, b)
		select {
		case c <- cp:
		default:
			// Dropped rather than blocked: the client can re-fetch scrollback.
		}
	}
}

// emit sends a control event to every subscriber.
func (l *liveSession) emit(e Event) {
	l.mu.RLock()
	chans := make([]chan Event, 0, len(l.events))
	for _, c := range l.events {
		chans = append(chans, c)
	}
	l.mu.RUnlock()

	for _, c := range chans {
		select {
		case c <- e:
		default:
		}
	}
}
