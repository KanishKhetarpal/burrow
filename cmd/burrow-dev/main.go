//go:build burrowdev

// Command burrow-dev serves the real web stack on loopback so the PWA can be
// rendered and clicked in a browser without a tailnet.
//
// It is a development harness only: it is behind the burrowdev build tag, so
// `go build ./...`, the release build and the installer never include it. It
// wires the real router, middleware, auth, file service and session manager,
// and replaces only the parts that need a tailnet or touch the machine — the
// identity resolver is a fake that maps loopback to a paired phone, power
// actions are no-ops, and sessions run an echo process instead of Claude.
//
// Passkeys cannot run here (the relying party is a tailnet name), so the
// harness mints a session directly:
//
//	go run -tags burrowdev ./cmd/burrow-dev
//	http://127.0.0.1:5199/__dev/login    signed in, step-up fresh
//	http://127.0.0.1:5199/__dev/logout   signed out -> Unlock screen
//	http://127.0.0.1:5199/__dev/pair     opens the pairing window
//	http://[::1]:5199/                   an unpaired laptop -> Pair screen while open
//	http://127.0.0.1:5199/__dev/lockdown enters lockdown; /__dev/unlock leaves it
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"burrow/internal/audit"
	"burrow/internal/auth"
	"burrow/internal/config"
	"burrow/internal/files"
	"burrow/internal/identity"
	"burrow/internal/power"
	"burrow/internal/pty"
	"burrow/internal/sessions"
	"burrow/internal/store"
	"burrow/internal/system"
	"burrow/internal/web"
)

const owner = "owner@example.com"

func main() {
	port := flag.Int("port", 5199, "loopback port")
	static := flag.String("static", "web/dist", "built PWA directory")
	flag.Parse()

	if err := run(*port, *static); err != nil {
		log.Fatal(err)
	}
}

func run(port int, static string) error {
	dir, err := os.MkdirTemp("", "burrow-dev-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	root := filepath.Join(dir, "Docs")
	project := filepath.Join(dir, "code", "demo")
	for _, d := range []string{filepath.Join(root, "reports"), project} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	samples := map[string]string{
		"notes.txt":             "Burrow dev harness.\nThis file lives in a temp root.\n",
		"reports/q3-summary.md": "# Q3\n\nNumbers go here.\n",
		"todo.txt":              "- pair the phone\n- run selftest\n",
	}
	for name, body := range samples {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(name)), []byte(body), 0o600); err != nil {
			return err
		}
	}

	origin := fmt.Sprintf("http://127.0.0.1:%d", port)
	cfg := &config.Config{
		Owner: config.Owner{TailscaleLogin: owner},
		Device: config.Device{MaxDevices: 3, Allowed: []config.AllowedDevice{
			{Name: "iPhone", Kind: config.KindPhone, NodeID: "nPHONE"},
		}},
		Agent: config.Agent{Hostname: "n-dev00000", StateDir: dir,
			Auth: config.AuthCfg{
				SessionIdleMinutes: 30, SessionAbsoluteHours: 12, StepupSeconds: 300,
				LoginMaxFailures: 5, LockoutMinutes: 15, PairingWindowMinutes: 10,
			}},
		Claude: config.Claude{
			Binary: "claude", DefaultMode: "chat", DefaultPermissionMode: "default",
			ServerSpawn: "same-dir", ServerCapacity: 8, MaxSessions: 3,
			RestrictedToken: true, ScrollbackBytes: 64 << 10, ReadyTimeoutSeconds: 30,
		},
		Projects:  []config.Project{{Name: "demo", Path: project}},
		Launchers: []config.Launcher{{Name: "Start personal work", Path: project}},
		Files: config.Files{
			Roots:             []config.Root{{Name: "Docs", Path: root, Writable: true}},
			UploadDefaultRoot: "Docs", MaxUploadMB: 8,
		},
		Wake: config.Wake{
			Method:             config.WakeSmartPlug,
			Shortcuts:          config.Shortcuts{On: "PC On", Off: "PC Off", PowerCycle: "PC Power Cycle"},
			WakeTimeoutSeconds: 180, PowerCycleAfterSeconds: 90, OffConfirmSeconds: 30,
		},
		SSH:     config.SSH{User: "pcuser", Host: "pc.tail1234.ts.net"},
		Audit:   config.AuditCfg{Path: filepath.Join(dir, "audit.jsonl")},
		Logging: config.LoggingCfg{Dir: dir, Level: "info", MaxFiles: 10, MaxSizeMB: 10},
	}
	cfg.SetPath(filepath.Join(dir, "burrow.yaml"))
	cfgFn := func() *config.Config { return cfg }

	st, err := store.OpenMemory()
	if err != nil {
		return err
	}
	defer st.Close()

	lg, err := audit.Open(cfg.Audit.Path)
	if err != nil {
		return err
	}
	defer lg.Close()
	logAudit := func(e audit.Entry) { _ = lg.Log(e) }

	am := auth.NewManager(st, cfgFn, logAudit, time.Now)
	if err := am.SetRelyingParty("n-dev00000.tail1234.ts.net"); err != nil {
		return err
	}

	// IPv4 loopback is the paired phone; IPv6 loopback is an owner laptop that
	// has not paired yet, which is what the Pair screen needs.
	resolver := identity.NewFake().
		Add("127.0.0.1", identity.Node{StableID: "nPHONE", Login: owner, Hostname: "iphone"}).
		Add("::1", identity.Node{StableID: "nLAPTOP", Login: owner, Hostname: "laptop"})

	fileRes, err := files.NewResolver([]files.RootSpec{{Name: "Docs", Path: root, Writable: true}}, nil)
	if err != nil {
		return err
	}

	sessMgr := sessions.NewManager(cfgFn, logAudit,
		func(pty.Config) (pty.Process, error) { return newEchoProc(), nil })
	defer sessMgr.Shutdown()

	srv := web.NewServer(web.Deps{
		Config:   cfgFn,
		Origin:   func() string { return origin },
		Resolver: resolver,
		Auth:     am,
		Sessions: sessMgr,
		Files:    files.NewService(fileRes, cfg.Files.MaxUploadMB),
		Power:    power.NewManager(noPower{}, logAudit),
		System:   system.New([]string{root}),
		Audit:    lg,
		Static:   os.DirFS(static),
		SSHKeyAdder: func(context.Context, string) (string, error) {
			return "SHA256:dev-harness-not-written", nil
		},
		Lockdown: func(ctx context.Context, actor string) ([]string, error) {
			if err := am.EnterLockdown(ctx, actor); err != nil {
				return nil, err
			}
			sessMgr.KillAll(actor)
			return nil, nil
		},
	})

	dev := store.Device{ID: "d1", Name: "iPhone", Kind: "phone", NodeID: "nPHONE",
		Login: owner, PairedAt: time.Now().UTC()}
	if err := st.AddDevice(context.Background(), dev); err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /__dev/login", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().UTC()
		id, err := auth.RandomHex(16)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		sess := store.AuthSession{ID: id, DeviceID: dev.ID, NodeID: dev.NodeID,
			CreatedAt: now, LastSeenAt: now, StepupAt: &now}
		if err := st.CreateAuthSession(r.Context(), sess); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		auth.SetSessionCookie(w, id, am.AbsoluteLifetime())
		http.Redirect(w, r, "/", http.StatusFound)
	})
	mux.HandleFunc("GET /__dev/logout", func(w http.ResponseWriter, r *http.Request) {
		auth.ClearSessionCookie(w)
		http.Redirect(w, r, "/", http.StatusFound)
	})
	mux.HandleFunc("GET /__dev/pair", func(w http.ResponseWriter, r *http.Request) {
		code, _, err := am.OpenPairing()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		log.Printf("pairing window open, code %s", code)
		http.Redirect(w, r, "/pair", http.StatusFound)
	})
	mux.HandleFunc("GET /__dev/lockdown", func(w http.ResponseWriter, r *http.Request) {
		_ = am.EnterLockdown(r.Context(), "dev")
		http.Redirect(w, r, "/", http.StatusFound)
	})
	mux.HandleFunc("GET /__dev/unlock", func(w http.ResponseWriter, r *http.Request) {
		_ = am.ExitLockdown(r.Context(), "dev")
		http.Redirect(w, r, "/", http.StatusFound)
	})
	mux.Handle("/", srv.Handler())

	log.Printf("burrow-dev on %s (open %s/__dev/login)", origin, origin)
	go func() {
		// Best effort: a machine without IPv6 loopback just has no Pair view.
		if err := http.ListenAndServe(fmt.Sprintf("[::1]:%d", port), mux); err != nil {
			log.Printf("[::1] listener: %v", err)
		}
	}()
	return http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", port), mux)
}

