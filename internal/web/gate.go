package web

import (
	"context"
	"net/http"
	"strings"

	"burrow/internal/audit"
	"burrow/internal/config"
	"burrow/internal/identity"
)

// ctxKey is the private key type for request-scoped values.
type ctxKey int

const (
	ctxNode ctxKey = iota
	ctxDevice
	ctxPairing
)

// NodeFrom returns the tailnet node behind a request. Only handlers mounted
// behind the gate may call it.
func NodeFrom(ctx context.Context) (identity.Node, bool) {
	n, ok := ctx.Value(ctxNode).(identity.Node)
	return n, ok
}

// DeviceFrom returns the paired device behind a request, if the node is one.
// A node in an open pairing window has no device yet.
func DeviceFrom(ctx context.Context) (config.AllowedDevice, bool) {
	d, ok := ctx.Value(ctxDevice).(config.AllowedDevice)
	return d, ok
}

// IsPairingCandidate reports whether the request came from an owner-owned node
// that is not yet paired, during an open pairing window.
func IsPairingCandidate(ctx context.Context) bool {
	v, _ := ctx.Value(ctxPairing).(bool)
	return v
}

// GateDeps is what the identity gate needs from the rest of the agent. Each is
// a function so the gate sees live state: a device revoked or a lockdown
// entered takes effect on the next request, with no restart.
type GateDeps struct {
	Resolver identity.Resolver
	// Config returns the current config (device allowlist, owner login).
	Config func() *config.Config
	// PairingOpen reports whether a pairing window is currently open.
	PairingOpen func() bool
	// Lockdown reports whether the agent is locked down.
	Lockdown func() bool
	// Audit records denials. Never nil in production; a no-op in tests.
	Audit func(audit.Entry)
	// WakerTag is the ACL tag a waker node carries. A node with this tag may
	// reach /healthz and nothing else.
	WakerTag string
}

// DefaultWakerTag matches policy/tailscale-acl.hujson.
const DefaultWakerTag = "tag:burrow-waker"

// Gate is the outermost authorization layer: it decides whether a request is
// acknowledged at all.
//
// Everything it refuses gets a bare 404, so a device that is not allowed cannot
// tell Burrow from a closed port (docs/03-SECURITY.md §3.2).
type Gate struct {
	deps GateDeps
}

// NewGate builds the gate.
func NewGate(deps GateDeps) *Gate {
	if deps.WakerTag == "" {
		deps.WakerTag = DefaultWakerTag
	}
	if deps.Audit == nil {
		deps.Audit = func(audit.Entry) {}
	}
	if deps.PairingOpen == nil {
		deps.PairingOpen = func() bool { return false }
	}
	if deps.Lockdown == nil {
		deps.Lockdown = func() bool { return false }
	}
	return &Gate{deps: deps}
}

// notFound is the single refusal response. No body, no header, nothing that
// distinguishes "wrong device" from "no such service".
func notFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
}

// Middleware wraps a handler with the identity gate.
func (g *Gate) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := g.deps.Config()
		if cfg == nil {
			notFound(w)
			return
		}

		node, err := g.deps.Resolver.WhoIs(r.Context(), r.RemoteAddr)
		if err != nil {
			g.deny(r, "", "whois failed")
			notFound(w)
			return
		}

		isHealth := r.URL.Path == "/healthz"

		// Lockdown: only /healthz answers, and only to an otherwise allowed
		// caller, so the PWA can tell "locked down" from "PC off".
		if g.deps.Lockdown() && !isHealth {
			g.deny(r, node.StableID, "lockdown")
			notFound(w)
			return
		}

		// A waker node may check health and nothing else.
		if node.HasTag(g.deps.WakerTag) {
			if !isHealth {
				g.deny(r, node.StableID, "waker node outside /healthz")
				notFound(w)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxNode, node)))
			return
		}

		// Any other tagged node is a machine, never a client device.
		if node.IsTagged() {
			g.deny(r, node.StableID, "tagged node is not a client device")
			notFound(w)
			return
		}

		// The owner login must match exactly. A different user on the same
		// tailnet is refused even if their node id somehow appeared in the
		// allowlist.
		if !strings.EqualFold(node.Login, cfg.Owner.TailscaleLogin) {
			g.deny(r, node.StableID, "login is not the owner")
			notFound(w)
			return
		}

		if dev, ok := cfg.AllowedNode(node.StableID); ok {
			ctx := context.WithValue(r.Context(), ctxNode, node)
			ctx = context.WithValue(ctx, ctxDevice, dev)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		// Not paired. During an open pairing window an owner-owned node may
		// reach the pairing endpoints only — that window is opened from the PC
		// console, so it cannot be triggered remotely.
		if g.deps.PairingOpen() && isPairingPath(r.URL.Path) {
			ctx := context.WithValue(r.Context(), ctxNode, node)
			ctx = context.WithValue(ctx, ctxPairing, true)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		g.deny(r, node.StableID, "node is not a paired device")
		notFound(w)
	})
}

// isPairingPath lists what an unpaired node may reach while the window is open.
// The PWA shell and its assets are included because /pair has to render.
//
// /healthz and /api/auth/state are included because the PWA asks both before
// it renders anything: refused, it decides the PC is off and shows the Wake
// screen, so a new device could never pair. Neither says more than loading
// /pair already does — that the agent is up and the window is open
// (docs/DEVIATIONS.md D-5).
func isPairingPath(p string) bool {
	switch p {
	case "/pair", "/manifest.webmanifest", "/sw.js", "/favicon.ico", "/apple-touch-icon.png",
		"/healthz", "/api/auth/state":
		return true
	}
	return strings.HasPrefix(p, "/api/auth/pair/") ||
		strings.HasPrefix(p, "/assets/") ||
		strings.HasPrefix(p, "/icons/")
}

func (g *Gate) deny(r *http.Request, nodeID, reason string) {
	actor := "node:unknown"
	if nodeID != "" {
		actor = "node:" + nodeID
	}
	g.deps.Audit(audit.Entry{
		Actor:  actor,
		Action: audit.ActionDenyIdentity,
		Target: r.URL.Path,
		Result: audit.ResultDenied,
		IP:     identity.HostOnly(r.RemoteAddr),
		Meta:   map[string]any{"reason": reason, "method": r.Method},
	})
}
