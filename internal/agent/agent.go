// Package agent assembles and runs the whole system: tsnet node, HTTP server,
// session manager, file service, power manager and the admin pipe.
package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"burrow/internal/adminpipe"
	"burrow/internal/apps"
	"burrow/internal/audit"
	"burrow/internal/auth"
	"burrow/internal/config"
	"burrow/internal/files"
	"burrow/internal/power"
	"burrow/internal/pty"
	"burrow/internal/sessions"
	"burrow/internal/store"
	"burrow/internal/system"
	"burrow/internal/tsnode"
	"burrow/internal/web"
)

// Options configures a run.
type Options struct {
	ConfigPath string
	// Static is the embedded PWA build.
	Static fs.FS
	// Logf receives the agent's own diagnostics. Never PTY content, cookies or
	// tokens (docs/02-ARCHITECTURE.md §4).
	Logf func(format string, args ...any)
	// AuthKey joins the tailnet on first start. Read from the environment by
	// the caller and never written to disk by Burrow.
	AuthKey string
}

// Agent is a running instance.
type Agent struct {
	opts  Options
	cfg   *config.Config
	store *store.Store
	audit *audit.Logger
	node  *tsnode.Node
	auth  *auth.Manager
	sess  *sessions.Manager
	files *files.Service
	power *power.Manager
	sys   system.Collector
	http  *http.Server
	pipe  net.Listener

	started time.Time
	logf    func(string, ...any)
}

// New builds an agent from a validated config.
func New(opts Options) (*Agent, error) {
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	cfg, err := config.Load(opts.ConfigPath)
	if err != nil {
		return nil, err
	}

	a := &Agent{opts: opts, cfg: cfg, logf: logf, started: time.Now()}

	if a.audit, err = audit.Open(cfg.Audit.Path); err != nil {
		return nil, err
	}
	if a.store, err = store.Open(filepath.Join(cfg.Agent.StateDir, "burrow.db")); err != nil {
		return nil, err
	}

	a.auth = auth.NewManager(a.store, a.configFn(), a.logAudit, time.Now)
	if err := a.auth.LoadLockdown(context.Background()); err != nil {
		return nil, err
	}
	if a.auth.Lockdown() {
		logf("starting in lockdown; clear it with `burrow unlock` on this PC")
	}

	roots := make([]files.RootSpec, 0, len(cfg.Files.Roots))
	rootPaths := make([]string, 0, len(cfg.Files.Roots))
	for _, r := range cfg.Files.Roots {
		roots = append(roots, files.RootSpec{Name: r.Name, Path: r.Path, Writable: r.Writable})
		rootPaths = append(rootPaths, r.Path)
	}
	fileRes, err := files.NewResolver(roots, cfg.Files.ExtraDenylist)
	if err != nil {
		return nil, err
	}
	a.files = files.NewService(fileRes, cfg.Files.MaxUploadMB)
	a.sys = system.New(rootPaths)

	a.power = power.NewManager(nil, a.logAudit)
	a.sess = sessions.NewManager(a.configFn(), a.logAudit, pty.Start)
	if cfg.Power.KeepAwakeWhileSessions {
		// The machine is held awake exactly while Claude has work in flight.
		a.sess.OnRunningCountChange(a.power.SetSessionsRunning)
	}

	a.node, err = tsnode.New(tsnode.Options{
		Hostname:   cfg.Agent.Hostname,
		StateDir:   filepath.Join(cfg.Agent.StateDir, "tsnet"),
		AuthKey:    opts.AuthKey,
		LogsUpload: cfg.Agent.TsnetLogsUpload,
		Logf:       func(f string, args ...any) { logf("tsnet: "+f, args...) },
	})
	if err != nil {
		return nil, err
	}
	return a, nil
}

func (a *Agent) configFn() func() *config.Config {
	return func() *config.Config { return a.cfg }
}

func (a *Agent) logAudit(e audit.Entry) {
	if err := a.audit.Log(e); err != nil {
		a.logf("audit: could not write entry: %v", err)
	}
}

