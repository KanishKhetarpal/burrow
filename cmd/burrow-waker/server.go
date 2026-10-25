package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"burrow/internal/identity"
	"burrow/internal/tsnode"
	"burrow/internal/wol"
)

// wakerConfig is /etc/burrow-waker/waker.yaml.
type wakerConfig struct {
	Hostname string `yaml:"hostname"`
	StateDir string `yaml:"state_dir"`
	Owner    string `yaml:"owner_login"`
	// AllowedNodeIDs is the phone's Tailscale StableID, and nothing else. The
	// waker is the one thing reachable while the PC is off, so its allowlist is
	// narrower than the agent's (docs/04-API.md §8).
	AllowedNodeIDs []string `yaml:"allowed_node_ids"`
	// AgentOrigin is the agent's HTTPS origin, used for the CORS header and
	// for the health probe.
	AgentOrigin string   `yaml:"agent_origin"`
	PC          pcConfig `yaml:"pc"`
}

type pcConfig struct {
	MAC string `yaml:"mac"`
	// LANPrefix is the home subnet, so the magic packet can go to the directed
	// broadcast rather than only 255.255.255.255.
	LANPrefix string `yaml:"lan_prefix"`
	// ProbeAddr is a LAN address:port that answers while the PC is booting but
	// before the agent is up, which is what distinguishes "booting" from
	// "offline".
	ProbeAddr string `yaml:"probe_addr"`
}

func loadConfig(path string) (*wakerConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg wakerConfig
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if cfg.Hostname == "" {
		return nil, errors.New("hostname is required")
	}
	if cfg.StateDir == "" {
		return nil, errors.New("state_dir is required")
	}
	if len(cfg.AllowedNodeIDs) == 0 {
		return nil, errors.New("allowed_node_ids must list the phone's node id")
	}
	if cfg.AgentOrigin == "" || !strings.HasPrefix(cfg.AgentOrigin, "https://") {
		return nil, errors.New("agent_origin must be the agent's https:// origin")
	}
	if _, err := wol.ParseMAC(cfg.PC.MAC); err != nil {
		return nil, err
	}
	if cfg.PC.LANPrefix != "" {
		if _, err := netip.ParsePrefix(cfg.PC.LANPrefix); err != nil {
			return nil, fmt.Errorf("lan_prefix: %w", err)
		}
	}
	return &cfg, nil
}

// Rate limits from docs/02-ARCHITECTURE.md §2.2. Waking the PC grants no access
// by itself, so the limits are about not letting the machine be flapped.
const (
	wakeMinInterval = 30 * time.Second
	wakeMaxPerDay   = 20
)

type server struct {
	cfg  *wakerConfig
	node *tsnode.Node
	logf func(string, ...any)

	mu         sync.Mutex
	lastWake   time.Time
	wakesToday int
	dayStart   time.Time
}

func newServer(cfg *wakerConfig, logf func(string, ...any)) (*server, error) {
	node, err := tsnode.New(tsnode.Options{
		Hostname: cfg.Hostname,
		StateDir: cfg.StateDir,
		AuthKey:  os.Getenv("TS_AUTHKEY"),
		Logf:     func(f string, a ...any) { logf("tsnet: "+f, a...) },
	})
	if err != nil {
		return nil, err
	}
	// Cleared straight away so it cannot be read back out of the environment.
	_ = os.Unsetenv("TS_AUTHKEY")

	return &server{cfg: cfg, node: node, logf: logf, dayStart: time.Now()}, nil
}

func (s *server) run(ctx context.Context) error {
	if err := s.node.Start(ctx); err != nil {
		return err
	}
	s.logf("waker is up as %s", s.node.FQDN())

	ln, err := s.node.ListenTLS(":443")
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("POST /api/wake", s.handleWake)
	mux.HandleFunc("OPTIONS /api/", s.handlePreflight)

	srv := &http.Server{
		Handler:           s.withGate(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		_ = s.node.Close()
	}()

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// withGate is the waker's identity gate: only the phone's node, and nothing
// else, gets any answer at all.
func (s *server) withGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		node, err := s.node.WhoIs(r.Context(), r.RemoteAddr)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		allowed := false
		for _, id := range s.cfg.AllowedNodeIDs {
			if node.StableID == id {
				allowed = true
				break
			}
		}
		if !allowed {
			s.logf("refused %s (node %s)", identity.HostOnly(r.RemoteAddr), node.StableID)
			http.NotFound(w, r)
			return
		}
		s.setCORS(w)
		next.ServeHTTP(w, r)
	})
}