// noPower accepts every power action without touching the machine.
type noPower struct{}

func (noPower) Suspend(bool) error { log.Print("power: suspend (ignored)"); return nil }
func (noPower) Shutdown(restart bool, d time.Duration) error {
	log.Printf("power: shutdown restart=%v delay=%s (ignored)", restart, d)
	return nil
}
func (noPower) CancelShutdown() error    { return nil }
func (noPower) LockWorkstation() error   { return nil }
func (noPower) KeepAwake(bool) error     { return nil }
func (noPower) InteractiveSession() bool { return true }

// echoProc stands in for Claude: it prints a banner and echoes what it is sent.
type echoProc struct {
	pr   *io.PipeReader
	pw   *io.PipeWriter
	once sync.Once
	done chan struct{}
}

func newEchoProc() *echoProc {
	pr, pw := io.Pipe()
	p := &echoProc{pr: pr, pw: pw, done: make(chan struct{})}
	go func() {
		_, _ = pw.Write([]byte("\x1b[1mburrow-dev echo session\x1b[0m\r\nType and it comes back.\r\n> "))
	}()
	return p
}

func (p *echoProc) Read(b []byte) (int, error) { return p.pr.Read(b) }
func (p *echoProc) Write(b []byte) (int, error) {
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		if c == '\r' {
			out = append(out, '\r', '\n', '>', ' ')
			continue
		}
		out = append(out, c)
	}
	go func() { _, _ = p.pw.Write(out) }()
	return len(b), nil
}
func (p *echoProc) Resize(pty.Size) error { return nil }
func (p *echoProc) PID() int              { return 4242 }
func (p *echoProc) Wait() (int, error)    { <-p.done; return 0, nil }
func (p *echoProc) Kill() error           { return p.Close() }
func (p *echoProc) Close() error {
	p.once.Do(func() {
		close(p.done)
		_ = p.pw.Close()
	})
	return nil
}