// Run starts everything and blocks until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	if err := a.node.Start(ctx); err != nil {
		return err
	}
	fqdn := a.node.FQDN()
	if fqdn == "" {
		return errors.New("agent: the tailnet did not report a hostname; is the node authorised?")
	}
	a.logf("tailnet node is up as %s", fqdn)

	// Register the "Open on PC" tasks. A failure here only costs those
	// buttons, so it is logged rather than stopping the agent.
	opener := apps.New()
	if err := opener.Sync(a.cfg.Apps); err != nil {
		a.logf("apps: %v", err)
	}

	// The FQDN is the WebAuthn relying-party id, so it can only be set now.
	if err := a.auth.SetRelyingParty(fqdn); err != nil {
		return err
	}

	srv := web.NewServer(web.Deps{
		Config:      a.configFn(),
		Origin:      a.node.Origin,
		Resolver:    a.node,
		Auth:        a.auth,
		Sessions:    a.sess,
		Files:       a.files,
		Power:       a.power,
		System:      a.sys,
		Audit:       a.audit,
		Static:      a.opts.Static,
		SSHKeyAdder: a.addSSHKey,
		Lockdown:    a.enterLockdown,
		Apps:        opener,
	})

	ln, err := a.node.ListenTLS(":443")
	if err != nil {
		return err
	}
	a.http = &http.Server{
		Handler: srv.Handler(),
		// Generous read timeout: a 2 GB upload over a phone connection takes
		// a while, and the handler streams rather than buffering.
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	if a.pipe, err = adminpipe.Listen(); err != nil {
		return err
	}
	go func() {
		if err := adminpipe.Serve(a.pipe, a.handleAdmin); err != nil && !errors.Is(err, net.ErrClosed) {
			a.logf("admin pipe: %v", err)
		}
	}()

	go a.housekeeping(ctx)

	errc := make(chan error, 1)
	go func() {
		a.logf("serving on https://%s", fqdn)
		if err := a.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
			return
		}
		errc <- nil
	}()

	select {
	case <-ctx.Done():
		return a.Shutdown()
	case err := <-errc:
		a.Shutdown()
		return err
	}
}

// housekeeping runs the periodic jobs: expiring sessions and keep-awake holds,
// and the idle auto-shutdown if it is switched on.
func (a *Agent) housekeeping(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()

	idleSince := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := a.auth.PurgeExpired(ctx); err != nil {
				a.logf("housekeeping: purge sessions: %v", err)
			}
			a.power.Tick()

			// Idle auto-shutdown exists because sleep is disabled: a sleeping
			// PC cannot be woken by the plug, so the only way to save power is
			// to shut down (docs/01-PRD.md US-14).
			mins := a.cfg.Power.AutoShutdownIdleMinutes
			if mins <= 0 {
				idleSince = time.Now()
				continue
			}
			if a.sess.RunningCount() > 0 || a.power.KeepAwake().Active {
				idleSince = time.Now()
				continue
			}
			if time.Since(idleSince) >= time.Duration(mins)*time.Minute {
				a.logf("idle for %d minutes; shutting down", mins)
				if _, err := a.power.Do(power.Shutdown, 60*time.Second, "auto-shutdown"); err != nil {
					a.logf("auto-shutdown failed: %v", err)
				}
				idleSince = time.Now()
			}
		}
	}
}

// Shutdown stops everything in a safe order.
func (a *Agent) Shutdown() error {
	a.logf("shutting down")
	if a.http != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = a.http.Shutdown(ctx)
	}
	if a.pipe != nil {
		_ = a.pipe.Close()
	}
	if a.sess != nil {
		a.sess.Shutdown()
	}
	if a.node != nil {
		_ = a.node.Close()
	}
	if a.store != nil {
		_ = a.store.Close()
	}
	if a.audit != nil {
		_ = a.audit.Close()
	}
	return nil
}

// FQDN reports the agent's tailnet name.
func (a *Agent) FQDN() string { return a.node.FQDN() }

// ---------- lockdown ----------

// enterLockdown kills sessions and stops the OS-level services it can.
//
// It returns what it could not stop rather than failing: locking the app down
// is the important half, and the owner needs to know if sshd or the SMB share
// is still up (docs/04-API.md §3).
func (a *Agent) enterLockdown(ctx context.Context, actor string) ([]string, error) {
	if err := a.auth.EnterLockdown(ctx, actor); err != nil {
		return nil, err
	}
	a.sess.KillAll(actor)

	var couldNotStop []string
	// Stopping these needs administrator rights, which the service may not
	// have; the app-level lockdown holds either way.
	for _, svc := range []string{"sshd", "LanmanServer"} {
		if err := stopService(ctx, svc); err != nil {
			couldNotStop = append(couldNotStop, svc)
			a.logf("lockdown: could not stop %s: %v", svc, err)
		}
	}
	return couldNotStop, nil
}

