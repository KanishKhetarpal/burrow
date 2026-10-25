// Package tsnode runs Burrow's embedded Tailscale node.
//
// The agent gets its listeners from tsnet and never from net.Listen, so it is
// reachable on the tailnet and nowhere else — not from the internet, and not
// from the home LAN even by mistake (docs/03-SECURITY.md §4, D2).
package tsnode

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"tailscale.com/tsnet"

	"burrow/internal/identity"
)

// Options configures a node.
type Options struct {
	// Hostname is the tailnet hostname. It appears in public Certificate
	// Transparency logs as <hostname>.<tailnet>.ts.net, so setup generates a
	// random, non-identifying one (docs/03-SECURITY.md T15).
	Hostname string
	// StateDir holds the node's keys and state. One directory per node.
	StateDir string
	// AuthKey is used only on first start, to join the tailnet. It is passed
	// in from the environment and never written to disk by Burrow.
	AuthKey string
	// LogsUpload false (the default) sets TS_NO_LOGS_NO_SUPPORT, which stops
	// the client uploading logs to Tailscale.
	LogsUpload bool
	// Logf receives tsnet's diagnostics. nil discards them.
	Logf func(format string, args ...any)
}

// Node is a running tsnet server.
type Node struct {
	srv  *tsnet.Server
	opts Options

	mu       sync.RWMutex
	fqdn     string
	closed   bool
	whoCache *whoCache
}

// New builds a node. Nothing starts until Start.
func New(opts Options) (*Node, error) {
	if strings.TrimSpace(opts.Hostname) == "" {
		return nil, errors.New("tsnode: hostname is required")
	}
	if strings.TrimSpace(opts.StateDir) == "" {
		return nil, errors.New("tsnode: state dir is required")
	}
	if err := os.MkdirAll(opts.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("tsnode: state dir: %w", err)
	}

	// Refusing log upload is a privacy control, so it is set on the process
	// rather than left to a tsnet field that may change name between versions.
	if !opts.LogsUpload {
		if err := os.Setenv("TS_NO_LOGS_NO_SUPPORT", "true"); err != nil {
			return nil, fmt.Errorf("tsnode: disable log upload: %w", err)
		}
	}

	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	srv := &tsnet.Server{
		Hostname: opts.Hostname,
		Dir:      filepath.Clean(opts.StateDir),
		AuthKey:  opts.AuthKey,
		Logf:     logf,
		// UserLogf would print the "to authenticate, visit ..." URL; it goes
		// to the same sink so the service log carries it on first start.
		UserLogf: logf,
	}
	return &Node{srv: srv, opts: opts, whoCache: newWhoCache()}, nil
}

// Start brings the node up and blocks until it has joined the tailnet.
func (n *Node) Start(ctx context.Context) error {
	if _, err := n.srv.Up(ctx); err != nil {
		return fmt.Errorf("tsnode: join tailnet: %w", err)
	}
	// CertDomains is only populated once the node is up.
	if domains := n.srv.CertDomains(); len(domains) > 0 {
		n.mu.Lock()
		n.fqdn = domains[0]
		n.mu.Unlock()
	}
	return nil
}

// FQDN is the node's fully-qualified tailnet name, which is also the WebAuthn
// relying-party ID and the only allowed Origin.
func (n *Node) FQDN() string {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.fqdn
}

// Origin is the single HTTPS origin the PWA is served from.
func (n *Node) Origin() string {
	if f := n.FQDN(); f != "" {
		return "https://" + f
	}
	return ""
}

// ListenTLS returns a TLS listener on the tailnet, using Tailscale-issued
// certificates. This is the only way the agent accepts connections.
func (n *Node) ListenTLS(addr string) (net.Listener, error) {
	ln, err := n.srv.ListenTLS("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("tsnode: listen %s: %w", addr, err)
	}
	return ln, nil
}

// Close shuts the node down.
func (n *Node) Close() error {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return nil
	}
	n.closed = true
	n.mu.Unlock()
	return n.srv.Close()
}

// Dial opens a connection over the tailnet. Used by selftest to dial the
// agent's own listener as its own node identity, which must be refused.
func (n *Node) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	return n.srv.Dial(ctx, network, addr)
}

// WhoIs implements identity.Resolver against the live tailnet.
func (n *Node) WhoIs(ctx context.Context, addr string) (identity.Node, error) {
	host := identity.HostOnly(addr)
	if host == "" {
		return identity.Node{}, identity.ErrUnknownNode
	}
	if cached, ok := n.whoCache.get(host); ok {
		return cached, nil
	}

	lc, err := n.srv.LocalClient()
	if err != nil {
		return identity.Node{}, fmt.Errorf("tsnode: local client: %w", err)
	}
	// WhoIs wants an address; a bare IP is accepted.
	who, err := lc.WhoIs(ctx, host)
	if err != nil {
		// Treat every lookup failure as "unknown node". The caller answers 404
		// either way, and distinguishing them would leak that the service is
		// there (docs/03-SECURITY.md §3.2).
		return identity.Node{}, errors.Join(identity.ErrUnknownNode, err)
	}
	if who == nil || who.Node == nil {
		return identity.Node{}, identity.ErrUnknownNode
	}

	out := identity.Node{
		StableID: string(who.Node.StableID),
		Hostname: who.Node.ComputedName,
		Tags:     append([]string(nil), who.Node.Tags...),
	}
	if who.UserProfile != nil {
		out.Login = who.UserProfile.LoginName
	}
	// A tagged node has no human owner; Tailscale reports a synthetic profile
	// for it, which must not be mistaken for the owner login.
	if len(out.Tags) > 0 {
		out.Login = ""
	}
	n.whoCache.put(host, out)
	return out, nil
}

// whoCache keeps WhoIs results briefly. The lookup is a local IPC call, but it
// happens on every request including each PTY WebSocket frame batch; the TTL is
// short so a node removed in the Tailscale admin loses access within seconds,
// which the lost-device drill requires (docs/07-TEST-PLAN.md M16).
type whoCache struct {
	mu      sync.RWMutex
	entries map[string]whoEntry
	ttl     time.Duration
	now     func() time.Time
}

type whoEntry struct {
	node identity.Node
	exp  time.Time
}

const whoCacheTTL = 5 * time.Second

func newWhoCache() *whoCache {
	return &whoCache{entries: map[string]whoEntry{}, ttl: whoCacheTTL, now: time.Now}
}

func (c *whoCache) get(host string) (identity.Node, bool) {
	c.mu.RLock()
	e, ok := c.entries[host]
	c.mu.RUnlock()
	if !ok || c.now().After(e.exp) {
		return identity.Node{}, false
	}
	return e.node, true
}

func (c *whoCache) put(host string, n identity.Node) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Bounded so a flood of source addresses cannot grow it without limit.
	if len(c.entries) > 256 {
		c.entries = map[string]whoEntry{}
	}
	c.entries[host] = whoEntry{node: n, exp: c.now().Add(c.ttl)}
}

// RandomHostname returns a non-identifying hostname of the form n-<8 hex>.
func RandomHostname(randHex func(n int) (string, error)) (string, error) {
	h, err := randHex(4) // 4 bytes -> 8 hex chars
	if err != nil {
		return "", fmt.Errorf("tsnode: random hostname: %w", err)
	}
	return "n-" + h, nil
}
