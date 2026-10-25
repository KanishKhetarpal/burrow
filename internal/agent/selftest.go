package agent

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"burrow/internal/adminpipe"
	"burrow/internal/audit"
	"burrow/internal/pty"
)

// selftest runs the checks that prove the install is sound.
//
// The negative check is the important one: the agent dials its own listener
// over the tailnet as its own node identity, which is not a paired device, and
// must get a 404. That is the identity gate working rather than the ACL merely
// hiding the service (docs/07-TEST-PLAN.md §4).
func (a *Agent) selftest(ctx context.Context) adminpipe.SelftestResult {
	var res adminpipe.SelftestResult
	add := func(name string, passed bool, format string, args ...any) {
		res.Checks = append(res.Checks, adminpipe.Check{
			Name: name, Passed: passed, Detail: fmt.Sprintf(format, args...),
		})
	}

	// --- tailnet ---
	fqdn := a.node.FQDN()
	add("tailnet node is up", fqdn != "", "%s", fqdn)

	// --- the self-dial, which must be refused ---
	if fqdn != "" {
		for _, path := range []string{"/", "/api/system"} {
			status, err := a.selfDial(ctx, fqdn, path)
			switch {
			case err != nil:
				add("self-dial "+path+" is refused", false, "could not dial: %v", err)
			case status == http.StatusNotFound:
				add("self-dial "+path+" is refused", true, "404 as expected")
			default:
				add("self-dial "+path+" is refused", false,
					"got %d; the agent's own node is not a paired device and must get 404", status)
			}
		}
		// /healthz is the documented exception.
		if status, err := a.selfDial(ctx, fqdn, "/healthz"); err == nil {
			add("/healthz answers", status == http.StatusOK, "got %d", status)
		}
	}

	// --- Claude Code is present and logged in ---
	claudePath, err := exec.LookPath(a.cfg.Claude.Binary)
	if err != nil {
		add("claude is on PATH", false,
			"%q was not found; sessions will fail to start", a.cfg.Claude.Binary)
	} else {
		add("claude is on PATH", true, "%s", claudePath)
		add("claude is signed in", a.claudeSignedIn(),
			"the service runs in session 0 with no browser, so Claude must already be logged in for this account")
	}

	// --- restricted token ---
	if tok, err := pty.RestrictedToken(); err != nil {
		add("restricted token", false, "could not build one: %v", err)
	} else {
		denyOnly, derr := pty.AdminDenyOnly(tok)
		tok.Close()
		if derr != nil {
			add("restricted token", false, "could not inspect it: %v", derr)
		} else {
			add("restricted token marks Administrators deny-only", denyOnly,
				"child processes run without the service's admin rights")
		}
	}

	// --- config paths exist ---
	for _, r := range a.cfg.Files.Roots {
		if st, err := os.Stat(r.Path); err != nil || !st.IsDir() {
			add("file root "+r.Name, false, "%s is missing", r.Path)
		} else {
			add("file root "+r.Name, true, "")
		}
	}
	for _, p := range a.cfg.Projects {
		if st, err := os.Stat(p.Path); err != nil || !st.IsDir() {
			add("project "+p.Name, false, "%s is missing", p.Path)
		} else {
			add("project "+p.Name, true, "")
		}
	}

	// --- audit chain ---
	if vr, err := audit.Verify(a.cfg.Audit.Path); err != nil {
		add("audit chain verifies", false, "%v", err)
	} else {
		detail := fmt.Sprintf("%d entries", vr.Entries)
		if !vr.OK && len(vr.Problems) > 0 {
			detail = fmt.Sprintf("%d entries, first problem on line %d: %s",
				vr.Entries, vr.Problems[0].Line, vr.Problems[0].Reason)
		}
		add("audit chain verifies", vr.OK, "%s", detail)
	}

	// --- a device is paired ---
	devices, err := a.auth.ListDevices(ctx)
	if err != nil {
		add("a device is paired", false, "%v", err)
	} else {
		names := make([]string, 0, len(devices))
		for _, d := range devices {
			names = append(names, d.Name)
		}
		add("a device is paired", len(devices) > 0, "%s", strings.Join(names, ", "))
	}

	// --- wake configuration ---
	if a.cfg.Wake.Method == "smartplug" {
		s := a.cfg.Wake.Shortcuts
		ok := s.On != "" && s.Off != "" && s.PowerCycle != ""
		add("smart-plug shortcut names are set", ok, "%s / %s / %s", s.On, s.Off, s.PowerCycle)
	}

	res.Passed = true
	for _, c := range res.Checks {
		if !c.Passed {
			res.Passed = false
		}
	}
	return res
}

// selfDial reaches the agent's own listener over the tailnet, so the request
// arrives with the agent node's identity rather than a paired device's.
func (a *Agent) selfDial(ctx context.Context, fqdn, path string) (int, error) {
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return a.node.Dial(ctx, network, addr)
			},
			// The certificate is a real Tailscale-issued one for this name, so
			// verification stays on.
			TLSClientConfig: &tls.Config{ServerName: fqdn},
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+fqdn+path, nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// claudeSignedIn looks for Claude Code's credential file.
//
// The service has no desktop and no browser, so an OAuth prompt can never be
// answered from a Burrow-launched session; the owner has to have logged in once
// in an ordinary terminal (docs/02-ARCHITECTURE.md §5).
func (a *Agent) claudeSignedIn() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	for _, candidate := range []string{
		filepath.Join(home, ".claude", ".credentials.json"),
		filepath.Join(home, ".claude.json"),
	} {
		if st, err := os.Stat(candidate); err == nil && st.Size() > 0 {
			return true
		}
	}
	return false
}

// sshFingerprint returns the OpenSSH-style SHA256 fingerprint of a base64 key
// body, which is what `ssh-keygen -lf` prints.
func sshFingerprint(base64Body string) string {
	raw, err := base64.StdEncoding.DecodeString(base64Body)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return "SHA256:" + strings.TrimRight(base64.StdEncoding.EncodeToString(sum[:]), "=")
}
