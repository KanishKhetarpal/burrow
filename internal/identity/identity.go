// Package identity resolves a request's remote address to a Tailscale node.
//
// It exists as an interface so the identity gate — the layer that decides
// whether a request is even acknowledged — can be unit tested without a
// tailnet (docs/07-TEST-PLAN.md §1, "Identity gate").
package identity

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
)

// ErrUnknownNode means the address does not belong to any node on the tailnet.
// The caller must answer 404 and audit the attempt; it must not say why.
var ErrUnknownNode = errors.New("identity: address does not belong to a tailnet node")

// Node is the identity behind a request.
type Node struct {
	// StableID is Tailscale's stable node identifier. It survives IP changes
	// and renames, which is why the device allowlist is keyed on it.
	StableID string
	// Login is the tailnet user who owns the node ("" for a tagged node).
	Login string
	// Tags are the node's ACL tags, e.g. "tag:burrow-waker".
	Tags []string
	// Hostname is for display only; never trust it for a decision.
	Hostname string
}

// HasTag reports whether the node carries the given ACL tag.
func (n Node) HasTag(tag string) bool {
	for _, t := range n.Tags {
		if strings.EqualFold(t, tag) {
			return true
		}
	}
	return false
}

// IsTagged reports whether the node is a tagged (machine) node rather than a
// user-owned device. Tagged nodes never own a Burrow session.
func (n Node) IsTagged() bool { return len(n.Tags) > 0 }

// Resolver maps a request's RemoteAddr to a tailnet node.
type Resolver interface {
	// WhoIs resolves addr ("host:port" or a bare IP) to a node.
	// It returns ErrUnknownNode when the address is not on the tailnet.
	WhoIs(ctx context.Context, addr string) (Node, error)
}

// Fake is an in-memory Resolver for tests.
type Fake struct {
	mu    sync.RWMutex
	nodes map[string]Node
	// Err, when set, is returned by every call. Used to test the WhoIs-error
	// path, which must also produce a 404 rather than a 500.
	Err error
}

// NewFake builds an empty fake resolver.
func NewFake() *Fake { return &Fake{nodes: map[string]Node{}} }

// Add registers a node at the given IP.
func (f *Fake) Add(ip string, n Node) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nodes == nil {
		f.nodes = map[string]Node{}
	}
	f.nodes[ip] = n
	return f
}

// WhoIs implements Resolver.
func (f *Fake) WhoIs(_ context.Context, addr string) (Node, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.Err != nil {
		return Node{}, f.Err
	}
	n, ok := f.nodes[HostOnly(addr)]
	if !ok {
		return Node{}, ErrUnknownNode
	}
	return n, nil
}

// HostOnly strips a port from "host:port", leaving IPv6 literals intact.
// A resolver keyed on the full RemoteAddr would miss on every new source port.
func HostOnly(addr string) string {
	if addr == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	// Not host:port — may already be a bare IP, possibly bracketed.
	return strings.Trim(addr, "[]")
}