func stopService(ctx context.Context, name string) error {
	dir, err := systemDir()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, filepath.Join(dir, "sc.exe"), "stop", name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ---------- ssh keys ----------

// addSSHKey appends a public key to the right authorized_keys file.
//
// Windows OpenSSH reads an administrator's keys from
// %ProgramData%\ssh\administrators_authorized_keys, not from the user's own
// file, and ignores the user file entirely for admin accounts. Writing to the
// wrong one is a silent failure, so both are handled
// (docs/03-SECURITY.md §5).
func (a *Agent) addSSHKey(ctx context.Context, publicKey string) (string, error) {
	path, err := authorizedKeysPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("agent: ssh key directory: %w", err)
	}

	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("agent: read authorized_keys: %w", err)
	}
	// The key body is the second field; comparing on it means re-adding the
	// same key with a different comment is recognised as a duplicate.
	fields := strings.Fields(publicKey)
	if len(fields) < 2 {
		return "", errors.New("agent: malformed public key")
	}
	if strings.Contains(string(existing), fields[1]) {
		return sshFingerprint(fields[1]), nil
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("agent: open authorized_keys: %w", err)
	}
	defer f.Close()

	line := publicKey
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		line = "\n" + line
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		return "", fmt.Errorf("agent: write authorized_keys: %w", err)
	}
	return sshFingerprint(fields[1]), nil
}

// ---------- admin pipe ----------

func (a *Agent) handleAdmin(cmd adminpipe.Command) adminpipe.Response {
	ctx := context.Background()

	switch cmd.Op {
	case adminpipe.OpStatus:
		devices, _ := a.auth.ListDevices(ctx)
		return adminpipe.OK(adminpipe.StatusData{
			Version:   web.Version,
			FQDN:      a.node.FQDN(),
			Lockdown:  a.auth.Lockdown(),
			Sessions:  a.sess.RunningCount(),
			Devices:   len(devices),
			Uptime:    time.Since(a.started).Round(time.Second).String(),
			KeepAwake: a.power.KeepAwake().Active,
		})

	case adminpipe.OpPairOpen:
		code, expires, err := a.auth.OpenPairing()
		if err != nil {
			return adminpipe.Fail("%v", err)
		}
		return adminpipe.OK(adminpipe.PairData{
			Code:    code,
			URL:     "https://" + a.node.FQDN() + "/pair",
			Expires: expires,
		})

	case adminpipe.OpPairClose:
		a.auth.ClosePairing()
		return adminpipe.OK(nil)

	case adminpipe.OpUnlock:
		if err := a.auth.ExitLockdown(ctx, "console"); err != nil {
			return adminpipe.Fail("%v", err)
		}
		return adminpipe.OK(nil)

	case adminpipe.OpDevices:
		devices, err := a.auth.ListDevices(ctx)
		if err != nil {
			return adminpipe.Fail("%v", err)
		}
		rows := make([]adminpipe.DeviceRow, 0, len(devices))
		for _, d := range devices {
			row := adminpipe.DeviceRow{Name: d.Name, Kind: d.Kind, NodeID: d.NodeID}
			if d.LastSeen != nil {
				row.LastSeen = d.LastSeen.Format(time.RFC3339)
			}
			rows = append(rows, row)
		}
		return adminpipe.OK(rows)

	case adminpipe.OpDeviceRevoke:
		target := cmd.Args["device"]
		if target == "" {
			return adminpipe.Fail("name a device")
		}
		devices, err := a.auth.ListDevices(ctx)
		if err != nil {
			return adminpipe.Fail("%v", err)
		}
		if len(devices) <= 1 {
			return adminpipe.Fail("that is the only paired device; pair another one first")
		}
		for _, d := range devices {
			if !strings.EqualFold(d.Name, target) && d.NodeID != target {
				continue
			}
			if err := a.auth.RevokeDevice(ctx, d.ID); err != nil {
				return adminpipe.Fail("%v", err)
			}
			remaining := make([]config.AllowedDevice, 0, len(a.cfg.Device.Allowed))
			for _, ad := range a.cfg.Device.Allowed {
				if ad.NodeID != d.NodeID {
					remaining = append(remaining, ad)
				}
			}
			a.cfg.Device.Allowed = remaining
			if err := config.SaveDevices(a.cfg); err != nil {
				return adminpipe.Fail("revoked the passkey but could not update the config: %v", err)
			}
			a.logAudit(audit.Entry{Actor: "console", Action: audit.ActionDeviceRevoke,
				Target: d.Name, Result: audit.ResultOK})
			return adminpipe.OK(nil)
		}
		return adminpipe.Fail("no device called %q", target)

	case adminpipe.OpKillAll:
		killed := a.sess.KillAll("console")
		return adminpipe.OK(map[string]int{"killed": killed})

	case adminpipe.OpSelftest:
		return adminpipe.OK(a.selftest(ctx))

	default:
		return adminpipe.Fail("unknown operation %q", cmd.Op)
	}
}
