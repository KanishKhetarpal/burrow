//go:build windows

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"burrow/internal/agent"
)

// ServiceName is the Windows service name.
const ServiceName = "Burrow"

// serviceDisplay is what appears in services.msc.
const serviceDisplay = "Burrow remote agent"

// burrowService adapts the agent to the service control manager.
type burrowService struct {
	a    *agent.Agent
	logf func(string, ...any)
}

// Execute runs the service. The agent is started on a goroutine and stopped
// when the control manager asks.
func (s *burrowService) Execute(args []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown

	status <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errc := make(chan error, 1)
	go func() { errc <- s.a.Run(ctx) }()

	status <- svc.Status{State: svc.Running, Accepts: accepted}

	for {
		select {
		case err := <-errc:
			if err != nil {
				s.logf("agent stopped with an error: %v", err)
				status <- svc.Status{State: svc.Stopped}
				// A non-zero exit code is what makes the configured failure
				// recovery (restart after 10s / 30s / 60s) kick in.
				return false, 1
			}
			status <- svc.Status{State: svc.Stopped}
			return false, 0

		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				s.logf("stop requested")
				status <- svc.Status{State: svc.StopPending}
				cancel()
				// Give the agent a moment to close listeners and kill
				// sessions before reporting stopped.
				select {
				case <-errc:
				case <-time.After(20 * time.Second):
					s.logf("agent did not stop in time")
				}
				status <- svc.Status{State: svc.Stopped}
				return false, 0
			default:
				s.logf("unexpected service control %d", c.Cmd)
			}
		}
	}
}

// runAsService hands control to the service manager.
func runAsService(a *agent.Agent, logf func(string, ...any)) error {
	isService, err := svc.IsWindowsService()
	if err != nil {
		return fmt.Errorf("could not tell whether this is a service: %w", err)
	}
	if !isService {
		return errors.New("-service was passed but this process was not started by the service manager")
	}
	return svc.Run(ServiceName, &burrowService{a: a, logf: logf})
}