// setCORS allows exactly the agent's origin. The PWA is served by the agent, so
// its fetch to the waker is cross-origin and needs this; nothing else does
// (docs/04-API.md §8).
func (s *server) setCORS(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", s.cfg.AgentOrigin)
	h.Set("Access-Control-Allow-Methods", "GET, POST")
	h.Set("Access-Control-Allow-Headers", "Content-Type, X-Burrow")
	h.Set("Access-Control-Max-Age", "600")
	// No credentials: the waker has no session and needs none.
	h.Set("Vary", "Origin")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
}

func (s *server) handlePreflight(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleWake(w http.ResponseWriter, r *http.Request) {
	if retry, ok := s.allowWake(); !ok {
		w.Header().Set("Retry-After", fmt.Sprint(int(retry.Seconds())))
		writeJSON(w, http.StatusTooManyRequests,
			map[string]any{"error": "rate limited", "retryAfterSec": int(retry.Seconds())})
		return
	}

	mac, err := wol.ParseMAC(s.cfg.PC.MAC)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "bad MAC in config"})
		return
	}
	if err := wol.Wake(wol.UDPSender{}, mac, s.cfg.PC.LANPrefix, nil); err != nil {
		s.logf("wake: %v", err)
		// Partial failure is still worth reporting as sent: the packet may have
		// reached the PC on one of the other addresses.
	}
	now := time.Now().UTC()
	s.logf("wake sent")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sentAt": now})
}

// allowWake applies both rate limits.
func (s *server) allowWake() (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()

	if now.Sub(s.dayStart) > 24*time.Hour {
		s.dayStart = now
		s.wakesToday = 0
	}
	if s.wakesToday >= wakeMaxPerDay {
		return s.dayStart.Add(24 * time.Hour).Sub(now), false
	}
	if since := now.Sub(s.lastWake); !s.lastWake.IsZero() && since < wakeMinInterval {
		return wakeMinInterval - since, false
	}
	s.lastWake = now
	s.wakesToday++
	return 0, true
}

// pcState is the three-way state the PWA's stepper shows.
type pcState string

const (
	stateOnline  pcState = "online"
	stateBooting pcState = "booting"
	stateOffline pcState = "offline"
)

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	last := s.lastWake
	s.mu.Unlock()

	resp := map[string]any{
		"pc":        s.probe(r.Context()),
		"checkedAt": time.Now().UTC(),
	}
	if !last.IsZero() {
		resp["lastWakeAt"] = last.UTC()
	} else {
		resp["lastWakeAt"] = nil
	}
	writeJSON(w, http.StatusOK, resp)
}

// probe decides the PC's state.
//
// "booting" matters to the user: between the plug coming on and the agent being
// up there is a minute or more where the PC is very much alive, and showing
// "offline" there would make them press the power-cycle button
// (docs/02-ARCHITECTURE.md §2.2).
func (s *server) probe(ctx context.Context) pcState {
	if s.agentHealthy(ctx) {
		return stateOnline
	}
	if s.cfg.PC.ProbeAddr != "" && s.tcpAlive(ctx, s.cfg.PC.ProbeAddr) {
		return stateBooting
	}
	return stateOffline
}

// agentHealthy checks the agent's /healthz over the tailnet. The ACL allows the
// waker to reach exactly this (docs/03-SECURITY.md §3.1).
func (s *server) agentHealthy(ctx context.Context) bool {
	// The status endpoint is reachable the moment the listener is up, and a
	// probe before the tailnet node exists must report "cannot reach it"
	// rather than crash the waker — which is the one thing that has to keep
	// running while the PC is off.
	if s.node == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	host := strings.TrimPrefix(s.cfg.AgentOrigin, "https://")
	client := &http.Client{
		Timeout: 4 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return s.node.Dial(ctx, network, addr)
			},
			TLSClientConfig: &tls.Config{ServerName: host},
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.AgentOrigin+"/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// tcpAlive dials a LAN port to see whether the machine is up at all.
func (s *server) tcpAlive(ctx context.Context, addr string) bool {
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
