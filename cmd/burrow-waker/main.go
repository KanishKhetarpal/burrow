// Command burrow-waker is a tiny tsnet node that sends a Wake-on-LAN packet to
// the PC.
//
// It is built and tested but not deployed in v1: Burrow wakes the PC with a
// smart plug, which needs no extra computer. The waker exists so a Raspberry Pi
// can be added later by switching wake.method to "waker", with no change to the
// agent or the app (docs/02-ARCHITECTURE.md §2.2, D7).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	cfgPath := flag.String("config", defaultConfigPath(), "config path")
	flag.Parse()

	logger := log.New(os.Stderr, "", log.LstdFlags|log.LUTC)

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		logger.Fatalf("waker: %v", err)
	}

	srv, err := newServer(cfg, logger.Printf)
	if err != nil {
		logger.Fatalf("waker: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := srv.run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Fatalf("waker: %v", err)
	}
}

func defaultConfigPath() string {
	// On Termux there is no /etc, so the home directory is used instead.
	if _, err := os.Stat("/etc"); err == nil {
		return "/etc/burrow-waker/waker.yaml"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "waker.yaml"
	}
	return fmt.Sprintf("%s/.burrow-waker/waker.yaml", home)
}