// cmdInstall registers the service.
//
// The service logs on as the owner's account rather than SYSTEM, so Claude Code
// uses his login, his ~/.claude and his git config. That means a password is
// needed, which is read from a secure console prompt and handed straight to the
// service control manager: it is never echoed, logged, or passed on a command
// line where another process could read it (docs/03-SECURITY.md §4).
func cmdInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	account := fs.String("account", "", `account to run as, e.g. ".\pcuser" (default: the current user)`)
	exePath := fs.String("exe", "", "path to burrow.exe (default: this binary)")
	cfgPath := fs.String("config", "", "config path to pass to the service")
	if err := fs.Parse(args); err != nil {
		return err
	}

	exe := *exePath
	if exe == "" {
		self, err := os.Executable()
		if err != nil {
			return fmt.Errorf("could not find this binary: %w", err)
		}
		exe = self
	}
	exe, err := filepath.Abs(exe)
	if err != nil {
		return err
	}

	acct := *account
	if acct == "" {
		u := os.Getenv("USERDOMAIN")
		n := os.Getenv("USERNAME")
		if u == "" || n == "" {
			return errors.New("could not work out the current account; pass -account")
		}
		acct = u + `\` + n
	}

	// The password is checked with Windows before the service is created. A
	// typo otherwise produces a service that installs fine and then fails to
	// start with nothing but "logon failure" in the event log.
	var password string
	for attempt := 1; ; attempt++ {
		password, err = readPasswordPrompt(fmt.Sprintf(
			"Windows password for %s (not shown, not logged): ", acct))
		if err != nil {
			return err
		}
		if password == "" {
			return errors.New("no password entered")
		}
		if err := checkWindowsPassword(acct, password); err == nil {
			break
		} else if attempt >= 3 {
			return fmt.Errorf("Windows did not accept that password three times: %w", err)
		}
		fmt.Println("Windows did not accept that password. Use your Windows account password, not your PIN.")
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("could not reach the service manager (run as administrator): %w", err)
	}
	defer m.Disconnect()

	if existing, err := m.OpenService(ServiceName); err == nil {
		existing.Close()
		return fmt.Errorf("the %s service already exists; run `burrow uninstall` first", ServiceName)
	}

	cmdArgs := []string{"run", "-service"}
	if *cfgPath != "" {
		cmdArgs = append(cmdArgs, "-config", *cfgPath)
	}

	s, err := m.CreateService(ServiceName, exe, mgr.Config{
		DisplayName: serviceDisplay,
		Description: "Private remote access to this PC over Tailscale. Only paired devices can reach it.",
		// Automatic (Delayed) so the network and tailscaled are up first; the
		// agent would otherwise race them on every boot.
		StartType:        mgr.StartAutomatic,
		DelayedAutoStart: true,
		ServiceStartName: acct,
		Password:         password,
		Dependencies:     []string{"Tailscale"},
	}, cmdArgs...)
	if err != nil {
		return fmt.Errorf("could not create the service: %w", err)
	}
	defer s.Close()

	// Failure recovery from docs/02-ARCHITECTURE.md §5: restart after
	// 10s / 30s / 60s, with the count resetting after a day of running.
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}, uint32((24 * time.Hour).Seconds())); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not set failure recovery: %v\n", err)
	}

	fmt.Printf("Installed the %s service, running as %s.\n", ServiceName, acct)
	fmt.Println("Start it with:  sc start Burrow")
	fmt.Println()
	fmt.Println("If it fails to start with a logon error, grant the account the")
	fmt.Println("\"Log on as a service\" right (secpol.msc → Local Policies → User Rights Assignment).")
	return nil
}

// cmdUninstall removes the service.
func cmdUninstall(args []string) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("could not reach the service manager (run as administrator): %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(ServiceName)
	if err != nil {
		return fmt.Errorf("the %s service is not installed", ServiceName)
	}
	defer s.Close()

	// Stopping first, so the agent closes its listeners and kills sessions
	// rather than being cut off mid-write.
	if st, err := s.Query(); err == nil && st.State != svc.Stopped {
		if _, err := s.Control(svc.Stop); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not stop the service: %v\n", err)
		} else {
			deadline := time.Now().Add(30 * time.Second)
			for time.Now().Before(deadline) {
				if st, err := s.Query(); err == nil && st.State == svc.Stopped {
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
		}
	}

	if err := s.Delete(); err != nil {
		return fmt.Errorf("could not remove the service: %w", err)
	}
	fmt.Printf("Removed the %s service.\n", ServiceName)
	fmt.Println("Config, state and the audit log are left in place; setup/uninstall.ps1 removes those.")
	return nil
}

// serviceState reports the service's state for `burrow status` when the pipe is
// unreachable, so "not running" can be told from "not installed".
//
// The service manager is opened read-only rather than with mgr.Connect, which
// asks for SC_MANAGER_ALL_ACCESS and therefore fails without elevation. Asking
// how the service is doing should not require an admin prompt.
func serviceState() (string, error) {
	h, err := windows.OpenSCManager(nil, nil,
		windows.SC_MANAGER_CONNECT|windows.SC_MANAGER_ENUMERATE_SERVICE)
	if err != nil {
		return "", fmt.Errorf("could not open the service manager: %w", err)
	}
	m := &mgr.Mgr{Handle: h}
	defer m.Disconnect()

	name, err := windows.UTF16PtrFromString(ServiceName)
	if err != nil {
		return "", err
	}
	sh, err := windows.OpenService(h, name, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return "not installed", nil
	}
	s := &mgr.Service{Name: ServiceName, Handle: sh}
	defer s.Close()

	st, err := s.Query()
	if err != nil {
		return "", err
	}
	switch st.State {
	case svc.Running:
		return "running", nil
	case svc.Stopped:
		return "stopped", nil
	case svc.StartPending:
		return "starting", nil
	case svc.StopPending:
		return "stopping", nil
	default:
		return fmt.Sprintf("state %d", st.State), nil
	}
}

// startedByManager reports whether the service is installed and running, used
// by the setup scripts through `burrow status`.
func serviceHint() string {
	state, err := serviceState()
	if err != nil {
		return ""
	}
	switch state {
	case "not installed":
		return "The Burrow service is not installed. Run `burrow install` from an elevated terminal."
	case "stopped":
		return "The Burrow service is installed but stopped. Start it with: sc start Burrow"
	case "running":
		return "The Burrow service is running but the admin pipe did not answer; check the log."
	default:
		return "The Burrow service is " + state + "."
	}
}

var procLogonUserW = windows.NewLazySystemDLL("advapi32.dll").NewProc("LogonUserW")

const (
	logon32LogonNetwork    = 3 // validates credentials; needs no logon right
	logon32ProviderDefault = 0
)

// checkWindowsPassword asks Windows whether the password is right for the
// account ("DOMAIN\user" or ".\user") without starting anything.
func checkWindowsPassword(account, password string) error {
	domain, user := ".", account
	if i := strings.LastIndex(account, `\`); i >= 0 {
		domain, user = account[:i], account[i+1:]
	}
	u, err := windows.UTF16PtrFromString(user)
	if err != nil {
		return err
	}
	d, err := windows.UTF16PtrFromString(domain)
	if err != nil {
		return err
	}
	pw, err := windows.UTF16PtrFromString(password)
	if err != nil {
		return err
	}
	var tok windows.Token
	r, _, errno := procLogonUserW.Call(
		uintptr(unsafe.Pointer(u)), uintptr(unsafe.Pointer(d)), uintptr(unsafe.Pointer(pw)),
		logon32LogonNetwork, logon32ProviderDefault, uintptr(unsafe.Pointer(&tok)))
	if r == 0 {
		return errno
	}
	tok.Close()
	return nil
}

// readPasswordPrompt reads a line from the console with echo off.
func readPasswordPrompt(prompt string) (string, error) {
	fmt.Print(prompt)
	pw, err := readPasswordNoEcho()
	fmt.Println()
	if err != nil {
		return "", err
	}
	return strings.TrimRight(pw, "\r\n"), nil
}
