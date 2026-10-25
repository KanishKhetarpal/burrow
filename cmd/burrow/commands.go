package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mdp/qrterminal/v3"

	"burrow/internal/adminpipe"
	"burrow/internal/audit"
	"burrow/internal/config"
)

// cmdPair opens a pairing window and prints the QR code and code.
//
// The code is printed on the PC's screen and nowhere else: that is what makes a
// six-digit code safe, since a window can only be opened from here
// (docs/03-SECURITY.md T10).
func cmdPair(args []string) error {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	small := fs.Bool("small", false, "print a half-height QR code for a short terminal")
	if err := fs.Parse(args); err != nil {
		return err
	}

	resp, err := adminpipe.Call(adminpipe.Command{Op: adminpipe.OpPairOpen})
	if err != nil {
		return err
	}
	data, err := adminpipe.Decode[adminpipe.PairData](resp)
	if err != nil {
		return err
	}

	fmt.Println()
	fmt.Println("Scan this on the device you are pairing:")
	fmt.Println()
	cfg := qrterminal.Config{
		Level:     qrterminal.L,
		Writer:    os.Stdout,
		BlackChar: qrterminal.BLACK,
		WhiteChar: qrterminal.WHITE,
		QuietZone: 1,
	}
	if *small {
		cfg.HalfBlocks = true
		cfg.BlackChar = qrterminal.BLACK_BLACK
		cfg.WhiteChar = qrterminal.WHITE_WHITE
		cfg.BlackWhiteChar = qrterminal.BLACK_WHITE
		cfg.WhiteBlackChar = qrterminal.WHITE_BLACK
	}
	qrterminal.GenerateWithConfig(data.URL, cfg)

	fmt.Println()
	fmt.Printf("  URL:  %s\n", data.URL)
	fmt.Printf("  Code: %s\n", data.Code)
	fmt.Printf("  Open until %s (%s from now)\n",
		data.Expires.Local().Format("15:04:05"),
		time.Until(data.Expires).Round(time.Second))
	fmt.Println()
	fmt.Println("Five wrong codes close the window; run `burrow pair` again if that happens.")
	fmt.Println("The code is shown here only. Do not send it anywhere.")
	return nil
}

func cmdStatus(args []string) error {
	resp, err := adminpipe.Call(adminpipe.Command{Op: adminpipe.OpStatus})
	if err != nil {
		// "The pipe did not answer" is not actionable on its own, so the
		// service's own state is checked to say what to do about it.
		if errors.Is(err, adminpipe.ErrNotRunning) {
			if hint := serviceHint(); hint != "" {
				return errors.New(hint)
			}
		}
		return err
	}
	st, err := adminpipe.Decode[adminpipe.StatusData](resp)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "version\t%s\n", st.Version)
	fmt.Fprintf(w, "address\thttps://%s\n", st.FQDN)
	fmt.Fprintf(w, "uptime\t%s\n", st.Uptime)
	fmt.Fprintf(w, "sessions\t%d running\n", st.Sessions)
	fmt.Fprintf(w, "devices\t%d paired\n", st.Devices)
	fmt.Fprintf(w, "keep awake\t%v\n", st.KeepAwake)
	if st.Lockdown {
		fmt.Fprintf(w, "lockdown\tON — run `burrow unlock` to clear it\n")
	} else {
		fmt.Fprintf(w, "lockdown\toff\n")
	}
	return w.Flush()
}

func cmdDevices(args []string) error {
	if len(args) > 0 && strings.EqualFold(args[0], "revoke") {
		if len(args) < 2 {
			return errors.New("usage: burrow devices revoke NAME")
		}
		resp, err := adminpipe.Call(adminpipe.Command{
			Op:   adminpipe.OpDeviceRevoke,
			Args: map[string]string{"device": args[1]},
		})
		if err != nil {
			return err
		}
		if _, err := adminpipe.Decode[struct{}](resp); err != nil {
			return err
		}
		fmt.Printf("Revoked %s. Its passkey and sessions are gone.\n", args[1])
		fmt.Println("Also remove it at login.tailscale.com to cut SSH, SMB and remote desktop.")
		return nil
	}

	resp, err := adminpipe.Call(adminpipe.Command{Op: adminpipe.OpDevices})
	if err != nil {
		return err
	}
	rows, err := adminpipe.Decode[[]adminpipe.DeviceRow](resp)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("No devices are paired. Run `burrow pair` to add one.")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tKIND\tNODE\tLAST SEEN")
	for _, d := range rows {
		seen := d.LastSeen
		if seen == "" {
			seen = "never"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", d.Name, d.Kind, d.NodeID, seen)
	}
	return w.Flush()
}

func cmdUnlock(args []string) error {
	resp, err := adminpipe.Call(adminpipe.Command{Op: adminpipe.OpUnlock})
	if err != nil {
		return err
	}
	if _, err := adminpipe.Decode[struct{}](resp); err != nil {
		return err
	}
	fmt.Println("Lockdown cleared. Every session was ended, so sign in again on your devices.")
	fmt.Println("If sshd or the file share was stopped, start them again:")
	fmt.Println("  sc start sshd")
	fmt.Println("  sc start LanmanServer")
	return nil
}

func cmdKillAll(args []string) error {
	resp, err := adminpipe.Call(adminpipe.Command{Op: adminpipe.OpKillAll})
	if err != nil {
		return err
	}
	out, err := adminpipe.Decode[map[string]int](resp)
	if err != nil {
		return err
	}
	fmt.Printf("Stopped %d session(s).\n", out["killed"])
	return nil
}

// cmdAudit verifies the hash chain. It reads the file directly rather than
// going through the pipe, so it still works when the service is stopped.
func cmdAudit(args []string) error {
	if len(args) == 0 || !strings.EqualFold(args[0], "verify") {
		return errors.New("usage: burrow audit verify [-path PATH]")
	}
	fs := flag.NewFlagSet("audit verify", flag.ExitOnError)
	path := fs.String("path", "", "audit log path (default: from the config)")
	cfgPath := fs.String("config", config.DefaultPath, "config path")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	auditPath := *path
	if auditPath == "" {
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			return fmt.Errorf("could not read the config to find the audit log: %w", err)
		}
		auditPath = cfg.Audit.Path
	}

	res, err := audit.Verify(auditPath)
	if err != nil {
		return err
	}
	fmt.Printf("%s\n%d entries\n", auditPath, res.Entries)
	if res.OK {
		fmt.Println("The chain verifies: no entry has been edited, inserted or removed.")
		return nil
	}
	fmt.Printf("\nThe chain is BROKEN — %d problem(s):\n", len(res.Problems))
	for _, p := range res.Problems {
		fmt.Printf("  line %d (seq %d): %s\n", p.Line, p.Seq, p.Reason)
	}
	return errors.New("the audit chain did not verify")
}

func cmdSelftest(args []string) error {
	resp, err := adminpipe.Call(adminpipe.Command{Op: adminpipe.OpSelftest})
	if err != nil {
		return err
	}
	res, err := adminpipe.Decode[adminpipe.SelftestResult](resp)
	if err != nil {
		return err
	}

	for _, c := range res.Checks {
		mark := "FAIL"
		if c.Passed {
			mark = "ok  "
		}
		line := fmt.Sprintf("%s  %s", mark, c.Name)
		if c.Detail != "" {
			line += "  — " + c.Detail
		}
		fmt.Println(line)
	}
	fmt.Println()
	if res.Passed {
		fmt.Println("All checks passed.")
		return nil
	}
	return errors.New("some checks failed; see above")
}
