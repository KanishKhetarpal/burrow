package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"burrow/internal/agent"
	"burrow/internal/config"
)

// The built PWA is embedded so the agent is a single file to deploy. The
// all: prefix includes the hashed asset files, whose names start with an
// underscore in some bundler layouts and would otherwise be skipped.
//
//go:embed all:webdist
var webDist embed.FS

// staticFS returns the PWA root, or nil when the binary was built without it.
func staticFS() fs.FS {
	sub, err := fs.Sub(webDist, "webdist")
	if err != nil {
		return nil
	}
	// A build with only the placeholder file counts as "no PWA", so the API
	// still serves and the static handler says so plainly.
	if _, err := sub.Open("index.html"); err != nil {
		return nil
	}
	return sub
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath, "config path")
	service := fs.Bool("service", false, "run as a Windows service (set by the service manager)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// TS_API_KEY is for the setup scripts, not the agent. The agent only ever
	// reads an auth key, and only on first start (docs/03-SECURITY.md §4).
	authKey := os.Getenv("TS_AUTHKEY")
	// Cleared immediately so it cannot be read out of the agent's own
	// environment later, and never logged.
	_ = os.Unsetenv("TS_AUTHKEY")

	logf, closeLog, err := openLog(*cfgPath)
	if err != nil {
		return err
	}
	defer closeLog()

	a, err := agent.New(agent.Options{
		ConfigPath: *cfgPath,
		Static:     staticFS(),
		Logf:       logf,
		AuthKey:    authKey,
	})
	if err != nil {
		return err
	}

	if *service {
		return runAsService(a, logf)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return a.Run(ctx)
}

// openLog sets up the agent's own log file alongside stderr.
//
// Burrow's log carries lifecycle and error lines only. PTY content, cookies,
// tokens and passkey material never reach it (docs/02-ARCHITECTURE.md §4).
func openLog(cfgPath string) (func(string, ...any), func(), error) {
	dir := filepath.Join(filepath.Dir(cfgPath), "logs")
	if cfg, err := config.Load(cfgPath); err == nil && cfg.Logging.Dir != "" {
		dir = cfg.Logging.Dir
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		// Not fatal: logging to stderr alone is better than refusing to start.
		return func(f string, a ...any) { log.Printf(f, a...) }, func() {}, nil
	}

	f, err := os.OpenFile(filepath.Join(dir, "burrow.log"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return func(f string, a ...any) { log.Printf(f, a...) }, func() {}, nil
	}

	logger := log.New(f, "", log.LstdFlags|log.LUTC)
	logf := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		logger.Print(line)
		// Also to stderr, so `burrow run` in a terminal shows what is
		// happening without tailing a file.
		log.Print(line)
	}
	return logf, func() { f.Close() }, nil
}
