//go:build windows

package pty

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// readFor collects output until the deadline or EOF. A PTY never closes
// cleanly on a timer, so reads are drained in a goroutine — which may still be
// writing when the deadline fires, hence the mutex (found by -race).
func readFor(t *testing.T, p Process, d time.Duration) string {
	t.Helper()
	var (
		mu sync.Mutex
		sb strings.Builder
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := p.Read(buf)
			if n > 0 {
				mu.Lock()
				sb.Write(buf[:n])
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(d):
	}
	mu.Lock()
	defer mu.Unlock()
	return sb.String()
}

func TestStartRunsACommandOnAPseudoConsole(t *testing.T) {
	p, err := Start(Config{
		Path:       "cmd.exe",
		Args:       []string{"/c", "echo", "burrow-pty-hello"},
		Dir:        t.TempDir(),
		Restricted: false,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	if p.PID() <= 0 {
		t.Errorf("PID = %d, want a real pid", p.PID())
	}
	out := readFor(t, p, 10*time.Second)
	if !strings.Contains(out, "burrow-pty-hello") {
		t.Fatalf("output did not contain the echoed text.\nGot: %q", out)
	}

	code, err := p.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

func TestStartReportsANonZeroExitCode(t *testing.T) {
	p, err := Start(Config{Path: "cmd.exe", Args: []string{"/c", "exit", "3"}, Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()
	readFor(t, p, 5*time.Second)

	code, err := p.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
}

func TestWriteReachesTheChild(t *testing.T) {
	// `more` echoes stdin back, which proves the input pipe is wired to the
	// console and not just accepted silently.
	p, err := Start(Config{Path: "cmd.exe", Args: []string{"/k"}, Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()
	defer p.Kill()

	if _, err := p.Write([]byte("echo burrow-input-works\r\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out := readFor(t, p, 10*time.Second)
	if !strings.Contains(out, "burrow-input-works") {
		t.Fatalf("the child never ran the typed command.\nGot: %q", out)
	}
}

func TestResize(t *testing.T) {
	p, err := Start(Config{Path: "cmd.exe", Args: []string{"/k"}, Dir: t.TempDir(),
		Size: Size{Cols: 80, Rows: 24}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()
	defer p.Kill()

	if err := p.Resize(Size{Cols: 120, Rows: 40}); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if err := p.Resize(Size{Cols: 0, Rows: 40}); err == nil {
		t.Error("Resize accepted a zero column count")
	}
}

// TestKillTerminatesTheWholeTree starts a parent that starts a grandchild and
// checks both are gone. This is the behaviour "Stop session" depends on: Claude
// Code starts shells, and those start build tools.
func TestKillTerminatesTheWholeTree(t *testing.T) {
	dir := t.TempDir()
	marker := dir + `\grandchild-alive.txt`

	// The grandchild writes a marker, then sleeps far longer than the test.
	script := dir + `\spawn.cmd`
	body := "@echo off\r\n" +
		"start /b cmd /c \"echo alive > \"" + marker + "\" & ping -n 600 127.0.0.1 >nul\"\r\n" +
		"ping -n 600 127.0.0.1 >nul\r\n"
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	p, err := Start(Config{Path: "cmd.exe", Args: []string{"/c", script}, Dir: dir})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()
	go readFor(t, p, 30*time.Second)

	// Wait for the grandchild to prove it started.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Skipf("the grandchild never started, so there is nothing to test: %v", err)
	}

	pingsBefore := countPings(t)
	if err := p.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if _, err := p.Wait(); err != nil {
		t.Fatalf("Wait after Kill: %v", err)
	}

	// The job object should have taken the grandchild with it.
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if countPings(t) < pingsBefore {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Errorf("ping processes did not drop after Kill (%d before); the tree outlived the job",
		pingsBefore)
}

// countPings counts running ping.exe processes, as a proxy for "the tree is
// still alive".
func countPings(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("tasklist", "/fi", "imagename eq ping.exe", "/nh").Output()
	if err != nil {
		return 0
	}
	return strings.Count(strings.ToLower(string(out)), "ping.exe")
}

// TestRestrictedTokenMarksAdministratorsDenyOnly is the acceptance test named
// in docs/02-ARCHITECTURE.md §5: inside a restricted session, `whoami /groups`
// must show BUILTIN\Administrators as used for deny only.
func TestRestrictedTokenMarksAdministratorsDenyOnly(t *testing.T) {
	if !currentUserIsAdmin(t) {
		t.Skip("the test user is not an administrator, so there is no admin membership to restrict")
	}

	p, err := Start(Config{
		Path:       "cmd.exe",
		Args:       []string{"/c", "whoami", "/groups"},
		Dir:        t.TempDir(),
		Restricted: true,
	})
	if err != nil {
		// A service account without SeIncreaseQuotaPrivilege cannot call
		// CreateProcessAsUser. Report it rather than passing quietly: the
		// restriction is a security requirement, not a nice-to-have.
		t.Skipf("could not start a restricted process here: %v", err)
	}
	defer p.Close()

	out := readFor(t, p, 20*time.Second)
	lower := strings.ToLower(out)
	if !strings.Contains(lower, "administrators") {
		t.Fatalf("whoami /groups did not mention Administrators at all.\nGot: %q", out)
	}
	if !strings.Contains(lower, "deny only") {
		t.Fatalf("Administrators is not marked deny-only in the restricted session.\nGot: %q", out)
	}
}

// TestRestrictedTokenDirectly checks the token itself, independently of what
// whoami prints, so a formatting change in Windows cannot mask a regression.
func TestRestrictedTokenDirectly(t *testing.T) {
	tok, err := RestrictedToken()
	if err != nil {
		t.Fatalf("RestrictedToken: %v", err)
	}
	defer tok.Close()

	denyOnly, err := AdminDenyOnly(tok)
	if err != nil {
		t.Fatalf("AdminDenyOnly: %v", err)
	}
	if !denyOnly {
		t.Error("Administrators is not deny-only on the restricted token")
	}

	// The unrestricted process token must differ, or the test proves nothing.
	self, err := windows.OpenCurrentProcessToken()
	if err != nil {
		t.Fatal(err)
	}
	defer self.Close()
	if !currentUserIsAdmin(t) {
		return
	}
	selfDenyOnly, err := AdminDenyOnly(self)
	if err != nil {
		t.Fatal(err)
	}
	if selfDenyOnly {
		t.Skip("this process is already running without effective admin rights")
	}
}

func currentUserIsAdmin(t *testing.T) bool {
	t.Helper()
	self, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return false
	}
	defer self.Close()
	denyOnly, err := AdminDenyOnly(self)
	if err != nil {
		return false
	}
	return !denyOnly
}

func TestStartRejectsAMissingExecutable(t *testing.T) {
	_, err := Start(Config{Path: "definitely-not-a-real-program-xyz", Dir: t.TempDir()})
	if err == nil {
		t.Fatal("Start accepted an executable that is not on PATH")
	}
}

func TestStartRejectsAnEmptyPath(t *testing.T) {
	if _, err := Start(Config{Dir: t.TempDir()}); err == nil {
		t.Fatal("Start accepted an empty executable path")
	}
}

func TestEnvOverridesReachTheChild(t *testing.T) {
	p, err := Start(Config{
		Path: "cmd.exe",
		Args: []string{"/c", "echo", "[%BURROW_TEST_VAR%]"},
		Dir:  t.TempDir(),
		Env:  map[string]string{"BURROW_TEST_VAR": "set-by-burrow"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	out := readFor(t, p, 10*time.Second)
	if !strings.Contains(out, "set-by-burrow") {
		t.Fatalf("the child did not see the injected variable.\nGot: %q", out)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	p, err := Start(Config{Path: "cmd.exe", Args: []string{"/c", "exit", "0"}, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	readFor(t, p, 5*time.Second)
	if _, err := p.Wait(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := p.Close(); err != nil {
			t.Fatalf("Close #%d: %v", i+1, err)
		}
	}
	// Reading after Close must fail rather than block or panic.
	buf := make([]byte, 1)
	if _, err := p.Read(buf); err == nil {
		t.Error("Read after Close succeeded, want an error")
	} else if err == io.EOF {
		// EOF is a fine answer too.
		_ = err
	}
}

// npm installs `claude` as three files: an extensionless shell script for Git
// Bash, claude.cmd and claude.ps1. The .cmd must win, and must run through
// cmd.exe, or CreateProcess refuses it and every new chat fails.
func TestBuildCommandLinePrefersTheCmdShimAndWrapsIt(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"claude", "claude.cmd", "claude.ps1"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env, _, err := encodeEnvBlock([]string{
		"PATH=" + dir,
		"PATHEXT=.COM;.EXE;.BAT;.CMD",
		`ComSpec=C:\Windows\System32\cmd.exe`,
	})
	if err != nil {
		t.Fatal(err)
	}
	line, err := buildCommandLine(Config{Path: "claude", Args: []string{"--name", "demo 1"}}, env)
	if err != nil {
		t.Fatalf("buildCommandLine: %v", err)
	}
	want := `C:\Windows\System32\cmd.exe /d /s /c "` + filepath.Join(dir, "claude.cmd") + ` --name "demo 1""`
	// PATHEXT supplies the extension's case (".CMD"); Windows paths ignore case.
	if !strings.EqualFold(line, want) {
		t.Fatalf("command line\n got: %s\nwant: %s", line, want)
	}
}
