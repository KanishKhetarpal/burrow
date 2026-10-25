// Command burrow is the Burrow agent and its CLI.
//
// Run with no arguments it prints usage. `burrow run` is the service entry
// point; everything else talks to a running service over the local admin pipe.
package main

import (
	"fmt"
	"os"
	"strings"

	"burrow/internal/web"
)

// usage is kept short and task-shaped, because it is read on a PC while
// following the runbook.
const usage = `burrow %s — a private remote for this PC

Usage:
  burrow run [-config PATH]      run the agent in the foreground (the service entry point)
  burrow install                 install the Windows service
  burrow uninstall               remove the Windows service
  burrow pair                    open a pairing window and print a QR code
  burrow status                  show the agent's state
  burrow devices                 list paired devices
  burrow devices revoke NAME     revoke a device's access
  burrow unlock                  clear lockdown (only works from this PC)
  burrow kill-all                stop every Claude session
  burrow audit verify [-path P]  check the audit log's hash chain
  burrow selftest                run the install checks
  burrow version                 print the version

Run from an elevated terminal for install, uninstall and unlock.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(fmt.Sprintf(usage, web.Version))
		os.Exit(2)
	}

	cmd := strings.ToLower(os.Args[1])
	args := os.Args[2:]

	var err error
	switch cmd {
	case "run":
		err = cmdRun(args)
	case "install":
		err = cmdInstall(args)
	case "uninstall":
		err = cmdUninstall(args)
	case "pair":
		err = cmdPair(args)
	case "status":
		err = cmdStatus(args)
	case "devices":
		err = cmdDevices(args)
	case "unlock":
		err = cmdUnlock(args)
	case "kill-all":
		err = cmdKillAll(args)
	case "audit":
		err = cmdAudit(args)
	case "selftest":
		err = cmdSelftest(args)
	case "version", "-v", "--version":
		fmt.Println(web.Version)
	case "help", "-h", "--help":
		fmt.Print(fmt.Sprintf(usage, web.Version))
	default:
		fmt.Fprintf(os.Stderr, "burrow: unknown command %q\n\n", cmd)
		fmt.Fprint(os.Stderr, fmt.Sprintf(usage, web.Version))
		os.Exit(2)
	}

	if err != nil {
		// Errors go to stderr as one line, so the setup scripts can capture
		// them without parsing a stack trace.
		fmt.Fprintf(os.Stderr, "burrow: %v\n", err)
		os.Exit(1)
	}
}
