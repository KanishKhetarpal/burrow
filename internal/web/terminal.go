package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"burrow/internal/sessions"
)

// Subprotocol identifies the terminal protocol (docs/04-API.md §5.3).
const TerminalSubprotocol = "burrow.term.v1"

// Terminal timings.
const (
	// terminalIdleTimeout closes a socket with no client input or ping. A phone
	// that went to sleep with the terminal open should not hold a session's
	// output fan-out open indefinitely.
	terminalIdleTimeout = 10 * time.Minute
	// terminalWriteTimeout bounds a single write, so one stalled client cannot
	// wedge the writer goroutine.
	terminalWriteTimeout = 10 * time.Second
	// closeIdle is the close code the client is told to reconnect on.
	closeIdle = websocket.StatusCode(4000)
)

// clientControl is a text frame from the client.
type clientControl struct {
	T    string `json:"t"`
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
}

// handleTerminal upgrades to a WebSocket carrying raw PTY bytes.
//
// It is mounted behind RequireSession, so the identity gate, the session cookie
// and the node binding have all been checked before the upgrade. The Origin
// check is done here as well, because the CSRF middleware only guards
// state-changing methods and a WebSocket handshake is a GET
// (docs/03-SECURITY.md §4).
func (s *Server) handleTerminal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.deps.Sessions.Get(id); err != nil {
		s.writeSessionError(w, err)
		return
	}

	origin := s.deps.Origin()
	if got := r.Header.Get("Origin"); got != "" && got != origin {
		WriteError(w, http.StatusForbidden, CodeCSRF, "request rejected")
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{TerminalSubprotocol},
		// Everything is same-origin; the check above is the authority, and
		// this stops the library second-guessing it with its own host match.
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		return // Accept already answered the request
	}
	defer conn.CloseNow()

	out, events, cancel, err := s.deps.Sessions.Subscribe(id)
	if err != nil {
		conn.Close(websocket.StatusInternalError, "session went away")
		return
	}
	defer cancel()

	ctx, stop := context.WithCancel(r.Context())
	defer stop()

	// Scrollback first, so the terminal opens showing what already happened
	// rather than an empty screen (docs/04-API.md §5.3).
	if b, err := s.deps.Sessions.Scrollback(id); err == nil && len(b) > 0 {
		wctx, wcancel := context.WithTimeout(ctx, terminalWriteTimeout)
		err := conn.Write(wctx, websocket.MessageBinary, b)
		wcancel()
		if err != nil {
			return
		}
	}
	// Then the current status, so the client knows whether it is waiting on a
	// prompt without polling the REST endpoint.
	if sess, err := s.deps.Sessions.Get(id); err == nil {
		s.writeControl(ctx, conn, sessions.Event{T: "status", Status: sess.Status, Prompt: sess.Prompt})
	}

	// activity carries "the client is still there", which resets the idle
	// timer. Only client input and pings count; server output does not, or a
	// chatty session would keep a dead client's socket open forever.
	activity := make(chan struct{}, 1)

	go s.readClient(ctx, stop, conn, id, activity)

	idle := time.NewTimer(terminalIdleTimeout)
	defer idle.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-idle.C:
			conn.Close(closeIdle, "idle")
			return

		case <-activity:
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(terminalIdleTimeout)

		case b, ok := <-out:
			if !ok {
				conn.Close(websocket.StatusNormalClosure, "session ended")
				return
			}
			wctx, wcancel := context.WithTimeout(ctx, terminalWriteTimeout)
			err := conn.Write(wctx, websocket.MessageBinary, b)
			wcancel()
			if err != nil {
				return
			}

		case ev, ok := <-events:
			if !ok {
				return
			}
			s.writeControl(ctx, conn, ev)
			if ev.T == "exit" {
				// Give the client a moment to render the exit, then close.
				conn.Close(websocket.StatusNormalClosure, "session exited")
				return
			}
		}
	}
}

// readClient handles frames from the browser.
func (s *Server) readClient(ctx context.Context, stop context.CancelFunc,
	conn *websocket.Conn, id string, activity chan<- struct{}) {
	defer stop()

	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		select {
		case activity <- struct{}{}:
		default:
		}

		switch typ {
		case websocket.MessageBinary:
			// Raw keystrokes. Read-only mode is enforced in the client, but
			// the server-side cap on frame size is what matters here: a
			// keystroke frame has no business being large.
			if len(data) > 4096 {
				conn.Close(websocket.StatusMessageTooBig, "input frame too large")
				return
			}
			if err := s.deps.Sessions.SendText(id, string(data), "device:terminal"); err != nil {
				if errors.Is(err, sessions.ErrNotRunning) {
					return
				}
			}

		case websocket.MessageText:
			var ctrl clientControl
			if err := json.Unmarshal(data, &ctrl); err != nil {
				continue
			}
			switch ctrl.T {
			case "resize":
				if ctrl.Cols > 0 && ctrl.Rows > 0 && ctrl.Cols <= 500 && ctrl.Rows <= 200 {
					_ = s.deps.Sessions.Resize(id, ctrl.Cols, ctrl.Rows)
				}
			case "ping":
				// The activity signal above is the whole point of a ping.
			}
		}
	}
}

// writeControl sends a JSON control frame.
func (s *Server) writeControl(ctx context.Context, conn *websocket.Conn, ev sessions.Event) {
	raw, err := json.Marshal(ev)
	if err != nil {
		return
	}
	wctx, cancel := context.WithTimeout(ctx, terminalWriteTimeout)
	defer cancel()
	_ = conn.Write(wctx, websocket.MessageText, raw)
}
